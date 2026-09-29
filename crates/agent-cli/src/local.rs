//! Local degraded mode: read and clean up `state.db` directly when no
//! compatible daemon is reachable. The schema is shared with the Go daemon
//! (see agentd_shared::sqlite_schema), so this must only touch its columns.

use std::{
    fs,
    os::unix::fs::OpenOptionsExt,
    time::{Duration, Instant},
};

use agentd_shared::{
    paths::AppPaths,
    process::process_exists,
    protocol::{Request, Response, read_response, write_request},
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
use tokio::{io::BufReader, net::UnixStream};

/// How long a worker socket gets to accept a connection before the session is
/// treated as not live.
const WORKER_CONNECT_TIMEOUT: Duration = Duration::from_millis(500);
/// How long the worker gets to answer a KillSession request.
const WORKER_REPLY_TIMEOUT: Duration = Duration::from_secs(2);
/// How long a stopped session gets to record its final state. The Go worker
/// gives the agent 5s between SIGTERM and SIGKILL, so this leaves headroom.
const WORKER_STOP_TIMEOUT: Duration = Duration::from_secs(8);
/// Each step of the signal escalation used when the worker does not stop.
const SIGNAL_WAIT_TIMEOUT: Duration = Duration::from_secs(5);
const POLL_INTERVAL: Duration = Duration::from_millis(100);

const SELECT_SESSION: &str =
    "SELECT session_id, agent, model, mode, cwd, status, worker_pid, agent_pid,
        exit_code, error, attention, attention_summary, created_at, updated_at, exited_at,
        workspace
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

    /// Records that a stopped session exited. Like go/internal/db
    /// MarkExitedIfActive it only touches rows still `creating`/`running`, so
    /// it never overwrites the final state a worker recorded itself.
    pub fn mark_exited_if_active(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL,
                 attention = ?3, attention_summary = ?4, updated_at = ?5, exited_at = ?5
             WHERE session_id = ?1 AND status IN ('creating', 'running')",
            params![
                session_id,
                status_to_str(SessionStatus::Exited),
                attention_to_str(AttentionLevel::Notice),
                "finished",
                now
            ],
        )?;
        Ok(())
    }

    /// Records that a session's worker is gone. Mirrors go/internal/db
    /// MarkUnknownRecovered: the recorded pids are cleared so nothing later
    /// signals a pid that may since have been reused, and a final state the
    /// worker already wrote is left alone.
    pub fn mark_unknown_recovered(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, attention = ?3,
                 attention_summary = ?4, updated_at = ?5
             WHERE session_id = ?1 AND status IN ('creating', 'running')",
            params![
                session_id,
                status_to_str(SessionStatus::UnknownRecovered),
                attention_to_str(AttentionLevel::Action),
                "daemon lost the live process",
                now
            ],
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
        // Create the file private to the user before SQLite does (SQLite
        // would use 0644 minus umask); the -wal/-shm files inherit its mode.
        match fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(&self.path) {
            Ok(_) => {}
            Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(err) => return Err(err).with_context(|| format!("failed to create {}", self.path)),
        }
        let mut conn = self.connect()?;
        // init_state_db words real schema mismatches itself; this context
        // only names the file, so a lock or I/O error is not misreported.
        init_state_db(&mut conn)
            .with_context(|| format!("failed to open state database {}", self.path))
    }
}

pub fn session_is_active(session: &SessionRecord) -> bool {
    matches!(session.status, SessionStatus::Creating | SessionStatus::Running)
}

/// Without a daemon nothing can vouch for a `running` row, so list and status
/// show it as recovered. The row itself is not changed.
pub fn normalize_degraded_session(session: SessionRecord) -> SessionRecord {
    match session.status {
        SessionStatus::Running => {
            let mut session = session;
            session.status = SessionStatus::UnknownRecovered;
            session
        }
        _ => session,
    }
}

/// A session is live only if its worker still accepts connections on
/// `sessions/<id>.sock`. The pids in `state.db` are not trusted for this: a
/// worker that died without cleaning up leaves pids that the OS may since have
/// handed to unrelated processes.
pub async fn worker_is_live(paths: &AppPaths, session_id: &str) -> bool {
    let socket = paths.session_socket_path(session_id);
    matches!(
        tokio::time::timeout(WORKER_CONNECT_TIMEOUT, UnixStream::connect(socket.as_std_path()))
            .await,
        Ok(Ok(_))
    )
}

