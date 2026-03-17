use std::fs;

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};
use nix::unistd::getuid;

pub const APP_DIR_NAME: &str = "agentd";

#[derive(Debug, Clone)]
pub struct AppPaths {
    pub root: Utf8PathBuf,
    pub socket: Utf8PathBuf,
    pub pid_file: Utf8PathBuf,
    pub database: Utf8PathBuf,
    pub config: Utf8PathBuf,
    pub logs_dir: Utf8PathBuf,
    pub worktrees_dir: Utf8PathBuf,
}

impl AppPaths {
    pub fn discover() -> Result<Self> {
        let root = discover_root(
            std::env::var_os("AGENTD_DIR"),
            std::env::var_os("XDG_RUNTIME_DIR"),
            std::env::var_os("TMPDIR"),
            getuid().as_raw(),
        )?;
        Ok(Self::from_root(root))
    }

    pub fn ensure_layout(&self) -> Result<()> {
        for path in [&self.root, &self.logs_dir, &self.worktrees_dir] {
            fs::create_dir_all(path.as_std_path())
                .with_context(|| format!("failed to create {}", path))?;
        }
        Ok(())
    }

    pub fn log_path(&self, session_id: &str) -> Utf8PathBuf {
        self.logs_dir.join(format!("{session_id}.log"))
    }

    pub fn rendered_log_path(&self, session_id: &str) -> Utf8PathBuf {
        self.logs_dir.join(format!("{session_id}.rendered.log"))
    }

    pub fn worktree_path(&self, session_id: &str) -> Utf8PathBuf {
        self.worktrees_dir.join(session_id)
    }

    pub fn as_utf8(path: &Utf8Path) -> &str {
        path.as_str()
    }

    fn from_root(root: Utf8PathBuf) -> Self {
        Self {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            worktrees_dir: root.join("worktrees"),
            root,
        }
    }
}

fn discover_root(
    agentd_dir: Option<std::ffi::OsString>,
    xdg_runtime_dir: Option<std::ffi::OsString>,
    tmpdir: Option<std::ffi::OsString>,
    uid: u32,
) -> Result<Utf8PathBuf> {
    if let Some(root) = utf8_env_path("AGENTD_DIR", agentd_dir)? {
        return Ok(root);
    }

    if let Some(runtime_dir) = utf8_env_path("XDG_RUNTIME_DIR", xdg_runtime_dir)? {
        return Ok(runtime_dir.join(APP_DIR_NAME));
    }

    if let Some(tmpdir) = utf8_env_path("TMPDIR", tmpdir)? {
        return Ok(tmpdir.join(format!("{APP_DIR_NAME}-{uid}")));
    }

    Ok(Utf8PathBuf::from(format!("/tmp/{APP_DIR_NAME}-{uid}")))
}

fn utf8_env_path(name: &str, value: Option<std::ffi::OsString>) -> Result<Option<Utf8PathBuf>> {
    let Some(value) = value else {
        return Ok(None);
    };

    let path = std::path::PathBuf::from(value);
    Utf8PathBuf::from_path_buf(path)
        .map(Some)
        .map_err(|_| anyhow::anyhow!("{name} is not valid UTF-8"))
}

#[cfg(test)]
mod tests {
    use super::{APP_DIR_NAME, AppPaths, discover_root};
    use camino::Utf8PathBuf;

    #[test]
    fn agentd_dir_is_used_as_exact_root() {
        let root = discover_root(
            Some("/custom/agentd-root".into()),
            Some("/run/user/501".into()),
            Some("/var/tmp".into()),
            501,
        )
        .unwrap();
        assert_eq!(root, Utf8PathBuf::from("/custom/agentd-root"));
    }

    #[test]
    fn xdg_runtime_dir_is_used_when_agentd_dir_is_unset() {
        let root = discover_root(
            None,
            Some("/run/user/501".into()),
            Some("/var/tmp".into()),
            501,
        )
        .unwrap();
        assert_eq!(root, Utf8PathBuf::from("/run/user/501").join(APP_DIR_NAME));
    }

    #[test]
    fn tmpdir_uses_uid_suffix_when_higher_priority_env_vars_are_unset() {
        let root = discover_root(None, None, Some("/var/tmp".into()), 501).unwrap();
        assert_eq!(
            root,
            Utf8PathBuf::from("/var/tmp").join(format!("{APP_DIR_NAME}-501"))
        );
    }

    #[test]
    fn tmp_fallback_uses_uid_suffix() {
        let root = discover_root(None, None, None, 501).unwrap();
        assert_eq!(root, Utf8PathBuf::from(format!("/tmp/{APP_DIR_NAME}-501")));
    }

    #[test]
    fn derived_paths_follow_selected_root() {
        let paths = AppPaths::from_root(Utf8PathBuf::from("/run/user/501").join(APP_DIR_NAME));
        assert_eq!(
            paths.socket,
            Utf8PathBuf::from("/run/user/501/agentd/agentd.sock")
        );
        assert_eq!(
            paths.pid_file,
            Utf8PathBuf::from("/run/user/501/agentd/agentd.pid")
        );
        assert_eq!(
            paths.database,
            Utf8PathBuf::from("/run/user/501/agentd/state.db")
        );
        assert_eq!(
            paths.config,
            Utf8PathBuf::from("/run/user/501/agentd/config.toml")
        );
        assert_eq!(
            paths.logs_dir,
            Utf8PathBuf::from("/run/user/501/agentd/logs")
        );
        assert_eq!(
            paths.worktrees_dir,
            Utf8PathBuf::from("/run/user/501/agentd/worktrees")
        );
    }
}
