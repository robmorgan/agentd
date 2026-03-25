use std::{
    fs,
    process::Command,
    time::{Duration, Instant},
};

use agentd_shared::{
    paths::AppPaths,
    session::{
        ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord, SessionStatus,
    },
    sqlite_schema::init_state_db,
};
use anyhow::{Context, Result, anyhow, bail};
use chrono::{DateTime, Utc};
use nix::{
    errno::Errno,
    sys::signal::{Signal, kill},
    unistd::Pid,
};
use rusqlite::{Connection, OptionalExtension, params};

#[derive(Debug)]
pub struct LocalStore {
    path: String,
}

impl LocalStore {
    pub fn open(paths: &AppPaths) -> Result<Self> {
        let store = Self { path: paths.database.to_string() };
        store.init()?;
        Ok(store)
    }

    pub fn list_sessions(&self) -> Result<Vec<SessionRecord>> {
        let conn = self.connect()?;
        let mut stmt = conn.prepare(
            "SELECT session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch,
                    worktree, status, integration_policy, integration_state, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
             FROM sessions ORDER BY created_at DESC",
        )?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>().map_err(Into::into).and_then(|sessions| {
            sessions.into_iter().map(refresh_commit_state).collect::<Result<Vec<_>>>()
        })
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch,
                    worktree, status, integration_policy, integration_state, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
             FROM sessions WHERE session_id = ?1",
            params![session_id],
            row_to_session,
        )
        .optional()
        .map_err(Into::into)
        .and_then(|session| session.map(refresh_commit_state).transpose())
    }

    pub fn mark_exited(&self, session_id: &str, exit_code: Option<i32>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, exit_code = ?3, updated_at = ?4, exited_at = ?4
             WHERE session_id = ?1",
            params![session_id, status_to_str(SessionStatus::Exited), exit_code, now],
        )?;
        Ok(())
    }

    pub fn mark_unknown_recovered(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, updated_at = ?3
             WHERE session_id = ?1",
            params![session_id, status_to_str(SessionStatus::UnknownRecovered), now],
        )?;
        Ok(())
    }

    pub fn delete_session(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        conn.execute("DELETE FROM sessions WHERE session_id = ?1", params![session_id])?;
        Ok(())
    }

    fn connect(&self) -> Result<Connection> {
        Connection::open(&self.path).with_context(|| format!("failed to open {}", self.path))
    }

    fn init(&self) -> Result<()> {
        let mut conn = self.connect()?;
        init_state_db(&mut conn)
            .with_context(|| format!("unsupported state database schema in {}", self.path))
    }
}

pub fn session_is_running(session: &SessionRecord) -> bool {
    session.status == SessionStatus::Running && process_exists(session.pid)
}

pub fn normalize_session(session: SessionRecord) -> SessionRecord {
    if session.status == SessionStatus::Running && !process_exists(session.pid) {
        let mut session = session;
        session.status = SessionStatus::UnknownRecovered;
        return session;
    }
    session
}

pub fn normalize_degraded_session(session: SessionRecord) -> SessionRecord {
    match session.status {
        SessionStatus::Running => {
            let mut session = session;
            session.status = SessionStatus::UnknownRecovered;
            session
        }
        _ => normalize_session(session),
    }
}