/// Stops a live session. The worker is asked first (a KillSession request on
/// its socket, which the Go worker answers with Ok before stopping the agent's
/// process group gracefully and recording the exit). Signals are only a
/// fallback, and only while the worker socket still answers, which is what
/// ties the recorded pids to a process that is really ours.
pub async fn stop_live_session(
    store: &LocalStore,
    paths: &AppPaths,
    session_id: &str,
) -> Result<()> {
    if request_worker_stop(paths, session_id).await.is_ok()
        && wait_for_session_to_stop(store, session_id, WORKER_STOP_TIMEOUT).await?
    {
        return Ok(());
    }

    let Some(session) = store.get_session(session_id)? else {
        return Ok(());
    };
    if !session_is_active(&session) {
        return Ok(());
    }
    if !worker_is_live(paths, session_id).await {
        // The worker went away without recording an outcome. Its pids can no
        // longer be trusted, so record the stop without signalling anything.
        store.mark_exited_if_active(session_id)?;
        return Ok(());
    }
    escalate_with_signals(store, &session).await
}

async fn request_worker_stop(paths: &AppPaths, session_id: &str) -> Result<()> {
    let socket = paths.session_socket_path(session_id);
    let exchange = async {
        let mut stream = UnixStream::connect(socket.as_std_path()).await?;
        write_request(
            &mut stream,
            &Request::KillSession { session_id: session_id.to_string(), remove: false },
        )
        .await?;
        let mut reader = BufReader::new(stream);
        match read_response(&mut reader).await? {
            Some(Response::Ok) => Ok(()),
            Some(Response::Error { message }) => bail!(message),
            Some(other) => bail!("unexpected worker response: {other:?}"),
            None => bail!("session worker closed the connection"),
        }
    };
    tokio::time::timeout(WORKER_CONNECT_TIMEOUT + WORKER_REPLY_TIMEOUT, exchange)
        .await
        .map_err(|_| anyhow!("timed out waiting for session `{session_id}` worker"))?
}

/// Polls `state.db` until the row leaves `creating`/`running` (or is gone).
async fn wait_for_session_to_stop(
    store: &LocalStore,
    session_id: &str,
    timeout: Duration,
) -> Result<bool> {
    let deadline = Instant::now() + timeout;
    loop {
        match store.get_session(session_id)? {
            Some(session) if session_is_active(&session) => {}
            _ => return Ok(true),
        }
        if Instant::now() >= deadline {
            return Ok(false);
        }
        tokio::time::sleep(POLL_INTERVAL).await;
    }
}

/// The worker answered on its socket but did not stop: SIGTERM it, then
/// SIGKILL the agent's process group and the worker, the way the Go daemon's
/// stopWorker does, and record the exit ourselves.
async fn escalate_with_signals(store: &LocalStore, session: &SessionRecord) -> Result<()> {
    let session_id = session.session_id.as_str();
    let worker = signalable_pid(session.worker_pid).ok_or_else(|| {
        anyhow!("session `{session_id}` did not stop and has no valid worker pid to signal")
    })?;

    send_signal(worker, Signal::SIGTERM, session_id)?;
    if !wait_for_exit(worker, SIGNAL_WAIT_TIMEOUT).await {
        if let Some(agent) = signalable_pid(session.agent_pid) {
            let _ = kill(Pid::from_raw(-agent.as_raw()), Signal::SIGKILL);
        }
        send_signal(worker, Signal::SIGKILL, session_id)?;
        if !wait_for_exit(worker, SIGNAL_WAIT_TIMEOUT).await {
            bail!("session `{session_id}` did not exit after SIGTERM and SIGKILL");
        }
    }
    store.mark_exited_if_active(session_id)
}

/// Only pids that name one ordinary process are ever signalled: 0 and 1 (and
/// anything that would turn negative as a pid_t) have special meanings to
/// kill(2).
fn signalable_pid(pid: Option<u32>) -> Option<Pid> {
    let pid = pid?;
    (pid > 1 && pid <= i32::MAX as u32).then(|| Pid::from_raw(pid as i32))
}

