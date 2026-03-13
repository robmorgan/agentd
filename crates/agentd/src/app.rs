use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    sync::{Arc, Mutex},
};

use anyhow::{Context, Result, anyhow, bail};
use camino::Utf8PathBuf;
use portable_pty::{CommandBuilder, PtySize, native_pty_system};
use tokio::task;

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    session::{CreateSessionResult, SessionRecord, SessionStatus, branch_name_from_task},
};

use crate::{
    db::{Database, NewSession},
    git,
    ids::generate_session_id,
};

#[derive(Clone)]
pub struct AppState {
    pub paths: AppPaths,
    pub db: Database,
    pub config: Config,
}

impl AppState {
    pub fn new(paths: AppPaths, db: Database, config: Config) -> Self {
        Self { paths, db, config }
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

        task::spawn_blocking(move || {
            let workspace = Utf8PathBuf::from(workspace);
            let repo_root = git::canonical_repo_root(&workspace)?;
            let agent = config.require_agent(&agent_name)?.clone();

            let session_id = unique_session_id(&db)?;
            let worktree = paths.worktree_path(&session_id);
            let branch = unique_branch_name(&repo_root, &task_text, &session_id)?;

            db.insert_session(&NewSession {
                session_id: &session_id,
                agent: &agent_name,
                workspace: repo_root.as_str(),
                task: &task_text,
                branch: &branch,
                worktree: worktree.as_str(),
            })?;

            if let Err(err) = git::create_worktree(&repo_root, &branch, &worktree) {
                let _ = db.mark_failed(&session_id, err.to_string());
                return Err(err);
            }

            let log_path = paths.log_path(&session_id);
            if let Err(err) = File::create(log_path.as_std_path()) {
                let err = anyhow!(err).context("failed to create session log file");
                let _ = db.mark_failed(&session_id, err.to_string());
                return Err(err);
            }

            let pty_system = native_pty_system();
            let pair = pty_system
                .openpty(PtySize {
                    rows: 48,
                    cols: 160,
                    pixel_width: 0,
                    pixel_height: 0,
                })
                .context("failed to allocate PTY")?;

            let mut command = CommandBuilder::new(agent.command);
            command.args(agent.args);
            command.cwd(worktree.as_std_path());
            for (key, value) in std::env::vars() {
                command.env(&key, &value);
            }
            command.env("AGENTD_SESSION_ID", &session_id);
            command.env("AGENTD_WORKSPACE", repo_root.as_str());
            command.env("AGENTD_WORKTREE", worktree.as_str());
            command.env("AGENTD_BRANCH", &branch);
            command.env("AGENTD_TASK", &task_text);

            let mut child = match pair.slave.spawn_command(command) {
                Ok(child) => child,
                Err(err) => {
                    let err = anyhow!(err).context("failed to spawn agent process");
                    let _ = db.mark_failed(&session_id, err.to_string());
                    return Err(err);
                }
            };

            let pid = child.process_id().map(|value| value as u32);
            if let Some(pid) = pid {
                db.mark_running(&session_id, pid)?;
            } else {
                db.mark_running(&session_id, 0)?;
            }

            let reader = pair.master.try_clone_reader().context("failed to clone PTY reader")?;
            let writer_db = db.clone();
            let writer_session_id = session_id.clone();
            let writer_log_path = log_path.clone();
            std::thread::spawn(move || {
                if let Err(err) = pump_pty_to_log(reader, &writer_log_path) {
                    let _ = writer_db.mark_failed(&writer_session_id, err.to_string());
                }
            });

            let exit_db = db.clone();
            let exit_session_id = session_id.clone();
            std::thread::spawn(move || {
                let status = child.wait();
                let code = status.ok().map(|value| value.exit_code() as i32);
                let _ = exit_db.mark_exited(&exit_session_id, code);
            });

            Ok(CreateSessionResult {
                session_id,
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

fn unique_branch_name(repo_root: &camino::Utf8Path, task_text: &str, session_id: &str) -> Result<String> {
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

fn pump_pty_to_log(mut reader: Box<dyn Read + Send>, log_path: &Utf8PathBuf) -> Result<()> {
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

        let mut file = file.lock().map_err(|_| anyhow!("log writer poisoned"))?;
        file.write_all(&buffer[..bytes_read])?;
        file.flush()?;
    }

    Ok(())
}

fn process_exists(pid: Option<u32>) -> bool {
    let Some(pid) = pid else {
        return false;
    };
    if pid == 0 {
        return false;
    }

    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(pid as i32),
        None,
    )
    .is_ok()
}
