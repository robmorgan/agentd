use anyhow::{Result, bail, ensure};
use rusqlite::Connection;

pub const CURRENT_SCHEMA_VERSION: i32 = 5;
const EXPECTED_SESSIONS_COLUMNS: &[&str] = &[
    "session_id",
    "thread_id",
    "agent",
    "model",
    "mode",
    "workspace",
    "repo_path",
    "repo_name",
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
        migrate_schema(&tx, schema_version)?;
        ensure_supported_schema(&tx, CURRENT_SCHEMA_VERSION)?;
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

fn migrate_schema(conn: &Connection, schema_version: i32) -> Result<()> {
    match schema_version {
        CURRENT_SCHEMA_VERSION => Ok(()),
        4 => migrate_v4_to_v5(conn),
        3 => migrate_v3_to_v4(conn),
        other => bail!(
            "unsupported state database schema version {other}; expected {CURRENT_SCHEMA_VERSION}. Remove or migrate the runtime root."
        ),
    }
}

fn migrate_v4_to_v5(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "
        ALTER TABLE sessions RENAME TO sessions_v4;
        CREATE TABLE sessions (
            session_id TEXT PRIMARY KEY,
            thread_id TEXT,
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
        INSERT INTO sessions (
            session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
            base_branch, branch, worktree, status, integration_policy, pid, exit_code, error,
            integration_state, attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
            base_branch, branch, worktree,
            CASE WHEN status = 'needs_input' THEN 'running' ELSE status END,
            integration_policy, pid, exit_code, error,
            integration_state, attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v4;
        DROP TABLE sessions_v4;
        PRAGMA user_version = 5;
        ",
    )?;
    Ok(())
}

fn migrate_v3_to_v4(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "
        ALTER TABLE sessions RENAME TO sessions_v3;
        CREATE TABLE sessions (
            session_id TEXT PRIMARY KEY,
            thread_id TEXT,
            agent TEXT NOT NULL,
            model TEXT,
            mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
            workspace TEXT NOT NULL,
            repo_path TEXT NOT NULL,
            repo_name TEXT NOT NULL,
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
        INSERT INTO sessions (
            session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
            base_branch, branch, worktree, status, integration_policy, pid, exit_code, error,
            integration_state, attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
            base_branch, branch, worktree, status, integration_policy, pid, exit_code, error,
            integration_state, attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v3;
        DROP TABLE sessions_v3;
        PRAGMA user_version = 4;
        ",
    )?;
    migrate_v4_to_v5(conn)
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
            base_branch TEXT NOT NULL,
            branch TEXT NOT NULL,
            worktree TEXT NOT NULL,
            status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'exited', 'failed', 'unknown_recovered')),
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
        PRAGMA user_version = 5;
        ",
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{CURRENT_SCHEMA_VERSION, init_state_db};
    use rusqlite::{Connection, params};
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

    #[test]
    fn init_migrates_v4_needs_input_rows_to_running() {
        let path = temp_db_path();
        let mut conn = Connection::open(&path).unwrap();
        let now = chrono::Utc::now().to_rfc3339();
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
            PRAGMA user_version = 4;
            ",
        )
        .unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
                base_branch, branch, worktree, status, integration_policy, pid, exit_code, error,
                integration_state, attention, attention_summary, created_at, updated_at, exited_at
            ) VALUES (
                ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?20, ?21
            )",
            params![
                "demo",
                Option::<String>::None,
                "codex",
                Option::<String>::None,
                "execute",
                "/tmp/repo",
                "/tmp/repo",
                "repo",
                "main",
                "agent/demo",
                "/tmp/worktree",
                "needs_input",
                "manual_review",
                123_u32,
                Option::<i32>::None,
                Option::<String>::None,
                "idle",
                "action",
                "needs input",
                now,
                Option::<String>::None,
            ],
        )
        .unwrap();

        init_state_db(&mut conn).unwrap();

        let user_version: i32 =
            conn.query_row("PRAGMA user_version", [], |row| row.get(0)).unwrap();
        let status: String = conn
            .query_row("SELECT status FROM sessions WHERE session_id = 'demo'", [], |row| {
                row.get(0)
            })
            .unwrap();

        assert_eq!(user_version, CURRENT_SCHEMA_VERSION);
        assert_eq!(status, "running");
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
                    session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name,
                    base_branch, branch, worktree, status, integration_policy, pid, exit_code, error,
                    integration_state, attention, attention_summary, created_at, updated_at, exited_at
                ) VALUES (
                    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?20, ?21
                )",
                params![
                    "demo",
                    Option::<String>::None,
                    "codex",
                    Option::<String>::None,
                    "execute",
                    "/tmp/repo",
                    "/tmp/repo",
                    "repo",
                    "main",
                    "agent/demo",
                    "/tmp/worktree",
                    "needs_input",
                    "manual_review",
                    123_u32,
                    Option::<i32>::None,
                    Option::<String>::None,
                    "idle",
                    "action",
                    "needs input",
                    now,
                    Option::<String>::None,
                ],
            )
            .unwrap_err()
            .to_string();

        assert!(err.contains("CHECK constraint failed"));
    }
}
