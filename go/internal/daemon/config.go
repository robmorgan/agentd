package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config mirrors crates/agentd-shared/src/config.rs; the CLI reads the same
// file. Unknown keys and tables are ignored.
type Config struct {
	DefaultAgent string                 `toml:"default_agent"`
	Agents       map[string]AgentConfig `toml:"agents"`
}

type AgentConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// ModelFlag is passed before the model name when a session requests a
	// model. Empty disables it.
	ModelFlag *string `toml:"model_flag"`
}

const defaultModelFlag = "--model"

func defaultConfig() *Config {
	return &Config{
		DefaultAgent: "codex",
		Agents: map[string]AgentConfig{
			"codex":  {Command: "codex"},
			"claude": {Command: "claude"},
		},
	}
}

// LoadConfig reads path, falling back to the built-in defaults when the file
// does not exist.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = "codex"
	}
	if len(cfg.Agents) > 0 {
		if _, ok := cfg.Agents[cfg.DefaultAgent]; !ok {
			return nil, fmt.Errorf("default_agent `%s` is not configured under [agents] in %s", cfg.DefaultAgent, path)
		}
	}
	return cfg, nil
}

func (c *Config) requireAgent(name, path string) (AgentConfig, error) {
	agent, ok := c.Agents[name]
	if !ok {
		return AgentConfig{}, fmt.Errorf("agent `%s` is not configured in %s", name, path)
	}
	return agent, nil
}

func (a AgentConfig) modelFlag() string {
	if a.ModelFlag == nil {
		return defaultModelFlag
	}
	return *a.ModelFlag
}
