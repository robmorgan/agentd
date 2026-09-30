// Package config reads config.toml, which the daemon and the agent CLI
// share. Unknown keys and tables are ignored.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DefaultAgent string                 `toml:"default_agent"`
	Agents       map[string]AgentConfig `toml:"agents"`
	Remote       RemoteConfig           `toml:"remote"`

	// agentOrder lists the Agents keys in file order, which a map loses.
	agentOrder []string
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
	// model. Empty disables it; unset means DefaultModelFlag.
	ModelFlag *string `toml:"model_flag"`
}

const DefaultModelFlag = "--model"

// preferredDefaultAgent is used when a config does not name default_agent
// and configures it (or configures no agents); otherwise the first
// configured agent is the default.
const preferredDefaultAgent = "claude"

// DefaultText is the config.toml written when none exists. It says the same
// as the built-in defaults, so creating the file changes nothing.
const DefaultText = `default_agent = "claude"

[agents.claude]
command = "claude"
args = []
model_flag = "--model"

[agents.codex]
command = "codex"
args = []
model_flag = "--model"
`

func defaultConfig() *Config {
	return &Config{
		DefaultAgent: preferredDefaultAgent,
		Agents: map[string]AgentConfig{
			"claude": {Command: "claude"},
			"codex":  {Command: "codex"},
		},
		agentOrder: []string{"claude", "codex"},
	}
}

// Load reads path, falling back to the built-in defaults when the file does
// not exist.
func Load(path string) (*Config, error) {
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
	for _, key := range meta.Keys() {
		if len(key) == 2 && key[0] == "agents" {
			cfg.agentOrder = append(cfg.agentOrder, key[1])
		}
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = preferredDefaultAgent
		if _, ok := cfg.Agents[preferredDefaultAgent]; !ok && len(cfg.agentOrder) > 0 {
			cfg.DefaultAgent = cfg.agentOrder[0]
		}
	}
	if len(cfg.Agents) > 0 {
		if _, ok := cfg.Agents[cfg.DefaultAgent]; !ok {
			return nil, fmt.Errorf("default_agent `%s` is not configured under [agents] in %s", cfg.DefaultAgent, path)
		}
	}
	return cfg, nil
}

// WriteDefault creates path with DefaultText, private to the user like
// everything under the runtime root. An existing file is left alone.
func WriteDefault(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if _, err := f.WriteString(DefaultText); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return f.Close()
}

// AgentNames lists the configured agents in file order.
func (c *Config) AgentNames() []string {
	return append([]string(nil), c.agentOrder...)
}

// RequireAgent returns the named agent, or an error naming the config file.
func (c *Config) RequireAgent(name, path string) (AgentConfig, error) {
	agent, ok := c.Agents[name]
	if !ok {
		return AgentConfig{}, fmt.Errorf("agent `%s` is not configured in %s", name, path)
	}
	return agent, nil
}

// Flag returns the flag that introduces a model name, or "" for none.
func (a AgentConfig) Flag() string {
	if a.ModelFlag == nil {
		return DefaultModelFlag
	}
	return *a.ModelFlag
}
