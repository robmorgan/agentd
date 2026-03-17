use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::Value;

use agentd_shared::{
    event::{NewSessionEvent, SessionEvent},
    paths::AppPaths,
    session::{SessionRecord, SessionStatus},
};

#[derive(Clone)]
pub struct Database {
    path: String,
}

pub struct NewSession<'a> {
    pub session_id: &'a str,
    pub agent: &'a str,
    pub agent_command: &'a str,
    pub agent_args_json: &'a str,
    pub workspace: &'a str,
    pub repo_path: &'a str,
    pub task: &'a str,
    pub base_branch: &'a str,
    pub branch: &'a str,
    pub worktree: &'a str,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionLaunchInfo {
    pub command: Option<String>,
    pub args: Option<Vec<String>>,
}

impl Database {
    pub fn open(paths: &AppPaths) -> Result<Self> {
        let db = Self {
            path: paths.database.to_string(),
        };
        db.init()?;
        Ok(db)
    }

    fn connect(&self) -> Result<Connection> {
        Connection::open(&self.path).with_context(|| format!("failed to open {}", self.path))
    }

    fn init(&self) -> Result<()> {
        let conn = self.connect()?;
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS sessions (
                session_id TEXT PRIMARY KEY,
                agent TEXT NOT NULL,
                agent_command TEXT,
                agent_args_json TEXT,
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
            CREATE INDEX IF NOT EXISTS idx_events_session_id_id
                ON events (session_id, id);
            ",
        )?;
        self.ensure_column(
            &conn,
            "repo_path",
            "ALTER TABLE sessions ADD COLUMN repo_path TEXT",
        )?;
        self.ensure_column(
            &conn,
            "agent_command",
            "ALTER TABLE sessions ADD COLUMN agent_command TEXT",
        )?;
        self.ensure_column(
            &conn,
            "agent_args_json",
            "ALTER TABLE sessions ADD COLUMN agent_args_json TEXT",
        )?;
        self.ensure_column(
            &conn,
            "base_branch",
            "ALTER TABLE sessions ADD COLUMN base_branch TEXT",
        )?;
        conn.execute(
            "UPDATE sessions
             SET repo_path = COALESCE(repo_path, workspace),
                 base_branch = COALESCE(base_branch, 'HEAD')
             WHERE repo_path IS NULL OR base_branch IS NULL",
            [],
        )?;
        Ok(())
    }

    fn ensure_column(&self, conn: &Connection, column: &str, ddl: &str) -> Result<()> {
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

    pub fn insert_session(&self, new_session: &NewSession<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, agent_command, agent_args_json, workspace, repo_path, task,
                base_branch, branch, worktree, status, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?12)",
            params![
                new_session.session_id,
                new_session.agent,
                new_session.agent_command,
                new_session.agent_args_json,
                new_session.workspace,
                new_session.repo_path,
                new_session.task,
                new_session.base_branch,
                new_session.branch,
                new_session.worktree,
                status_to_str(SessionStatus::Creating),
                now,
            ],
        )?;
        Ok(())
    }

    pub fn mark_running(&self, session_id: &str, pid: u32) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = ?3,
                 exit_code = NULL,
                 error = NULL,
                 updated_at = ?4,
                 exited_at = NULL
             WHERE session_id = ?1",
            params![session_id, status_to_str(SessionStatus::Running), pid, now],
        )?;
        Ok(())
    }

    pub fn mark_failed(&self, session_id: &str, error: String) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, pid = NULL, error = ?3, updated_at = ?4
             WHERE session_id = ?1",
            params![session_id, status_to_str(SessionStatus::Failed), error, now],
        )?;
        Ok(())
    }

    pub fn mark_paused(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = NULL,
                 exit_code = NULL,
                 error = NULL,
                 updated_at = ?3,
                 exited_at = NULL
             WHERE session_id = ?1",
            params![session_id, status_to_str(SessionStatus::Paused), now],
        )?;
        Ok(())
    }

    pub fn mark_exited(&self, session_id: &str, exit_code: Option<i32>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2, pid = NULL, exit_code = ?3, updated_at = ?4, exited_at = ?4
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
             SET status = ?2, pid = NULL, updated_at = ?3
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::UnknownRecovered),
                now
            ],
        )?;
        Ok(())
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, agent, workspace, repo_path, task, base_branch, branch, worktree,
                    status, pid, exit_code, error, created_at, updated_at, exited_at
             FROM sessions WHERE session_id = ?1",
            params![session_id],
            row_to_session,
        )
        .optional()
        .map_err(Into::into)
    }

    pub fn list_sessions(&self) -> Result<Vec<SessionRecord>> {
        let conn = self.connect()?;
        let mut stmt = conn.prepare(
            "SELECT session_id, agent, workspace, repo_path, task, base_branch, branch, worktree,
                    status, pid, exit_code, error, created_at, updated_at, exited_at
             FROM sessions ORDER BY created_at DESC",
        )?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>()
            .map_err(Into::into)
    }

    pub fn set_launch_info(&self, session_id: &str, command: &str, args_json: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET agent_command = ?2, agent_args_json = ?3, updated_at = ?4
             WHERE session_id = ?1",
            params![session_id, command, args_json, now],
        )?;
        Ok(())
    }

    pub fn get_launch_info(&self, session_id: &str) -> Result<Option<SessionLaunchInfo>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT agent_command, agent_args_json
             FROM sessions
             WHERE session_id = ?1",
            params![session_id],
            |row| {
                Ok(SessionLaunchInfo {
                    command: row.get(0)?,
                    args: parse_agent_args_json(row.get(1)?)?,
                })
            },
        )
        .optional()
        .map_err(Into::into)
    }

    pub fn delete_session(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        conn.execute(
            "DELETE FROM events WHERE session_id = ?1",
            params![session_id],
        )?;
        conn.execute(
            "DELETE FROM sessions WHERE session_id = ?1",
            params![session_id],
        )?;
        Ok(())
    }

    pub fn append_events(
        &self,
        session_id: &str,
        events: &[NewSessionEvent],
    ) -> Result<Vec<SessionEvent>> {
        if events.is_empty() {
            return Ok(Vec::new());
        }

        let mut conn = self.connect()?;
        let tx = conn.transaction()?;
        let mut inserted = Vec::with_capacity(events.len());

        for event in events {
            validate_new_event(event)?;
            let timestamp = Utc::now();
            let payload_json = serde_json::to_string(&event.payload_json)?;
            tx.execute(
                "INSERT INTO events (session_id, timestamp, type, payload_json)
                 VALUES (?1, ?2, ?3, ?4)",
                params![
                    session_id,
                    timestamp.to_rfc3339(),
                    event.event_type,
                    payload_json,
                ],
            )?;
            inserted.push(SessionEvent {
                id: tx.last_insert_rowid(),
                session_id: session_id.to_string(),
                timestamp,
                event_type: event.event_type.clone(),
                payload_json: event.payload_json.clone(),
            });
        }

        tx.commit()?;
        Ok(inserted)
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
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        agent: row.get(1)?,
        workspace: row.get(2)?,
        repo_path: row.get(3)?,
        task: row.get(4)?,
        base_branch: row.get(5)?,
        branch: row.get(6)?,
        worktree: row.get(7)?,
        status: str_to_status(&row.get::<_, String>(8)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(8, rusqlite::types::Type::Text, Box::new(err))
        })?,
        pid: row.get::<_, Option<u32>>(9)?,
        exit_code: row.get(10)?,
        error: row.get(11)?,
        created_at: parse_time(row.get::<_, String>(12)?)?,
        updated_at: parse_time(row.get::<_, String>(13)?)?,
        exited_at: row
            .get::<_, Option<String>>(14)?
            .map(parse_time)
            .transpose()?,
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

