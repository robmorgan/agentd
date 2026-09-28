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
	Remote       RemoteConfig           `toml:"remote"`
}

// RemoteConfig enables remote access over QUIC. It is off unless Listen is
// set; even then only clients listed in the authorized-clients file can
// connect.
type RemoteConfig struct {
	// Listen is the UDP host:port to accept QUIC connections on. Prefer a
	// Tailscale or WireGuard address over a public one.
	Listen string `toml:"listen"`
}

type AgentConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// ModelFlag is passed before the model name when a session requests a
	// model. Empty disables it.
	ModelFlag *string `toml:"model_flag"`
}

const defaultModelFlag = "--model"

// preferredDefaultAgent is used when a config does not name default_agent
// and configures it (or configures no agents); otherwise the first
// configured agent is the default. crates/agentd-shared resolves it the same
// way.
const preferredDefaultAgent = "claude"

func defaultConfig() *Config {
	return &Config{
		DefaultAgent: preferredDefaultAgent,
		Agents: map[string]AgentConfig{
			"claude": {Command: "claude"},
			"codex":  {Command: "codex"},
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
	meta, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = preferredDefaultAgent
		if _, ok := cfg.Agents[preferredDefaultAgent]; !ok && len(cfg.Agents) > 0 {
			// Maps lose file order; the decoder's key list keeps it.
			for _, key := range meta.Keys() {
				if len(key) == 2 && key[0] == "agents" {
					cfg.DefaultAgent = key[1]
					break
				}
			}
		}
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
