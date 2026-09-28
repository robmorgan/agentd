package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// Daemonize starts `<exe> serve` detached from the caller's session and
// terminal, with output appended to logs/agentd.log, and returns without
// waiting. The agent CLI polls the socket to know when it is up.
func Daemonize(p *paths.AppPaths, exe string) error {
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(p.LogsDir, "agentd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to daemonize agentd: %w", err)
	}
	return cmd.Process.Release()
}

// Upgrade replaces the running daemon with exe. Like the Rust daemon it
// refuses while sessions are running, since their workers run the old binary.
func Upgrade(p *paths.AppPaths, exe string) error {
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	store, err := db.Open(p.Database)
	if err != nil {
		return err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return err
	}
	var running []string
	for _, rec := range recs {
		if rec.Status != session.StatusRunning {
			continue
		}
		if conn, err := net.DialTimeout("unix", p.SessionSocketPath(rec.SessionID), workerDialTimeout); err == nil {
			conn.Close()
			running = append(running, fmt.Sprintf("%s (%s)", rec.SessionID, rec.Agent))
		}
	}
	if len(running) > 0 {
		return fmt.Errorf("cannot upgrade agentd while sessions are running: %s", strings.Join(running, ", "))
	}
	if err := stopDaemon(p); err != nil {
		return err
	}
	if err := Daemonize(p, exe); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if conn, err := net.Dial("unix", p.Socket); err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for upgraded agentd to start")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopDaemon stops the running daemon, if any. The daemon's lock says
// whether one is running, and it is asked to stop over its own socket, so no
// pid is ever read from disk and signalled.
func stopDaemon(p *paths.AppPaths) error {
	running, err := daemonRunning(p.LockPath())
	if err != nil || !running {
		return err
	}
	conn, err := net.DialTimeout("unix", p.Socket, workerDialTimeout)
	if err != nil {
		return fmt.Errorf("agentd holds %s but does not answer on %s: %w", p.LockPath(), p.Socket, err)
	}
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	err = protocol.WriteManagementRequest(conn, &protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}})
	if err == nil {
		_, err = protocol.ReadManagementResponse(bufio.NewReader(conn))
	}
	conn.Close()
	if err != nil {
		return fmt.Errorf("failed to ask agentd to stop: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, err := daemonRunning(p.LockPath())
		if err != nil || !running {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("agentd did not stop")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
