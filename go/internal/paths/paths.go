// Package paths resolves the agentd runtime root, mirroring
// crates/agentd-shared/src/paths.rs.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const AppDirName = "agentd"

type AppPaths struct {
	Root        string
	Socket      string
	PIDFile     string
	Database    string
	Config      string
	LogsDir     string
	SessionsDir string
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
		Root:        root,
		Socket:      filepath.Join(root, "agentd.sock"),
		PIDFile:     filepath.Join(root, "agentd.pid"),
		Database:    filepath.Join(root, "state.db"),
		Config:      filepath.Join(root, "config.toml"),
		LogsDir:     filepath.Join(root, "logs"),
		SessionsDir: filepath.Join(root, "sessions"),
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

// EnsureLayout creates the runtime root and its subdirectories private to
// the current user. The root holds the control socket, session sockets,
// state.db and full agent transcripts, and may live in shared temp space, so
// a root that is (or is reached through a symlink) owned by another user is
// refused, and an existing root with group/other access is tightened to 0700.
func (p *AppPaths) EnsureLayout() error {
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", p.Root, err)
	}
	if err := checkOwned(p.Root); err != nil {
		return err
	}
	for _, dir := range []string{p.Root, p.LogsDir, p.SessionsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("failed to restrict %s: %w", dir, err)
		}
	}
	return nil
}

func checkOwned(root string) error {
	uid := os.Getuid()
	for _, stat := range []func(string) (os.FileInfo, error){os.Lstat, os.Stat} {
		info, err := stat(root)
		if err != nil {
			return fmt.Errorf("failed to inspect %s: %w", root, err)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			return fmt.Errorf("refusing to use runtime root %s: it is owned by uid %d, not %d", root, st.Uid, uid)
		}
	}
	return nil
}

// WorkerLogPath is where a session worker's own stderr goes.
func (p *AppPaths) WorkerLogPath(sessionID string) string {
	return filepath.Join(p.LogsDir, sessionID+".worker.log")
}

// DaemonLogPath is where a daemonized `agentd serve` logs. It sits in the
// root rather than logs/, where any name could collide with a session's.
func (p *AppPaths) DaemonLogPath() string {
	return filepath.Join(p.Root, "agentd.log")
}

// LockPath is held (flock) by the running daemon for its whole lifetime.
func (p *AppPaths) LockPath() string {
	return filepath.Join(p.Root, "agentd.lock")
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
