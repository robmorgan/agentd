use std::{
    collections::HashMap,
    fs,
    io::{Read, Write},
    sync::{Arc, Mutex, mpsc as std_mpsc},
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use camino::Utf8PathBuf;
use chrono::Utc;
use nix::{
    errno::Errno,
    sys::signal::{Signal, kill},
    unistd::Pid,
};
use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use tokio::sync::mpsc;
use tokio::{sync::broadcast, task};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    session::{
        ApplyState, AttachmentKind, AttachmentRecord, AttentionLevel, CreateSessionResult,
        IntegrationPolicy, MergeStatus, SessionDiff, SessionMode, SessionRecord, SessionStatus,
        WorktreeRecord, branch_name_from_title, repo_name_from_path,
    },
};

use crate::{
    db::{Database, NewSession},
    git,
    ids::generate_session_id,
    terminal_state::{GhosttyTerminalState, TerminalStateEngine},
};

#[derive(Clone)]
pub struct AppState {
    pub paths: AppPaths,
    pub db: Database,
    pub config: Config,
    runtimes: SessionRuntimeRegistry,
}

impl AppState {
    pub fn new(paths: AppPaths, db: Database, config: Config) -> Self {
        Self { paths, db, config, runtimes: SessionRuntimeRegistry::default() }
    }

    pub async fn create_session(
        &self,
        workspace: String,
        title: Option<String>,
        agent_name: String,
        model: Option<String>,
        _integration_policy: IntegrationPolicy,
    ) -> Result<CreateSessionResult> {
        let paths = self.paths.clone();
        let db = self.db.clone();
        let config = self.config.clone();
        let runtimes = self.runtimes.clone();

        task::spawn_blocking(move || {
            let workspace = Utf8PathBuf::from(workspace);
            let repo_root = git::canonical_repo_root(&workspace)?;
            let base_branch = git::current_branch(&repo_root)?;
            let agent = config.require_agent(&paths, &agent_name)?.clone();
            let repo_name = repo_name_from_path(repo_root.as_str());
            let mode = SessionMode::Execute;

            let session_id = unique_session_id(&db)?;
            let worktree = paths.worktree_path(&session_id);
            let title = normalize_session_title(title, &agent_name, &repo_name, &session_id);
            let branch = unique_branch_name(&repo_root, &title, &session_id)?;

            db.insert_session(&NewSession {
                session_id: &session_id,
                thread_id: None,
                agent: &agent_name,
                model: model.as_deref(),
                mode,
                workspace: repo_root.as_str(),
                repo_path: repo_root.as_str(),
                repo_name: &repo_name,
                title: &title,
                base_branch: &base_branch,
                branch: &branch,
                worktree: worktree.as_str(),
                integration_policy: IntegrationPolicy::ManualReview,
            })?;

            if let Err(err) = git::create_worktree(&repo_root, &base_branch, &branch, &worktree) {
                let _ = record_session_failure(&db, &session_id, err.to_string());
                return Err(err);
            }
            let launch = LaunchCommand {
                agent_name: agent_name.clone(),
                command: agent.command,
                args: agent.args,
            };
            if let Err(err) = start_session_runtime(
                &paths,
                &db,
                &config,
                &runtimes,
                SessionStartRequest {
                    session_id: &session_id,
                    repo_root: repo_root.as_str(),
                    worktree: worktree.as_str(),
                    branch: &branch,
                    title: &title,
                    launch: &launch,
                    model: model.as_deref(),
                },
            ) {
                let _ = record_session_failure(&db, &session_id, err.to_string());
                return Err(err);
            }

            Ok(CreateSessionResult {
                session_id,
                base_branch,
                branch,
                worktree: worktree.to_string(),
                status: SessionStatus::Running,
                mode,
                integration_policy: IntegrationPolicy::ManualReview,
            })
        })
        .await?
    }

