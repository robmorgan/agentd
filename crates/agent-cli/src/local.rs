use std::{
    fs::{self, File},
    io::{Read, Seek, SeekFrom, Write},
    process::Command,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use chrono::{DateTime, Utc};
use nix::{
    errno::Errno,
    sys::signal::{Signal, kill},
    unistd::Pid,
};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::Value;

use agentd_shared::{
    event::SessionEvent,
    paths::AppPaths,
    session::{AttentionLevel, SessionRecord, SessionStatus},
};

pub struct LocalStore {
    path: String,
}

impl LocalStore {
    pub fn open(paths: &AppPaths) -> Result<Self> {
        let store = Self {
            path: paths.database.to_string(),
        };
        store.init()?;
        Ok(store)
    }

    pub fn list_sessions(&self) -> Result<Vec<SessionRecord>> {
        let conn = self.connect()?;
        let mut stmt = conn.prepare(
            "SELECT session_id, thread_id, agent, model, workspace, repo_path, task, base_branch, branch,
                    worktree, status, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
             FROM sessions ORDER BY created_at DESC",
        )?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>()
            .map_err(Into::into)
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, thread_id, agent, model, workspace, repo_path, task, base_branch, branch,
                    worktree, status, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
             FROM sessions WHERE session_id = ?1",
            params![session_id],
            row_to_session,
        )
        .optional()
        .map_err(Into::into)
    }

    pub fn list_events_since(
        &self,
        session_id: &str,
        after_id: Option<i64>,
    ) -> Result<Vec<SessionEvent>> {
        let conn = self.connect()?;
        let after_id = after_id.unwrap_or(0);
        let mut stmt = conn.prepare(
            "SELECT id, session_id, timestamp, type, payload_json
             FROM events
             WHERE session_id = ?1 AND id > ?2
             ORDER BY id ASC",
        )?;
        let rows = stmt.query_map(params![session_id, after_id], row_to_event)?;
        rows.collect::<rusqlite::Result<Vec<_>>>()
            .map_err(Into::into)
    }

    pub fn mark_exited(&self, session_id: &str, exit_code: Option<i32>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, exit_code = ?3, updated_at = ?4, exited_at = ?4
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::Exited),
                exit_code,
                now
            ],
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
            params![
                session_id,
                status_to_str(SessionStatus::UnknownRecovered),
                now
            ],
        )?;
        Ok(())
    }

    pub fn delete_session(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        conn.execute(
            "DELETE FROM events WHERE session_id = ?1",
            params![session_id],
        )?;
        let _ = conn.execute(
            "DELETE FROM threads WHERE session_id = ?1",
            params![session_id],
        );
        conn.execute(
            "DELETE FROM sessions WHERE session_id = ?1",
            params![session_id],
        )?;
        Ok(())
    }

    fn connect(&self) -> Result<Connection> {
        Connection::open(&self.path).with_context(|| format!("failed to open {}", self.path))
    }

    fn init(&self) -> Result<()> {
        let conn = self.connect()?;
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS sessions (
                session_id TEXT PRIMARY KEY,
                thread_id TEXT,
                agent TEXT NOT NULL,
                model TEXT,
                workspace TEXT NOT NULL,
                repo_path TEXT,
                task TEXT NOT NULL,
                base_branch TEXT,
                branch TEXT NOT NULL,
                worktree TEXT NOT NULL,
                status TEXT NOT NULL,
                pid INTEGER,
                exit_code INTEGER,
                error TEXT,
                attention TEXT NOT NULL DEFAULT 'info',
                attention_summary TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                exited_at TEXT
            );
            CREATE TABLE IF NOT EXISTS events (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                session_id TEXT NOT NULL,
                timestamp TEXT NOT NULL,
                type TEXT NOT NULL,
                payload_json TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS threads (
                thread_id TEXT PRIMARY KEY,
                session_id TEXT NOT NULL UNIQUE,
                agent TEXT NOT NULL,
                title TEXT NOT NULL,
                initial_prompt TEXT NOT NULL,
                upstream_thread_id TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_events_session_id_id
                ON events (session_id, id);",
        )?;
        ensure_column(
            &conn,
            "thread_id",
            "ALTER TABLE sessions ADD COLUMN thread_id TEXT",
        )?;
        ensure_column(
            &conn,
            "model",
            "ALTER TABLE sessions ADD COLUMN model TEXT",
        )?;
        ensure_column(
            &conn,
            "repo_path",
            "ALTER TABLE sessions ADD COLUMN repo_path TEXT",
        )?;
        ensure_column(
            &conn,
            "base_branch",
            "ALTER TABLE sessions ADD COLUMN base_branch TEXT",
        )?;
        ensure_column(
            &conn,
            "attention",
            "ALTER TABLE sessions ADD COLUMN attention TEXT NOT NULL DEFAULT 'info'",
        )?;
        ensure_column(
            &conn,
            "attention_summary",
            "ALTER TABLE sessions ADD COLUMN attention_summary TEXT",
        )?;
        conn.execute(
            "UPDATE sessions
             SET repo_path = COALESCE(repo_path, workspace),
                 base_branch = COALESCE(base_branch, 'HEAD'),
                 attention = COALESCE(attention, 'info'),
                 attention_summary = COALESCE(attention_summary, task)
             WHERE repo_path IS NULL OR base_branch IS NULL OR attention IS NULL OR attention_summary IS NULL",
            [],
        )?;
        Ok(())
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

pub fn print_log_file(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    let rendered = paths.rendered_log_path(session_id);
    let path = if rendered.exists() {
        rendered
    } else {
        paths.log_path(session_id)
    };
    let mut file =
        File::open(path.as_std_path()).with_context(|| format!("failed to open {}", path))?;
    let mut buffer = Vec::new();
    file.read_to_end(&mut buffer)?;
    print!("{}", String::from_utf8_lossy(&buffer));
    std::io::stdout().flush()?;

    if !follow {
        return Ok(());
    }

    let mut offset = buffer.len() as u64;
    loop {
        let metadata =
            fs::metadata(path.as_std_path()).with_context(|| format!("failed to stat {}", path))?;
        if metadata.len() < offset {
            offset = 0;
        }
        if metadata.len() > offset {
            file.seek(SeekFrom::Start(offset))?;
            let mut chunk = Vec::new();
            file.read_to_end(&mut chunk)?;
            print!("{}", String::from_utf8_lossy(&chunk));
            std::io::stdout().flush()?;
            offset = metadata.len();
        }
        std::thread::sleep(Duration::from_millis(250));
    }
}

fn ensure_column(conn: &Connection, column: &str, ddl: &str) -> Result<()> {
    let mut stmt = conn.prepare("PRAGMA table_info(sessions)")?;
    let mut rows = stmt.query([])?;
    while let Some(row) = rows.next()? {
        let existing: String = row.get(1)?;
        if existing == column {
            return Ok(());
        }
    }
    conn.execute(ddl, [])?;
    Ok(())
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        thread_id: row.get(1)?,
        agent: row.get(2)?,
        model: row.get(3)?,
        workspace: row.get(4)?,
        repo_path: row.get(5)?,
        task: row.get(6)?,
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
        pid: row.get::<_, Option<u32>>(11)?,
        exit_code: row.get(12)?,
        error: row.get(13)?,
        attention: str_to_attention(&row.get::<_, String>(14)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                14,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(15)?,
        created_at: parse_time(row.get::<_, String>(16)?)?,
        updated_at: parse_time(row.get::<_, String>(17)?)?,
        exited_at: row
            .get::<_, Option<String>>(18)?
            .map(parse_time)
            .transpose()?,
    })
}

fn row_to_event(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionEvent> {
    Ok(SessionEvent {
        id: row.get(0)?,
        session_id: row.get(1)?,
        timestamp: parse_time(row.get::<_, String>(2)?)?,
        event_type: row.get(3)?,
        payload_json: parse_payload(row.get::<_, String>(4)?)?,
    })
}

fn parse_time(value: String) -> rusqlite::Result<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(&value)
        .map(|dt| dt.with_timezone(&Utc))
        .map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(err))
        })
}

