package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "missing.toml"))
	if err != nil || cfg.DefaultAgent != "claude" || cfg.Agents["codex"].Command != "codex" {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("default_agent = \"x\"\n[agents.y]\ncommand = \"y\"\n"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default_agent `x`") {
		t.Fatalf("got %v", err)
	}
	os.WriteFile(path, []byte("[agents.y]\ncommand = \"y\"\nmodel_flag = \"\"\n[agents.z]\ncommand = \"z\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["y"].Flag(); got != "" {
		t.Fatalf("empty model_flag = %q", got)
	}
	if got := cfg.Agents["z"].Flag(); got != "--model" {
		t.Fatalf("default model_flag = %q", got)
	}

	// Without default_agent: claude if configured, else the first agent.
	os.WriteFile(path, []byte("[agents.codex]\ncommand = \"codex\"\n[agents.claude]\ncommand = \"claude\"\n"), 0o600)
	if cfg, err := Load(path); err != nil || cfg.DefaultAgent != "claude" {
		t.Fatalf("with claude configured: %+v, %v", cfg, err)
	}
	os.WriteFile(path, []byte("[agents.zed]\ncommand = \"zed\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	if cfg, err := Load(path); err != nil || cfg.DefaultAgent != "zed" {
		t.Fatalf("without claude: %+v, %v", cfg, err)
	}
}

func TestRequireAgentNamesTheConfigFile(t *testing.T) {
	_, err := defaultConfig().RequireAgent("missing", "/x/config.toml")
	if err == nil || !strings.Contains(err.Error(), "/x/config.toml") {
		t.Fatalf("got %v", err)
	}
}

// The written default parses to the built-in defaults, is private, and is
// never overwritten.
func TestWriteDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	if cfg.DefaultAgent != want.DefaultAgent || !slices.Equal(cfg.AgentNames(), want.AgentNames()) {
		t.Fatalf("got %+v", cfg)
	}
	for name, agent := range cfg.Agents {
		if agent.Command != want.Agents[name].Command || agent.Flag() != "--model" {
			t.Fatalf("%s: %+v", name, agent)
		}
	}
	if got, want := cfg.Attach.AttachScrollbackRows(), want.Attach.AttachScrollbackRows(); got != want {
		t.Fatalf("attach scrollback rows = %d, built-in default %d", got, want)
	}
	os.WriteFile(path, []byte("default_agent = \"zed\"\n"), 0o600)
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "default_agent = \"zed\"\n" {
		t.Fatalf("overwrote: %q", data)
	}
}

func TestAgentNamesKeepFileOrder(t *testing.T) {
	if got := defaultConfig().AgentNames(); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Fatalf("defaults: %v", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("default_agent = \"claude\"\n[agents.claude]\ncommand = \"claude\"\n[agents.zed]\ncommand = \"zed\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentNames(); !slices.Equal(got, []string{"claude", "zed", "codex"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAttachScrollbackRows(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		text string
		want int
		bad  bool
	}{
		{"", DefaultAttachScrollbackRows, false},
		{"[attach]\nscrollback_rows = 0\n", 0, false},
		{"[attach]\nscrollback_rows = -1\n", -1, false},
		{"[attach]\nscrollback_rows = 250\n", 250, false},
		{"[attach]\nscrollback_rows = -2\n", 0, true},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.bad {
			if err == nil {
				t.Fatalf("%q: loaded", tc.text)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if got := cfg.Attach.AttachScrollbackRows(); got != tc.want {
			t.Fatalf("%q: scrollback rows = %d, want %d", tc.text, got, tc.want)
		}
	}
}
