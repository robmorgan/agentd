package transport

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// UnixListener serves streams on a Unix socket, one stream per connection.
type UnixListener struct {
	l    *net.UnixListener
	path string
}

// UnixOptions adjusts ListenUnix.
type UnixOptions struct {
	// KeepSocketOnClose leaves the socket file in place when the listener
	// closes, for callers that remove it themselves (e.g. only while the
	// path still refers to their own socket).
	KeepSocketOnClose bool
}

// ListenUnix binds a Unix socket at path, private to the current user
// (0600). Any file already at path must have been removed by the caller.
func ListenUnix(path string, opts UnixOptions) (*UnixListener, error) {
	// The kernel's limit (sun_path, including its NUL) otherwise surfaces
	// as a bare "invalid argument".
	if limit := len(syscall.RawSockaddrUnix{}.Path) - 1; len(path) > limit {
		return nil, fmt.Errorf("cannot create socket %s: the path is %d bytes, over this system's limit of %d; use a shorter session name, or set AGENTD_DIR to a shorter directory", path, len(path), limit)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("failed to bind %s: %w", path, err)
	}
	if opts.KeepSocketOnClose {
		l.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("failed to restrict %s: %w", path, err)
	}
	return &UnixListener{l: l, path: path}, nil
}

func (u *UnixListener) Accept() (Stream, error) {
	conn, err := u.l.AcceptUnix()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (u *UnixListener) Close() error { return u.l.Close() }
func (u *UnixListener) Addr() string { return u.path }

// DialUnix opens a stream to the Unix socket at path.
func DialUnix(path string, timeout time.Duration) (Stream, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	return conn.(*net.UnixConn), nil
}
