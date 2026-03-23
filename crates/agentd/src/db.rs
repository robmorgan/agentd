use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};

use agentd_shared::{
    paths::AppPaths,
    session::{
        AttentionLevel, GitSyncStatus, IntegrationPolicy, IntegrationState, SessionMode,
        SessionRecord, SessionStatus, repo_name_from_path,
    },
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
    pub agent_command: &'a str,
    pub agent_args_json: &'a str,
    pub resume_session_id: Option<&'a str>,
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
        let conn = self.connect()?;
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS sessions (
                session_id TEXT PRIMARY KEY,
                thread_id TEXT,
                agent TEXT NOT NULL,
                model TEXT,
                mode TEXT NOT NULL DEFAULT 'execute',
                agent_command TEXT,
                agent_args_json TEXT,
                resume_session_id TEXT,
                workspace TEXT NOT NULL,
                repo_path TEXT,
                repo_name TEXT,
                title TEXT,
                task TEXT NOT NULL,
                base_branch TEXT,
                branch TEXT NOT NULL,
                worktree TEXT NOT NULL,
                status TEXT NOT NULL,
                integration_policy TEXT NOT NULL DEFAULT 'manual_review',
                pid INTEGER,
                exit_code INTEGER,
                error TEXT,
                integration_state TEXT NOT NULL DEFAULT 'clean',
                git_sync TEXT NOT NULL DEFAULT 'unknown',
                git_status_summary TEXT,
                has_conflicts INTEGER NOT NULL DEFAULT 0,
                attention TEXT NOT NULL DEFAULT 'info',
                attention_summary TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                exited_at TEXT
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
            CREATE INDEX IF NOT EXISTS idx_threads_session_id
                ON threads (session_id);
            ",
        )?;
        conn.execute("DROP TABLE IF EXISTS events", [])?;
        conn.execute("DROP TABLE IF EXISTS plans", [])?;
        self.ensure_column(&conn, "thread_id", "ALTER TABLE sessions ADD COLUMN thread_id TEXT")?;
        self.ensure_column(&conn, "model", "ALTER TABLE sessions ADD COLUMN model TEXT")?;
        self.ensure_column(
            &conn,
            "mode",
            "ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'execute'",
        )?;
        self.ensure_column(&conn, "repo_path", "ALTER TABLE sessions ADD COLUMN repo_path TEXT")?;
        self.ensure_column(&conn, "repo_name", "ALTER TABLE sessions ADD COLUMN repo_name TEXT")?;
        self.ensure_column(&conn, "title", "ALTER TABLE sessions ADD COLUMN title TEXT")?;
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
            "integration_policy",
            "ALTER TABLE sessions ADD COLUMN integration_policy TEXT NOT NULL DEFAULT 'manual_review'",
        )?;
        self.ensure_column(
            &conn,
            "integration_state",
            "ALTER TABLE sessions ADD COLUMN integration_state TEXT NOT NULL DEFAULT 'clean'",
        )?;
        self.ensure_column(
            &conn,
            "git_sync",
            "ALTER TABLE sessions ADD COLUMN git_sync TEXT NOT NULL DEFAULT 'unknown'",
        )?;
        self.ensure_column(
            &conn,
            "git_status_summary",
            "ALTER TABLE sessions ADD COLUMN git_status_summary TEXT",
        )?;
        self.ensure_column(
            &conn,
            "has_conflicts",
            "ALTER TABLE sessions ADD COLUMN has_conflicts INTEGER NOT NULL DEFAULT 0",
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
             SET mode = COALESCE(mode, 'execute'),
                 repo_path = COALESCE(repo_path, workspace),
                 repo_name = COALESCE(repo_name, repo_path, workspace),
                 title = COALESCE(title, task, repo_name, workspace),
                 base_branch = COALESCE(base_branch, 'HEAD'),
                 integration_policy = COALESCE(integration_policy, 'manual_review'),
                 integration_state = COALESCE(integration_state, 'clean'),
                 git_sync = COALESCE(git_sync, 'unknown'),
                 has_conflicts = COALESCE(has_conflicts, 0),
                 attention = COALESCE(attention, 'info'),
                 attention_summary = COALESCE(attention_summary, title, task)
             WHERE mode IS NULL OR repo_path IS NULL OR repo_name IS NULL OR title IS NULL OR base_branch IS NULL OR integration_policy IS NULL OR integration_state IS NULL OR git_sync IS NULL OR has_conflicts IS NULL OR attention IS NULL OR attention_summary IS NULL",
            [],
        )?;
        self.backfill_repo_names(&conn)?;
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

    fn backfill_repo_names(&self, conn: &Connection) -> Result<()> {
        let mut stmt = conn.prepare(
            "SELECT session_id, repo_path, workspace, repo_name
             FROM sessions",
        )?;
        let rows = stmt.query_map([], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, Option<String>>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, Option<String>>(3)?,
            ))
        })?;

        for row in rows {
            let (session_id, repo_path, workspace, repo_name) = row?;
            let current = repo_name.unwrap_or_default();
            let desired = repo_name_from_path(repo_path.as_deref().unwrap_or(&workspace));
            if current != desired {
                conn.execute(
                    "UPDATE sessions SET repo_name = ?2 WHERE session_id = ?1",
                    params![session_id, desired],
                )?;
            }
        }

        Ok(())
    }

    pub fn insert_session(&self, new_session: &NewSession<'_>) -> Result<()> {
        let conn = self.connect()?;
        let now = Utc::now().to_rfc3339();
        conn.execute(
            "INSERT INTO sessions (
                session_id, thread_id, agent, model, mode, agent_command, agent_args_json, resume_session_id, workspace,
                repo_path, repo_name, title, task, base_branch, branch, worktree, status, integration_policy, attention, attention_summary,
                integration_state, git_sync, git_status_summary, has_conflicts, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21, ?22, ?23, ?24, ?24)",
            params![
                new_session.session_id,
                new_session.thread_id,
                new_session.agent,
                new_session.model,
                session_mode_to_str(new_session.mode),
                new_session.agent_command,
                new_session.agent_args_json,
                new_session.resume_session_id,
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
        conn.execute("DELETE FROM threads WHERE session_id = ?1", params![session_id])?;
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
        integration_policy: str_to_integration_policy(&row.get::<_, String>(13)?).map_err(|err| {
            rusqlite::Error::FromSqlConversionFailure(
                13,
                rusqlite::types::Type::Text,
                Box::new(err),
            )
        })?,
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
        session::{IntegrationPolicy, SessionMode, SessionStatus},
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
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
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
    fn legacy_paused_rows_decode_as_unknown_recovered() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        let db = Database::open(&paths).unwrap();
        db.insert_session(&super::NewSession {
            session_id: "legacy",
            thread_id: None,
            agent: "codex",
            model: None,
            mode: SessionMode::Execute,
            agent_command: "codex",
            agent_args_json: "[]",
            resume_session_id: None,
            workspace: "/tmp/repo",
            repo_path: "/tmp/repo",
            repo_name: "repo",
            title: "legacy",
            base_branch: "main",
            branch: "agent/legacy",
            worktree: "/tmp/worktree",
            integration_policy: IntegrationPolicy::AutoApplySafe,
        })
        .unwrap();
        let conn = rusqlite::Connection::open(paths.database.as_str()).unwrap();
        conn.execute(
            "UPDATE sessions SET status = ?2, attention = ?3, attention_summary = ?4 WHERE session_id = ?1",
            params!["legacy", "paused", "notice", "paused"],
        )
        .unwrap();

        let session = db.get_session("legacy").unwrap().unwrap();
        assert_eq!(session.status, SessionStatus::UnknownRecovered);
    }
}
