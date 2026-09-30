// Package paths resolves the agentd runtime root and the files under it.
package paths

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
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
	if home == "" {
		// Without $HOME (cron, some service managers), fall back to the
		// passwd entry.
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	root, err := discoverRoot(
		os.Getenv("AGENTD_DIR"),
		os.Getenv("XDG_RUNTIME_DIR"),
		home,
		os.Getenv("TMPDIR"),
		os.Getuid(),
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

// discoverRoot picks the root: AGENTD_DIR, else ~/.agentd. The root holds
// state that must survive reboots (state.db, logs, the remote keys), so a
// per-boot runtime directory such as XDG_RUNTIME_DIR (/run/user/UID, a
// tmpfs) is only a fallback for an account without a home directory, as are
// the temp directories.
func discoverRoot(agentdDir, xdgRuntimeDir, homeDir, tmpDir string, uid int) (string, error) {
	if agentdDir != "" {
		return agentdDir, nil
	}
	if homeDir != "" {
		return filepath.Join(homeDir, "."+AppDirName), nil
	}
	if xdgRuntimeDir != "" {
		return filepath.Join(xdgRuntimeDir, AppDirName), nil
	}
	if tmpDir != "" {
		return filepath.Join(tmpDir, fmt.Sprintf("%s-%d", AppDirName, uid)), nil
	}
	return fmt.Sprintf("/tmp/%s-%d", AppDirName, uid), nil
}

// EnsureLayout creates the runtime root, logs/ and sessions/ as private
// (0700) directories, tightening them if they already exist. The root holds
// the control socket, session sockets, state.db and full agent transcripts,
// and may live in shared temp space, so a directory that is a symlink, is not
// a directory, or belongs to another user is refused rather than used.
func (p *AppPaths) EnsureLayout() error {
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", p.Root, err)
	}
	for _, dir := range []string{p.Root, p.LogsDir, p.SessionsDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("failed to inspect %s: %w", dir, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to use %s: it is a symlink; point AGENTD_DIR at a real directory", dir)
	case !info.IsDir():
		return fmt.Errorf("refusing to use %s: it is not a directory", dir)
	}
	uid := os.Getuid()
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return fmt.Errorf("refusing to use %s: it is owned by uid %d, not %d", dir, st.Uid, uid)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("failed to restrict %s: %w", dir, err)
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

// RemoteKeyPath is the daemon's key for remote (QUIC) connections.
func (p *AppPaths) RemoteKeyPath() string {
	return filepath.Join(p.Root, "remote", "daemon.key")
}

// AuthorizedClientsPath lists the client keys allowed to connect remotely.
func (p *AppPaths) AuthorizedClientsPath() string {
	return filepath.Join(p.Root, "remote", "authorized_clients")
}

// ClientKeyPath is this machine's key for connecting to remote daemons.
func (p *AppPaths) ClientKeyPath() string {
	return filepath.Join(p.Root, "remote", "client.key")
}

// HostsPath lists the remote daemons this machine knows, with their pinned
// keys.
func (p *AppPaths) HostsPath() string {
	return filepath.Join(p.Root, "hosts.toml")
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
