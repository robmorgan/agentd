//! SQLite schema for `state.db`.
//!
//! It must match go/internal/db/db.go exactly: the Go daemon and the CLI's
//! local fallback may open the same file.

use anyhow::{Context, Result, bail};
use rusqlite::{Connection, TransactionBehavior};

pub const CURRENT_SCHEMA_VERSION: i32 = 1;

const CREATE_SESSIONS_TABLE: &str = "
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    model TEXT,
    mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
    cwd TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'exited', 'failed', 'unknown_recovered')),
    worker_pid INTEGER,
    agent_pid INTEGER,
    exit_code INTEGER,
    error TEXT,
    attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
    attention_summary TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    exited_at TEXT
);";

/// Creates the schema in an empty database, or checks the version of an
/// existing one. The version read and the table creation run in one
/// IMMEDIATE transaction, so they happen under SQLite's write lock: a second
/// process (the Go daemon, another CLI) opening the same file waits on the
/// connection's busy timeout instead of racing the creation. A DEFERRED
/// transaction would fail with SQLITE_BUSY at once when two readers both try
/// to upgrade to a write lock, without consulting the busy handler.
/// rusqlite's `Connection::open` sets a 5s busy timeout.
///
/// Only a real version mismatch is reported as an unsupported schema; I/O and
/// locking errors keep their own message.
pub fn init_state_db(conn: &mut Connection) -> Result<()> {
    let tx = conn
        .transaction_with_behavior(TransactionBehavior::Immediate)
        .context("failed to lock state database for schema initialization")?;
    let schema_version: i32 = tx.query_row("PRAGMA user_version", [], |row| row.get(0))?;
    let has_objects: bool = tx.query_row(
        "SELECT EXISTS(
            SELECT 1
            FROM sqlite_master
            WHERE type IN ('table', 'index', 'trigger', 'view')
              AND name NOT LIKE 'sqlite_%'
        )",
        [],
        |row| row.get(0),
    )?;

    if !has_objects {
        tx.execute_batch(&format!(
            "{CREATE_SESSIONS_TABLE}\nPRAGMA user_version = {CURRENT_SCHEMA_VERSION};"
        ))?;
    } else if schema_version != CURRENT_SCHEMA_VERSION {
        bail!(
            "unsupported state database schema version {schema_version}; remove the runtime root to start fresh"
        );
    }

    tx.commit().context("failed to commit state database schema")?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{CURRENT_SCHEMA_VERSION, init_state_db};
    use rusqlite::{Connection, params};
    use std::{
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

    /// The `sessions` columns, in order, as go/internal/db creates them.
    const EXPECTED_SESSIONS_COLUMNS: &[&str] = &[
        "session_id",
        "agent",
        "model",
        "mode",
        "cwd",
        "status",
        "worker_pid",
        "agent_pid",
        "exit_code",
        "error",
        "attention",
        "attention_summary",
        "created_at",
        "updated_at",
        "exited_at",
    ];

    static TEST_DB_COUNTER: AtomicU64 = AtomicU64::new(0);

    fn temp_db_path() -> String {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
        let counter = TEST_DB_COUNTER.fetch_add(1, Ordering::Relaxed);
        format!("/tmp/agentd-shared-schema-{suffix}-{}-{counter}.db", std::process::id())
    }

    fn user_version(conn: &Connection) -> i32 {
        conn.query_row("PRAGMA user_version", [], |row| row.get(0)).unwrap()
    }

    fn columns(conn: &Connection) -> Vec<String> {
        conn.prepare("PRAGMA table_info(sessions)")
            .unwrap()
            .query_map([], |row| row.get::<_, String>(1))
            .unwrap()
            .collect::<rusqlite::Result<Vec<_>>>()
            .unwrap()
    }

    #[test]
    fn init_creates_current_schema_for_fresh_database() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();

        init_state_db(&mut conn).unwrap();

        assert_eq!(user_version(&conn), CURRENT_SCHEMA_VERSION);
        assert_eq!(CURRENT_SCHEMA_VERSION, 1);
        assert_eq!(columns(&conn), EXPECTED_SESSIONS_COLUMNS);

        // Reopening an already-current database leaves it alone.
        init_state_db(&mut conn).unwrap();
        assert_eq!(user_version(&conn), CURRENT_SCHEMA_VERSION);
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn current_schema_rejects_needs_input_status() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        init_state_db(&mut conn).unwrap();
        let now = chrono::Utc::now().to_rfc3339();

        let err = conn
            .execute(
                "INSERT INTO sessions (
                    session_id, agent, model, mode, cwd, status, attention, attention_summary,
                    created_at, updated_at
                ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9)",
                params![
                    "demo",
                    "codex",
                    Option::<String>::None,
                    "execute",
                    "/tmp/repo",
                    "needs_input",
                    "action",
                    "needs input",
                    now,
                ],
            )
            .unwrap_err()
            .to_string();

        assert!(err.contains("CHECK constraint failed"));
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn unknown_schema_version_is_refused() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        conn.execute_batch("CREATE TABLE sessions (x); PRAGMA user_version = 99;").unwrap();
        let err = init_state_db(&mut conn).unwrap_err().to_string();
        assert!(err.contains("unsupported state database schema version 99"), "{err}");
        assert!(err.contains("remove the runtime root to start fresh"), "{err}");
        let _ = std::fs::remove_file(path);
    }

    /// Several connections initializing one fresh file at once must all
    /// succeed: the IMMEDIATE transaction serializes them on the write lock.
    #[test]
    fn concurrent_init_of_fresh_database_succeeds() {
        let path = temp_db_path();
        let barrier = std::sync::Arc::new(std::sync::Barrier::new(8));
        let handles = (0..8)
            .map(|_| {
                let path = path.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    let mut conn = Connection::open(&path).unwrap();
                    barrier.wait();
                    init_state_db(&mut conn)
                })
            })
            .collect::<Vec<_>>();
        for handle in handles {
            handle.join().unwrap().unwrap();
        }
        let conn = Connection::open(&path).unwrap();
        assert_eq!(user_version(&conn), CURRENT_SCHEMA_VERSION);
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn locked_database_is_not_reported_as_unsupported_schema() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        init_state_db(&mut conn).unwrap();

        let holder = Connection::open(&path).unwrap();
        holder.execute_batch("BEGIN EXCLUSIVE").unwrap();
        conn.busy_timeout(std::time::Duration::from_millis(50)).unwrap();
        let err = format!("{:#}", init_state_db(&mut conn).unwrap_err());
        assert!(err.contains("locked"), "{err}");
        assert!(!err.contains("unsupported"), "{err}");
        assert!(!err.contains("remove the runtime root"), "{err}");
        holder.execute_batch("ROLLBACK").unwrap();
        let _ = std::fs::remove_file(path);
    }
}
