package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// errDaemonRunning means another daemon holds the runtime root's lock.
var errDaemonRunning = errors.New("agentd is already running")

// acquireLock takes the runtime root's exclusive lock, which the daemon holds
// for its whole lifetime. The kernel drops it when the process exits however
// it exits, so unlike a pid file it can never go stale or name a recycled
// pid. It waits up to wait for a previous daemon that is still draining
// connections after a restart.
func acquireLock(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("failed to lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w (lock %s is held)", errDaemonRunning, path)
		}
		time.Sleep(pollInterval)
	}
}

func releaseLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// daemonRunning reports whether some daemon currently holds the lock.
func daemonRunning(path string) (bool, error) {
	f, err := acquireLock(path, 0)
	if errors.Is(err, errDaemonRunning) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	releaseLock(f)
	return false, nil
}
