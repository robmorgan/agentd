use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::Value;

use agentd_shared::{
    event::{NewSessionEvent, SessionEvent},
    paths::AppPaths,
    session::{AttentionLevel, IntegrationState, SessionRecord, SessionStatus},
};

#[derive(Clone)]
pub struct Database {
    path: String,
}

pub struct NewSession<'a> {
    pub session_id: &'a str,
    pub thread_id: Option<&'a str>,
    pub agent: &'a str,
    pub model: Option<&'a str>,
    pub agent_command: &'a str,
    pub agent_args_json: &'a str,
    pub resume_session_id: Option<&'a str>,
    pub workspace: &'a str,
    pub repo_path: &'a str,
    pub task: &'a str,
    pub base_branch: &'a str,
    pub branch: &'a str,
    pub worktree: &'a str,
}

pub struct NewThread<'a> {
    pub thread_id: &'a str,
    pub session_id: &'a str,
    pub agent: &'a str,
    pub title: &'a str,
    pub initial_prompt: &'a str,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionLaunchInfo {
    pub command: Option<String>,
    pub args: Option<Vec<String>>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThreadRecord {
    pub thread_id: String,
    pub session_id: String,
    pub agent: String,
    pub title: String,
    pub initial_prompt: String,
    pub upstream_thread_id: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
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
                thread_id TEXT,
                agent TEXT NOT NULL,
                model TEXT,
                agent_command TEXT,
                agent_args_json TEXT,
                resume_session_id TEXT,
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
                integration_state TEXT NOT NULL DEFAULT 'clean',
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
                ON events (session_id, id);
            CREATE INDEX IF NOT EXISTS idx_threads_session_id
                ON threads (session_id);
            ",
        )?;
        self.ensure_column(
            &conn,
            "thread_id",
            "ALTER TABLE sessions ADD COLUMN thread_id TEXT",
        )?;
        self.ensure_column(&conn, "model", "ALTER TABLE sessions ADD COLUMN model TEXT")?;
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
            "resume_session_id",
            "ALTER TABLE sessions ADD COLUMN resume_session_id TEXT",
        )?;
        self.ensure_column(
            &conn,
            "base_branch",
            "ALTER TABLE sessions ADD COLUMN base_branch TEXT",
        )?;
        self.ensure_column(
            &conn,
            "integration_state",
            "ALTER TABLE sessions ADD COLUMN integration_state TEXT NOT NULL DEFAULT 'clean'",
        )?;
        self.ensure_column(
            &conn,
            "attention",
            "ALTER TABLE sessions ADD COLUMN attention TEXT NOT NULL DEFAULT 'info'",
        )?;
        self.ensure_column(
            &conn,
            "attention_summary",
            "ALTER TABLE sessions ADD COLUMN attention_summary TEXT",
        )?;
        conn.execute(
            "UPDATE sessions
             SET repo_path = COALESCE(repo_path, workspace),
                 base_branch = COALESCE(base_branch, 'HEAD'),
                 integration_state = COALESCE(integration_state, 'clean'),
                 attention = COALESCE(attention, 'info'),
                 attention_summary = COALESCE(attention_summary, task)
             WHERE repo_path IS NULL OR base_branch IS NULL OR integration_state IS NULL OR attention IS NULL OR attention_summary IS NULL",
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
                session_id, thread_id, agent, model, agent_command, agent_args_json, resume_session_id, workspace,
                repo_path, task, base_branch, branch, worktree, status, attention, attention_summary,
                integration_state, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?18)",
            params![
                new_session.session_id,
                new_session.thread_id,
                new_session.agent,
                new_session.model,
                new_session.agent_command,
                new_session.agent_args_json,
                new_session.resume_session_id,
                new_session.workspace,
                new_session.repo_path,
                new_session.task,
                new_session.base_branch,
                new_session.branch,
                new_session.worktree,
                status_to_str(SessionStatus::Creating),
                attention_to_str(AttentionLevel::Info),
                new_session.task,
                integration_state_to_str(IntegrationState::Clean),
                now,
            ],
        )?;
        Ok(())
    }

    pub fn insert_thread(&self, new_thread: &NewThread<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO threads (
                thread_id, session_id, agent, title, initial_prompt, created_at, updated_at
             ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?6)",
            params![
                new_thread.thread_id,
                new_thread.session_id,
                new_thread.agent,
                new_thread.title,
                new_thread.initial_prompt,
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
                 integration_state = ?3,
                 pid = ?4,
                 exit_code = NULL,
                 error = NULL,
                 attention = ?5,
                 attention_summary = ?6,
                 updated_at = ?7,
                 exited_at = NULL
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::Running),
                integration_state_to_str(IntegrationState::Clean),
                pid,
                attention_to_str(AttentionLevel::Info),
                "running",
                now,
            ],
        )?;
        Ok(())
    }

    pub fn mark_failed(&self, session_id: &str, error: String) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = NULL,
                 error = ?3,
                 attention = ?4,
                 attention_summary = ?3,
                 updated_at = ?5
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::Failed),
                error,
                attention_to_str(AttentionLevel::Action),
                now,
            ],
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
                 attention = ?3,
                 attention_summary = ?4,
                 updated_at = ?5,
                 exited_at = NULL
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::Paused),
                attention_to_str(AttentionLevel::Notice),
                "paused",
                now,
            ],
        )?;
        Ok(())
    }

    pub fn mark_needs_input(
        &self,
        session_id: &str,
        exit_code: Option<i32>,
        summary: String,
    ) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = NULL,
                 exit_code = ?3,
                 error = NULL,
                 attention = ?4,
                 attention_summary = ?5,
                 updated_at = ?6,
                 exited_at = ?6
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::NeedsInput),
                exit_code,
                attention_to_str(AttentionLevel::Action),
                summary,
                now,
            ],
        )?;
        Ok(())
    }

    pub fn mark_exited(&self, session_id: &str, exit_code: Option<i32>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = NULL,
                 exit_code = ?3,
                 integration_state = ?4,
                 attention = ?5,
                 attention_summary = ?6,
                 updated_at = ?7,
                 exited_at = ?7
             WHERE session_id = ?1",
            params![
                session_id,
                status_to_str(SessionStatus::Exited),
                exit_code,
                integration_state_to_str(IntegrationState::Clean),
                attention_to_str(AttentionLevel::Notice),
                match exit_code {
                    Some(code) => format!("finished (exit {code})"),
                    None => "finished".to_string(),
                },
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
             SET status = ?2,
                 pid = NULL,
                 attention = ?3,
                 attention_summary = ?4,
                 updated_at = ?5
             WHERE session_id = ?1",
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

    pub fn set_integration_state(
        &self,
        session_id: &str,
        integration_state: IntegrationState,
        attention: AttentionLevel,
        attention_summary: &str,
    ) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET integration_state = ?2,
                 attention = ?3,
                 attention_summary = ?4,
                 updated_at = ?5
             WHERE session_id = ?1",
            params![
                session_id,
                integration_state_to_str(integration_state),
                attention_to_str(attention),
                attention_summary,
                now
            ],
        )?;
        Ok(())
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, thread_id, agent, model, workspace, repo_path, task, base_branch, branch,
                    worktree, status, integration_state, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
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
            "SELECT session_id, thread_id, agent, model, workspace, repo_path, task, base_branch, branch,
                    worktree, status, integration_state, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
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

    pub fn set_resume_session_id(&self, session_id: &str, resume_session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET resume_session_id = ?2, updated_at = ?3
             WHERE session_id = ?1",
            params![session_id, resume_session_id, now],
        )?;
        Ok(())
    }

    pub fn get_resume_session_id(&self, session_id: &str) -> Result<Option<String>> {
        let conn = self.connect()?;
        let resume_session_id = conn
            .query_row(
                "SELECT resume_session_id FROM sessions WHERE session_id = ?1",
                params![session_id],
                |row| row.get::<_, Option<String>>(0),
            )
            .optional()?;
        Ok(resume_session_id.flatten())
    }

    pub fn get_thread(&self, thread_id: &str) -> Result<Option<ThreadRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT thread_id, session_id, agent, title, initial_prompt, upstream_thread_id,
                    created_at, updated_at
             FROM threads WHERE thread_id = ?1",
            params![thread_id],
            row_to_thread,
        )
        .optional()
        .map_err(Into::into)
    }

    pub fn set_thread_upstream_id(&self, thread_id: &str, upstream_thread_id: &str) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE threads
             SET upstream_thread_id = ?2, updated_at = ?3
             WHERE thread_id = ?1",
            params![thread_id, upstream_thread_id, now],
        )?;
        Ok(())
    }

    pub fn delete_session(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        conn.execute(
            "DELETE FROM events WHERE session_id = ?1",
            params![session_id],
        )?;
        conn.execute(
            "DELETE FROM threads WHERE session_id = ?1",
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
        integration_state: str_to_integration_state(&row.get::<_, String>(11)?).map_err(
            |err| {
                rusqlite::Error::FromSqlConversionFailure(
                    11,
                    rusqlite::types::Type::Text,
                    Box::new(err),
                )
            },
        )?,
        pid: row.get::<_, Option<u32>>(12)?,
        exit_code: row.get(13)?,
        error: row.get(14)?,
        attention: str_to_attention(&row.get::<_, String>(15)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                15,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(16)?,
        created_at: parse_time(row.get::<_, String>(17)?)?,
        updated_at: parse_time(row.get::<_, String>(18)?)?,
        exited_at: row
            .get::<_, Option<String>>(19)?
            .map(parse_time)
            .transpose()?,
    })
}