fn parse_agent_args_json(value: Option<String>) -> rusqlite::Result<Option<Vec<String>>> {
    let Some(value) = value else {
        return Ok(None);
    };
    serde_json::from_str(&value).map(Some).map_err(|err| {
        rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(err))
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

fn validate_new_event(event: &NewSessionEvent) -> Result<()> {
    if event.event_type.trim().is_empty() {
        anyhow::bail!("event type must not be empty");
    }
    if !event.payload_json.is_object() {
        anyhow::bail!("event payload must be a JSON object");
    }
    Ok(())
}

fn status_to_str(status: SessionStatus) -> &'static str {
    match status {
        SessionStatus::Creating => "creating",
        SessionStatus::Running => "running",
        SessionStatus::Paused => "paused",
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
        "exited" => Ok(SessionStatus::Exited),
        "failed" => Ok(SessionStatus::Failed),
        "unknown_recovered" => Ok(SessionStatus::UnknownRecovered),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session status `{value}`"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::Database;
    use agentd_shared::{event::NewSessionEvent, paths::AppPaths};
    use serde_json::json;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = camino::Utf8PathBuf::from(format!("/tmp/agentd-db-test-{suffix}"));
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
    fn event_rows_round_trip_in_order() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            agent: "codex",
            agent_command: "codex",
            agent_args_json: "[]",
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            task: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
        })
        .unwrap();

        let inserted = db
            .append_events(
                "demo",
                &[
                    NewSessionEvent {
                        event_type: "SESSION_STARTED".to_string(),
                        payload_json: json!({"source":"daemon"}),
                    },
                    NewSessionEvent {
                        event_type: "COMMAND_EXECUTED".to_string(),
                        payload_json: json!({"command":"cargo test"}),
                    },
                ],
            )
            .unwrap();

        assert_eq!(inserted.len(), 2);
        assert!(inserted[0].id < inserted[1].id);

        let listed = db.list_events_since("demo", None).unwrap();
        assert_eq!(listed.len(), 2);
        assert_eq!(listed[0].event_type, "SESSION_STARTED");
        assert_eq!(listed[1].event_type, "COMMAND_EXECUTED");
    }

    #[test]
    fn deleting_session_removes_events() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            agent: "codex",
            agent_command: "codex",
            agent_args_json: "[]",
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            task: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
        })
        .unwrap();
        db.append_events(
            "demo",
            &[NewSessionEvent {
                event_type: "SESSION_STARTED".to_string(),
                payload_json: json!({"source":"daemon"}),
            }],
        )
        .unwrap();

        db.delete_session("demo").unwrap();

        assert!(db.list_events_since("demo", None).unwrap().is_empty());
    }
}
