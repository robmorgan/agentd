use anyhow::{Result, bail, ensure};
use rusqlite::Connection;

pub const CURRENT_SCHEMA_VERSION: i32 = 3;
const EXPECTED_SESSIONS_COLUMNS: &[&str] = &[
    "session_id",
    "thread_id",
    "agent",
    "model",
    "mode",
    "workspace",
    "repo_path",
    "repo_name",
    "title",
    "base_branch",
    "branch",
    "worktree",
    "status",
    "integration_policy",
    "pid",
    "exit_code",
    "error",
    "integration_state",
    "attention",
    "attention_summary",
    "created_at",
    "updated_at",
    "exited_at",
];

pub fn init_state_db(conn: &mut Connection) -> Result<()> {
    let tx = conn.transaction()?;
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
        create_schema(&tx)?;
    } else {
        ensure_supported_schema(&tx, schema_version)?;
    }

    tx.commit()?;
    Ok(())
}

fn ensure_supported_schema(conn: &Connection, schema_version: i32) -> Result<()> {
    ensure!(
        schema_version == CURRENT_SCHEMA_VERSION,
        "unsupported state database schema version {schema_version}; expected {CURRENT_SCHEMA_VERSION}. Remove or migrate the runtime root."
    );

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

fn create_schema(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "
        CREATE TABLE sessions (
            session_id TEXT PRIMARY KEY,
            thread_id TEXT,
            agent TEXT NOT NULL,
            model TEXT,
            mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
            workspace TEXT NOT NULL,
            repo_path TEXT NOT NULL,
            repo_name TEXT NOT NULL,
            title TEXT NOT NULL,
            base_branch TEXT NOT NULL,
            branch TEXT NOT NULL,
            worktree TEXT NOT NULL,
            status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'needs_input', 'exited', 'failed', 'unknown_recovered')),
            integration_policy TEXT NOT NULL CHECK (integration_policy IN ('manual_review', 'auto_apply_safe')),
            pid INTEGER,
            exit_code INTEGER,
            error TEXT,
            integration_state TEXT NOT NULL CHECK (integration_state IN ('idle', 'auto_applying', 'applied', 'discarded')),
            attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
            attention_summary TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            exited_at TEXT
        );
        PRAGMA user_version = 3;
        ",
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{CURRENT_SCHEMA_VERSION, init_state_db};
    use rusqlite::Connection;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temp_db_path() -> String {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
        format!("/tmp/agentd-shared-schema-{suffix}.db")
    }

    #[test]
    fn init_creates_current_schema_for_fresh_database() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();

        init_state_db(&mut conn).unwrap();

        let user_version: i32 =
            conn.query_row("PRAGMA user_version", [], |row| row.get(0)).unwrap();
        let columns = conn
            .prepare("PRAGMA table_info(sessions)")
            .unwrap()
            .query_map([], |row| row.get::<_, String>(1))
            .unwrap()
            .collect::<rusqlite::Result<Vec<_>>>()
            .unwrap();

        assert_eq!(user_version, CURRENT_SCHEMA_VERSION);
        assert!(columns.iter().any(|column| column == "repo_name"));
        assert!(!columns.iter().any(|column| column == "task"));
    }

    #[test]
    fn init_rejects_legacy_schema() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        conn.execute_batch(
            "
            CREATE TABLE sessions (
                session_id TEXT PRIMARY KEY,
                agent TEXT NOT NULL,
                workspace TEXT NOT NULL,
                task TEXT NOT NULL,
                branch TEXT NOT NULL,
                worktree TEXT NOT NULL,
                status TEXT NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            ",
        )
        .unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, workspace, task, branch, worktree, status, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8)",
            rusqlite::params![
                "legacy",
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

        let err = init_state_db(&mut conn).unwrap_err().to_string();
        assert!(err.contains("unsupported state database schema version 0"));
    }
}