    pub async fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db.get_session(&session_id)?;
            if let Some(session) = session {
                Ok(Some(refresh_merge_state(&db, session)?))
            } else {
                Ok(None)
            }
        })
        .await?
    }

    pub async fn list_sessions(&self) -> Result<Vec<SessionRecord>> {
        let db = self.db.clone();
        task::spawn_blocking(move || {
            let sessions = db.list_sessions()?;
            sessions
                .into_iter()
                .map(|session| refresh_merge_state(&db, session))
                .collect::<Result<Vec<_>>>()
        })
        .await?
    }

    pub async fn reconcile_sessions(&self) -> Result<()> {
        let sessions = self.list_sessions().await?;
        for session in sessions {
            if accepts_live_io(&session) {
                let db = self.db.clone();
                let session_id = session.session_id.clone();
                task::spawn_blocking(move || db.mark_unknown_recovered(&session_id)).await??;
            }
        }
        Ok(())
    }

    pub async fn has_running_sessions(&self) -> Result<bool> {
        let sessions = self.list_sessions().await?;
        Ok(sessions.into_iter().any(|session| accepts_live_io(&session)))
    }

    pub async fn create_worktree(&self, session_id: &str) -> Result<WorktreeRecord> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            ensure_session_not_running(&session)?;

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if worktree.exists() {
                bail!("worktree `{worktree}` already exists");
            }

            git::create_worktree(&repo_root, &session.base_branch, &session.branch, &worktree)?;

            Ok(WorktreeRecord {
                session_id: session.session_id,
                repo_path: session.repo_path,
                base_branch: session.base_branch,
                branch: session.branch,
                worktree: session.worktree,
            })
        })
        .await?
    }

    pub async fn cleanup_worktree(&self, session_id: &str) -> Result<WorktreeRecord> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            ensure_session_not_running(&session)?;
            let session = refresh_merge_state(&db, session)?;
            ensure_not_mergeable(&session, "cleanup")?;

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if !worktree.exists() {
                bail!("worktree `{worktree}` does not exist");
            }

            git::remove_worktree(&repo_root, &worktree)?;

            Ok(WorktreeRecord {
                session_id: session.session_id,
                repo_path: session.repo_path,
                base_branch: session.base_branch,
                branch: session.branch,
                worktree: session.worktree,
            })
        })
        .await?
    }

    pub async fn kill_session(&self, session_id: &str, remove: bool) -> Result<(bool, bool)> {
        let db = self.db.clone();
        let paths = self.paths.clone();
        let session_id = session_id.to_string();

        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;

            let was_running =
                session.status == SessionStatus::Running && process_exists(session.pid);
            if !was_running && !remove {
                bail!("session `{session_id}` is not running");
            }

            if was_running {
                terminate_session_process(&session_id, session.pid)?;
                let _ = db.mark_exited(&session_id, None);
            }

            if remove {
                let session = refresh_merge_state(&db, session)?;
                ensure_not_mergeable(&session, "remove")?;
                remove_session_artifacts(&paths, &session)?;
                db.delete_session(&session_id)?;
            }

            Ok((remove, was_running))
        })
        .await?
    }

    pub async fn diff_session(&self, session_id: &str) -> Result<SessionDiff> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            let session = refresh_merge_state(&db, session)?;
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if !worktree.exists() {
                bail!("worktree `{worktree}` does not exist");
            }

            let diff = git::diff_against_base(&worktree, &session.base_branch)?;
            Ok(SessionDiff {
                session_id: session.session_id,
                base_branch: session.base_branch,
                branch: session.branch,
                worktree: session.worktree,
                diff,
            })
        })
        .await?
    }

    pub async fn apply_session(&self, session_id: &str) -> Result<SessionRecord> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            let session = refresh_merge_state(&db, session)?;
            let review = merge_state_for_apply(&session)?;

            match review.merge_status {
                MergeStatus::Ready | MergeStatus::UpToDate => {}
                MergeStatus::Blocked | MergeStatus::Conflicted => bail!(review.summary),
                MergeStatus::Unknown => {
                    bail!("session `{}` is not ready to merge", session.session_id)
                }
            }

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            match git::preview_merge(&repo_root, &session.base_branch, &session.branch)? {
                git::MergePreview::NoChanges => {
                    db.set_apply_state(
                        &session.session_id,
                        ApplyState::Applied,
                        AttentionLevel::Info,
                        "changes already present on base branch",
                    )?;
                }
                git::MergePreview::HasChanges => {
                    git::apply_merge(&repo_root, &session.branch)?;
                    db.set_apply_state(
                        &session.session_id,
                        ApplyState::Applied,
                        AttentionLevel::Info,
                        &format!("changes merged into {}", session.base_branch),
                    )?;
                }
                git::MergePreview::Conflicted => bail!(manual_accept_conflict_summary(&session)),
            }
            db.get_session(&session_id)?
                .map(|session| refresh_merge_state(&db, session))
                .transpose()?
                .ok_or_else(|| anyhow!("session `{session_id}` not found after apply"))
        })
        .await?
    }

    pub async fn discard_session(&self, session_id: &str, force: bool) -> Result<SessionRecord> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            if !force {
                ensure_session_not_running(&session)?;
            }

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if worktree.exists() {
                git::remove_worktree(&repo_root, &worktree)?;
            }
            db.set_apply_state(
                &session.session_id,
                ApplyState::Discarded,
                AttentionLevel::Info,
                "changes discarded",
            )?;
            db.get_session(&session_id)?
                .map(|session| refresh_merge_state(&db, session))
                .transpose()?
                .ok_or_else(|| anyhow!("session `{session_id}` not found after discard"))
        })
        .await?
    }

    pub async fn send_input(
        &self,
        session_id: &str,
        data: Vec<u8>,
        source_session_id: Option<String>,
    ) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;
        runtime.write_input(&data)?;

        let _ = source_session_id;
        Ok(())
    }

    pub async fn write_attached_input(&self, session_id: &str, data: Vec<u8>) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;
        runtime.write_input(&data)
    }

    pub async fn resize_attached_session(
        &self,
        session_id: &str,
        cols: u16,
        rows: u16,
    ) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;
        runtime.resize(cols, rows)
    }

    pub async fn attach_session(
        &self,
        session_id: &str,
        kind: AttachmentKind,
        cols: u16,
        rows: u16,
    ) -> Result<(
        AttachmentHandle,
        AttachmentRecord,
        Vec<u8>,
        broadcast::Receiver<Vec<u8>>,
        mpsc::UnboundedReceiver<AttachControl>,
    )> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;
        let (handle, attachment, snapshot, receiver, control_rx) =
            runtime.attach(session_id, kind, cols, rows)?;
        Ok((handle, attachment, snapshot, receiver, control_rx))
    }

    pub async fn list_attachments(&self, session_id: &str) -> Result<Vec<AttachmentRecord>> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;
        runtime.list_attachments(session_id)
    }

    pub async fn get_history(&self, session_id: &str, vt: bool) -> Result<String> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;

        if let Some(runtime) = self.runtimes.get(session_id) {
            let history = runtime.history()?;
            return Ok(if vt { history.vt } else { history.plain });
        }

        if let Some(history) = self.runtimes.get_history(session_id) {
            return Ok(if vt { history.vt } else { history.plain });
        }

        bail!(
            "history for session `{}` is not available; it is only retained until the daemon restarts",
            session.session_id
        )
    }

    pub async fn detach_session(&self, session_id: &str, all: bool) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;

        if !all {
            bail!(
                "shared attach no longer supports per-client detach by session id; use Ctrl-], close the local UI, or `agent detach {session_id} --attach <attach_id>`"
            );
        }

        runtime.request_detach_all()
    }

    pub async fn detach_attachment(&self, session_id: &str, attach_id: &str) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;

        runtime.request_detach(attach_id)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LaunchCommand {
    pub agent_name: String,
    pub command: String,
    pub args: Vec<String>,
}

struct SessionStartRequest<'a> {
    session_id: &'a str,
    repo_root: &'a str,
    worktree: &'a str,
    branch: &'a str,
    title: &'a str,
    launch: &'a LaunchCommand,
    model: Option<&'a str>,
}

