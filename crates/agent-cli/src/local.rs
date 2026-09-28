//! Local degraded mode: read and clean up `state.db` directly when no
//! compatible daemon is reachable. The schema is shared with the Go daemon
//! (see agentd_shared::sqlite_schema), so this must only touch v8 columns.

use std::{
    fs,
    time::{Duration, Instant},
};

use agentd_shared::{
    paths::AppPaths,
    process::process_exists,
    session::{AttentionLevel, SessionMode, SessionRecord, SessionStatus},
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

const SELECT_SESSION: &str =
    "SELECT session_id, agent, model, mode, cwd, status, worker_pid, agent_pid,
        exit_code, error, attention, attention_summary, created_at, updated_at, exited_at
 FROM sessions";

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
        let mut stmt = conn.prepare(&format!("{SELECT_SESSION} ORDER BY created_at DESC"))?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>().map_err(Into::into)
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            &format!("{SELECT_SESSION} WHERE session_id = ?1"),
            params![session_id],
            row_to_session,
        )
        .optional()
        .map_err(Into::into)
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
    session.status == SessionStatus::Running && process_exists(session.worker_pid)
}

pub fn normalize_session(session: SessionRecord) -> SessionRecord {
    if session.status == SessionStatus::Running && !process_exists(session.worker_pid) {
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

/// Removes what agentd itself owns for a session: its logs. The session's
/// working directory belongs to the user and is never touched.
pub fn remove_session_artifacts(paths: &AppPaths, session: &SessionRecord) -> Result<()> {
    remove_log_if_present(paths, session)
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        agent: row.get(1)?,
        model: row.get(2)?,
        mode: str_to_mode(&row.get::<_, String>(3)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(3, rusqlite::types::Type::Text, Box::new(err))
        })?,
        cwd: row.get(4)?,
        status: str_to_status(&row.get::<_, String>(5)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(5, rusqlite::types::Type::Text, Box::new(err))
        })?,
        worker_pid: row.get::<_, Option<u32>>(6)?,
        agent_pid: row.get::<_, Option<u32>>(7)?,
        exit_code: row.get(8)?,
        error: row.get(9)?,
        attention: str_to_attention(&row.get::<_, String>(10)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                10,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(11)?,
        created_at: parse_time(row.get::<_, String>(12)?)?,
        updated_at: parse_time(row.get::<_, String>(13)?)?,
        exited_at: row.get::<_, Option<String>>(14)?.map(parse_time).transpose()?,
    })
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
            Ok(()) => {
                if !process_exists(Some(pid.as_raw() as u32)) {
                    return true;
                }
            }
            Err(Errno::ESRCH) => return true,
            Err(_) => return false,
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
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
    use super::{LocalStore, normalize_degraded_session, remove_session_artifacts};
    use agentd_shared::{
        paths::AppPaths,
        session::{AttentionLevel, SessionMode, SessionRecord, SessionStatus},
    };
    use chrono::Utc;
    use rusqlite::{Connection, params};
    use std::{
        fs,
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

    static TEST_PATH_COUNTER: AtomicU64 = AtomicU64::new(0);

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos()
            + u128::from(TEST_PATH_COUNTER.fetch_add(1, Ordering::Relaxed));
        let root = camino::Utf8PathBuf::from(format!("/tmp/agent-local-test-{suffix}"));
        AppPaths {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            sessions_dir: root.join("sessions"),
            root,
        }
    }

    fn demo_session(cwd: &str) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            cwd: cwd.to_string(),
            status: SessionStatus::Running,
            worker_pid: Some(1),
            agent_pid: Some(2),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at: now,
            updated_at: now,
            exited_at: None,
        }
    }

    /// Rows written the way go/internal/db writes them must read back here.
    #[test]
    fn local_store_reads_and_updates_v8_rows() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let store = LocalStore::open(&paths).unwrap();
        let conn = Connection::open(paths.database.as_std_path()).unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, model, mode, cwd, status, worker_pid, agent_pid,
                attention, attention_summary, created_at, updated_at
            ) VALUES (?1, 'sh', NULL, 'execute', ?2, 'running', 999999, 999998, 'info', 'running',
                '2026-09-28T01:02:03.5+00:00', '2026-09-28T01:02:03+00:00')",
            params!["demo", "/work/demo"],
        )
        .unwrap();

        let session = store.get_session("demo").unwrap().unwrap();
        assert_eq!(session.cwd, "/work/demo");
        assert_eq!(session.status, SessionStatus::Running);
        assert_eq!(session.worker_pid, Some(999999));
        assert_eq!(store.list_sessions().unwrap().len(), 1);

        store.mark_exited("demo", Some(3)).unwrap();
        let session = store.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, SessionStatus::Exited);
        assert_eq!(session.exit_code, Some(3));
        assert!(session.exited_at.is_some());

        store.delete_session("demo").unwrap();
        assert!(store.get_session("demo").unwrap().is_none());
    }

    #[test]
    fn remove_session_artifacts_removes_logs_but_not_cwd() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let cwd = paths.root.join("work");
        fs::create_dir_all(cwd.as_std_path()).unwrap();
        fs::write(cwd.join("file.txt").as_std_path(), "keep\n").unwrap();
        fs::write(paths.log_path("demo").as_std_path(), "log").unwrap();
        fs::write(paths.rendered_log_path("demo").as_std_path(), "log").unwrap();

        remove_session_artifacts(&paths, &demo_session(cwd.as_str())).unwrap();

        assert!(!paths.log_path("demo").exists());
        assert!(!paths.rendered_log_path("demo").exists());
        assert!(cwd.join("file.txt").exists());
    }

    #[test]
    fn degraded_mode_normalizes_running_session_to_recovered() {
        let session = demo_session("/tmp/repo");
        let normalized = normalize_degraded_session(session);
        assert_eq!(normalized.status, SessionStatus::UnknownRecovered);
    }

    #[test]
    fn degraded_mode_preserves_exited_status() {
        let mut session = demo_session("/tmp/repo");
        session.status = SessionStatus::Exited;
        let normalized = normalize_degraded_session(session);
        assert_eq!(normalized.status, SessionStatus::Exited);
    }
}
