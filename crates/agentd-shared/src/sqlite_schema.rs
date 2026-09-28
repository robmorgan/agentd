//! SQLite schema for `state.db`.
//!
//! Schema v8 replaces the git columns of the Rust daemon's v7 layout with a
//! single `cwd` column (see docs/drop-worktrees.md). It must match
//! go/internal/db/db.go exactly: the Go daemon and the CLI's local fallback may
//! open the same file. Opening a v6 or v7 database migrates it in place.

use anyhow::{Context, Result, bail};
use rusqlite::{Connection, TransactionBehavior};

pub const CURRENT_SCHEMA_VERSION: i32 = 8;
pub const EXPECTED_SESSIONS_COLUMNS: &[&str] = &[
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

/// The Rust daemon's v7 layout, kept only so a v6 database can be stepped
/// through v7 on its way to v8.
const CREATE_SESSIONS_TABLE_V7: &str = "
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    model TEXT,
    mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
    workspace TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'exited', 'failed', 'unknown_recovered')),
    integration_policy TEXT NOT NULL CHECK (integration_policy IN ('manual_review', 'auto_apply_safe')),
    worker_pid INTEGER,
    agent_pid INTEGER,
    exit_code INTEGER,
    error TEXT,
    integration_state TEXT NOT NULL CHECK (integration_state IN ('idle', 'auto_applying', 'applied', 'discarded')),
    attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
    attention_summary TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    exited_at TEXT
);";

/// Creates or migrates the schema. The version read and any migration run in
/// one IMMEDIATE transaction, so they happen under SQLite's write lock: a
/// second process (the Go daemon, another CLI) opening the same file waits on
/// the connection's busy timeout instead of racing the migration. A DEFERRED
/// transaction would fail with SQLITE_BUSY at once when two readers both try
/// to upgrade to a write lock, without consulting the busy handler.
/// rusqlite's `Connection::open` sets a 5s busy timeout.
///
/// Only a real version or layout mismatch is reported as an unsupported
/// schema; I/O and locking errors keep their own message.
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
    } else {
        migrate_schema(&tx, schema_version)?;
        ensure_supported_schema(&tx)?;
    }

    tx.commit().context("failed to commit state database schema")?;
    Ok(())
}

fn ensure_supported_schema(conn: &Connection) -> Result<()> {
    let mut stmt = conn.prepare("PRAGMA table_info(sessions)")?;
    let columns =
        stmt.query_map([], |row| row.get::<_, String>(1))?.collect::<rusqlite::Result<Vec<_>>>()?;

    if columns.is_empty() {
        bail!(
            "unsupported state database schema: missing `sessions` table. Remove or migrate the runtime root."
        );
    }
    if columns.iter().map(String::as_str).collect::<Vec<_>>() != EXPECTED_SESSIONS_COLUMNS {
        bail!(
            "unsupported state database schema: `sessions` does not match the current layout. Remove or migrate the runtime root."
        );
    }

    Ok(())
}

fn migrate_schema(conn: &Connection, schema_version: i32) -> Result<()> {
    match schema_version {
        CURRENT_SCHEMA_VERSION => Ok(()),
        6 => {
            migrate_v6_to_v7(conn)?;
            migrate_v7_to_v8(conn)
        }
        7 => migrate_v7_to_v8(conn),
        other => bail!(
            "unsupported state database schema version {other}; expected {CURRENT_SCHEMA_VERSION}. Remove or migrate the runtime root."
        ),
    }
}

