use std::{collections::HashMap, fs};

use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};

use crate::paths::AppPaths;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    #[serde(default)]
    pub agents: HashMap<String, AgentConfig>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentConfig {
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default = "default_model_flag")]
    pub model_flag: Option<String>,
}

fn default_model_flag() -> Option<String> {
    Some("--model".to_string())
}

impl Config {
    pub fn load(paths: &AppPaths) -> Result<Self> {
        if !paths.config.exists() {
            return Ok(Self::default());
        }

        let contents = fs::read_to_string(paths.config.as_std_path())
            .with_context(|| format!("failed to read {}", paths.config))?;
        toml::from_str(&contents).with_context(|| format!("failed to parse {}", paths.config))
    }

    pub fn write_default(paths: &AppPaths) -> Result<()> {
        let contents = toml::to_string_pretty(&Self::default())
            .context("failed to serialize default config")?;
        fs::write(paths.config.as_std_path(), contents)
            .with_context(|| format!("failed to write {}", paths.config))?;
        Ok(())
    }

    pub fn require_agent<'a>(&'a self, paths: &AppPaths, name: &str) -> Result<&'a AgentConfig> {
        match self.agents.get(name) {
            Some(agent) => Ok(agent),
            None => bail!("agent `{name}` is not configured in {}", paths.config),
        }
    }
}

impl Default for Config {
    fn default() -> Self {
        let mut agents = HashMap::new();
        agents.insert(
            "claude".to_string(),
            AgentConfig {
                command: "claude".to_string(),
                args: Vec::new(),
                model_flag: default_model_flag(),
            },
        );
        agents.insert(
            "codex".to_string(),
            AgentConfig {
                command: "codex".to_string(),
                args: Vec::new(),
                model_flag: default_model_flag(),
            },
        );
        Self { agents }
    }
}

#[cfg(test)]
mod tests {
    use super::Config;
    use crate::paths::AppPaths;
    use camino::Utf8PathBuf;

    fn test_paths() -> AppPaths {
        let root = Utf8PathBuf::from("/tmp/agentd-config-test");
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
    fn require_agent_error_mentions_resolved_config_path() {
        let paths = test_paths();
        let err = Config::default()
            .require_agent(&paths, "missing")
            .unwrap_err()
            .to_string();
        assert!(err.contains(paths.config.as_str()));
    }
}
