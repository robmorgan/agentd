use std::{fs, io::Write, os::unix::fs::OpenOptionsExt};

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

fn default_agent_name() -> String {
    "codex".to_string()
}

impl Config {
    pub fn load(paths: &AppPaths) -> Result<Self> {
        if !paths.config.exists() {
            return Ok(Self::default());
        }

        let contents = fs::read_to_string(paths.config.as_std_path())
            .with_context(|| format!("failed to read {}", paths.config))?;
        let config: Self = toml::from_str(&contents)
            .with_context(|| format!("failed to parse {}", paths.config))?;
        config.validate(paths)
    }

    pub fn write_default(paths: &AppPaths) -> Result<()> {
        let contents = toml::to_string_pretty(&Self::default())
            .context("failed to serialize default config")?;
        // Files under the runtime root are private to the user, like the root.
        let mut file = fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(paths.config.as_std_path())
            .with_context(|| format!("failed to write {}", paths.config))?;
        file.write_all(contents.as_bytes())
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
        Self { default_agent: default_agent_name(), agents }
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
            sessions_dir: root.join("sessions"),
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
    fn write_default_creates_private_file() {
        use std::os::unix::fs::PermissionsExt;
        let suffix =
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos();
        let root =
            Utf8PathBuf::from(format!("/tmp/agentd-config-mode-{}-{suffix}", std::process::id()));
        let paths =
            AppPaths { config: root.join("config.toml"), root: root.clone(), ..test_paths() };
        std::fs::create_dir_all(root.as_std_path()).unwrap();

        Config::write_default(&paths).unwrap();

        let mode = std::fs::metadata(paths.config.as_std_path()).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
        assert_eq!(Config::load(&paths).unwrap().default_agent, "codex");
        let _ = std::fs::remove_dir_all(root.as_std_path());
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