fn migrate_v6_to_v7(conn: &Connection) -> Result<()> {
    conn.execute_batch(&format!(
        "
        ALTER TABLE sessions RENAME TO sessions_v6;
        {CREATE_SESSIONS_TABLE_V7}
        INSERT INTO sessions (
            session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
            status, integration_policy, worker_pid, agent_pid, exit_code, error, integration_state,
            attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
            status, integration_policy, NULL, pid, exit_code, error, integration_state,
            attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v6;
        DROP TABLE sessions_v6;
        PRAGMA user_version = 7;
        "
    ))?;
    Ok(())
}

/// Drops the git columns. A v7 session always ran inside its worktree, so that
/// path becomes its cwd; `workspace` is only a fallback for rows the Rust
/// daemon left half-created.
fn migrate_v7_to_v8(conn: &Connection) -> Result<()> {
    conn.execute_batch(&format!(
        "
        ALTER TABLE sessions RENAME TO sessions_v7;
        {CREATE_SESSIONS_TABLE}
        INSERT INTO sessions (
            session_id, agent, model, mode, cwd, status, worker_pid, agent_pid, exit_code, error,
            attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, agent, model, mode,
            CASE WHEN worktree <> '' THEN worktree ELSE workspace END,
            status, worker_pid, agent_pid, exit_code, error,
            attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v7;
        DROP TABLE sessions_v7;
        PRAGMA user_version = 8;
        "
    ))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{
        CREATE_SESSIONS_TABLE_V7, CURRENT_SCHEMA_VERSION, EXPECTED_SESSIONS_COLUMNS, init_state_db,
    };
    use rusqlite::{Connection, params};
    use std::{
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

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
        assert_eq!(CURRENT_SCHEMA_VERSION, 8);
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

    /// Mirrors TestMigrateV7ToV8 in go/internal/db/db_test.go: a database laid
    /// out the way the Rust daemon writes it keeps its rows, and the worktree
    /// path becomes the session cwd.
    #[test]
    fn v7_database_migrates_worktree_to_cwd() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        conn.execute_batch(&format!(
            "{CREATE_SESSIONS_TABLE_V7}
            INSERT INTO sessions (
                session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
                status, integration_policy, worker_pid, agent_pid, exit_code, error, integration_state,
                attention, attention_summary, created_at, updated_at, exited_at
            ) VALUES
            ('wt', 'codex', NULL, 'execute', '/repo', '/repo', 'repo', 'main', 'agent/wt', '/root/worktrees/wt',
             'exited', 'manual_review', NULL, NULL, 0, NULL, 'applied',
             'notice', 'finished', '2026-09-01T10:00:00+00:00', '2026-09-01T11:00:00+00:00', '2026-09-01T11:00:00+00:00'),
            ('half', 'codex', 'm', 'execute', '/repo', '/repo', 'repo', 'main', 'agent/half', '',
             'failed', 'manual_review', NULL, NULL, NULL, 'worktree failed', 'idle',
             'action', 'worktree failed', '2026-09-02T10:00:00+00:00', '2026-09-02T10:00:00+00:00', NULL);
            PRAGMA user_version = 7;"
        ))
        .unwrap();

        init_state_db(&mut conn).unwrap();

        assert_eq!(user_version(&conn), 8);
        assert_eq!(columns(&conn), EXPECTED_SESSIONS_COLUMNS);
        let row = |id: &str| {
            conn.query_row(
                "SELECT cwd, status, model, exit_code, exited_at FROM sessions WHERE session_id = ?1",
                [id],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, Option<String>>(2)?,
                        row.get::<_, Option<i32>>(3)?,
                        row.get::<_, Option<String>>(4)?,
                    ))
                },
            )
            .unwrap()
        };
        let (cwd, status, _, exit_code, exited_at) = row("wt");
        assert_eq!(cwd, "/root/worktrees/wt");
        assert_eq!(status, "exited");
        assert_eq!(exit_code, Some(0));
        assert!(exited_at.is_some());
        let (cwd, _, model, _, _) = row("half");
        assert_eq!(cwd, "/repo", "empty worktree falls back to workspace");
        assert_eq!(model.as_deref(), Some("m"));
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn unknown_schema_version_is_refused() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        conn.execute_batch("CREATE TABLE sessions (x); PRAGMA user_version = 99;").unwrap();
        let err = init_state_db(&mut conn).unwrap_err().to_string();
        assert!(err.contains("unsupported state database schema version 99"), "{err}");
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
        assert!(!err.contains("Remove or migrate"), "{err}");
        holder.execute_batch("ROLLBACK").unwrap();
        let _ = std::fs::remove_file(path);
    }
}
