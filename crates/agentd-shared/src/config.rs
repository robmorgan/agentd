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
}

impl Config {
    pub fn load(paths: &AppPaths) -> Result<Self> {
        if !paths.config.exists() {
            return Ok(Self::default());
        }

        let contents = fs::read_to_string(paths.config.as_std_path())
            .with_context(|| format!("failed to read {}", paths.config))?;
        toml::from_str(&contents)
            .with_context(|| format!("failed to parse {}", paths.config))
    }

    pub fn write_default(paths: &AppPaths) -> Result<()> {
        let contents = toml::to_string_pretty(&Self::default())
            .context("failed to serialize default config")?;
        fs::write(paths.config.as_std_path(), contents)
            .with_context(|| format!("failed to write {}", paths.config))?;
        Ok(())
    }

    pub fn require_agent(&self, name: &str) -> Result<&AgentConfig> {
        match self.agents.get(name) {
            Some(agent) => Ok(agent),
            None => bail!("agent `{name}` is not configured in ~/.agentd/config.toml"),
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
            },
        );
        agents.insert(
            "codex".to_string(),
            AgentConfig {
                command: "codex".to_string(),
                args: Vec::new(),
            },
        );
        Self { agents }
    }
}
