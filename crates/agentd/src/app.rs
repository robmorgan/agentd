use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::Path,
    sync::{
        mpsc,
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
        AttentionLevel, CreateSessionResult, IntegrationState, SessionDiff, SessionMode,
        SessionRecord, SessionStatus, WorktreeRecord, branch_name_from_task,
    },
};

use crate::{
    codex,
    codex_json::{CodexJsonStream, preview_line},
    db::{Database, NewSession, NewThread, SessionLaunchInfo},
    git,
    ids::{generate_session_id, generate_thread_id},
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
        model: Option<String>,
        mode: SessionMode,
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
            let prompt_text = build_initial_prompt(&task_text, mode);

            let session_id = unique_session_id(&db)?;
            let thread_id = (agent_name == "codex")
                .then(|| unique_thread_id(&db))
                .transpose()?;
            let worktree = paths.worktree_path(&session_id);
            let branch = unique_branch_name(&repo_root, &task_text, &session_id)?;

            db.insert_session(&NewSession {
                session_id: &session_id,
                thread_id: thread_id.as_deref(),
                agent: &agent_name,
                model: model.as_deref(),
                mode,
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
            if let Some(thread_id) = &thread_id {
                db.insert_thread(&NewThread {
                    thread_id,
                    session_id: &session_id,
                    agent: &agent_name,
                    title: &task_text,
                    initial_prompt: &prompt_text,
                })?;
                let _ = db.append_events(
                    &session_id,
                    &[daemon_event(
                        "THREAD_CREATED",
                        serde_json::json!({
                            "source": "daemon",
                            "thread_id": thread_id,
                            "session_id": &session_id,
                            "agent": &agent_name,
                            "title": &task_text,
                            "session_mode": mode.as_str(),
                        }),
                    )],
                );
            }

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
                    prompt_text: &prompt_text,
                    log_path: &log_path,
                    launch: &launch,
                    model: model.as_deref(),
                    session_mode: mode,
                    thread_id: thread_id.as_deref(),
                    resume_session_id: None,
                    resume_prompt: None,
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
            let thread_id = session.thread_id.clone();
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
                        prompt_text: &task_text,
                        log_path: &log_path,
                        launch: &launch,
                        model: session.model.as_deref(),
                        session_mode: session.mode,
                        thread_id: thread_id.as_deref(),
                        resume_session_id: Some(&resume_session_id),
                        resume_prompt: None,
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
        let thread_id = session.thread_id.clone();
        task::spawn_blocking(move || {
            if let Some(thread_id) = thread_id {
                if let Some(thread) = db.get_thread(&thread_id)? {
                    if let Some(upstream_thread_id) = thread.upstream_thread_id {
                        db.set_resume_session_id(&session_id, &upstream_thread_id)?;
                        return Ok(Some(upstream_thread_id));
                    }
                }
            }
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

    pub async fn apply_session(&self, session_id: &str) -> Result<SessionRecord> {
        let db = self.db.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            ensure_session_not_running(&session)?;
            ensure_pending_review(&session, "apply")?;

            let repo_root = Utf8PathBuf::from(session.repo_path.clone());
            let worktree = Utf8PathBuf::from(session.worktree.clone());
            if !worktree.exists() {
                bail!("worktree `{worktree}` does not exist");
            }

            let current_branch = git::current_branch(&repo_root)?;
            if current_branch != session.base_branch {
                bail!(
                    "repo `{}` is on branch `{}`, expected `{}`",
                    repo_root,
                    current_branch,
                    session.base_branch
                );
            }

            let status = git::working_tree_status(&repo_root)?;
            if !status.trim().is_empty() {
                bail!("repo `{repo_root}` has uncommitted changes");
            }

            let committed_patch = git::committed_patch_against_base(&worktree, &session.base_branch)?;
            let worktree_patch = git::worktree_patch_against_head(&worktree)?;
            if committed_patch.trim().is_empty() && worktree_patch.trim().is_empty() {
                bail!("session `{session_id}` has no changes to apply");
            }

            git::apply_patch(&repo_root, &committed_patch)?;
            git::apply_patch(&repo_root, &worktree_patch)?;
            git::commit_all(
                &repo_root,
                &format!("{} ({})", session.task, session.session_id),
            )?;

            db.set_integration_state(
                &session_id,
                IntegrationState::Applied,
                AttentionLevel::Notice,
                "changes applied to repo",
            )?;
            db.append_events(
                &session_id,
                &[daemon_event(
                    "SESSION_APPLIED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "base_branch": session.base_branch,
                        "branch": session.branch,
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
        let paths = self.paths.clone();
        let session_id = session_id.to_string();
        task::spawn_blocking(move || {
            let session = db
                .get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
            ensure_session_not_running(&session)?;
            if !force {
                ensure_pending_review(&session, "discard")?;
            }

            remove_session_artifacts(&paths, &session)?;
            db.set_integration_state(
                &session_id,
                IntegrationState::Discarded,
                AttentionLevel::Notice,
                "changes discarded",
            )?;
            db.append_events(
                &session_id,
                &[daemon_event(
                    "SESSION_DISCARDED",
                    serde_json::json!({
                        "source": "daemon",
                        "repo_path": session.repo_path,
                        "branch": session.branch,
                        "force": force,
                    }),
                )],
            )?;

            db.get_session(&session_id)?
                .ok_or_else(|| anyhow!("session `{session_id}` not found after discard"))
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

    pub async fn reply_to_session(&self, session_id: &str, prompt: String) -> Result<SessionRecord> {
        let session = self
            .get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;

        match session.status {
            SessionStatus::Running => {
                let mut data = prompt.into_bytes();
                data.push(b'\n');
                self.send_input(session_id, data, None).await?;
            }
            SessionStatus::NeedsInput => {
                let launch = self.resolve_launch_command(&session).await?;
                let resume_session_id = self
                    .resolve_resume_session_id(&session, &launch)
                    .await?
                    .ok_or_else(|| {
                        anyhow!(
                            "session `{}` does not have an exact Codex resume id",
                            session.session_id
                        )
                    })?;
                let paths = self.paths.clone();
                let db = self.db.clone();
                let runtimes = self.runtimes.clone();
                let session_id = session.session_id.clone();
                let repo_path = session.repo_path.clone();
                let worktree = session.worktree.clone();
                let branch = session.branch.clone();
                let task_text = session.task.clone();
                let thread_id = session.thread_id.clone();
                let model = session.model.clone();
                let prompt_preview = preview_text(&prompt, 160);
                let resume_prompt = prompt.clone();
                let log_path = self.paths.log_path(&session.session_id);
                task::spawn_blocking(move || {
                    db.append_events(
                        &session_id,
                        &[daemon_event(
                            "SESSION_REPLY_REQUESTED",
                            serde_json::json!({
                                "source": "daemon",
                                "prompt_preview": prompt_preview,
                            }),
                        )],
                    )?;
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
                            prompt_text: &task_text,
                            log_path: &log_path,
                            launch: &launch,
                            model: model.as_deref(),
                            session_mode: session.mode,
                            thread_id: thread_id.as_deref(),
                            resume_session_id: Some(&resume_session_id),
                            resume_prompt: Some(&resume_prompt),
                            mode: SessionStartMode::Resume,
                        },
                    )
                })
                .await??;
            }
            SessionStatus::Creating
            | SessionStatus::Paused
            | SessionStatus::Exited
            | SessionStatus::Failed
            | SessionStatus::UnknownRecovered => {
                bail!("session `{}` is not accepting replies", session.session_id);
            }
        }

        self.get_session(session_id)
            .await?
            .ok_or_else(|| anyhow!("session `{session_id}` not found after reply"))
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
    prompt_text: &'a str,
    log_path: &'a Utf8PathBuf,
    launch: &'a LaunchCommand,
    model: Option<&'a str>,
    session_mode: SessionMode,
    thread_id: Option<&'a str>,
    resume_session_id: Option<&'a str>,
    resume_prompt: Option<&'a str>,
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
    let use_codex_json = request.launch.agent_name == "codex" && request.thread_id.is_some();
    let rendered_log_path = paths.rendered_log_path(request.session_id);
    if use_codex_json {
        prepare_log_file(&rendered_log_path, request.mode)?;
    }

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
    configure_spawn_command(&mut command, &request)?;
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
                "session_mode": request.session_mode.as_str(),
                "thread_id": request.thread_id,
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
    let writer_rendered_log_path = rendered_log_path.clone();
    let writer_thread_id = if use_codex_json {
        request.thread_id.map(|value| value.to_string())
    } else {
        None
    };
    let writer_runtime = runtime.clone();
    let (writer_done_tx, writer_done_rx) = mpsc::channel();
    std::thread::spawn(move || {
        let result = if let Some(thread_id) = writer_thread_id {
            pump_codex_json_to_logs(
                reader,
                &writer_log_path,
                &writer_rendered_log_path,
                &writer_runtime,
                &writer_db,
                &writer_session_id,
                &thread_id,
                request.session_mode == SessionMode::Plan,
            )
        } else {
            pump_pty_to_log(reader, &writer_log_path, &writer_runtime)
        };
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

fn unique_thread_id(db: &Database) -> Result<String> {
    for _ in 0..16 {
        let candidate = generate_thread_id();
        if db.get_thread(&candidate)?.is_none() {
            return Ok(candidate);
        }
    }
    bail!("failed to allocate a unique thread id")
}

fn daemon_event(event_type: &str, payload_json: serde_json::Value) -> NewSessionEvent {
    NewSessionEvent {
        event_type: event_type.to_string(),
        payload_json,
    }
}

fn build_initial_prompt(task_text: &str, mode: SessionMode) -> String {
    match mode {
        SessionMode::Execute => task_text.to_string(),
        SessionMode::Plan => format!(
            "You are in planning mode for this repository.\n\
             \n\
             Task:\n\
             {task_text}\n\
             \n\
             Requirements:\n\
             - Ask follow-up questions if you need clarification before finalizing the plan.\n\
             - Do not edit files, run implementation commands, or apply changes.\n\
             - When you have enough information, reply with exactly one <proposed_plan>...</proposed_plan> block in Markdown.\n\
             - Stop after producing the plan.\n"
        ),
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

    let outcome = classify_session_outcome(db, &session, exit_code)?;
    let has_changes = session_has_pending_changes(&session)?;
    let latest_plan = if session.mode == SessionMode::Plan {
        db.latest_plan(session_id)?
    } else {
        None
    };
    match &outcome {
        SessionOutcome::Finished => {
            db.mark_exited(session_id, exit_code)?;
            if has_changes {
                db.set_integration_state(
                    session_id,
                    IntegrationState::PendingReview,
                    AttentionLevel::Notice,
                    "changes ready to apply",
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
            } else if session.mode == SessionMode::Plan && latest_plan.is_none() {
                db.set_integration_state(
                    session_id,
                    IntegrationState::Clean,
                    AttentionLevel::Action,
                    "planning finished without a final plan",
                )?;
                db.append_events(
                    session_id,
                    &[daemon_event(
                        "SESSION_PLAN_MISSING",
                        serde_json::json!({
                            "source": "daemon",
                            "reason": "missing_proposed_plan_block",
                            "exit_code": exit_code,
                        }),
                    )],
                )?;
            }
        }
        SessionOutcome::NeedsInput { question } => {
            db.mark_needs_input(session_id, exit_code, question.clone())?;
            db.append_events(
                session_id,
                &[daemon_event(
                    "SESSION_NEEDS_INPUT",
                    serde_json::json!({
                        "source": "daemon",
                        "reason": "clarification_only_noop",
                        "question": question,
                        "exit_code": exit_code,
                        "has_tool_use": false,
                        "has_worktree_changes": false,
                    }),
                )],
            )?;
        }
    }

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
                "outcome": outcome.as_str(),
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
    Ok(
        git::has_worktree_changes(&worktree)?
            || git::has_committed_diff_against_base(&worktree, &session.base_branch)?,
    )
}

enum SessionOutcome {
    Finished,
    NeedsInput { question: String },
}

impl SessionOutcome {
    fn as_str(&self) -> &'static str {
        match self {
            Self::Finished => "finished",
            Self::NeedsInput { .. } => "needs_input",
        }
    }
}

fn classify_session_outcome(
    db: &Database,
    session: &SessionRecord,
    exit_code: Option<i32>,
) -> Result<SessionOutcome> {
    if exit_code != Some(0) || session.agent != "codex" || session.thread_id.is_none() {
        return Ok(SessionOutcome::Finished);
    }

    if session_has_pending_changes(session)? {
        return Ok(SessionOutcome::Finished);
    }

    let events = db.list_events_since(&session.session_id, None)?;
    let boundary_id = events
        .iter()
        .rev()
        .find(|event| {
            matches!(
                event.event_type.as_str(),
                "SESSION_STARTED" | "SESSION_RESUMED" | "SESSION_REPLY_REQUESTED"
            )
        })
        .map(|event| event.id)
        .unwrap_or(0);

    let mut agent_messages = Vec::new();
    for event in &events {
        if event.id <= boundary_id {
            continue;
        }
        if event.event_type != "CODEX_ITEM_COMPLETED" {
            continue;
        }
        let Some(item) = event.payload_json.get("raw").and_then(|raw| raw.get("item")) else {
            continue;
        };
        let Some(item_type) = item.get("type").and_then(|value| value.as_str()) else {
            return Ok(SessionOutcome::Finished);
        };
        if item_type != "agent_message" {
            return Ok(SessionOutcome::Finished);
        }
        if let Some(text) = item.get("text").and_then(|value| value.as_str()) {
            agent_messages.push(text.to_string());
        }
    }

    if agent_messages.len() != 1 {
        return Ok(SessionOutcome::Finished);
    }
    let question = agent_messages.pop().unwrap_or_default();
    if !looks_like_blocking_question(&question) {
        return Ok(SessionOutcome::Finished);
    }

    Ok(SessionOutcome::NeedsInput {
        question: preview_text(&question, 240),
    })
}

fn looks_like_blocking_question(text: &str) -> bool {
    let trimmed = text.trim();
    if !trimmed.ends_with('?') {
        return false;
    }

    let normalized = trimmed.to_ascii_lowercase();
    [
        "do you want",
        "can you",
        "could you",
        "which",
        "what",
        "if yes",
        "if so",
        "should i",
    ]
    .iter()
    .any(|prefix| normalized.contains(prefix))
}

fn persist_extracted_plans(
    db: &Database,
    session_id: &str,
    events: &[SessionEvent],
) -> Result<()> {
    for event in events {
        let Some(body_markdown) = extract_plan_markdown(event) else {
            continue;
        };
        if db
            .latest_plan(session_id)?
            .as_ref()
            .is_some_and(|plan| plan.body_markdown == body_markdown)
        {
            continue;
        }
        let summary = summarize_plan(&body_markdown);
        let plan = db.insert_plan(session_id, &summary, &body_markdown, event.id)?;
        db.set_integration_state(
            session_id,
            IntegrationState::Clean,
            AttentionLevel::Notice,
            &format!("plan ready v{}: {}", plan.version, plan.summary),
        )?;
        db.append_events(
            session_id,
            &[daemon_event(
                "SESSION_PLAN_READY",
                serde_json::json!({
                    "source": "daemon",
                    "version": plan.version,
                    "summary": plan.summary,
                    "body_markdown": plan.body_markdown,
                    "source_event_id": plan.source_event_id,
                }),
            )],
        )?;
    }
    Ok(())
}

fn extract_plan_markdown(event: &SessionEvent) -> Option<String> {
    let item = event.payload_json.get("raw")?.get("item")?;
    if item.get("type").and_then(|value| value.as_str()) != Some("agent_message") {
        return None;
    }
    let text = item.get("text").and_then(|value| value.as_str())?;
    let start_tag = "<proposed_plan>";
    let end_tag = "</proposed_plan>";
    let start = text.find(start_tag)? + start_tag.len();
    let end = text[start..].find(end_tag)? + start;
    let body = text[start..end].trim();
    if body.is_empty() {
        return None;
    }
    Some(body.to_string())
}

fn summarize_plan(body_markdown: &str) -> String {
    for line in body_markdown.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let trimmed = trimmed.trim_start_matches('#').trim();
        let trimmed = trimmed.trim_start_matches(['-', '*', '1', '2', '3', '4', '5', '6', '7', '8', '9', '.', ' ']).trim();
        if !trimmed.is_empty() {
            return preview_text(trimmed, 80);
        }
    }
    "plan ready".to_string()
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

fn configure_spawn_command(
    command: &mut CommandBuilder,
    request: &SessionStartRequest<'_>,
) -> Result<()> {
    command.args(request.launch.args.clone());
    if let Some(model) = request.model {
        command.arg("--model");
        command.arg(model);
    }
    if request.launch.agent_name == "codex" && request.thread_id.is_some() {
        command.arg("exec");
        match request.mode {
            SessionStartMode::Create => {
                command.arg("--json");
                if !request.prompt_text.is_empty() {
                    command.arg(request.prompt_text);
                }
            }
            SessionStartMode::Resume => {
                let resume_session_id = request.resume_session_id.ok_or_else(|| {
                    anyhow!(
                        "session `{}` does not have an exact Codex resume id",
                        request.session_id
                    )
                })?;
                command.arg("resume");
                command.arg("--json");
                command.arg(resume_session_id);
                if let Some(prompt) = request.resume_prompt.filter(|value| !value.is_empty()) {
                    command.arg(prompt);
                }
            }
        }
        return Ok(());
    }

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

fn pump_codex_json_to_logs(
    mut reader: Box<dyn Read + Send>,
    raw_log_path: &Utf8PathBuf,
    rendered_log_path: &Utf8PathBuf,
    runtime: &SessionRuntime,
    db: &Database,
    session_id: &str,
    thread_id: &str,
    is_plan_session: bool,
) -> Result<()> {
    let mut raw_file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(raw_log_path.as_std_path())
        .with_context(|| format!("failed to open {}", raw_log_path))?;
    let mut rendered_file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(rendered_log_path.as_std_path())
        .with_context(|| format!("failed to open {}", rendered_log_path))?;
    let mut stream = CodexJsonStream::default();
    let mut bound_upstream_thread_id = db
        .get_thread(thread_id)?
        .and_then(|thread| thread.upstream_thread_id);
    let mut buffer = [0_u8; 8192];

    loop {
        let bytes_read = reader.read(&mut buffer)?;
        if bytes_read == 0 {
            break;
        }

        raw_file.write_all(&buffer[..bytes_read])?;
        raw_file.flush()?;
        runtime.publish_output(&buffer[..bytes_read])?;

        let (messages, issues) = stream.push_bytes(&buffer[..bytes_read]);
        persist_codex_output(
            db,
            session_id,
            thread_id,
            &mut bound_upstream_thread_id,
            &mut rendered_file,
            messages,
            issues,
            is_plan_session,
        )?;
    }

    let (messages, issues) = stream.finish();
    persist_codex_output(
        db,
        session_id,
        thread_id,
        &mut bound_upstream_thread_id,
        &mut rendered_file,
        messages,
        issues,
        is_plan_session,
    )?;
    Ok(())
}

fn persist_codex_output(
    db: &Database,
    session_id: &str,
    thread_id: &str,
    bound_upstream_thread_id: &mut Option<String>,
    rendered_file: &mut File,
    messages: Vec<crate::codex_json::ParsedCodexMessage>,
    issues: Vec<crate::codex_json::ParseIssue>,
    is_plan_session: bool,
) -> Result<()> {
    let mut events = Vec::with_capacity(messages.len() + issues.len() + 1);

    for message in messages {
        if let Some(upstream_thread_id) = message.upstream_thread_id.as_deref() {
            if bound_upstream_thread_id.as_deref() != Some(upstream_thread_id) {
                db.set_thread_upstream_id(thread_id, upstream_thread_id)?;
                db.set_resume_session_id(session_id, upstream_thread_id)?;
                *bound_upstream_thread_id = Some(upstream_thread_id.to_string());
                events.push(daemon_event(
                    "THREAD_UPSTREAM_BOUND",
                    serde_json::json!({
                        "source": "daemon",
                        "thread_id": thread_id,
                        "session_id": session_id,
                        "upstream_thread_id": upstream_thread_id,
                    }),
                ));
            }
        }
        rendered_file.write_all(message.rendered.as_bytes())?;
        events.push(NewSessionEvent {
            event_type: message.event_type,
            payload_json: message.payload_json,
        });
    }

    for issue in issues {
        let rendered = format!("[invalid codex json] {}\n", preview_line(&issue.line));
        rendered_file.write_all(rendered.as_bytes())?;
        events.push(daemon_event(
            "CODEX_JSON_PARSE_ERROR",
            serde_json::json!({
                "source": "daemon",
                "thread_id": thread_id,
                "line_preview": preview_line(&issue.line),
                "error": issue.error,
            }),
        ));
    }

    if !events.is_empty() {
        let inserted = db.append_events(session_id, &events)?;
        if is_plan_session {
            persist_extracted_plans(db, session_id, &inserted)?;
        }
        rendered_file.flush()?;
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

fn preview_text(text: &str, limit: usize) -> String {
    let mut preview = text.replace('\n', "\\n").replace('\r', "\\r");
    if preview.len() > limit {
        preview.truncate(limit);
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

fn ensure_pending_review(session: &SessionRecord, action: &str) -> Result<()> {
    if session.integration_state != IntegrationState::PendingReview {
        bail!(
            "session `{}` is not pending review; cannot {action}",
            session.session_id
        );
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

#[cfg(test)]
mod tests {
    use super::{AttachControl, SessionOutcome, SessionRuntime, classify_session_outcome, daemon_event};
    use crate::db::{Database, NewSession, NewThread};
    use crate::terminal_state::TerminalStateEngine;
    use agentd_shared::{event::NewSessionEvent, paths::AppPaths, session::SessionMode};
    use anyhow::Result;
    use serde_json::json;
    use std::{
        fs,
        process::Command,
        sync::Arc,
        time::{SystemTime, UNIX_EPOCH},
    };

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

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
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

    #[test]
    fn classify_session_outcome_marks_clarification_only_run_as_needs_input() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        db.insert_session(&NewSession {
            session_id: "demo",
            thread_id: Some("thread-demo"),
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            mode: SessionMode::Execute,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: Some("resume-1"),
            workspace: repo.as_str(),
            repo_path: repo.as_str(),
            task: "clarify",
            base_branch: "main",
            branch: "agent/clarify",
            worktree: repo.as_str(),
        })
        .unwrap();
        db.insert_thread(&NewThread {
            thread_id: "thread-demo",
            session_id: "demo",
            agent: "codex",
            title: "clarify",
            initial_prompt: "clarify",
        })
        .unwrap();
        db.append_events(
            "demo",
            &[NewSessionEvent {
                event_type: "CODEX_ITEM_COMPLETED".to_string(),
                payload_json: json!({
                    "raw": {
                        "item": {
                            "type": "agent_message",
                            "text": "Do you want me to make the change?"
                        }
                    }
                }),
            }],
        )
        .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        let outcome = classify_session_outcome(&db, &session, Some(0)).unwrap();
        assert!(matches!(
            outcome,
            SessionOutcome::NeedsInput { ref question }
                if question == "Do you want me to make the change?"
        ));
    }

    #[test]
    fn classify_session_outcome_uses_latest_resume_boundary() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        let repo = paths.root.join("repo");
        init_git_repo(repo.as_str());

        db.insert_session(&NewSession {
            session_id: "demo",
            thread_id: Some("thread-demo"),
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            mode: SessionMode::Plan,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: Some("resume-1"),
            workspace: repo.as_str(),
            repo_path: repo.as_str(),
            task: "plan",
            base_branch: "main",
            branch: "agent/plan",
            worktree: repo.as_str(),
        })
        .unwrap();
        db.insert_thread(&NewThread {
            thread_id: "thread-demo",
            session_id: "demo",
            agent: "codex",
            title: "plan",
            initial_prompt: "plan",
        })
        .unwrap();
        db.append_events(
            "demo",
            &[
                daemon_event("SESSION_STARTED", json!({"source":"daemon"})),
                NewSessionEvent {
                    event_type: "CODEX_ITEM_COMPLETED".to_string(),
                    payload_json: json!({
                        "raw": {
                            "item": {
                                "type": "agent_message",
                                "text": "What area should I focus on?"
                            }
                        }
                    }),
                },
                daemon_event("SESSION_REPLY_REQUESTED", json!({"source":"daemon"})),
                daemon_event("SESSION_RESUMED", json!({"source":"daemon"})),
                NewSessionEvent {
                    event_type: "CODEX_ITEM_COMPLETED".to_string(),
                    payload_json: json!({
                        "raw": {
                            "item": {
                                "type": "agent_message",
                                "text": "Should I optimize for speed or correctness?"
                            }
                        }
                    }),
                },
            ],
        )
        .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        let outcome = classify_session_outcome(&db, &session, Some(0)).unwrap();
        assert!(matches!(
            outcome,
            SessionOutcome::NeedsInput { ref question }
                if question == "Should I optimize for speed or correctness?"
        ));
    }
}
