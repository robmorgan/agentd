use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};

use agentd_shared::{
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
    pub workspace: &'a str,
    pub task: &'a str,
    pub branch: &'a str,
    pub worktree: &'a str,
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
            );",
        )?;
        Ok(())
    }

    pub fn insert_session(&self, new_session: &NewSession<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, workspace, task, branch, worktree, status,
                created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8)",
            params![
                new_session.session_id,
                new_session.agent,
                new_session.workspace,
                new_session.task,
                new_session.branch,
                new_session.worktree,
                status_to_str(SessionStatus::Creating),
                now,
            ],
        )?;
        Ok(())
    }

    pub fn mark_running(&self, session_id: &str, pid: u32) -> Result<()> {
        self.update_state(session_id, SessionStatus::Running, Some(pid), None, None)
    }

    pub fn mark_failed(&self, session_id: &str, error: String) -> Result<()> {
        self.update_state(session_id, SessionStatus::Failed, None, None, Some(error))
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
        self.update_state(session_id, SessionStatus::UnknownRecovered, None, None, None)
    }

    fn update_state(
        &self,
        session_id: &str,
        status: SessionStatus,
        pid: Option<u32>,
        exit_code: Option<i32>,
        error: Option<String>,
    ) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET status = ?2,
                 pid = COALESCE(?3, pid),
                 exit_code = COALESCE(?4, exit_code),
                 error = COALESCE(?5, error),
                 updated_at = ?6
             WHERE session_id = ?1",
            params![session_id, status_to_str(status), pid, exit_code, error, now],
        )?;
        Ok(())
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, agent, workspace, task, branch, worktree, status, pid, exit_code,
                    error, created_at, updated_at, exited_at
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
            "SELECT session_id, agent, workspace, task, branch, worktree, status, pid, exit_code,
                    error, created_at, updated_at, exited_at
             FROM sessions ORDER BY created_at DESC",
        )?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>().map_err(Into::into)
    }
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        agent: row.get(1)?,
        workspace: row.get(2)?,
        task: row.get(3)?,
        branch: row.get(4)?,
        worktree: row.get(5)?,
        status: str_to_status(&row.get::<_, String>(6)?)
            .map_err(|err| rusqlite::Error::FromSqlConversionFailure(6, rusqlite::types::Type::Text, Box::new(err)))?,
        pid: row.get::<_, Option<u32>>(7)?,
        exit_code: row.get(8)?,
        error: row.get(9)?,
        created_at: parse_time(row.get::<_, String>(10)?)?,
        updated_at: parse_time(row.get::<_, String>(11)?)?,
        exited_at: row.get::<_, Option<String>>(12)?.map(parse_time).transpose()?,
    })
}

fn parse_time(value: String) -> rusqlite::Result<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(&value)
        .map(|dt| dt.with_timezone(&Utc))
        .map_err(|err| rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(err)))
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
        "exited" => Ok(SessionStatus::Exited),
        "failed" => Ok(SessionStatus::Failed),
        "unknown_recovered" => Ok(SessionStatus::UnknownRecovered),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session status `{value}`"),
        )),
    }
}
