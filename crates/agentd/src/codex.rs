use std::{
    fs,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result};
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};

pub fn discover_resume_session_id(worktree: &str) -> Result<Option<String>> {
    let Some(database_path) = discover_state_database_path()? else {
        return Ok(None);
    };
    discover_resume_session_id_from_database(&database_path, worktree)
}

fn discover_resume_session_id_from_database(
    database_path: &Path,
    worktree: &str,
) -> Result<Option<String>> {
    let connection = Connection::open_with_flags(
        database_path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .with_context(|| format!("failed to open {}", database_path.display()))?;

    connection
        .query_row(
            "SELECT id
             FROM threads
             WHERE cwd = ?1 AND archived = 0
             ORDER BY updated_at DESC, created_at DESC
             LIMIT 1",
            params![worktree],
            |row| row.get(0),
        )
        .optional()
        .map_err(Into::into)
}

fn discover_state_database_path() -> Result<Option<PathBuf>> {
    let Some(home) = std::env::var_os("HOME") else {
        return Ok(None);
    };
    discover_state_database_path_from_home(Path::new(&home))
}

fn discover_state_database_path_from_home(home: &Path) -> Result<Option<PathBuf>> {
    let codex_root = home.join(".codex");
    let entries = match fs::read_dir(&codex_root) {
        Ok(entries) => entries,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => {
            return Err(err).with_context(|| format!("failed to read {}", codex_root.display()));
        }
    };

    let mut best_match = None;
    for entry in entries {
        let entry = entry?;
        let file_name = entry.file_name();
        let Some(file_name) = file_name.to_str() else {
            continue;
        };
        let Some(version) = parse_state_database_version(file_name) else {
            continue;
        };
        match &best_match {
            Some((best_version, _)) if *best_version >= version => {}
            _ => best_match = Some((version, entry.path())),
        }
    }

    Ok(best_match.map(|(_, path)| path))
}

fn parse_state_database_version(file_name: &str) -> Option<u64> {
    let version = file_name.strip_prefix("state_")?.strip_suffix(".sqlite")?;
    version.parse().ok()
}

#[cfg(test)]
mod tests {
    use super::{
        discover_resume_session_id_from_database, discover_state_database_path_from_home,
        parse_state_database_version,
    };
    use rusqlite::{Connection, params};
    use std::{
        fs,
        path::PathBuf,
        time::{SystemTime, UNIX_EPOCH},
    };

    fn temp_root(label: &str) -> PathBuf {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
        PathBuf::from(format!("/tmp/agentd-codex-{label}-{suffix}"))
    }

    #[test]
    fn parse_state_database_version_accepts_expected_names() {
        assert_eq!(parse_state_database_version("state_5.sqlite"), Some(5));
        assert_eq!(parse_state_database_version("state_12.sqlite"), Some(12));
        assert_eq!(parse_state_database_version("state.sqlite"), None);
    }

    #[test]
    fn discover_state_database_prefers_highest_version() {
        let root = temp_root("version");
        let codex_root = root.join(".codex");
        fs::create_dir_all(&codex_root).unwrap();
        fs::write(codex_root.join("state_1.sqlite"), "").unwrap();
        fs::write(codex_root.join("state_5.sqlite"), "").unwrap();
        fs::write(codex_root.join("state_3.sqlite"), "").unwrap();

        let path = discover_state_database_path_from_home(&root).unwrap().unwrap();
        assert_eq!(path.file_name().and_then(|value| value.to_str()), Some("state_5.sqlite"));
    }

    #[test]
    fn discover_resume_session_id_returns_latest_thread_for_cwd() {
        let root = temp_root("threads");
        let codex_root = root.join(".codex");
        fs::create_dir_all(&codex_root).unwrap();
        let database_path = codex_root.join("state_5.sqlite");
        let connection = Connection::open(&database_path).unwrap();
        connection
            .execute_batch(
                "CREATE TABLE threads (
                    id TEXT PRIMARY KEY,
                    rollout_path TEXT NOT NULL,
                    created_at INTEGER NOT NULL,
                    updated_at INTEGER NOT NULL,
                    source TEXT NOT NULL,
                    model_provider TEXT NOT NULL,
                    cwd TEXT NOT NULL,
                    title TEXT NOT NULL,
                    sandbox_policy TEXT NOT NULL,
                    approval_mode TEXT NOT NULL,
                    tokens_used INTEGER NOT NULL DEFAULT 0,
                    has_user_event INTEGER NOT NULL DEFAULT 0,
                    archived INTEGER NOT NULL DEFAULT 0,
                    archived_at INTEGER,
                    git_sha TEXT,
                    git_branch TEXT,
                    git_origin_url TEXT,
                    cli_version TEXT NOT NULL DEFAULT '',
                    first_user_message TEXT NOT NULL DEFAULT '',
                    agent_nickname TEXT,
                    agent_role TEXT,
                    memory_mode TEXT NOT NULL DEFAULT 'enabled'
                );",
            )
            .unwrap();
        connection
            .execute(
                "INSERT INTO threads (
                    id, rollout_path, created_at, updated_at, source, model_provider, cwd, title,
                    sandbox_policy, approval_mode, archived
                 ) VALUES (?1, '', 10, 20, 'interactive', 'openai', ?2, 'old', 'workspace-write', 'on-request', 0)",
                params!["older-thread", "/tmp/worktree"],
            )
            .unwrap();
        connection
            .execute(
                "INSERT INTO threads (
                    id, rollout_path, created_at, updated_at, source, model_provider, cwd, title,
                    sandbox_policy, approval_mode, archived
                 ) VALUES (?1, '', 11, 30, 'interactive', 'openai', ?2, 'new', 'workspace-write', 'on-request', 0)",
                params!["newer-thread", "/tmp/worktree"],
            )
            .unwrap();
        connection
            .execute(
                "INSERT INTO threads (
                    id, rollout_path, created_at, updated_at, source, model_provider, cwd, title,
                    sandbox_policy, approval_mode, archived
                 ) VALUES (?1, '', 12, 40, 'interactive', 'openai', ?2, 'archived', 'workspace-write', 'on-request', 1)",
                params!["archived-thread", "/tmp/worktree"],
            )
            .unwrap();
        drop(connection);

        let discovered =
            discover_resume_session_id_from_database(&database_path, "/tmp/worktree").unwrap();

        assert_eq!(discovered.as_deref(), Some("newer-thread"));
    }
}
