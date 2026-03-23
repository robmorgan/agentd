use anyhow::Result;
use rusqlite::Connection;

pub const CURRENT_SCHEMA_VERSION: i32 = 1;

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
    } else if schema_version != CURRENT_SCHEMA_VERSION {
        reset_schema(&tx)?;
        create_schema(&tx)?;
    }

    tx.commit()?;
    Ok(())
}

fn reset_schema(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "
        DROP TABLE IF EXISTS plans;
        DROP TABLE IF EXISTS events;
        DROP TABLE IF EXISTS threads;
        DROP TABLE IF EXISTS sessions;
        PRAGMA user_version = 0;
        ",
    )?;
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
            integration_state TEXT NOT NULL CHECK (integration_state IN ('clean', 'auto_applying', 'pending_review', 'applied', 'discarded')),
            git_sync TEXT NOT NULL CHECK (git_sync IN ('unknown', 'in_sync', 'needs_sync', 'conflicted')),
            git_status_summary TEXT,
            has_conflicts INTEGER NOT NULL DEFAULT 0 CHECK (has_conflicts IN (0, 1)),
            attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
            attention_summary TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            exited_at TEXT
        );
        PRAGMA user_version = 1;
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
    fn init_resets_legacy_schema() {
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

        init_state_db(&mut conn).unwrap();

        let row_count: i64 =
            conn.query_row("SELECT COUNT(*) FROM sessions", [], |row| row.get(0)).unwrap();
        let has_threads_table: bool = conn
            .query_row(
                "SELECT EXISTS(
                SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'threads'
            )",
                [],
                |row| row.get(0),
            )
            .unwrap();

        assert_eq!(row_count, 0);
        assert!(!has_threads_table);
    }
}