pub fn terminate_session_process(session_id: &str, pid: Option<u32>) -> Result<()> {
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

pub fn process_exists(pid: Option<u32>) -> bool {
    let Some(pid) = pid else {
        return false;
    };
    if pid == 0 {
        return false;
    }
    kill(Pid::from_raw(pid as i32), None).is_ok()
}

pub fn remove_session_artifacts(paths: &AppPaths, session: &SessionRecord) -> Result<()> {
    remove_worktree_if_present(session)?;
    remove_log_if_present(paths, session)?;
    Ok(())
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        agent: row.get(1)?,
        model: row.get(2)?,
        mode: str_to_mode(&row.get::<_, String>(3)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(3, rusqlite::types::Type::Text, Box::new(err))
        })?,
        workspace: row.get(4)?,
        repo_path: row.get(5)?,
        repo_name: row.get(6)?,
        base_branch: row.get(7)?,
        branch: row.get(8)?,
        worktree: row.get(9)?,
        status: str_to_status(&row.get::<_, String>(10)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                10,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        integration_policy: str_to_integration_policy(&row.get::<_, String>(11)?).map_err(
            |err| {
                rusqlite::Error::FromSqlConversionFailure(
                    11,
                    rusqlite::types::Type::Text,
                    Box::new(err),
                )
            },
        )?,
        apply_state: str_to_apply_state(&row.get::<_, String>(12)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                12,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        dirty_count: 0,
        ahead_count: 0,
        has_commits: false,
        has_pending_changes: false,
        pid: row.get::<_, Option<u32>>(13)?,
        exit_code: row.get(14)?,
        error: row.get(15)?,
        attention: str_to_attention(&row.get::<_, String>(16)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                16,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(17)?,
        created_at: parse_time(row.get::<_, String>(18)?)?,
        updated_at: parse_time(row.get::<_, String>(19)?)?,
        exited_at: row.get::<_, Option<String>>(20)?.map(parse_time).transpose()?,
    })
}

fn refresh_commit_state(mut session: SessionRecord) -> Result<SessionRecord> {
    session.dirty_count = if std::path::Path::new(&session.worktree).exists() {
        worktree_dirty_count(&session.worktree)?
    } else {
        0
    };
    session.ahead_count =
        branch_ahead_count(&session.repo_path, &session.base_branch, &session.branch)?;
    session.has_commits = session.ahead_count > 0;
    session.has_pending_changes = session.has_commits || session.dirty_count > 0;
    Ok(session)
}

fn worktree_dirty_count(worktree: &str) -> Result<u32> {
    let output = Command::new("git")
        .arg("-C")
        .arg(worktree)
        .args(["status", "--porcelain"])
        .output()
        .with_context(|| format!("failed to inspect worktree state for {worktree}"))?;
    if !output.status.success() {
        bail!("{}", String::from_utf8_lossy(&output.stderr).trim());
    }

    Ok(String::from_utf8(output.stdout)?.lines().filter(|line| !line.trim().is_empty()).count()
        as u32)
}

fn branch_ahead_count(repo_path: &str, base_branch: &str, branch: &str) -> Result<u32> {
    let exists = Command::new("git")
        .arg("-C")
        .arg(repo_path)
        .args(["show-ref", "--verify", "--quiet"])
        .arg(format!("refs/heads/{branch}"))
        .status()
        .with_context(|| format!("failed to inspect branch {branch}"))?;
    if !exists.success() {
        return Ok(0);
    }

    let output = Command::new("git")
        .arg("-C")
        .arg(repo_path)
        .args(["rev-list", "--count", &format!("{base_branch}..{branch}")])
        .output()
        .with_context(|| format!("failed to inspect branch diff for {branch}"))?;
    if !output.status.success() {
        bail!("{}", String::from_utf8_lossy(&output.stderr).trim());
    }

    Ok(String::from_utf8(output.stdout)?.trim().parse::<u32>().unwrap_or(0))
}

fn parse_time(value: String) -> rusqlite::Result<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(&value).map(|dt| dt.with_timezone(&Utc)).map_err(|err| {
        rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(err))
    })
}

fn status_to_str(status: SessionStatus) -> &'static str {
    match status {
        SessionStatus::Creating => "creating",
        SessionStatus::Running => "running",
        SessionStatus::Exited => "exited",
        SessionStatus::Failed => "failed",
        SessionStatus::UnknownRecovered => "unknown_recovered",
    }
}

fn str_to_status(value: &str) -> std::result::Result<SessionStatus, std::io::Error> {
    match value {
        "creating" => Ok(SessionStatus::Creating),
        "running" => Ok(SessionStatus::Running),
        "paused" => Ok(SessionStatus::UnknownRecovered),
        "exited" => Ok(SessionStatus::Exited),
        "failed" => Ok(SessionStatus::Failed),
        "unknown_recovered" => Ok(SessionStatus::UnknownRecovered),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session status `{value}`"),
        )),
    }
}

fn str_to_apply_state(value: &str) -> std::result::Result<ApplyState, std::io::Error> {
    match value {
        "idle" | "clean" | "pending_review" => Ok(ApplyState::Idle),
        "auto_applying" => Ok(ApplyState::AutoApplying),
        "applied" => Ok(ApplyState::Applied),
        "discarded" => Ok(ApplyState::Discarded),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown apply state `{value}`"),
        )),
    }
}

fn str_to_integration_policy(
    value: &str,
) -> std::result::Result<IntegrationPolicy, std::io::Error> {
    match value {
        "manual_review" => Ok(IntegrationPolicy::ManualReview),
        "auto_apply_safe" => Ok(IntegrationPolicy::AutoApplySafe),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown integration policy `{value}`"),
        )),
    }
}

fn str_to_attention(value: &str) -> std::result::Result<AttentionLevel, std::io::Error> {
    match value {
        "info" => Ok(AttentionLevel::Info),
        "notice" => Ok(AttentionLevel::Notice),
        "action" => Ok(AttentionLevel::Action),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown attention level `{value}`"),
        )),
    }
}

fn str_to_mode(value: &str) -> std::result::Result<SessionMode, std::io::Error> {
    match value {
        "execute" => Ok(SessionMode::Execute),
        "plan" => Ok(SessionMode::Plan),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session mode `{value}`"),
        )),
    }
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

