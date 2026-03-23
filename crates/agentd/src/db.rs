use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};

use agentd_shared::{
    paths::AppPaths,
    session::{
        AttentionLevel, GitSyncStatus, IntegrationPolicy, IntegrationState, SessionMode,
        SessionRecord, SessionStatus,
    },
    sqlite_schema::init_state_db,
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
    pub mode: SessionMode,
    pub workspace: &'a str,
    pub repo_path: &'a str,
    pub repo_name: &'a str,
    pub title: &'a str,
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
    }

    pub fn insert_session(&self, new_session: &NewSession<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO sessions (
                session_id, thread_id, agent, model, mode, workspace,
                repo_path, repo_name, title, base_branch, branch, worktree, status, integration_policy, attention, attention_summary,
                integration_state, git_sync, git_status_summary, has_conflicts, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21, ?21)",
            params![
                new_session.session_id,
                new_session.thread_id,
                new_session.agent,
                new_session.model,
                session_mode_to_str(new_session.mode),
                new_session.workspace,
                new_session.repo_path,
                new_session.repo_name,
                new_session.title,
                new_session.base_branch,
                new_session.branch,
                new_session.worktree,
                status_to_str(SessionStatus::Creating),
                integration_policy_to_str(new_session.integration_policy),
                attention_to_str(AttentionLevel::Info),
                new_session.title,
                integration_state_to_str(IntegrationState::Clean),
                git_sync_status_to_str(GitSyncStatus::Unknown),
                Option::<String>::None,
                false,
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

    pub fn set_git_review_state(
        &self,
        session_id: &str,
        git_sync: GitSyncStatus,
        git_status_summary: Option<&str>,
        has_conflicts: bool,
    ) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "UPDATE sessions
             SET git_sync = ?2,
                 git_status_summary = ?3,
                 has_conflicts = ?4,
                 updated_at = ?5
             WHERE session_id = ?1",
            params![
                session_id,
                git_sync_status_to_str(git_sync),
                git_status_summary,
                has_conflicts,
                now
            ],
        )?;
        Ok(())
    }

    pub fn get_session(&self, session_id: &str) -> Result<Option<SessionRecord>> {
        let conn = self.connect()?;
        conn.query_row(
            "SELECT session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name, title, base_branch, branch,
                    worktree, status, integration_policy, integration_state, git_sync, git_status_summary, has_conflicts, pid, exit_code, error, attention, attention_summary,
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
            "SELECT session_id, thread_id, agent, model, mode, workspace, repo_path, repo_name, title, base_branch, branch,
                    worktree, status, integration_policy, integration_state, git_sync, git_status_summary, has_conflicts, pid, exit_code, error, attention, attention_summary,
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
        title: row.get(8)?,
        base_branch: row.get(9)?,
        branch: row.get(10)?,
        worktree: row.get(11)?,
        status: str_to_status(&row.get::<_, String>(12)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                12,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        integration_policy: str_to_integration_policy(&row.get::<_, String>(13)?).map_err(
            |err| {
                rusqlite::Error::FromSqlConversionFailure(
                    13,
                    rusqlite::types::Type::Text,
                    Box::new(err),
                )
            },
        )?,
        integration_state: str_to_integration_state(&row.get::<_, String>(14)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                14,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        git_sync: str_to_git_sync_status(&row.get::<_, String>(15)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                15,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        git_status_summary: row.get(16)?,
        has_conflicts: row.get::<_, bool>(17)?,
        pid: row.get::<_, Option<u32>>(18)?,
        exit_code: row.get(19)?,
        error: row.get(20)?,
        attention: str_to_attention(&row.get::<_, String>(21)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                21,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
        attention_summary: row.get(22)?,
        created_at: parse_time(row.get::<_, String>(23)?)?,
        updated_at: parse_time(row.get::<_, String>(24)?)?,
        exited_at: row.get::<_, Option<String>>(25)?.map(parse_time).transpose()?,
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
        "paused" => Ok(SessionStatus::UnknownRecovered),
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

fn git_sync_status_to_str(status: GitSyncStatus) -> &'static str {
    status.as_str()
}

fn integration_policy_to_str(policy: IntegrationPolicy) -> &'static str {
    policy.as_str()
}

fn integration_state_to_str(state: IntegrationState) -> &'static str {
    state.as_str()
}

fn session_mode_to_str(mode: SessionMode) -> &'static str {
    mode.as_str()
}

fn str_to_integration_state(value: &str) -> std::result::Result<IntegrationState, std::io::Error> {
    match value {
        "clean" => Ok(IntegrationState::Clean),
        "auto_applying" => Ok(IntegrationState::AutoApplying),
        "pending_review" => Ok(IntegrationState::PendingReview),
        "applied" => Ok(IntegrationState::Applied),
        "discarded" => Ok(IntegrationState::Discarded),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown integration state `{value}`"),
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

fn str_to_git_sync_status(value: &str) -> std::result::Result<GitSyncStatus, std::io::Error> {
    match value {
        "unknown" => Ok(GitSyncStatus::Unknown),
        "in_sync" => Ok(GitSyncStatus::InSync),
        "needs_sync" => Ok(GitSyncStatus::NeedsSync),
        "conflicted" => Ok(GitSyncStatus::Conflicted),
        _ => Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("unknown git sync status `{value}`"),
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
            title: "test",
            base_branch: "main",
            branch: "agent/test",
            worktree: "/tmp/worktree",
            integration_policy: IntegrationPolicy::AutoApplySafe,
        })
        .unwrap();

        let session = db.get_session("demo").unwrap().unwrap();
        assert_eq!(session.title, "test");
        assert_eq!(session.repo_name, "repo");
        assert_eq!(session.branch, "agent/test");
    }

    #[test]
    fn open_resets_legacy_schema() {
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

        let db = Database::open(&paths).unwrap();
        assert!(db.get_session("legacy").unwrap().is_none());
    }
}
