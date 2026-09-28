package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/session"
)

// Daemonize starts `<exe> serve` detached from the caller's session and
// terminal, with output appended to logs/agentd.log, and returns without
// waiting. The agent CLI polls the socket to know when it is up.
func Daemonize(p *paths.AppPaths, exe string) error {
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(p.LogsDir, "agentd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
		if rec.WorkerPID != nil && rec.Status == session.StatusRunning && processExists(int(*rec.WorkerPID)) {
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

func stopDaemon(p *paths.AppPaths) error {
	data, err := os.ReadFile(p.PIDFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", p.PIDFile, err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("failed to parse pid from %s: %w", p.PIDFile, err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to send %v to agentd: %w", sig, err)
		}
		if waitForExit(pid, 5*time.Second) {
			return nil
		}
	}
	return errors.New("agentd did not exit after SIGTERM and SIGKILL")
}
