use std::fs;

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};

pub const APP_DIR_NAME: &str = ".agentd";

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
        let home = dirs::home_dir().context("could not determine home directory")?;
        let home = Utf8PathBuf::from_path_buf(home)
            .map_err(|_| anyhow::anyhow!("home directory is not valid UTF-8"))?;
        let root = home.join(APP_DIR_NAME);
        Ok(Self {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            worktrees_dir: root.join("worktrees"),
            root,
        })
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

    pub fn worktree_path(&self, session_id: &str) -> Utf8PathBuf {
        self.worktrees_dir.join(session_id)
    }

    pub fn as_utf8(path: &Utf8Path) -> &str {
        path.as_str()
    }
}
