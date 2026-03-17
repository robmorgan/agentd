use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use camino::Utf8PathBuf;
use nix::{
    errno::Errno,
    sys::signal::{Signal, kill},
    unistd::Pid,
};
use portable_pty::{CommandBuilder, PtySize, native_pty_system};
use tokio::{sync::broadcast, task};

use agentd_shared::{
    config::Config,
    event::{NewSessionEvent, SessionEvent},
    paths::AppPaths,
    session::{
        CreateSessionResult, SessionDiff, SessionRecord, SessionStatus, WorktreeRecord,
        branch_name_from_task,
    },
};

use crate::{
    codex,
    db::{Database, NewSession, SessionLaunchInfo},
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
        Self {
            paths,
            db,
            config,
            runtimes: SessionRuntimeRegistry::default(),
        }
    }

    pub async fn create_session(
        &self,
        workspace: String,
        task_text: String,
        agent_name: String,
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
            let agent_args_json =
                serde_json::to_string(&agent.args).context("failed to serialize agent args")?;

            let session_id = unique_session_id(&db)?;
            let worktree = paths.worktree_path(&session_id);
            let branch = unique_branch_name(&repo_root, &task_text, &session_id)?;

            db.insert_session(&NewSession {
                session_id: &session_id,
                agent: &agent_name,
                agent_command: &agent.command,
                agent_args_json: &agent_args_json,
                resume_session_id: None,
                workspace: repo_root.as_str(),
                repo_path: repo_root.as_str(),
                task: &task_text,
                base_branch: &base_branch,
                branch: &branch,
                worktree: worktree.as_str(),
            })?;

            if let Err(err) = git::create_worktree(&repo_root, &base_branch, &branch, &worktree) {
                let _ = record_session_failure(&db, &session_id, err.to_string());
                return Err(err);
            }
            let _ = db.append_events(
                &session_id,
                &[daemon_event(
                    "WORKTREE_CREATED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": repo_root.as_str(),
                        "base_branch": base_branch,
                        "branch": branch,
                        "worktree": worktree.as_str()
                    }),
                )],
            );

            let log_path = paths.log_path(&session_id);
            if let Err(err) = prepare_log_file(&log_path, SessionStartMode::Create) {
                let err = anyhow!(err).context("failed to create session log file");
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
                &runtimes,
                SessionStartRequest {
                    session_id: &session_id,
                    repo_root: repo_root.as_str(),
                    worktree: worktree.as_str(),
                    branch: &branch,
                    task_text: &task_text,
                    log_path: &log_path,
                    launch: &launch,
                    resume_session_id: None,
                    mode: SessionStartMode::Create,
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
            })
        })
        .await?
    }

    pub async fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || db.get_session(&session_id)).await?
    }

    pub async fn list_sessions(&self) -> Result<Vec<SessionRecord>> {
        let db = self.db.clone();
        task::spawn_blocking(move || db.list_sessions()).await?
    }

    pub async fn append_session_events(
        &self,
        session_id: &str,
        events: Vec<NewSessionEvent>,
    ) -> Result<Vec<SessionEvent>> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            db.get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            db.append_events(&session_id, &events)
        })
        .await?
    }

    pub async fn list_events_since(
        &self,
        session_id: &str,
        after_id: Option<i64>,
    ) -> Result<Vec<SessionEvent>> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            db.get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            db.list_events_since(&session_id, after_id)
        })
        .await?
    }

    pub async fn reconcile_sessions(&self) -> Result<()> {
        let sessions = self.list_sessions().await?;
        for session in sessions {
            if session.status == SessionStatus::Running && !process_exists(session.pid) {
                let db = self.db.clone();
                let session_id = session.session_id.clone();
                task::spawn_blocking(move || db.mark_unknown_recovered(&session_id)).await??;
            }
        }
        Ok(())
    }

    pub async fn resume_paused_sessions(&self) -> Result<()> {
        let sessions = self.list_sessions().await?;
        for session in sessions {
            if session.status != SessionStatus::Paused {
                continue;
            }

            let launch = match self.resolve_launch_command(&session).await {
                Ok(launch) => launch,
                Err(err) => {
                    let error = err.to_string();
                    let db = self.db.clone();
                    let session_id = session.session_id.clone();
                    task::spawn_blocking(move || record_session_failure(&db, &session_id, error))
                        .await??;
                    continue;
                }
            };
            let paths = self.paths.clone();
            let db = self.db.clone();
            let runtimes = self.runtimes.clone();
            let session_id = session.session_id.clone();
            let repo_path = session.repo_path.clone();
            let worktree = session.worktree.clone();
            let branch = session.branch.clone();
            let task_text = session.task.clone();
            let log_path = self.paths.log_path(&session.session_id);
            let resume_session_id = match self.resolve_resume_session_id(&session, &launch).await {
                Ok(Some(resume_session_id)) => resume_session_id,
                Ok(None) => {
                    let error = format!(
                        "session `{}` does not have an exact Codex resume id",
                        session.session_id
                    );
                    let db = self.db.clone();
                    let session_id = session.session_id.clone();
                    task::spawn_blocking(move || record_session_failure(&db, &session_id, error))
                        .await??;
                    continue;
                }
                Err(err) => {
                    let error = err.to_string();
                    let db = self.db.clone();
                    let session_id = session.session_id.clone();
                    task::spawn_blocking(move || record_session_failure(&db, &session_id, error))
                        .await??;
                    continue;
                }
            };

            let result = task::spawn_blocking(move || {
                start_session_runtime(
                    &paths,
                    &db,
                    &runtimes,
                    SessionStartRequest {
                        session_id: &session_id,
                        repo_root: &repo_path,
                        worktree: &worktree,
                        branch: &branch,
                        task_text: &task_text,
                        log_path: &log_path,
                        launch: &launch,
                        resume_session_id: Some(&resume_session_id),
                        mode: SessionStartMode::Resume,
                    },
                )
            })
            .await?;

            if let Err(err) = result {
                let error = err.to_string();
                let db = self.db.clone();
                let session_id = session.session_id.clone();
                task::spawn_blocking(move || record_session_failure(&db, &session_id, error))
                    .await??;
            }
        }

        Ok(())
    }

    pub async fn has_running_sessions(&self) -> Result<bool> {
        let sessions = self.list_sessions().await?;
        Ok(sessions
            .into_iter()
            .any(|session| session.status == SessionStatus::Running && process_exists(session.pid)))
    }

    pub async fn resolve_launch_command(&self, session: &SessionRecord) -> Result<LaunchCommand> {
        let db = self.db.clone();
        let config = self.config.clone();
        let paths = self.paths.clone();
        let session_id = session.session_id.clone();
        let agent_name = session.agent.clone();
        task::spawn_blocking(move || {
            let launch = db
                .get_launch_info(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            resolve_launch_command(&config, &paths, &agent_name, launch)
        })
        .await?
    }

    async fn resolve_resume_session_id(
        &self,
        session: &SessionRecord,
        launch: &LaunchCommand,
    ) -> Result<Option<String>> {
        if !is_resumable_command(&launch.command) {
            return Ok(None);
        }

        let db = self.db.clone();
        let session_id = session.session_id.clone();
        let worktree = session.worktree.clone();
        task::spawn_blocking(move || {
            if let Some(resume_session_id) = db.get_resume_session_id(&session_id)? {
                return Ok(Some(resume_session_id));
            }

            let discovered = codex::discover_resume_session_id(&worktree)?;
            if let Some(ref resume_session_id) = discovered {
                db.set_resume_session_id(&session_id, resume_session_id)?;
            }
            Ok(discovered)
        })
        .await?
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
            let _ = db.append_events(
                &session_id,
                &[daemon_event(
                    "WORKTREE_CREATED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "base_branch": session.base_branch,
                        "branch": session.branch,
                        "worktree": session.worktree
                    }),
                )],
            );

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

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if !worktree.exists() {
                bail!("worktree `{worktree}` does not exist");
            }

            git::remove_worktree(&repo_root, &worktree)?;
            let _ = db.append_events(
                &session_id,
                &[daemon_event(
                    "WORKTREE_REMOVED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "branch": session.branch,
                        "worktree": session.worktree
                    }),
                )],
            );

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
                let _ = db.append_events(
                    &session_id,
                    &[daemon_event(
                        "SESSION_KILLED",
                        serde_json::json!({
                            "source": "daemon",
                            "pid": session.pid,
                            "signal": "SIGTERM"
                        }),
                    )],
                );
                let _ = db.mark_exited(&session_id, None);
            }

            if remove {
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

        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            db.append_events(
                &session_id,
                &[daemon_event(
                    "SESSION_INPUT_INJECTED",
                    serde_json::json!({
                        "source": "daemon",
                        "target_session_id": session_id,
                        "source_session_id": source_session_id,
                        "byte_count": data.len(),
                        "preview": preview_input(&data),
                    }),
                )],
            )?;
            Ok(())
        })
        .await?
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

    pub async fn attach_session(
        &self,
        session_id: &str,
    ) -> Result<(
        AttachLease,
        Vec<u8>,
        broadcast::Receiver<Vec<u8>>,
        broadcast::Receiver<AttachControl>,
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
        let (lease, snapshot, receiver, control_rx) = runtime
            .try_attach()
            .ok_or_else(|| anyhow!("session `{session_id}` already has an attached client"))?;
        Ok((lease, snapshot, receiver, control_rx))
    }

    pub async fn switch_attached_session(
        &self,
        source_session_id: &str,
        target_session_id: &str,
    ) -> Result<()> {
        if source_session_id == target_session_id {
            bail!("source and target sessions must differ");
        }

        let source = self
            .get_session(source_session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{source_session_id}` not found"))?;
        ensure_session_running(&source)?;
        let source_runtime = self
            .runtimes
            .get(source_session_id)
            .ok_or_else(|| anyhow!("session `{source_session_id}` does not have a live PTY"))?;

        let target = self
            .get_session(target_session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{target_session_id}` not found"))?;
        ensure_session_running(&target)?;
        self.runtimes
            .get(target_session_id)
            .ok_or_else(|| anyhow!("session `{target_session_id}` does not have a live PTY"))?;

        source_runtime.request_switch(target_session_id)
    }

    pub async fn detach_session(&self, session_id: &str) -> Result<()> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
        ensure_session_running(&session)?;

        let runtime = self
            .runtimes
            .get(session_id)
            .ok_or_else(|| anyhow!("session `{session_id}` does not have a live PTY"))?;

        runtime.request_detach()
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LaunchCommand {
    pub agent_name: String,
    pub command: String,
    pub args: Vec<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SessionStartMode {
    Create,
    Resume,
}

struct SessionStartRequest<'a> {
    session_id: &'a str,
    repo_root: &'a str,
    worktree: &'a str,
    branch: &'a str,
    task_text: &'a str,
    log_path: &'a Utf8PathBuf,
    launch: &'a LaunchCommand,
    resume_session_id: Option<&'a str>,
    mode: SessionStartMode,
}

pub fn is_resumable_command(command: &str) -> bool {
    Path::new(command)
        .file_name()
        .and_then(|value| value.to_str())
        == Some("codex")
}

fn resolve_launch_command(
    config: &Config,
    paths: &AppPaths,
    agent_name: &str,
    launch: SessionLaunchInfo,
) -> Result<LaunchCommand> {
    let command = match launch.command {
        Some(command) => command,
        None => config.require_agent(paths, agent_name)?.command.clone(),
    };
    let args = match launch.args {
        Some(args) => args,
        None => config
            .agents
            .get(agent_name)
            .map(|agent| agent.args.clone())
            .unwrap_or_default(),
    };
    Ok(LaunchCommand {
        agent_name: agent_name.to_string(),
        command,
        args,
    })
}

fn prepare_log_file(log_path: &Utf8PathBuf, mode: SessionStartMode) -> Result<()> {
    match mode {
        SessionStartMode::Create => {
            File::create(log_path.as_std_path())
                .with_context(|| format!("failed to create {}", log_path))?;
        }
        SessionStartMode::Resume => {
            OpenOptions::new()
                .create(true)
                .append(true)
                .open(log_path.as_std_path())
                .with_context(|| format!("failed to open {}", log_path))?;
        }
    }
    Ok(())
}

fn start_session_runtime(
    paths: &AppPaths,
    db: &Database,
    runtimes: &SessionRuntimeRegistry,
    request: SessionStartRequest<'_>,
) -> Result<()> {
    prepare_log_file(request.log_path, request.mode)?;

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
    command.args(request.launch.args.clone());
    if request.mode == SessionStartMode::Resume {
        if !is_resumable_command(&request.launch.command) {
            bail!(
                "agent `{}` does not support resume-based upgrades",
                request.launch.agent_name
            );
        }
        let resume_session_id = request.resume_session_id.ok_or_else(|| {
            anyhow!(
                "session `{}` does not have an exact Codex resume id",
                request.session_id
            )
        })?;
        command.arg("resume");
        command.arg(resume_session_id);
    }
    command.cwd(request.worktree);
    for (key, value) in std::env::vars() {
        command.env(&key, &value);
    }
    command.env("AGENTD_SESSION_ID", request.session_id);
    command.env("AGENTD_SOCKET", paths.socket.as_str());
    command.env("AGENTD_WORKSPACE", request.repo_root);
    command.env("AGENTD_WORKTREE", request.worktree);
    command.env("AGENTD_BRANCH", request.branch);
    command.env("AGENTD_TASK", request.task_text);

    let mut child = pair
        .slave
        .spawn_command(command)
        .map_err(|err| anyhow!(err).context("failed to spawn agent process"))?;

    let pid = child.process_id().map(|value| value as u32).unwrap_or(0);
    db.mark_running(request.session_id, pid)?;
    let start_event = match request.mode {
        SessionStartMode::Create => "SESSION_STARTED",
        SessionStartMode::Resume => "SESSION_RESUMED",
    };
    db.append_events(
        request.session_id,
        &[daemon_event(
            start_event,
            serde_json::json!({
                "source": "daemon",
                "pid": pid,
                "agent": request.launch.agent_name,
                "branch": request.branch,
                "worktree": request.worktree,
                "resume_session_id": request.resume_session_id
            }),
        )],
    )?;

    let reader = pair
        .master
        .try_clone_reader()
        .context("failed to clone PTY reader")?;
    let writer = pair
        .master
        .take_writer()
        .context("failed to clone PTY writer")?;
    let terminal_state =
        GhosttyTerminalState::new(DEFAULT_PTY_COLS, DEFAULT_PTY_ROWS, MAX_SCROLLBACK_BYTES)
            .context("failed to initialize libghostty-vt state")?;
    let runtime = runtimes.insert(
        request.session_id.to_string(),
        SessionRuntime::new(writer, Box::new(terminal_state), OUTPUT_BUFFER_CAPACITY),
    );
    let writer_db = db.clone();
    let writer_session_id = request.session_id.to_string();
    let writer_log_path = request.log_path.clone();
    let writer_runtime = runtime.clone();
    std::thread::spawn(move || {
        if let Err(err) = pump_pty_to_log(reader, &writer_log_path, &writer_runtime) {
            let _ = record_session_failure(&writer_db, &writer_session_id, err.to_string());
        }
    });

    let exit_db = db.clone();
    let exit_session_id = request.session_id.to_string();
    let exit_runtimes = runtimes.clone();
    let start_mode = request.mode;
    std::thread::spawn(move || {
        let status = child.wait();
        let code = status.ok().map(|value| value.exit_code() as i32);
        exit_runtimes.remove(&exit_session_id);
        let _ = exit_db.mark_exited(&exit_session_id, code);
        let _ = exit_db.append_events(
            &exit_session_id,
            &[daemon_event(
                "SESSION_FINISHED",
                serde_json::json!({
                    "source": "daemon",
                    "exit_code": code,
                    "mode": match start_mode {
                        SessionStartMode::Create => "create",
                        SessionStartMode::Resume => "resume",
                    }
                }),
            )],
        );
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
}

impl SessionRuntimeRegistry {
    fn insert(&self, session_id: String, runtime: SessionRuntime) -> Arc<SessionRuntime> {
        let runtime = Arc::new(runtime);
        let mut inner = self
            .inner
            .lock()
            .expect("session runtime registry poisoned");
        inner.insert(session_id, runtime.clone());
        runtime
    }

    fn get(&self, session_id: &str) -> Option<Arc<SessionRuntime>> {
        let inner = self
            .inner
            .lock()
            .expect("session runtime registry poisoned");
        inner.get(session_id).cloned()
    }

    fn remove(&self, session_id: &str) {
        let mut inner = self
            .inner
            .lock()
            .expect("session runtime registry poisoned");
        inner.remove(session_id);
    }
}

struct SessionRuntime {
    writer: Mutex<Box<dyn Write + Send>>,
    state: Mutex<SessionRuntimeState>,
    output_tx: broadcast::Sender<Vec<u8>>,
    control_tx: broadcast::Sender<AttachControl>,
    attached: AtomicBool,
}

impl SessionRuntime {
    fn new(
        writer: Box<dyn Write + Send>,
        terminal_state: Box<dyn TerminalStateEngine>,
        output_buffer_capacity: usize,
    ) -> Self {
        let (output_tx, _) = broadcast::channel(output_buffer_capacity);
        let (control_tx, _) = broadcast::channel(4);
        Self {
            writer: Mutex::new(writer),
            state: Mutex::new(SessionRuntimeState { terminal_state }),
            output_tx,
            control_tx,
            attached: AtomicBool::new(false),
        }
    }

    fn write_input(&self, data: &[u8]) -> Result<()> {
        let mut writer = self
            .writer
            .lock()
            .map_err(|_| anyhow!("PTY writer poisoned"))?;
        writer.write_all(data)?;
        writer.flush()?;
        Ok(())
    }

    fn publish_output(&self, data: &[u8]) -> Result<()> {
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("session runtime state poisoned"))?;
        state.terminal_state.feed(data)?;
        let _ = self.output_tx.send(data.to_vec());
        Ok(())
    }

    fn try_attach(
        self: &Arc<Self>,
    ) -> Option<(
        AttachLease,
        Vec<u8>,
        broadcast::Receiver<Vec<u8>>,
        broadcast::Receiver<AttachControl>,
    )> {
        self.attached
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .ok()?;
        let mut state = self.state.lock().ok()?;
        let snapshot = state.terminal_state.snapshot().ok()?;
        let receiver = self.output_tx.subscribe();
        let control_rx = self.control_tx.subscribe();
        Some((
            AttachLease {
                runtime: self.clone(),
            },
            snapshot,
            receiver,
            control_rx,
        ))
    }

    fn request_switch(&self, target_session_id: &str) -> Result<()> {
        if !self.attached.load(Ordering::Acquire) {
            bail!("session does not have an attached client");
        }
        self.control_tx
            .send(AttachControl::SwitchSession(target_session_id.to_string()))
            .map_err(|_| anyhow!("session does not have an attached client"))?;
        Ok(())
    }

    fn request_detach(&self) -> Result<()> {
        if !self.attached.load(Ordering::Acquire) {
            bail!("session does not have an attached client");
        }
        self.control_tx
            .send(AttachControl::Detach)
            .map_err(|_| anyhow!("session does not have an attached client"))?;
        Ok(())
    }
}

struct SessionRuntimeState {
    terminal_state: Box<dyn TerminalStateEngine>,
}

pub struct AttachLease {
    runtime: Arc<SessionRuntime>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AttachControl {
    SwitchSession(String),
    Detach,
}

impl Drop for AttachLease {
    fn drop(&mut self) {
        self.runtime.attached.store(false, Ordering::Release);
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

fn daemon_event(event_type: &str, payload_json: serde_json::Value) -> NewSessionEvent {
    NewSessionEvent {
        event_type: event_type.to_string(),
        payload_json,
    }
}

fn record_session_failure(db: &Database, session_id: &str, error: String) -> Result<()> {
    db.mark_failed(session_id, error.clone())?;
    db.append_events(
        session_id,
        &[daemon_event(
            "SESSION_FAILED",
            serde_json::json!({
                "source": "daemon",
                "error": error
            }),
        )],
    )?;
    Ok(())
}

fn unique_branch_name(
    repo_root: &camino::Utf8Path,
    task_text: &str,
    session_id: &str,
) -> Result<String> {
    let base = branch_name_from_task(task_text);
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

fn pump_pty_to_log(
    mut reader: Box<dyn Read + Send>,
    log_path: &Utf8PathBuf,
    runtime: &SessionRuntime,
) -> Result<()> {
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(log_path.as_std_path())
        .with_context(|| format!("failed to open {}", log_path))?;
    let file = Arc::new(Mutex::new(file));
    let mut buffer = [0_u8; 8192];

    loop {
        let bytes_read = reader.read(&mut buffer)?;
        if bytes_read == 0 {
            break;
        }

        {
            let mut file = file.lock().map_err(|_| anyhow!("log writer poisoned"))?;
            file.write_all(&buffer[..bytes_read])?;
            file.flush()?;
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
        Err(err) => Err(anyhow!(err)).context(format!(
            "failed to send {signal:?} to session `{session_id}`"
        )),
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
    let log_path = paths.log_path(&session.session_id);
    match fs::remove_file(log_path.as_std_path()) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(anyhow!(err)).context(format!("failed to remove {}", log_path)),
    }
}

fn preview_input(data: &[u8]) -> String {
    let mut preview = String::from_utf8_lossy(data)
        .replace('\n', "\\n")
        .replace('\r', "\\r");
    if preview.len() > 120 {
        preview.truncate(120);
        preview.push_str("...");
    }
    preview
}

fn ensure_session_running(session: &SessionRecord) -> Result<()> {
    if session.status != SessionStatus::Running || !process_exists(session.pid) {
        bail!("session `{}` is not running", session.session_id);
    }
    Ok(())
}

fn ensure_session_not_running(session: &SessionRecord) -> Result<()> {
    if session.status == SessionStatus::Running && process_exists(session.pid) {
        bail!("session `{}` is still running", session.session_id);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{AttachControl, SessionRuntime};
    use crate::terminal_state::TerminalStateEngine;
    use anyhow::Result;
    use std::sync::Arc;

    struct StubTerminalState;

    impl TerminalStateEngine for StubTerminalState {
        fn feed(&mut self, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        fn snapshot(&mut self) -> Result<Vec<u8>> {
            Ok(b"snapshot".to_vec())
        }
    }

    #[test]
    fn request_detach_requires_active_attacher() {
        let runtime = SessionRuntime::new(Box::new(Vec::new()), Box::new(StubTerminalState), 8);
        let err = runtime.request_detach().unwrap_err();
        assert_eq!(err.to_string(), "session does not have an attached client");
    }

    #[test]
    fn request_detach_notifies_attached_client() {
        let runtime = Arc::new(SessionRuntime::new(
            Box::new(Vec::new()),
            Box::new(StubTerminalState),
            8,
        ));
        let (_lease, snapshot, _output_rx, mut control_rx) = runtime.try_attach().unwrap();
        assert_eq!(snapshot, b"snapshot".to_vec());

        runtime.request_detach().unwrap();

        let control = control_rx.try_recv().unwrap();
        assert_eq!(control, AttachControl::Detach);
    }
}
