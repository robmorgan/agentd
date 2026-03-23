use std::fs;

use anyhow::{Context, Result, bail};
use indexmap::IndexMap;
use serde::{Deserialize, Serialize};

use crate::paths::AppPaths;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    #[serde(default = "default_agent_name")]
    pub default_agent: String,
    #[serde(default)]
    pub agents: IndexMap<String, AgentConfig>,
    #[serde(default)]
    pub git: GitConfig,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentConfig {
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default = "default_model_flag")]
    pub model_flag: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GitConfig {
    #[serde(default = "default_integration_policy")]
    pub default_integration_policy: String,
    #[serde(default = "default_auto_commit_message")]
    pub auto_commit_message: String,
}

fn default_model_flag() -> Option<String> {
    Some("--model".to_string())
}

fn default_agent_name() -> String {
    "codex".to_string()
}

fn default_integration_policy() -> String {
    "auto_apply_safe".to_string()
}

fn default_auto_commit_message() -> String {
    "agentd: finalize session {session_id}".to_string()
}

impl Config {
    pub fn load(paths: &AppPaths) -> Result<Self> {
        if !paths.config.exists() {
            return Ok(Self::default());
        }

        let contents = fs::read_to_string(paths.config.as_std_path())
            .with_context(|| format!("failed to read {}", paths.config))?;
        let config: Self =
            toml::from_str(&contents).with_context(|| format!("failed to parse {}", paths.config))?;
        config.validate(paths)
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

    pub fn default_agent_name<'a>(&'a self, paths: &AppPaths) -> Result<&'a str> {
        if self.agents.is_empty() {
            return Ok(self.default_agent.as_str());
        }
        if self.agents.contains_key(&self.default_agent) {
            return Ok(self.default_agent.as_str());
        }
        bail!(
            "default_agent `{}` is not configured under [agents] in {}",
            self.default_agent,
            paths.config
        )
    }

    fn validate(self, paths: &AppPaths) -> Result<Self> {
        self.default_agent_name(paths)?;
        Ok(self)
    }
}

impl Default for Config {
    fn default() -> Self {
        let mut agents = IndexMap::new();
        agents.insert(
            "codex".to_string(),
            AgentConfig {
                command: "codex".to_string(),
                args: Vec::new(),
                model_flag: default_model_flag(),
            },
        );
        agents.insert(
            "claude".to_string(),
            AgentConfig {
                command: "claude".to_string(),
                args: Vec::new(),
                model_flag: default_model_flag(),
            },
        );
        Self { default_agent: default_agent_name(), agents, git: GitConfig::default() }
    }
}

impl Default for GitConfig {
    fn default() -> Self {
        Self {
            default_integration_policy: default_integration_policy(),
            auto_commit_message: default_auto_commit_message(),
        }
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
        let err = Config::default().require_agent(&paths, "missing").unwrap_err().to_string();
        assert!(err.contains(paths.config.as_str()));
    }

    #[test]
    fn default_config_uses_codex_default_agent_and_order() {
        let config = Config::default();
        assert_eq!(config.default_agent, "codex");
        assert_eq!(
            config.agents.keys().map(String::as_str).collect::<Vec<_>>(),
            vec!["codex", "claude"]
        );
    }

    #[test]
    fn load_preserves_agent_order_from_toml() {
        let paths = test_paths();
        let config: Config = toml::from_str(
            r#"
default_agent = "claude"

[agents.claude]
command = "claude"

[agents.codex]
command = "codex"

[agents.zed]
command = "zed"
"#,
        )
        .unwrap();

        let config = config.validate(&paths).unwrap();
        assert_eq!(
            config.agents.keys().map(String::as_str).collect::<Vec<_>>(),
            vec!["claude", "codex", "zed"]
        );
        assert_eq!(config.default_agent_name(&paths).unwrap(), "claude");
    }

    #[test]
    fn missing_default_agent_defaults_to_codex() {
        let paths = test_paths();
        let config: Config = toml::from_str(
            r#"
[agents.codex]
command = "codex"
"#,
        )
        .unwrap();

        let config = config.validate(&paths).unwrap();
        assert_eq!(config.default_agent, "codex");
        assert_eq!(config.default_agent_name(&paths).unwrap(), "codex");
    }

    #[test]
    fn invalid_default_agent_returns_clear_error() {
        let paths = test_paths();
        let config: Config = toml::from_str(
            r#"
default_agent = "missing"

[agents.codex]
command = "codex"
"#,
        )
        .unwrap();

        let err = config.validate(&paths).unwrap_err().to_string();
        assert!(err.contains("default_agent `missing`"));
        assert!(err.contains(paths.config.as_str()));
    }
}