fn remove_worktree_if_present(session: &SessionRecord) -> Result<()> {
    if !std::path::Path::new(&session.worktree).exists() {
        return Ok(());
    }

    let output = Command::new("git")
        .arg("-C")
        .arg(&session.repo_path)
        .args(["worktree", "remove", "--force", &session.worktree])
        .output()
        .with_context(|| format!("failed to remove worktree {}", session.worktree))?;
    if !output.status.success() {
        bail!("failed to remove worktree: {}", String::from_utf8_lossy(&output.stderr).trim());
    }

    let prune = Command::new("git")
        .arg("-C")
        .arg(&session.repo_path)
        .args(["worktree", "prune"])
        .output()
        .with_context(|| format!("failed to prune worktrees for {}", session.repo_path))?;
    if !prune.status.success() {
        bail!("failed to prune worktrees: {}", String::from_utf8_lossy(&prune.stderr).trim());
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

#[cfg(test)]
mod tests {
    use super::{LocalStore, normalize_degraded_session, refresh_commit_state};
    use agentd_shared::{
        paths::AppPaths,
        session::{
            ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord,
            SessionStatus,
        },
    };
    use chrono::Utc;
    use rusqlite::params;
    use std::{
        fs,
        process::Command,
        time::{SystemTime, UNIX_EPOCH},
    };

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
        let root = camino::Utf8PathBuf::from(format!("/tmp/agent-local-test-{suffix}"));
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

    #[test]
    fn refresh_commit_state_marks_dirty_worktree_as_pending() {
        let repo = init_git_repo("dirty");
        fs::write(repo.join("README.md"), "dirty\n").unwrap();

        let session = refresh_commit_state(demo_session(
            repo.as_str(),
            repo.as_str(),
            "agent/demo",
            false,
            false,
        ))
        .unwrap();

        assert!(!session.has_commits);
        assert!(session.has_pending_changes);
    }

    #[test]
    fn refresh_commit_state_marks_committed_branch_as_pending() {
        let repo = init_git_repo("committed");
        run_git(repo.as_str(), &["checkout", "-b", "agent/demo"]);
        fs::write(repo.join("README.md"), "committed\n").unwrap();
        run_git(repo.as_str(), &["add", "README.md"]);
        run_git(repo.as_str(), &["commit", "-m", "session change"]);

        let session = refresh_commit_state(demo_session(
            repo.as_str(),
            repo.as_str(),
            "agent/demo",
            false,
            false,
        ))
        .unwrap();

        assert!(session.has_commits);
        assert!(session.has_pending_changes);
    }

    fn init_git_repo(name: &str) -> camino::Utf8PathBuf {
        let root = test_paths().root.join(name);
        fs::create_dir_all(root.as_str()).unwrap();
        run_git(root.as_str(), &["init", "-b", "main"]);
        run_git(root.as_str(), &["config", "user.name", "Test User"]);
        run_git(root.as_str(), &["config", "user.email", "test@example.com"]);
        fs::write(root.join("README.md"), "base\n").unwrap();
        run_git(root.as_str(), &["add", "README.md"]);
        run_git(root.as_str(), &["commit", "-m", "initial"]);
        root
    }

    fn run_git(repo: &str, args: &[&str]) {
        let output = Command::new("git").args(["-C", repo]).args(args).output().unwrap();
        assert!(
            output.status.success(),
            "git {:?} failed in {}: {}",
            args,
            repo,
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    fn demo_session(
        repo_path: &str,
        worktree: &str,
        branch: &str,
        has_commits: bool,
        has_pending_changes: bool,
    ) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: repo_path.to_string(),
            repo_path: repo_path.to_string(),
            repo_name: "repo".to_string(),
            base_branch: "main".to_string(),
            branch: branch.to_string(),
            worktree: worktree.to_string(),
            status: SessionStatus::Running,
            integration_policy: IntegrationPolicy::ManualReview,
            apply_state: ApplyState::Idle,
            dirty_count: if has_pending_changes && !has_commits { 1 } else { 0 },
            ahead_count: if has_commits { 1 } else { 0 },
            has_commits,
            has_pending_changes,
            pid: Some(1),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at: now,
            updated_at: now,
            exited_at: None,
        }
    }

    #[test]
    fn degraded_mode_normalizes_running_session_to_recovered() {
        let session = demo_session("/tmp/repo", "/tmp/worktree", "agent/demo", false, false);
        let normalized = normalize_degraded_session(session);
        assert_eq!(normalized.status, SessionStatus::UnknownRecovered);
    }

    #[test]
    fn degraded_mode_preserves_exited_status() {
        let mut session = demo_session("/tmp/repo", "/tmp/worktree", "agent/demo", false, false);
        session.status = SessionStatus::Exited;
        let normalized = normalize_degraded_session(session);
        assert_eq!(normalized.status, SessionStatus::Exited);
    }
}
