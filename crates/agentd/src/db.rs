use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};

use agentd_shared::{
    paths::AppPaths,
    session::{
        ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord, SessionStatus,
    },
    sqlite_schema::init_state_db,
};

#[derive(Clone, Debug)]
pub struct Database {
    path: String,
}

pub struct NewSession<'a> {
    pub session_id: &'a str,
    pub thread_id: Option<&'a str>,
    pub agent: &'a str,
    pub model: Option<&'a str>,
    pub mode: SessionMode,
    pub workspace: &'a str,
    pub repo_path: &'a str,
    pub repo_name: &'a str,
    pub base_branch: &'a str,
    pub branch: &'a str,
    pub worktree: &'a str,
    pub integration_policy: IntegrationPolicy,
}

impl Database {
    pub fn open(paths: &AppPaths) -> Result<Self> {
        let db = Self { path: paths.database.to_string() };
        db.init()?;
        Ok(db)
    }

    fn connect(&self) -> Result<Connection> {
        Connection::open(&self.path).with_context(|| format!("failed to open {}", self.path))
    }

    fn init(&self) -> Result<()> {
        let mut conn = self.connect()?;
        init_state_db(&mut conn)
            .with_context(|| format!("unsupported state database schema in {}", self.path))?;
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS threads (
                thread_id TEXT PRIMARY KEY,
                session_id TEXT NOT NULL UNIQUE,
                agent TEXT NOT NULL,
                title TEXT NOT NULL,
                initial_prompt TEXT NOT NULL,
                upstream_thread_id TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_threads_session_id
                ON threads (session_id);",
        )?;
        Ok(())
    }

