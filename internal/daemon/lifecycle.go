package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// Daemonize starts `<exe> serve` detached from the caller's session and
// terminal, with output appended to agentd.log in the root, and returns without
// waiting. The agent CLI polls the socket to know when it is up.
func Daemonize(p *paths.AppPaths, exe string) error {
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.DaemonLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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

// Upgrade replaces the running daemon with exe. It refuses while sessions are
// running, since their workers run the old binary.
func Upgrade(p *paths.AppPaths, exe string) error {
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	running, err := runningSessions(p)
	if err != nil {
		return err
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
	_, err = waitForDaemon(p)
	return err
}

// Restart stops the running daemon, if any, and starts exe in its place, so
// that it rereads config.toml. Sessions keep running in their workers and
// are picked up by the new daemon. It returns the new daemon's status.
func Restart(p *paths.AppPaths, exe string) (*protocol.ManagementStatus, error) {
	if err := p.EnsureLayout(); err != nil {
		return nil, err
	}
	if err := stopDaemon(p); err != nil {
		return nil, err
	}
	if err := Daemonize(p, exe); err != nil {
		return nil, err
	}
	return waitForDaemon(p)
}

// Status returns the running daemon's status, or nil when none is running.
func Status(p *paths.AppPaths) (*protocol.ManagementStatus, error) {
	running, err := daemonRunning(p.LockPath())
	if err != nil || !running {
		return nil, err
	}
	return managementStatus(p)
}

// waitForDaemon waits for a just-started daemon to answer. Something
// answering on the socket is not enough: it must be the new daemon, speaking
// this protocol.
func waitForDaemon(p *paths.AppPaths) (*protocol.ManagementStatus, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if status, err := managementStatus(p); err == nil && status.ProtocolVersion == protocol.ProtocolVersion {
			return status, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for agentd to start; see %s", p.DaemonLogPath())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runningSessions lists sessions whose worker still answers.
func runningSessions(p *paths.AppPaths) ([]string, error) {
	store, err := db.Open(p.Database)
	if err != nil {
		return nil, err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return nil, err
	}
	var running []string
	for _, rec := range recs {
		if rec.Status == session.StatusRunning && answers(p.SessionSocketPath(rec.SessionID)) {
			running = append(running, fmt.Sprintf("%s (%s)", rec.SessionID, rec.Agent))
		}
	}
	return running, nil
}

func managementStatus(p *paths.AppPaths) (*protocol.ManagementStatus, error) {
	conn, err := transport.DialUnix(p.Socket, workerDialTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	if err := protocol.WriteManagementRequest(conn, &protocol.ManagementRequest{Status: protocol.Empty}); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadManagementResponse(bufio.NewReader(conn))
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Status == nil {
		return nil, errors.New("unexpected management response")
	}
	return resp.Status, nil
}

// stopDaemon stops the running daemon, if any. The daemon's lock says
// whether one is running, and it is asked to stop over its own socket, so no
// pid is ever read from disk and signalled.
func stopDaemon(p *paths.AppPaths) error {
	running, err := daemonRunning(p.LockPath())
	if err != nil || !running {
		return err
	}
	if err := requestShutdown(p); err != nil {
		return err
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

func requestShutdown(p *paths.AppPaths) error {
	conn, err := transport.DialUnix(p.Socket, workerDialTimeout)
	if err != nil {
		return fmt.Errorf("agentd does not answer on %s: %w", p.Socket, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	err = protocol.WriteManagementRequest(conn, &protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}})
	if err == nil {
		_, err = protocol.ReadManagementResponse(bufio.NewReader(conn))
	}
	if err != nil {
		return fmt.Errorf("failed to ask agentd to stop: %w", err)
	}
	return nil
}

func answers(socket string) bool {
	conn, err := transport.DialUnix(socket, workerDialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