fn start_session_runtime(
    paths: &AppPaths,
    db: &Database,
    config: &Config,
    runtimes: &SessionRuntimeRegistry,
    request: SessionStartRequest<'_>,
) -> Result<()> {
    let pty_system = native_pty_system();
    let pair = pty_system
        .openpty(PtySize {
            rows: DEFAULT_PTY_ROWS,
            cols: DEFAULT_PTY_COLS,
            pixel_width: 0,
            pixel_height: 0,
        })
        .context("failed to allocate PTY")?;

    let mut command = CommandBuilder::new(&request.launch.command);
    configure_spawn_command(&mut command, &request, config)?;
    command.cwd(request.worktree);
    for (key, value) in std::env::vars() {
        command.env(&key, &value);
    }
    command.env("AGENTD_SESSION_ID", request.session_id);
    command.env("AGENTD_SOCKET", paths.socket.as_str());
    command.env("AGENTD_WORKSPACE", request.repo_root);
    command.env("AGENTD_WORKTREE", request.worktree);
    command.env("AGENTD_BRANCH", request.branch);
    command.env("AGENTD_TITLE", request.title);

    let mut child = pair
        .slave
        .spawn_command(command)
        .map_err(|err| anyhow!(err).context("failed to spawn agent process"))?;

    let pid = child.process_id().map(|value| value as u32).unwrap_or(0);
    db.mark_running(request.session_id, pid)?;

    let reader = pair.master.try_clone_reader().context("failed to clone PTY reader")?;
    let writer = pair.master.take_writer().context("failed to clone PTY writer")?;
    let terminal_state =
        GhosttyTerminalState::new(DEFAULT_PTY_COLS, DEFAULT_PTY_ROWS, MAX_SCROLLBACK_BYTES)
            .context("failed to initialize libghostty-vt state")?;
    let runtime = runtimes.insert(
        request.session_id.to_string(),
        SessionRuntime::new(pair.master, writer, Box::new(terminal_state), OUTPUT_BUFFER_CAPACITY),
    );
    let writer_db = db.clone();
    let writer_session_id = request.session_id.to_string();
    let writer_runtime = runtime.clone();
    let (writer_done_tx, writer_done_rx) = std_mpsc::channel();
    std::thread::spawn(move || {
        let result = pump_pty(reader, &writer_runtime);
        if let Err(err) = result {
            let _ = record_session_failure(&writer_db, &writer_session_id, err.to_string());
        }
        let _ = writer_done_tx.send(());
    });

    let exit_db = db.clone();
    let exit_session_id = request.session_id.to_string();
    let exit_runtimes = runtimes.clone();
    std::thread::spawn(move || {
        let status = child.wait();
        let code = status.ok().map(|value| value.exit_code() as i32);
        exit_runtimes.freeze_history(&exit_session_id);
        exit_runtimes.remove(&exit_session_id);
        let _ = writer_done_rx.recv();
        let _ = finalize_session_exit(&exit_db, &exit_session_id, code);
    });

    Ok(())
}

const OUTPUT_BUFFER_CAPACITY: usize = 256;
const DEFAULT_PTY_ROWS: u16 = 48;
const DEFAULT_PTY_COLS: u16 = 160;
const MAX_SCROLLBACK_BYTES: usize = 10_000_000;

#[derive(Clone, Default)]
struct SessionRuntimeRegistry {
    inner: Arc<Mutex<HashMap<String, Arc<SessionRuntime>>>>,
    history: Arc<Mutex<HashMap<String, SessionHistory>>>,
}

#[derive(Clone, Debug)]
struct SessionHistory {
    plain: String,
    vt: String,
}

impl SessionRuntimeRegistry {
    fn insert(&self, session_id: String, runtime: SessionRuntime) -> Arc<SessionRuntime> {
        let runtime = Arc::new(runtime);
        {
            let mut history = self.history.lock().expect("session history cache poisoned");
            history.remove(&session_id);
        }
        let mut inner = self.inner.lock().expect("session runtime registry poisoned");
        inner.insert(session_id, runtime.clone());
        runtime
    }

    fn get(&self, session_id: &str) -> Option<Arc<SessionRuntime>> {
        let inner = self.inner.lock().expect("session runtime registry poisoned");
        inner.get(session_id).cloned()
    }

    fn remove(&self, session_id: &str) {
        let mut inner = self.inner.lock().expect("session runtime registry poisoned");
        inner.remove(session_id);
    }

    fn freeze_history(&self, session_id: &str) {
        let runtime = {
            let inner = self.inner.lock().expect("session runtime registry poisoned");
            inner.get(session_id).cloned()
        };
        let Some(runtime) = runtime else {
            return;
        };
        if let Ok(history) = runtime.history() {
            let mut cache = self.history.lock().expect("session history cache poisoned");
            cache.insert(session_id.to_string(), history);
        }
    }

    fn get_history(&self, session_id: &str) -> Option<SessionHistory> {
        let history = self.history.lock().expect("session history cache poisoned");
        history.get(session_id).cloned()
    }
}

struct SessionRuntime {
    master: Mutex<Box<dyn MasterPty + Send>>,
    writer: Mutex<Box<dyn Write + Send>>,
    state: Mutex<SessionRuntimeState>,
    output_tx: broadcast::Sender<Vec<u8>>,
}

impl SessionRuntime {
    fn new(
        master: Box<dyn MasterPty + Send>,
        writer: Box<dyn Write + Send>,
        terminal_state: Box<dyn TerminalStateEngine>,
        output_buffer_capacity: usize,
    ) -> Self {
        let (output_tx, _) = broadcast::channel(output_buffer_capacity);
        Self {
            master: Mutex::new(master),
            writer: Mutex::new(writer),
            state: Mutex::new(SessionRuntimeState {
                terminal_state,
                has_client_dimensions: false,
                next_attach_ordinal: 1,
                attachments: HashMap::new(),
            }),
            output_tx,
        }
    }

    fn write_input(&self, data: &[u8]) -> Result<()> {
        let mut writer = self.writer.lock().map_err(|_| anyhow!("PTY writer poisoned"))?;
        writer.write_all(data)?;
        writer.flush()?;
        Ok(())
    }

    fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        let master = self.master.lock().map_err(|_| anyhow!("PTY master poisoned"))?;
        master.resize(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })?;
        drop(master);
        let mut state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        state.terminal_state.resize(cols, rows)?;
        state.has_client_dimensions = true;
        Ok(())
    }

    fn publish_output(&self, data: &[u8]) -> Result<()> {
        let mut state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        state.terminal_state.feed(data)?;
        let _ = self.output_tx.send(data.to_vec());
        Ok(())
    }

    fn history(&self) -> Result<SessionHistory> {
        let mut state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        Ok(SessionHistory {
            plain: state.terminal_state.format_plain()?,
            vt: state.terminal_state.format_vt()?,
        })
    }

    fn attach(
        self: &Arc<Self>,
        session_id: &str,
        kind: AttachmentKind,
        cols: u16,
        rows: u16,
    ) -> Result<(
        AttachmentHandle,
        AttachmentRecord,
        Vec<u8>,
        broadcast::Receiver<Vec<u8>>,
        mpsc::UnboundedReceiver<AttachControl>,
    )> {
        let mut state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        let attach_id = format!("{}-{}", kind.as_str(), state.next_attach_ordinal);
        state.next_attach_ordinal += 1;
        let had_client_dimensions = state.has_client_dimensions;
        let connected_at = Utc::now();
        drop(state);
        let snapshot = if had_client_dimensions {
            let snapshot = {
                let mut state =
                    self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
                state.terminal_state.vt_snapshot()?
            };
            self.resize(cols, rows)?;
            snapshot
        } else {
            self.resize(cols, rows)?;
            let mut state =
                self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
            state.terminal_state.vt_snapshot()?
        };
        let mut state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        let (control_tx, control_rx) = mpsc::unbounded_channel();
        state
            .attachments
            .insert(attach_id.clone(), RuntimeAttachment { kind, connected_at, control_tx });
        let receiver = self.output_tx.subscribe();
        Ok((
            AttachmentHandle { runtime: self.clone(), attach_id: attach_id.clone() },
            AttachmentRecord { attach_id, session_id: session_id.to_string(), kind, connected_at },
            snapshot,
            receiver,
            control_rx,
        ))
    }

    fn list_attachments(&self, session_id: &str) -> Result<Vec<AttachmentRecord>> {
        let state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        let mut attachments = state
            .attachments
            .iter()
            .map(|(attach_id, attachment)| AttachmentRecord {
                attach_id: attach_id.clone(),
                session_id: session_id.to_string(),
                kind: attachment.kind,
                connected_at: attachment.connected_at,
            })
            .collect::<Vec<_>>();
        attachments.sort_by(|left, right| left.connected_at.cmp(&right.connected_at));
        Ok(attachments)
    }

    fn request_detach(&self, attach_id: &str) -> Result<()> {
        let state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        let attachment = state
            .attachments
            .get(attach_id)
            .ok_or_else(|| anyhow!("attachment `{attach_id}` not found"))?;
        attachment
            .control_tx
            .send(AttachControl::Detach)
            .map_err(|_| anyhow!("attachment `{attach_id}` is no longer connected"))?;
        Ok(())
    }

    fn request_detach_all(&self) -> Result<()> {
        let state = self.state.lock().map_err(|_| anyhow!("session runtime state poisoned"))?;
        for (attach_id, attachment) in &state.attachments {
            attachment
                .control_tx
                .send(AttachControl::Detach)
                .map_err(|_| anyhow!("attachment `{attach_id}` is no longer connected"))?;
        }
        Ok(())
    }

    fn remove_attachment(&self, attach_id: &str) {
        if let Ok(mut state) = self.state.lock() {
            state.attachments.remove(attach_id);
        }
    }
}

struct SessionRuntimeState {
    terminal_state: Box<dyn TerminalStateEngine>,
    has_client_dimensions: bool,
    next_attach_ordinal: u64,
    attachments: HashMap<String, RuntimeAttachment>,
}

struct RuntimeAttachment {
    kind: AttachmentKind,
    connected_at: chrono::DateTime<Utc>,
    control_tx: mpsc::UnboundedSender<AttachControl>,
}

