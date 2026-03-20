use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::Path,
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
    event::NewSessionEvent,
    paths::AppPaths,
    session::{
        AttachmentKind, AttachmentRecord, AttentionLevel, CreateSessionResult, GitSyncStatus,
        IntegrationState, SessionDiff, SessionMode, SessionRecord, SessionStatus, WorktreeRecord,
        branch_name_from_title, repo_name_from_path,
    },
};

use crate::{
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
        title: Option<String>,
        agent_name: String,
        model: Option<String>,
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
                agent_command: &agent.command,
                agent_args_json: &agent_args_json,
                resume_session_id: None,
                workspace: repo_root.as_str(),
                repo_path: repo_root.as_str(),
                repo_name: &repo_name,
                title: &title,
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
                &config,
                &runtimes,
                SessionStartRequest {
                    session_id: &session_id,
                    repo_root: repo_root.as_str(),
                    worktree: worktree.as_str(),
                    branch: &branch,
                    title: &title,
                    log_path: &log_path,
                    launch: &launch,
                    model: model.as_deref(),
                    session_mode: mode,
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
                mode,
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
                Ok(Some(refresh_review_state(&db, session)?))
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
                .map(|session| refresh_review_state(&db, session))
                .collect::<Result<Vec<_>>>()
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
        let config = self.config.clone();
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
            let title = session.title.clone();
            let log_path = self.paths.log_path(&session.session_id);
            let config = config.clone();

            let result = task::spawn_blocking(move || {
                start_session_runtime(
                    &paths,
                    &db,
                    &config,
                    &runtimes,
                    SessionStartRequest {
                        session_id: &session_id,
                        repo_root: &repo_path,
                        worktree: &worktree,
                        branch: &branch,
                        title: &title,
                        log_path: &log_path,
                        launch: &launch,
                        model: session.model.as_deref(),
                        session_mode: session.mode,
                        resume_session_id: None,
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
            ensure_not_pending_review(&session, "cleanup")?;

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
                ensure_not_pending_review(&session, "remove")?;
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
            let session = refresh_review_state(&db, session)?;
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
            ensure_session_not_running(&session)?;
            ensure_pending_review(&session, "accept")?;

            let session = refresh_review_state(&db, session)?;
            let review = review_state_for_accept(&session)?;
            db.set_git_review_state(
                &session.session_id,
                review.git_sync,
                Some(&review.summary),
                review.has_conflicts,
            )?;

            match review.git_sync {
                GitSyncStatus::NeedsSync => bail!(review.summary),
                GitSyncStatus::Conflicted => bail!(review.summary),
                GitSyncStatus::Unknown => {
                    bail!("session `{}` is not ready to apply", session.session_id)
                }
                GitSyncStatus::InSync => {}
            }

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            if !git::has_branch_diff_against_base(
                &repo_root,
                &session.base_branch,
                &session.branch,
            )? {
                db.set_integration_state(
                    &session.session_id,
                    IntegrationState::Applied,
                    AttentionLevel::Info,
                    "changes already present on base branch",
                )?;
                db.set_git_review_state(
                    &session.session_id,
                    GitSyncStatus::InSync,
                    Some("changes already present on base branch"),
                    false,
                )?;
            } else {
                git::apply_squash_merge(
                    &repo_root,
                    &session.branch,
                    &apply_commit_message(&session),
                )?;
                db.set_integration_state(
                    &session.session_id,
                    IntegrationState::Applied,
                    AttentionLevel::Info,
                    &format!("changes applied to {}", session.base_branch),
                )?;
                db.set_git_review_state(
                    &session.session_id,
                    GitSyncStatus::InSync,
                    Some(&format!("changes applied to {}", session.base_branch)),
                    false,
                )?;
            }
            db.append_events(
                &session.session_id,
                &[daemon_event(
                    "SESSION_APPLIED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "base_branch": session.base_branch,
                        "branch": session.branch,
                        "worktree": session.worktree,
                    }),
                )],
            )?;

            db.get_session(&session_id)?
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
            ensure_pending_review(&session, "discard")?;

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if worktree.exists() {
                git::remove_worktree(&repo_root, &worktree)?;
            }
            db.set_integration_state(
                &session.session_id,
                IntegrationState::Discarded,
                AttentionLevel::Info,
                "changes discarded",
            )?;
            db.set_git_review_state(
                &session.session_id,
                GitSyncStatus::Unknown,
                Some("changes discarded"),
                false,
            )?;
            db.append_events(
                &session.session_id,
                &[
                    daemon_event(
                        "WORKTREE_REMOVED",
                        serde_json::json!({
                            "source": "daemon",
                            "repo_path": session.repo_path,
                            "branch": session.branch,
                            "worktree": session.worktree
                        }),
                    ),
                    daemon_event(
                        "SESSION_DISCARDED",
                        serde_json::json!({
                            "source": "daemon",
                            "repo_path": session.repo_path,
                            "base_branch": session.base_branch,
                            "branch": session.branch,
                            "worktree": session.worktree,
                            "forced": force,
                        }),
                    ),
                ],
            )?;

            db.get_session(&session_id)?
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
            runtime.attach(session_id, kind)?;
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
    title: &'a str,
    log_path: &'a Utf8PathBuf,
    launch: &'a LaunchCommand,
    model: Option<&'a str>,
    session_mode: SessionMode,
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
    config: &Config,
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
                "session_mode": request.session_mode.as_str(),
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
        SessionRuntime::new(
            pair.master,
            writer,
            Box::new(terminal_state),
            OUTPUT_BUFFER_CAPACITY,
        ),
    );
    let writer_db = db.clone();
    let writer_session_id = request.session_id.to_string();
    let writer_log_path = request.log_path.clone();
    let writer_runtime = runtime.clone();
    let (writer_done_tx, writer_done_rx) = std_mpsc::channel();
    std::thread::spawn(move || {
        let result = pump_pty_to_log(reader, &writer_log_path, &writer_runtime);
        if let Err(err) = result {
            let _ = record_session_failure(&writer_db, &writer_session_id, err.to_string());
        }
        let _ = writer_done_tx.send(());
    });

    let exit_db = db.clone();
    let exit_session_id = request.session_id.to_string();
    let exit_runtimes = runtimes.clone();
    let start_mode = request.mode;
    std::thread::spawn(move || {
        let status = child.wait();
        let code = status.ok().map(|value| value.exit_code() as i32);
        exit_runtimes.remove(&exit_session_id);
        let _ = writer_done_rx.recv();
        let _ = finalize_session_exit(&exit_db, &exit_session_id, code, start_mode);
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
                next_attach_ordinal: 1,
                attachments: HashMap::new(),
            }),
            output_tx,
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

    fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        let master = self
            .master
            .lock()
            .map_err(|_| anyhow!("PTY master poisoned"))?;
        master.resize(PtySize {
            rows,
            cols,
            pixel_width: 0,
            pixel_height: 0,
        })?;
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

    fn attach(
        self: &Arc<Self>,
        session_id: &str,
        kind: AttachmentKind,
    ) -> Result<(
        AttachmentHandle,
        AttachmentRecord,
        Vec<u8>,
        broadcast::Receiver<Vec<u8>>,
        mpsc::UnboundedReceiver<AttachControl>,
    )> {
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("session runtime state poisoned"))?;
        let attach_id = format!("{}-{}", kind.as_str(), state.next_attach_ordinal);
        state.next_attach_ordinal += 1;
        let connected_at = Utc::now();
        let snapshot = state.terminal_state.snapshot()?;
        let (control_tx, control_rx) = mpsc::unbounded_channel();
        state.attachments.insert(
            attach_id.clone(),
            RuntimeAttachment {
                kind,
                connected_at,
                control_tx,
            },
        );
        let receiver = self.output_tx.subscribe();
        Ok((
            AttachmentHandle {
                runtime: self.clone(),
                attach_id: attach_id.clone(),
            },
            AttachmentRecord {
                attach_id,
                session_id: session_id.to_string(),
                kind,
                connected_at,
            },
            snapshot,
            receiver,
            control_rx,
        ))
    }

    fn list_attachments(&self, session_id: &str) -> Result<Vec<AttachmentRecord>> {
        let state = self
            .state
            .lock()
            .map_err(|_| anyhow!("session runtime state poisoned"))?;
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
        let state = self
            .state
            .lock()
            .map_err(|_| anyhow!("session runtime state poisoned"))?;
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
        let state = self
            .state
            .lock()
            .map_err(|_| anyhow!("session runtime state poisoned"))?;
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

fn finalize_session_exit(
    db: &Database,
    session_id: &str,
    exit_code: Option<i32>,
    start_mode: SessionStartMode,
) -> Result<()> {
    let Some(session) = db.get_session(session_id)? else {
        return Ok(());
    };
    if session.status == SessionStatus::Failed {
        return Ok(());
    }

    let has_changes = session_has_pending_changes(&session)?;
    let outcome = if exit_code == Some(0) {
        db.mark_exited(session_id, exit_code)?;
        if has_changes {
            db.set_integration_state(
                session_id,
                IntegrationState::PendingReview,
                AttentionLevel::Notice,
                "changes ready to review",
            )?;
            db.append_events(
                session_id,
                &[daemon_event(
                    "SESSION_PENDING_REVIEW",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "base_branch": session.base_branch,
                        "branch": session.branch,
                        "worktree": session.worktree,
                    }),
                )],
            )?;
            "pending_review"
        } else {
            "complete"
        }
    } else {
        let error = match exit_code {
            Some(code) => format!("agent exited with code {code}"),
            None => "agent exited unexpectedly".to_string(),
        };
        db.mark_failed(session_id, error)?;
        "failed"
    };

    db.append_events(
        session_id,
        &[daemon_event(
            "SESSION_FINISHED",
            serde_json::json!({
                "source": "daemon",
                "exit_code": exit_code,
                "integration_state": if has_changes {
                    IntegrationState::PendingReview.as_str()
                } else {
                    IntegrationState::Clean.as_str()
                },
                "mode": match start_mode {
                    SessionStartMode::Create => "create",
                    SessionStartMode::Resume => "resume",
                },
                "outcome": outcome,
            }),
        )],
    )?;
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

    if request.mode == SessionStartMode::Resume && request.resume_session_id.is_some() {
        bail!(
            "resume-based interactive sessions are no longer supported for `{}`",
            request.launch.agent_name
        );
    }
    Ok(())
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
    for log_path in [
        paths.log_path(&session.session_id),
        paths.rendered_log_path(&session.session_id),
    ] {
        match fs::remove_file(log_path.as_std_path()) {
            Ok(()) => {}
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
            Err(err) => return Err(anyhow!(err)).context(format!("failed to remove {}", log_path)),
        }
    }
    Ok(())
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
    let accepts_live_io = matches!(
        session.status,
        SessionStatus::Running | SessionStatus::NeedsInput
    ) && process_exists(session.pid);
    if !accepts_live_io {
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

fn ensure_not_pending_review(session: &SessionRecord, action: &str) -> Result<()> {
    if session.integration_state == IntegrationState::PendingReview {
        bail!(
            "session `{}` has unapplied changes; use `agent diff {}` and `agent accept {}` before {action}, or `agent discard {}` to drop them",
            session.session_id,
            session.session_id,
            session.session_id,
            session.session_id
        );
    }
    Ok(())
}

fn ensure_pending_review(session: &SessionRecord, action: &str) -> Result<()> {
    if session.integration_state != IntegrationState::PendingReview {
        bail!(
            "session `{}` is not waiting for review; cannot {action}",
            session.session_id
        );
    }
    Ok(())
}

#[derive(Debug, Clone)]
struct ReviewState {
    git_sync: GitSyncStatus,
    summary: String,
    has_conflicts: bool,
}

fn refresh_review_state(db: &Database, session: SessionRecord) -> Result<SessionRecord> {
    if session.integration_state != IntegrationState::PendingReview {
        if session.git_sync != GitSyncStatus::Unknown
            || session.git_status_summary.is_some()
            || session.has_conflicts
        {
            db.set_git_review_state(&session.session_id, GitSyncStatus::Unknown, None, false)?;
            return db
                .get_session(&session.session_id)?
                .ok_or_else(|| anyhow!("session `{}` disappeared", session.session_id));
        }
        return Ok(session);
    }

    let review = inspect_review_state(&session)?;
    if session.git_sync != review.git_sync
        || session.git_status_summary.as_deref() != Some(review.summary.as_str())
        || session.has_conflicts != review.has_conflicts
    {
        db.set_git_review_state(
            &session.session_id,
            review.git_sync,
            Some(&review.summary),
            review.has_conflicts,
        )?;
        return db
            .get_session(&session.session_id)?
            .ok_or_else(|| anyhow!("session `{}` disappeared", session.session_id));
    }

    Ok(session)
}

fn inspect_review_state(session: &SessionRecord) -> Result<ReviewState> {
    let worktree = Utf8PathBuf::from(session.worktree.clone());
    if !worktree.exists() {
        return Ok(ReviewState {
            git_sync: GitSyncStatus::NeedsSync,
            summary: format!(
                "worktree {} is missing; recreate or discard it",
                session.worktree
            ),
            has_conflicts: false,
        });
    }
    if git::has_worktree_changes(&worktree)? {
        return Ok(ReviewState {
            git_sync: GitSyncStatus::NeedsSync,
            summary:
                "session worktree has uncommitted changes; commit or discard them before accept"
                    .to_string(),
            has_conflicts: false,
        });
    }

    let repo_root = Utf8PathBuf::from(session.repo_path.clone());
    if git::has_worktree_changes(&repo_root)? {
        return Ok(ReviewState {
            git_sync: GitSyncStatus::NeedsSync,
            summary: format!(
                "repo {} has local changes; clean it before accept",
                session.repo_path
            ),
            has_conflicts: false,
        });
    }

    let current_branch = git::current_branch(&repo_root)?;
    if current_branch != session.base_branch {
        return Ok(ReviewState {
            git_sync: GitSyncStatus::NeedsSync,
            summary: format!(
                "repo is on {}; switch to {} before accept",
                current_branch, session.base_branch
            ),
            has_conflicts: false,
        });
    }

    if !git::has_branch_diff_against_base(&repo_root, &session.base_branch, &session.branch)? {
        return Ok(ReviewState {
            git_sync: GitSyncStatus::InSync,
            summary: "changes already present on base branch".to_string(),
            has_conflicts: false,
        });
    }

    Ok(ReviewState {
        git_sync: GitSyncStatus::InSync,
        summary: format!("review ready: squash into {}", session.base_branch),
        has_conflicts: false,
    })
}

fn review_state_for_accept(session: &SessionRecord) -> Result<ReviewState> {
    let review = inspect_review_state(session)?;
    if review.git_sync != GitSyncStatus::InSync {
        return Ok(review);
    }

    let repo_root = Utf8PathBuf::from(session.repo_path.clone());
    let merge_clean =
        git::preflight_squash_merge(&repo_root, &session.base_branch, &session.branch)?;
    if merge_clean {
        return Ok(review);
    }

    Ok(ReviewState {
        git_sync: GitSyncStatus::Conflicted,
        summary: format!(
            "squash apply would conflict with {}; resolve it manually",
            session.base_branch
        ),
        has_conflicts: true,
    })
}

fn apply_commit_message(session: &SessionRecord) -> String {
    format!("Apply session {}: {}", session.session_id, session.title)
}

#[cfg(test)]
mod tests {
    use super::{
        AttachControl, SessionRuntime, SessionStartMode, apply_commit_message,
        finalize_session_exit, inspect_review_state, review_state_for_accept,
    };
    use crate::db::{Database, NewSession};
    use crate::terminal_state::TerminalStateEngine;
    use agentd_shared::{
        paths::AppPaths,
        session::{AttachmentKind, GitSyncStatus, SessionMode},
    };
    use anyhow::{Error, Result};
    use nix::libc;
    use portable_pty::{MasterPty, PtySize};
    use std::{
        fs,
        io::{Read, Write},
        process::Command,
        sync::Arc,
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

    static TEST_PATH_COUNTER: AtomicU64 = AtomicU64::new(0);

    struct StubTerminalState;

    impl TerminalStateEngine for StubTerminalState {
        fn feed(&mut self, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        fn snapshot(&mut self) -> Result<Vec<u8>> {
            Ok(b"snapshot".to_vec())
        }
    }

    #[derive(Debug)]
    struct StubMasterPty;

    impl MasterPty for StubMasterPty {
        fn resize(&self, _size: PtySize) -> std::result::Result<(), Error> {
            Ok(())
        }

        fn get_size(&self) -> std::result::Result<PtySize, Error> {
            Ok(PtySize {
                rows: 24,
                cols: 80,
                pixel_width: 0,
                pixel_height: 0,
            })
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
            runtime.attach("demo", AttachmentKind::Attach).unwrap();
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
            runtime.attach("demo", AttachmentKind::Attach).unwrap();
        let (_second_handle, second, _snapshot, _second_output, _second_control) =
            runtime.attach("demo", AttachmentKind::Tui).unwrap();

        assert_ne!(first.attach_id, second.attach_id);
        assert_eq!(runtime.list_attachments("demo").unwrap().len(), 2);
    }

    #[test]
    fn finalize_session_exit_marks_clean_sessions_complete() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "clean");
        finalize_session_exit(&db, "demo", Some(0), SessionStartMode::Create).unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(
            session.status,
            agentd_shared::session::SessionStatus::Exited
        );
        assert_eq!(
            session.integration_state,
            agentd_shared::session::IntegrationState::Clean
        );
    }

    #[test]
    fn finalize_session_exit_marks_changed_sessions_pending_review() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "changed");
        fs::write(repo.join("README.md"), "updated\n").unwrap();
        finalize_session_exit(&db, "demo", Some(0), SessionStartMode::Create).unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(
            session.status,
            agentd_shared::session::SessionStatus::Exited
        );
        assert_eq!(
            session.integration_state,
            agentd_shared::session::IntegrationState::PendingReview
        );
    }

    #[test]
    fn inspect_review_state_blocks_dirty_session_worktree() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "changed");
        fs::write(repo.join("README.md"), "repo dirty\n").unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        let review = inspect_review_state(&session).unwrap();
        assert_eq!(review.git_sync, GitSyncStatus::NeedsSync);
        assert!(
            review
                .summary
                .contains("session worktree has uncommitted changes")
        );
    }

    #[test]
    fn review_state_for_accept_detects_conflicts() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "changed");
        assert!(
            Command::new("git")
                .args(["-C", repo.as_str(), "checkout", "agent/demo"])
                .output()
                .unwrap()
                .status
                .success()
        );
        fs::write(repo.join("README.md"), "agent version\n").unwrap();
        commit_all(repo.as_str(), "session change");
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
        let review = review_state_for_accept(&session).unwrap();
        assert_eq!(review.git_sync, GitSyncStatus::Conflicted);
        assert!(review.has_conflicts);
    }

    #[test]
    fn apply_commit_message_uses_session_title() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        insert_session(&db, repo.as_str(), "demo", "changed");
        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(
            apply_commit_message(&session),
            "Apply session demo: changed"
        );
    }

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos()
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
            Command::new("git")
                .args(["-C", path, "add", "."])
                .output()
                .unwrap()
                .status
                .success()
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
        assert!(
            Command::new("git")
                .args(["-C", repo, "branch", "-f", "agent/demo"])
                .output()
                .unwrap()
                .status
                .success()
        );
        db.insert_session(&NewSession {
            session_id,
            thread_id: None,
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            mode: SessionMode::Execute,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
            workspace: repo,
            repo_path: repo,
            repo_name: "repo",
            title,
            base_branch: "main",
            branch: "agent/demo",
            worktree: repo,
        })
        .unwrap();
    }
}