fn row_to_thread(row: &rusqlite::Row<'_>) -> rusqlite::Result<ThreadRecord> {
    Ok(ThreadRecord {
        thread_id: row.get(0)?,
        session_id: row.get(1)?,
        agent: row.get(2)?,
        title: row.get(3)?,
        initial_prompt: row.get(4)?,
        upstream_thread_id: row.get(5)?,
        created_at: parse_time(row.get::<_, String>(6)?)?,
        updated_at: parse_time(row.get::<_, String>(7)?)?,
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

fn attention_to_str(attention: AttentionLevel) -> &'static str {
    match attention {
        AttentionLevel::Info => "info",
        AttentionLevel::Notice => "notice",
        AttentionLevel::Action => "action",
    }
}

fn integration_state_to_str(state: IntegrationState) -> &'static str {
    state.as_str()
}

fn str_to_integration_state(value: &str) -> std::result::Result<IntegrationState, std::io::Error> {
    match value {
        "clean" => Ok(IntegrationState::Clean),
        "pending_review" => Ok(IntegrationState::PendingReview),
        "applied" => Ok(IntegrationState::Applied),
        "discarded" => Ok(IntegrationState::Discarded),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown integration state `{value}`"),
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
            thread_id: None,
            agent: "codex",
            model: None,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
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
            thread_id: None,
            agent: "codex",
            model: None,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
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

    #[test]
    fn resume_session_id_round_trips() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            thread_id: None,
            agent: "codex",
            model: None,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            task: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
        })
        .unwrap();

        assert!(db.get_resume_session_id("demo").unwrap().is_none());
        db.set_resume_session_id("demo", "thread-123").unwrap();
        assert_eq!(
            db.get_resume_session_id("demo").unwrap().as_deref(),
            Some("thread-123")
        );
    }

    #[test]
    fn mark_needs_input_persists_status_and_summary() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            thread_id: None,
            agent: "codex",
            model: None,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            task: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
        })
        .unwrap();

        db.mark_needs_input("demo", Some(0), "Can you clarify?".to_string())
            .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.status, agentd_shared::session::SessionStatus::NeedsInput);
        assert_eq!(
            session.attention,
            agentd_shared::session::AttentionLevel::Action
        );
        assert_eq!(session.attention_summary.as_deref(), Some("Can you clarify?"));
        assert_eq!(session.exit_code, Some(0));
    }

    #[test]
    fn thread_rows_round_trip_and_link_to_sessions() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            thread_id: Some("thread-demo"),
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            task: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
        })
        .unwrap();
        db.insert_thread(&super::NewThread {
            thread_id: "thread-demo",
            session_id: "demo",
            agent: "codex",
            title: "test",
            initial_prompt: "test",
        })
        .unwrap();

        db.set_thread_upstream_id("thread-demo", "codex-upstream")
            .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.thread_id.as_deref(), Some("thread-demo"));
        assert_eq!(session.model.as_deref(), Some("gpt-5.3-codex"));

        let thread = db.get_thread("thread-demo").unwrap().unwrap();
        assert_eq!(thread.thread_id, "thread-demo");
        assert_eq!(thread.upstream_thread_id.as_deref(), Some("codex-upstream"));
    }
}