pub struct AttachmentHandle {
    runtime: Arc<SessionRuntime>,
    attach_id: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AttachControl {
    Detach,
}

impl Drop for AttachmentHandle {
    fn drop(&mut self) {
        self.runtime.remove_attachment(&self.attach_id);
    }
}

fn unique_session_id(db: &Database) -> Result<String> {
    for _ in 0..16 {
        let candidate = generate_session_id();
        if db.get_session(&candidate)?.is_none() {
            return Ok(candidate);
        }
    }
    bail!("failed to allocate a unique session id")
}

fn record_session_failure(db: &Database, session_id: &str, error: String) -> Result<()> {
    db.mark_failed(session_id, error)
}

fn finalize_session_exit(db: &Database, session_id: &str, exit_code: Option<i32>) -> Result<()> {
    let Some(session) = db.get_session(session_id)? else {
        return Ok(());
    };
    if session.status == SessionStatus::Failed {
        return Ok(());
    }

    let has_changes = session_has_pending_changes(&session)?;
    if exit_code == Some(0) {
        db.mark_exited(session_id, exit_code)?;
        if has_changes {
            finalize_mergeable_session(db, &session)?;
        }
    } else {
        let error = match exit_code {
            Some(code) => format!("agent exited with code {code}"),
            None => "agent exited unexpectedly".to_string(),
        };
        db.mark_failed(session_id, error)?;
    }
    Ok(())
}

fn finalize_mergeable_session(db: &Database, session: &SessionRecord) -> Result<()> {
    let worktree = Utf8PathBuf::from(&session.worktree);
    if !worktree.exists() {
        db.set_apply_state(
            &session.session_id,
            ApplyState::Idle,
            AttentionLevel::Action,
            &format!("worktree {} is missing; recreate or discard it", session.worktree),
        )?;
        return Ok(());
    }

    if git::has_worktree_changes(&worktree)? {
        git::commit_all(&worktree, &auto_commit_message(session))?;
    }

    let merge = inspect_merge_state(session)?;
    let attention = match merge.merge_status {
        MergeStatus::Ready => AttentionLevel::Notice,
        MergeStatus::Blocked => AttentionLevel::Notice,
        MergeStatus::Conflicted => AttentionLevel::Action,
        MergeStatus::UpToDate => AttentionLevel::Info,
        MergeStatus::Unknown => AttentionLevel::Action,
    };
    let summary = match merge.merge_status {
        MergeStatus::Unknown => "could not determine merge readiness".to_string(),
        _ => merge.summary,
    };
    db.set_apply_state(&session.session_id, ApplyState::Idle, attention, &summary)?;

    if merge.merge_status == MergeStatus::Conflicted {
        return Ok(());
    }

    Ok(())
}

fn session_has_pending_changes(session: &SessionRecord) -> Result<bool> {
    let worktree = Utf8PathBuf::from(&session.worktree);
    if !worktree.exists() {
        return Ok(false);
    }
    Ok(git::has_worktree_changes(&worktree)?
        || git::has_committed_diff_against_base(&worktree, &session.base_branch)?)
}

fn auto_commit_message(session: &SessionRecord) -> String {
    format!("agentd: finalize session {}", session.session_id)
}

fn normalize_session_title(
    title: Option<String>,
    agent_name: &str,
    repo_name: &str,
    session_id: &str,
) -> String {
    let trimmed = title.as_deref().map(str::trim).unwrap_or_default();
    if !trimmed.is_empty() {
        return trimmed.to_string();
    }

    let short_id = session_id.split('-').next_back().unwrap_or(session_id);
    format!("{agent_name} @ {repo_name} ({short_id})")
}

fn unique_branch_name(
    repo_root: &camino::Utf8Path,
    title: &str,
    session_id: &str,
) -> Result<String> {
    let base = branch_name_from_title(title);
    if !git::branch_exists(repo_root, &base)? {
        return Ok(base);
    }

    let suffix = session_id.split('-').next_back().unwrap_or(session_id);
    let branch = format!("{base}-{suffix}");
    if git::branch_exists(repo_root, &branch)? {
        bail!("branch `{branch}` already exists")
    }
    Ok(branch)
}

fn configure_spawn_command(
    command: &mut CommandBuilder,
    request: &SessionStartRequest<'_>,
    config: &Config,
) -> Result<()> {
    command.args(request.launch.args.clone());
    if let Some(model) = request.model {
        if let Some(flag) = config
            .agents
            .get(&request.launch.agent_name)
            .and_then(|agent| agent.model_flag.as_deref())
        {
            command.arg(flag);
            command.arg(model);
        }
    }
    Ok(())
}

fn pump_pty(mut reader: Box<dyn Read + Send>, runtime: &SessionRuntime) -> Result<()> {
    let mut buffer = [0_u8; 8192];

    loop {
        let bytes_read = reader.read(&mut buffer)?;
        if bytes_read == 0 {
            break;
        }
        runtime.publish_output(&buffer[..bytes_read])?;
    }

    Ok(())
}

fn terminate_session_process(session_id: &str, pid: Option<u32>) -> Result<()> {
    let pid = pid.ok_or_else(|| anyhow!("session `{session_id}` has no recorded pid"))?;
    if pid == 0 {
        bail!("session `{session_id}` has an invalid pid");
    }

    let pid = Pid::from_raw(pid as i32);
    send_signal(pid, Signal::SIGTERM, session_id)?;
    if wait_for_exit(pid, Duration::from_secs(5)) {
        return Ok(());
    }

    send_signal(pid, Signal::SIGKILL, session_id)?;
    if wait_for_exit(pid, Duration::from_secs(5)) {
        return Ok(());
    }

    bail!("session `{session_id}` did not exit after SIGTERM and SIGKILL")
}

fn send_signal(pid: Pid, signal: Signal, session_id: &str) -> Result<()> {
    match kill(pid, Some(signal)) {
        Ok(()) => Ok(()),
        Err(Errno::ESRCH) => bail!("session `{session_id}` is not running"),
        Err(err) => Err(anyhow!(err))
            .context(format!("failed to send {signal:?} to session `{session_id}`")),
    }
}

fn wait_for_exit(pid: Pid, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    loop {
        match kill(pid, None) {
            Ok(()) => {}
            Err(Errno::ESRCH) => return true,
            Err(_) => return false,
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

fn process_exists(pid: Option<u32>) -> bool {
    let Some(pid) = pid else {
        return false;
    };
    if pid == 0 {
        return false;
    }

    nix::sys::signal::kill(nix::unistd::Pid::from_raw(pid as i32), None).is_ok()
}

fn remove_session_artifacts(paths: &AppPaths, session: &SessionRecord) -> Result<()> {
    remove_worktree_if_present(session)?;
    remove_log_if_present(paths, session)?;
    Ok(())
}

fn remove_worktree_if_present(session: &SessionRecord) -> Result<()> {
    let repo_root = Utf8PathBuf::from(session.repo_path.clone());
    let worktree = Utf8PathBuf::from(session.worktree.clone());
    if worktree.exists() {
        git::remove_worktree(&repo_root, &worktree)?;
    }
    Ok(())
}

fn remove_log_if_present(paths: &AppPaths, session: &SessionRecord) -> Result<()> {
    for log_path in
        [paths.log_path(&session.session_id), paths.rendered_log_path(&session.session_id)]
    {
        match fs::remove_file(log_path.as_std_path()) {
            Ok(()) => {}
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
            Err(err) => return Err(anyhow!(err)).context(format!("failed to remove {}", log_path)),
        }
    }
    Ok(())
}

fn ensure_session_running(session: &SessionRecord) -> Result<()> {
    if !accepts_live_io(session) {
        bail!("session `{}` is not running", session.session_id);
    }
    Ok(())
}

fn ensure_session_not_running(session: &SessionRecord) -> Result<()> {
    if accepts_live_io(session) {
        bail!("session `{}` is still running", session.session_id);
    }
    Ok(())
}

fn accepts_live_io(session: &SessionRecord) -> bool {
    matches!(session.status, SessionStatus::Running | SessionStatus::NeedsInput)
        && process_exists(session.pid)
}

#[derive(Debug, Clone)]
struct MergeState {
    merge_status: MergeStatus,
    summary: String,
    has_conflicts: bool,
}

fn ensure_not_mergeable(session: &SessionRecord, action: &str) -> Result<()> {
    match session.merge_status {
        MergeStatus::Ready | MergeStatus::Blocked | MergeStatus::Conflicted => {
            let summary = session
                .merge_summary
                .clone()
                .unwrap_or_else(|| "session has mergeable committed work".to_string());
            bail!(
                "session `{}` has mergeable committed work; use `agent diff {}` and `agent merge {}` before {action}, or `agent discard {}` to drop it\n{}",
                session.session_id,
                session.session_id,
                session.session_id,
                session.session_id,
                summary
            );
        }
        MergeStatus::Unknown | MergeStatus::UpToDate => Ok(()),
    }
}

fn refresh_merge_state(_db: &Database, mut session: SessionRecord) -> Result<SessionRecord> {
    let merge = inspect_merge_state(&session)?;
    session.merge_status = merge.merge_status;
    session.merge_summary = Some(merge.summary);
    session.has_conflicts = merge.has_conflicts;
    Ok(session)
}

fn inspect_merge_state(session: &SessionRecord) -> Result<MergeState> {
    let worktree = Utf8PathBuf::from(session.worktree.clone());
    if !worktree.exists() {
        return Ok(MergeState {
            merge_status: MergeStatus::Blocked,
            summary: format!("worktree {} is missing; recreate or discard it", session.worktree),
            has_conflicts: false,
        });
    }

    let repo_root = Utf8PathBuf::from(session.repo_path.clone());
    let current_branch = git::current_branch(&repo_root)?;
    if git::has_worktree_changes(&repo_root)? {
        let summary = if current_branch == session.base_branch {
            format!(
                "upstream checkout {} on {} has uncommitted changes; clean it before merge",
                session.repo_path, session.base_branch
            )
        } else {
            format!(
                "repo {} has local changes on {}; clean it before merge",
                session.repo_path, current_branch
            )
        };
        return Ok(MergeState {
            merge_status: MergeStatus::Blocked,
            summary,
            has_conflicts: false,
        });
    }

    if current_branch != session.base_branch {
        return Ok(MergeState {
            merge_status: MergeStatus::Blocked,
            summary: format!(
                "repo is on {}; switch to {} before merge",
                current_branch, session.base_branch
            ),
            has_conflicts: false,
        });
    }

    let has_worktree_changes = git::has_worktree_changes(&worktree)?;
    if !has_worktree_changes
        && !git::has_committed_diff_against_base(&worktree, &session.base_branch)?
    {
        return Ok(MergeState {
            merge_status: MergeStatus::UpToDate,
            summary: "branch is already up to date with base".to_string(),
            has_conflicts: false,
        });
    }

    let dirty_suffix = if has_worktree_changes {
        " Uncommitted worktree changes are excluded from merge."
    } else {
        ""
    };

    match git::preview_merge(&repo_root, &session.base_branch, &session.branch)? {
        git::MergePreview::NoChanges => Ok(MergeState {
            merge_status: MergeStatus::UpToDate,
            summary: format!("changes already present on base branch{dirty_suffix}"),
            has_conflicts: false,
        }),
        git::MergePreview::HasChanges => Ok(MergeState {
            merge_status: MergeStatus::Ready,
            summary: format!("ready to merge into {}.{dirty_suffix}", session.base_branch),
            has_conflicts: false,
        }),
        git::MergePreview::Conflicted => Ok(MergeState {
            merge_status: MergeStatus::Conflicted,
            summary: format!("{}{}", manual_accept_conflict_summary(session), dirty_suffix),
            has_conflicts: true,
        }),
    }
}

fn merge_state_for_apply(session: &SessionRecord) -> Result<MergeState> {
    inspect_merge_state(session)
}

fn manual_accept_conflict_summary(session: &SessionRecord) -> String {
    format!(
        "merge would conflict with {}\nrun:\n  git -C {} checkout {}\n  git -C {} merge {}",
        session.base_branch,
        shell_quote(&session.repo_path),
        shell_quote(&session.base_branch),
        shell_quote(&session.repo_path),
        shell_quote(&session.branch),
    )
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

#[cfg(test)]
mod tests {
    use super::{
        AttachControl, SessionRuntime, finalize_session_exit, inspect_merge_state,
        manual_accept_conflict_summary, merge_state_for_apply, shell_quote,
    };
    use crate::app::SessionRuntimeRegistry;
    use crate::db::{Database, NewSession};
    use crate::terminal_state::TerminalStateEngine;
    use agentd_shared::{
        paths::AppPaths,
        session::{
            ApplyState, AttachmentKind, IntegrationPolicy, MergeStatus, SessionMode, SessionRecord,
            SessionStatus,
        },
    };
    use anyhow::{Error, Result};
    use nix::libc;
    use portable_pty::{MasterPty, PtySize};
    use std::{
        fs,
        io::{Read, Write},
        process::Command,
        sync::atomic::{AtomicU64, Ordering},
        sync::{Arc, Mutex},
        time::{SystemTime, UNIX_EPOCH},
    };

    static TEST_PATH_COUNTER: AtomicU64 = AtomicU64::new(0);

    struct StubTerminalState;

    impl TerminalStateEngine for StubTerminalState {
        fn feed(&mut self, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        fn resize(&mut self, _cols: u16, _rows: u16) -> Result<()> {
            Ok(())
        }

        fn vt_snapshot(&mut self) -> Result<Vec<u8>> {
            Ok(b"snapshot".to_vec())
        }

        fn format_plain(&mut self) -> Result<String> {
            Ok("plain".to_string())
        }

        fn format_vt(&mut self) -> Result<String> {
            Ok("vt".to_string())
        }
    }

    #[derive(Debug)]
    struct StubMasterPty;

    impl MasterPty for StubMasterPty {
        fn resize(&self, _size: PtySize) -> std::result::Result<(), Error> {
            Ok(())
        }

        fn get_size(&self) -> std::result::Result<PtySize, Error> {
            Ok(PtySize { rows: 24, cols: 80, pixel_width: 0, pixel_height: 0 })
        }

        fn try_clone_reader(&self) -> std::result::Result<Box<dyn Read + Send>, Error> {
            Ok(Box::new(std::io::empty()))
        }

        fn take_writer(&self) -> std::result::Result<Box<dyn Write + Send>, Error> {
            Ok(Box::new(std::io::sink()))
        }

        #[cfg(unix)]
        fn process_group_leader(&self) -> Option<libc::pid_t> {
            None
        }

        #[cfg(unix)]
        fn as_raw_fd(&self) -> Option<std::os::fd::RawFd> {
            None
        }

        #[cfg(unix)]
        fn tty_name(&self) -> Option<std::path::PathBuf> {
            None
        }
    }

    #[derive(Debug, Default)]
    struct RecordedOps {
        events: Mutex<Vec<String>>,
    }

    struct RecordingTerminalState {
        ops: Arc<RecordedOps>,
    }

    impl TerminalStateEngine for RecordingTerminalState {
        fn feed(&mut self, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        fn resize(&mut self, cols: u16, rows: u16) -> Result<()> {
            self.ops.events.lock().unwrap().push(format!("terminal_resize:{cols}x{rows}"));
            Ok(())
        }

        fn vt_snapshot(&mut self) -> Result<Vec<u8>> {
            self.ops.events.lock().unwrap().push("snapshot".to_string());
            Ok(b"snapshot".to_vec())
        }

        fn format_plain(&mut self) -> Result<String> {
            Ok("plain".to_string())
        }

        fn format_vt(&mut self) -> Result<String> {
            Ok("vt".to_string())
        }
    }

    #[derive(Debug)]
    struct RecordingMasterPty {
        ops: Arc<RecordedOps>,
    }

    impl MasterPty for RecordingMasterPty {
        fn resize(&self, size: PtySize) -> std::result::Result<(), Error> {
            self.ops.events.lock().unwrap().push(format!("pty_resize:{}x{}", size.cols, size.rows));
            Ok(())
        }

        fn get_size(&self) -> std::result::Result<PtySize, Error> {
            Ok(PtySize { rows: 24, cols: 80, pixel_width: 0, pixel_height: 0 })
        }

        fn try_clone_reader(&self) -> std::result::Result<Box<dyn Read + Send>, Error> {
            Ok(Box::new(std::io::empty()))
        }

        fn take_writer(&self) -> std::result::Result<Box<dyn Write + Send>, Error> {
            Ok(Box::new(std::io::sink()))
        }

        #[cfg(unix)]
        fn process_group_leader(&self) -> Option<libc::pid_t> {
            None
        }

        #[cfg(unix)]
        fn as_raw_fd(&self) -> Option<std::os::fd::RawFd> {
            None
        }

        #[cfg(unix)]
        fn tty_name(&self) -> Option<std::path::PathBuf> {
            None
        }
    }

    #[test]
    fn request_detach_requires_active_attacher() {
        let runtime = SessionRuntime::new(
            Box::new(StubMasterPty),
            Box::new(std::io::sink()),
            Box::new(StubTerminalState),
            8,
        );
        let err = runtime.request_detach("attach-1").unwrap_err();
        assert_eq!(err.to_string(), "attachment `attach-1` not found");
    }

    #[test]
    fn request_detach_notifies_attached_client() {
        let runtime = Arc::new(SessionRuntime::new(
            Box::new(StubMasterPty),
            Box::new(std::io::sink()),
            Box::new(StubTerminalState),
            8,
        ));
        let (_handle, attachment, snapshot, _output_rx, mut control_rx) =
            runtime.attach("demo", AttachmentKind::Attach, 120, 48).unwrap();
        assert_eq!(snapshot, b"snapshot".to_vec());

        runtime.request_detach(&attachment.attach_id).unwrap();

        let control = control_rx.try_recv().unwrap();
        assert_eq!(control, AttachControl::Detach);
    }

    #[test]
    fn multiple_attachments_are_allowed() {
        let runtime = Arc::new(SessionRuntime::new(
            Box::new(StubMasterPty),
            Box::new(std::io::sink()),
            Box::new(StubTerminalState),
            8,
        ));
        let (_first_handle, first, _snapshot, _first_output, _first_control) =
            runtime.attach("demo", AttachmentKind::Attach, 120, 48).unwrap();
        let (_second_handle, second, _snapshot, _second_output, _second_control) =
            runtime.attach("demo", AttachmentKind::Tui, 120, 48).unwrap();

        assert_ne!(first.attach_id, second.attach_id);
        assert_eq!(runtime.list_attachments("demo").unwrap().len(), 2);
    }

    #[test]
    fn first_attach_resizes_before_snapshot() {
        let ops = Arc::new(RecordedOps::default());
        let runtime = Arc::new(SessionRuntime::new(
            Box::new(RecordingMasterPty { ops: ops.clone() }),
            Box::new(std::io::sink()),
            Box::new(RecordingTerminalState { ops: ops.clone() }),
            8,
        ));

        let _ = runtime.attach("demo", AttachmentKind::Attach, 120, 48).unwrap();

        let events = ops.events.lock().unwrap().clone();
        assert_eq!(
            events,
            vec![
                "pty_resize:120x48".to_string(),
                "terminal_resize:120x48".to_string(),
                "snapshot".to_string(),
            ]
        );
    }

    #[test]
    fn reattach_snapshots_before_resize() {
        let ops = Arc::new(RecordedOps::default());
        let runtime = Arc::new(SessionRuntime::new(
            Box::new(RecordingMasterPty { ops: ops.clone() }),
            Box::new(std::io::sink()),
            Box::new(RecordingTerminalState { ops: ops.clone() }),
            8,
        ));

        let _ = runtime.attach("demo", AttachmentKind::Attach, 120, 48).unwrap();
        ops.events.lock().unwrap().clear();

        let _ = runtime.attach("demo", AttachmentKind::Attach, 160, 50).unwrap();

        let events = ops.events.lock().unwrap().clone();
        assert_eq!(
            events,
            vec![
                "snapshot".to_string(),
                "pty_resize:160x50".to_string(),
                "terminal_resize:160x50".to_string(),
            ]
        );
    }

    #[test]
    fn runtime_history_uses_terminal_state_formats() {
        let runtime = SessionRuntime::new(
            Box::new(StubMasterPty),
            Box::new(std::io::sink()),
            Box::new(StubTerminalState),
            8,
        );

        let history = runtime.history().unwrap();

        assert_eq!(history.plain, "plain");
        assert_eq!(history.vt, "vt");
    }

    #[test]
    fn freeze_history_caches_runtime_output() {
        let registry = SessionRuntimeRegistry::default();
        registry.insert(
            "demo".to_string(),
            SessionRuntime::new(
                Box::new(StubMasterPty),
                Box::new(std::io::sink()),
                Box::new(StubTerminalState),
                8,
            ),
        );

        registry.freeze_history("demo");
        registry.remove("demo");

        let history = registry.get_history("demo").unwrap();
        assert_eq!(history.plain, "plain");
        assert_eq!(history.vt, "vt");
    }

    #[test]
    fn finalize_session_exit_marks_clean_sessions_complete() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "clean");
        finalize_session_exit(&db, "demo", Some(0)).unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, agentd_shared::session::SessionStatus::Exited);
        assert_eq!(session.apply_state, ApplyState::Idle);
    }

    #[test]
    fn finalize_session_exit_commits_changes_and_leaves_session_mergeable() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        let worktree = paths.root.join("worktree");
        init_git_repo(repo.as_str());

        insert_session_with_worktree(&db, repo.as_str(), worktree.as_str(), "demo", "changed");
        fs::write(worktree.join("README.md"), "updated\n").unwrap();
        finalize_session_exit(&db, "demo", Some(0)).unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, agentd_shared::session::SessionStatus::Exited);
        assert_eq!(session.apply_state, ApplyState::Idle);
        let review = inspect_merge_state(&session).unwrap();
        assert_eq!(review.merge_status, MergeStatus::Ready);
    }

    #[test]
    fn inspect_merge_state_allows_dirty_session_worktree_but_excludes_it() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        let worktree = paths.root.join("worktree");
        init_git_repo(repo.as_str());

        insert_session_with_worktree(&db, repo.as_str(), worktree.as_str(), "demo", "changed");
        fs::write(worktree.join("README.md"), "committed change\n").unwrap();
        commit_all(worktree.as_str(), "session change");
        fs::write(worktree.join("README.md"), "repo dirty\n").unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        let review = inspect_merge_state(&session).unwrap();
        assert_eq!(review.merge_status, MergeStatus::Ready);
        assert!(review.summary.contains("excluded from merge"));
    }

    #[test]
    fn merge_state_for_apply_detects_conflicts() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        let worktree = paths.root.join("worktree");
        init_git_repo(repo.as_str());

        insert_session_with_worktree(&db, repo.as_str(), worktree.as_str(), "demo", "changed");
        assert!(
            Command::new("git")
                .args(["-C", worktree.as_str(), "checkout", "agent/demo"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(worktree.join("README.md"), "agent version\n").unwrap();
        commit_all(worktree.as_str(), "session change");
        assert!(
            Command::new("git")
                .args(["-C", repo.as_str(), "checkout", "main"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(repo.join("README.md"), "base version\n").unwrap();
        commit_all(repo.as_str(), "base change");

        let session = db.get_session("demo").unwrap().unwrap();
        let review = merge_state_for_apply(&session).unwrap();
        assert_eq!(review.merge_status, MergeStatus::Conflicted);
        assert!(review.has_conflicts);
        assert!(review.summary.contains("git -C"));
        assert!(review.summary.contains("checkout 'main'"));
        assert!(review.summary.contains("merge 'agent/demo'"));
    }

    #[test]
    fn inspect_merge_state_blocks_dirty_upstream_checkout() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        let worktree = paths.root.join("worktree");
        init_git_repo(repo.as_str());

        insert_session_with_worktree(&db, repo.as_str(), worktree.as_str(), "demo", "changed");
        fs::write(repo.join("README.md"), "repo dirty\n").unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        let review = inspect_merge_state(&session).unwrap();
        assert_eq!(review.merge_status, MergeStatus::Blocked);
        assert!(review.summary.contains("upstream checkout"));
        assert!(review.summary.contains("main"));
    }

    #[test]
    fn manual_accept_conflict_summary_shell_quotes_values() {
        let session = SessionRecord {
            session_id: "demo".to_string(),
            thread_id: None,
            agent: "codex".to_string(),
            model: Some("gpt-5.3-codex".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/work".to_string(),
            repo_path: "/tmp/repo path/it's".to_string(),
            repo_name: "repo".to_string(),
            title: "ship it's".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status: SessionStatus::Exited,
            integration_policy: IntegrationPolicy::AutoApplySafe,
            apply_state: ApplyState::Idle,
            merge_status: MergeStatus::Conflicted,
            merge_summary: None,
            has_conflicts: true,
            pid: None,
            exit_code: Some(0),
            error: None,
            attention: agentd_shared::session::AttentionLevel::Notice,
            attention_summary: None,
            created_at: chrono::Utc::now(),
            updated_at: chrono::Utc::now(),
            exited_at: None,
        };

        let summary = manual_accept_conflict_summary(&session);
        assert!(summary.contains("git -C '/tmp/repo path/it'\"'\"'s' checkout 'main'"));
        assert!(summary.contains("git -C '/tmp/repo path/it'\"'\"'s' merge 'agent/demo'"));
    }

    #[test]
    fn inspect_merge_state_marks_noop_merge_as_already_present() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        let worktree = paths.root.join("worktree");
        init_git_repo(repo.as_str());

        insert_session_with_worktree(&db, repo.as_str(), worktree.as_str(), "demo", "changed");
        assert!(
            Command::new("git")
                .args(["-C", worktree.as_str(), "checkout", "agent/demo"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(worktree.join("README.md"), "branch copy\n").unwrap();
        commit_all(worktree.as_str(), "session change");
        assert!(
            Command::new("git")
                .args(["-C", repo.as_str(), "checkout", "main"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(repo.join("README.md"), "branch copy\n").unwrap();
        commit_all(repo.as_str(), "same change on main");

        let session = db.get_session("demo").unwrap().unwrap();
        let review = inspect_merge_state(&session).unwrap();
        assert_eq!(review.merge_status, MergeStatus::UpToDate);
        assert_eq!(review.summary, "changes already present on base branch");
        assert!(!review.has_conflicts);
    }

    #[test]
    fn shell_quote_escapes_single_quotes() {
        assert_eq!(shell_quote("it's"), "'it'\"'\"'s'");
    }

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos()
            + u128::from(TEST_PATH_COUNTER.fetch_add(1, Ordering::Relaxed));
        let root = camino::Utf8PathBuf::from(format!("/tmp/agentd-app-test-{suffix}"));
        AppPaths {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            worktrees_dir: root.join("worktrees"),
            root,
        }
    }

    fn init_git_repo(path: &str) {
        fs::create_dir_all(path).unwrap();
        assert!(
            Command::new("git")
                .args(["init", "-b", "main", path])
                .output()
                .unwrap()
                .status
                .success()
        );
        assert!(
            Command::new("git")
                .args(["-C", path, "config", "user.email", "agentd@example.com"])
                .output()
                .unwrap()
                .status
                .success()
        );
        assert!(
            Command::new("git")
                .args(["-C", path, "config", "user.name", "agentd"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(format!("{path}/README.md"), "hello\n").unwrap();
        assert!(
            Command::new("git")
                .args(["-C", path, "add", "README.md"])
                .output()
                .unwrap()
                .status
                .success()
        );
        assert!(
            Command::new("git")
                .args(["-C", path, "commit", "-m", "init"])
                .output()
                .unwrap()
                .status
                .success()
        );
    }

    fn commit_all(path: &str, message: &str) {
        assert!(
            Command::new("git").args(["-C", path, "add", "."]).output().unwrap().status.success()
        );
        assert!(
            Command::new("git")
                .args(["-C", path, "commit", "-m", message])
                .output()
                .unwrap()
                .status
                .success()
        );
    }

    fn insert_session(db: &Database, repo: &str, session_id: &str, title: &str) {
        insert_session_with_worktree(db, repo, repo, session_id, title);
    }

    fn insert_session_with_worktree(
        db: &Database,
        repo: &str,
        worktree: &str,
        session_id: &str,
        title: &str,
    ) {
        if worktree != repo {
            assert!(
                Command::new("git")
                    .args(["-C", repo, "worktree", "add", "-b", "agent/demo", worktree, "main"])
                    .output()
                    .unwrap()
                    .status
                    .success()
            );
        } else {
            assert!(
                Command::new("git")
                    .args(["-C", repo, "branch", "-f", "agent/demo"])
                    .output()
                    .unwrap()
                    .status
                    .success()
            );
        }

        db.insert_session(&NewSession {
            session_id,
            thread_id: None,
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            mode: SessionMode::Execute,
            workspace: repo,
            repo_path: repo,
            repo_name: "repo",
            title,
            base_branch: "main",
            branch: "agent/demo",
            worktree,
            integration_policy: IntegrationPolicy::AutoApplySafe,
        })
        .unwrap();
    }
}
