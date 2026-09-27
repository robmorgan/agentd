// Package paths resolves the agentd runtime root, mirroring
// crates/agentd-shared/src/paths.rs.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const AppDirName = "agentd"

type AppPaths struct {
	Root         string
	Socket       string
	PIDFile      string
	Database     string
	Config       string
	LogsDir      string
	SessionsDir  string
	WorktreesDir string
}

func Discover() (*AppPaths, error) {
	home, _ := os.UserHomeDir()
	root, err := discoverRoot(
		os.Getenv("AGENTD_DIR"),
		os.Getenv("XDG_RUNTIME_DIR"),
		home,
		os.Getenv("TMPDIR"),
		os.Getuid(),
		runtime.GOOS == "darwin",
	)
	if err != nil {
		return nil, err
	}
	return FromRoot(root), nil
}

func FromRoot(root string) *AppPaths {
	return &AppPaths{
		Root:         root,
		Socket:       filepath.Join(root, "agentd.sock"),
		PIDFile:      filepath.Join(root, "agentd.pid"),
		Database:     filepath.Join(root, "state.db"),
		Config:       filepath.Join(root, "config.toml"),
		LogsDir:      filepath.Join(root, "logs"),
		SessionsDir:  filepath.Join(root, "sessions"),
		WorktreesDir: filepath.Join(root, "worktrees"),
	}
}

func discoverRoot(agentdDir, xdgRuntimeDir, homeDir, tmpDir string, uid int, preferHomeRoot bool) (string, error) {
	if agentdDir != "" {
		return agentdDir, nil
	}
	if xdgRuntimeDir != "" {
		return filepath.Join(xdgRuntimeDir, AppDirName), nil
	}
	if preferHomeRoot && homeDir != "" {
		return filepath.Join(homeDir, "."+AppDirName), nil
	}
	if tmpDir != "" {
		return filepath.Join(tmpDir, fmt.Sprintf("%s-%d", AppDirName, uid)), nil
	}
	return fmt.Sprintf("/tmp/%s-%d", AppDirName, uid), nil
}

func (p *AppPaths) EnsureLayout() error {
	for _, dir := range []string{p.Root, p.LogsDir, p.SessionsDir, p.WorktreesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	return nil
}

func (p *AppPaths) LogPath(sessionID string) string {
	return filepath.Join(p.LogsDir, sessionID+".log")
}

func (p *AppPaths) RenderedLogPath(sessionID string) string {
	return filepath.Join(p.LogsDir, sessionID+".rendered.log")
}

func (p *AppPaths) SessionSocketPath(sessionID string) string {
	return filepath.Join(p.SessionsDir, sessionID+".sock")
}

func (p *AppPaths) WorktreePath(sessionID string) string {
	return filepath.Join(p.WorktreesDir, sessionID)
}