/// Removes what agentd itself owns for a session: its logs. The session's
/// working directory belongs to the user and is never touched.
/// Mirrors what the Go daemon removes for `agent rm`.
pub fn remove_session_artifacts(paths: &AppPaths, session: &SessionRecord) -> Result<()> {
    let id = session.session_id.as_str();
    for path in [
        paths.log_path(id),
        paths.rendered_log_path(id),
        paths.worker_log_path(id),
        paths.session_socket_path(id),
    ] {
        match fs::remove_file(path.as_std_path()) {
            Ok(()) => {}
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
            Err(err) => return Err(anyhow!(err)).context(format!("failed to remove {path}")),
        }
    }
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
        workspace: row.get(15)?,
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

fn attention_to_str(attention: AttentionLevel) -> &'static str {
    match attention {
        AttentionLevel::Info => "info",
        AttentionLevel::Notice => "notice",
        AttentionLevel::Action => "action",
    }
}

fn str_to_status(value: &str) -> std::result::Result<SessionStatus, std::io::Error> {
    match value {
        "creating" => Ok(SessionStatus::Creating),
        "running" => Ok(SessionStatus::Running),
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
        Ok(()) | Err(Errno::ESRCH) => Ok(()),
        Err(err) => Err(anyhow!(err))
            .context(format!("failed to send {signal:?} to session `{session_id}`")),
    }
}

async fn wait_for_exit(pid: Pid, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    loop {
        if !process_exists(Some(pid.as_raw() as u32)) {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(POLL_INTERVAL).await;
    }
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
            workspace: None,
        }
    }

    /// Rows written the way go/internal/db writes them must read back here.
    #[test]
    fn local_store_reads_and_updates_go_written_rows() {
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

        store.mark_exited_if_active("demo").unwrap();
        let session = store.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, SessionStatus::Exited);
        assert_eq!(session.worker_pid, None);
        assert!(session.exited_at.is_some());

        // Neither transition overwrites a final state already recorded.
        store.mark_unknown_recovered("demo").unwrap();
        store.mark_exited_if_active("demo").unwrap();
        let again = store.get_session("demo").unwrap().unwrap();
        assert_eq!(again.status, SessionStatus::Exited);
        assert_eq!(again.updated_at, session.updated_at);

        store.delete_session("demo").unwrap();
        assert!(store.get_session("demo").unwrap().is_none());
    }

    /// Mirrors go/internal/db MarkUnknownRecovered: stale pids are cleared.
    #[test]
    fn mark_unknown_recovered_clears_pids() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let store = LocalStore::open(&paths).unwrap();
        let conn = Connection::open(paths.database.as_std_path()).unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, model, mode, cwd, status, worker_pid, agent_pid,
                attention, attention_summary, created_at, updated_at
            ) VALUES ('demo', 'sh', NULL, 'execute', '/w', 'running', 999999, 999998, 'info', NULL,
                '2026-09-28T01:02:03+00:00', '2026-09-28T01:02:03+00:00')",
            [],
        )
        .unwrap();

        store.mark_unknown_recovered("demo").unwrap();

        let session = store.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, SessionStatus::UnknownRecovered);
        assert_eq!((session.worker_pid, session.agent_pid), (None, None));
        assert_eq!(session.attention, AttentionLevel::Action);
    }

    #[test]
    fn state_db_is_created_private() {
        use std::os::unix::fs::PermissionsExt;
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        LocalStore::open(&paths).unwrap();
        let mode = fs::metadata(paths.database.as_std_path()).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
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
        fs::write(paths.worker_log_path("demo").as_std_path(), "log").unwrap();
        fs::write(paths.session_socket_path("demo").as_std_path(), "").unwrap();

        remove_session_artifacts(&paths, &demo_session(cwd.as_str())).unwrap();

        assert!(!paths.log_path("demo").exists());
        assert!(!paths.rendered_log_path("demo").exists());
        assert!(!paths.worker_log_path("demo").exists());
        assert!(!paths.session_socket_path("demo").exists());
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