    pub fn insert_session(&self, new_session: &NewSession<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO sessions (
                session_id, thread_id, agent, model, mode, workspace,
                repo_path, repo_name, base_branch, branch, worktree, status, integration_policy, attention, attention_summary,
                integration_state, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?17)",
            params![
                new_session.session_id,
                new_session.thread_id,
                new_session.agent,
                new_session.model,
                session_mode_to_str(new_session.mode),
                new_session.workspace,
                new_session.repo_path,
                new_session.repo_name,
                new_session.base_branch,
                new_session.branch,
                new_session.worktree,
                status_to_str(SessionStatus::Creating),
                integration_policy_to_str(new_session.integration_policy),
                attention_to_str(AttentionLevel::Info),
                new_session.session_id,
                apply_state_to_str(ApplyState::Idle),
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
                apply_state_to_str(ApplyState::Idle),
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

    pub fn mark_exited(
        &self,
        session_id: &str,
        exit_code: Option<i32>,
        apply_state: ApplyState,
    ) -> Result<()> {
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
                apply_state_to_str(apply_state),
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

    pub fn set_apply_state(
        &self,
        session_id: &str,
        apply_state: ApplyState,
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
                apply_state_to_str(apply_state),
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
            "SELECT session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch,
                    worktree, status, integration_policy, integration_state, pid, exit_code, error, attention, attention_summary,
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
            "SELECT session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch,
                    worktree, status, integration_policy, integration_state, pid, exit_code, error, attention, attention_summary,
                    created_at, updated_at, exited_at
             FROM sessions ORDER BY created_at DESC",
        )?;
        let rows = stmt.query_map([], row_to_session)?;
        rows.collect::<rusqlite::Result<Vec<_>>>().map_err(Into::into)
    }

    pub fn delete_session(&self, session_id: &str) -> Result<()> {
        let conn = self.connect()?;
        conn.execute("DELETE FROM sessions WHERE session_id = ?1", params![session_id])?;
        Ok(())
    }
}

fn row_to_session(row: &rusqlite::Row<'_>) -> rusqlite::Result<SessionRecord> {
    Ok(SessionRecord {
        session_id: row.get(0)?,
        thread_id: row.get(1)?,
        agent: row.get(2)?,
        model: row.get(3)?,
        mode: str_to_session_mode(&row.get::<_, String>(4)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(4, rusqlite::types::Type::Text, Box::new(err))
        })?,
        workspace: row.get(5)?,
        repo_path: row.get(6)?,
        repo_name: row.get(7)?,
        base_branch: row.get(8)?,
        branch: row.get(9)?,
        worktree: row.get(10)?,
        status: str_to_status(&row.get::<_, String>(11)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                11,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        integration_policy: str_to_integration_policy(&row.get::<_, String>(12)?).map_err(
            |err| {
                rusqlite::Error::FromSqlConversionFailure(
                    12,
                    rusqlite::types::Type::Text,
                    Box::new(err),
                )
            },
        )?,
        apply_state: str_to_apply_state(&row.get::<_, String>(13)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                13,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        dirty_count: 0,
        ahead_count: 0,
        has_commits: false,
        has_pending_changes: false,
        pid: row.get::<_, Option<u32>>(14)?,
        exit_code: row.get(15)?,
        error: row.get(16)?,
        attention: str_to_attention(&row.get::<_, String>(17)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                17,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(18)?,
        created_at: parse_time(row.get::<_, String>(19)?)?,
        updated_at: parse_time(row.get::<_, String>(20)?)?,
        exited_at: row.get::<_, Option<String>>(21)?.map(parse_time).transpose()?,
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

fn attention_to_str(attention: AttentionLevel) -> &'static str {
    match attention {
        AttentionLevel::Info => "info",
        AttentionLevel::Notice => "notice",
        AttentionLevel::Action => "action",
    }
}

fn integration_policy_to_str(policy: IntegrationPolicy) -> &'static str {
    policy.as_str()
}

fn apply_state_to_str(state: ApplyState) -> &'static str {
    state.as_str()
}

fn session_mode_to_str(mode: SessionMode) -> &'static str {
    mode.as_str()
}

fn str_to_apply_state(value: &str) -> std::result::Result<ApplyState, std::io::Error> {
    match value {
        "idle" | "clean" | "pending_review" => Ok(ApplyState::Idle),
        "auto_applying" => Ok(ApplyState::AutoApplying),
        "applied" => Ok(ApplyState::Applied),
        "discarded" => Ok(ApplyState::Discarded),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown apply state `{value}`"),
        )),
    }
}

fn str_to_integration_policy(
    value: &str,
) -> std::result::Result<IntegrationPolicy, std::io::Error> {
    match value {
        "manual_review" => Ok(IntegrationPolicy::ManualReview),
        "auto_apply_safe" => Ok(IntegrationPolicy::AutoApplySafe),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown integration policy `{value}`"),
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

fn str_to_session_mode(value: &str) -> std::result::Result<SessionMode, std::io::Error> {
    match value {
        "execute" => Ok(SessionMode::Execute),
        "plan" => Ok(SessionMode::Plan),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown session mode `{value}`"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::Database;
    use agentd_shared::{
        paths::AppPaths,
        session::{IntegrationPolicy, SessionMode},
    };
    use rusqlite::params;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
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
    fn insert_session_round_trips_active_fields() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "demo",
            thread_id: None,
            agent: "codex",
            model: Some("gpt-5.3-codex"),
            mode: SessionMode::Execute,
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            repo_name: "repo",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
            integration_policy: IntegrationPolicy::AutoApplySafe,
        })
        .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.repo_name, "repo");
        assert_eq!(session.branch, "agent/test");
    }

    #[test]
    fn open_rejects_legacy_schema() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let conn = rusqlite::Connection::open(paths.database.as_str()).unwrap();
        conn.execute_batch(
            "CREATE TABLE sessions (
                session_id TEXT PRIMARY KEY,
                agent TEXT NOT NULL,
                workspace TEXT NOT NULL,
                task TEXT NOT NULL,
                branch TEXT NOT NULL,
                worktree TEXT NOT NULL,
                status TEXT NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );",
        )
        .unwrap();
        conn.execute(
            "INSERT INTO sessions (
                session_id, agent, workspace, task, branch, worktree, status, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8)",
            params![
                "legacy",
                "codex",
                "/tmp/repo",
                "legacy",
                "agent/legacy",
                "/tmp/worktree",
                "running",
                chrono::Utc::now().to_rfc3339(),
            ],
        )
        .unwrap();

        let err = Database::open(&paths).unwrap_err().to_string();
        assert!(err.contains("unsupported state database schema"));
    }
}