fn parse_payload(value: String) -> rusqlite::Result<Value> {
    serde_json::from_str(&value).map_err(|err| {
        rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(err))
    })
}

fn status_to_str(status: SessionStatus) -> &'static str {
    match status {
        SessionStatus::Creating => "creating",
        SessionStatus::Running => "running",
        SessionStatus::Paused => "paused",
        SessionStatus::NeedsInput => "needs_input",
        SessionStatus::Exited => "exited",
        SessionStatus::Failed => "failed",
        SessionStatus::UnknownRecovered => "unknown_recovered",
    }
}

fn str_to_status(value: &str) -> std::result::Result<SessionStatus, std::io::Error> {
    match value {
        "creating" => Ok(SessionStatus::Creating),
        "running" => Ok(SessionStatus::Running),
        "paused" => Ok(SessionStatus::Paused),
        "needs_input" => Ok(SessionStatus::NeedsInput),
        "exited" => Ok(SessionStatus::Exited),
        "failed" => Ok(SessionStatus::Failed),
        "unknown_recovered" => Ok(SessionStatus::UnknownRecovered),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session status `{value}`"),
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
        bail!(
            "failed to remove worktree: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    let prune = Command::new("git")
        .arg("-C")
        .arg(&session.repo_path)
        .args(["worktree", "prune"])
        .output()
        .with_context(|| format!("failed to prune worktrees for {}", session.repo_path))?;
    if !prune.status.success() {
        bail!(
            "failed to prune worktrees: {}",
            String::from_utf8_lossy(&prune.stderr).trim()
        );
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

#[cfg(test)]
mod tests {
    use super::LocalStore;
    use agentd_shared::paths::AppPaths;
    use rusqlite::params;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
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
    fn open_migrates_legacy_rows() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let conn = rusqlite::Connection::open(paths.database.as_str()).unwrap();
        conn.execute_batch(
            "CREATE TABLE sessions (
                session_id TEXT PRIMARY KEY,
                agent TEXT NOT NULL,
                workspace TEXT NOT NULL,
                task TEXT NOT NULL,
                branch TEXT NOT NULL,
                worktree TEXT NOT NULL,
                status TEXT NOT NULL,
                pid INTEGER,
                exit_code INTEGER,
                error TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                exited_at TEXT
            );
            CREATE TABLE events (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                session_id TEXT NOT NULL,
                timestamp TEXT NOT NULL,
                type TEXT NOT NULL,
                payload_json TEXT NOT NULL
            );",
        )
        .unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, workspace, task, branch, worktree, status, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8)",
            params![
                "demo",
                "codex",
                "/tmp/repo",
                "test",
                "agent/test",
                "/tmp/worktree",
                "running",
                chrono::Utc::now().to_rfc3339(),
            ],
        )
        .unwrap();

        let store = LocalStore::open(&paths).unwrap();
        let session = store.get_session("demo").unwrap().unwrap();
        assert_eq!(session.repo_path, "/tmp/repo");
        assert_eq!(session.base_branch, "HEAD");
    }
}
