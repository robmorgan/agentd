package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// Upgrade replaces the running daemon with exe, then hands every running
// session's worker over to exe as well (HandoffSessions), so sessions keep
// running, on the new binary, through an upgrade. It returns each session's
// outcome; one whose handoff failed keeps running on its old binary.
func Upgrade(p *paths.AppPaths, exe string) ([]HandoffResult, error) {
	if err := p.EnsureLayout(); err != nil {
		return nil, err
	}
	if err := stopDaemon(p); err != nil {
		return nil, err
	}
	if err := Daemonize(p, exe); err != nil {
		return nil, err
	}
	if _, err := waitForDaemon(p); err != nil {
		return nil, err
	}
	return HandoffSessions(p, exe)
}

// handoffTimeout bounds one worker's handoff: probing the new binary,
// letting clients go, and the new image restoring the terminal.
const handoffTimeout = 60 * time.Second

// HandoffResult is the outcome of asking one session's worker to hand off.
type HandoffResult struct {
	SessionID string
	// HandedOff is the new image's answer, or nil if the handoff did not
	// happen (Err says why).
	HandedOff *protocol.HandedOff
	Err       error
	// Lost reports that the worker went away during a failed handoff, so
	// the session did not survive it (and is recorded as failed).
	Lost bool
}

// HandoffSessions asks the worker of every running session, one at a time,
// to hand its session over to exe without stopping the agent (see
// internal/worker/handoff.go). A worker that cannot (it predates handoff, or
// the new binary does not run) keeps its session running as it was.
func HandoffSessions(p *paths.AppPaths, exe string) ([]HandoffResult, error) {
	store, err := db.Open(p.Database)
	if err != nil {
		return nil, err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return nil, err
	}
	var results []HandoffResult
	for _, rec := range recs {
		if rec.Status != session.StatusRunning || !answers(p.SessionSocketPath(rec.SessionID)) {
			continue
		}
		results = append(results, handoffWorker(p, rec.SessionID, exe))
	}
	return results, nil
}

func handoffWorker(p *paths.AppPaths, id, exe string) HandoffResult {
	res := HandoffResult{SessionID: id}
	socket := p.SessionSocketPath(id)
	caps, err := workerCapabilities(socket)
	if err != nil {
		res.Err = err
		return res
	}
	if !protocol.HasCapability(caps, protocol.CapWorkerHandoff) {
		res.Err = errors.New("its worker was started by an agentd without live handoff; it picks up the new binary when the session is restarted")
		return res
	}
	conn, err := transport.DialUnix(socket, workerDialTimeout)
	if err != nil {
		res.Err = err
		return res
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(handoffTimeout))
	resp, err := exchangeOn(conn, &protocol.Request{HandoffSession: &protocol.HandoffSession{SessionID: id, Executable: exe}})
	switch {
	case err == nil && resp.HandedOff != nil:
		res.HandedOff = resp.HandedOff
		return res
	case err == nil && resp.Error != nil:
		res.Err = errors.New(resp.Error.Message)
		return res
	case err == nil:
		res.Err = errors.New("unexpected response to the handoff request")
		return res
	}
	// The connection ended without an answer: the new image failed after
	// the exec, or the worker died. Tell which from whether the session
	// still answers.
	if answers(socket) {
		res.Err = fmt.Errorf("the handoff's outcome is unknown (%v); the session is still running", err)
	} else {
		res.Err = fmt.Errorf("the session's worker exited during the handoff (%v); see %s", err, p.WorkerLogPath(id))
		res.Lost = true
	}
	return res
}

// exchangeOn writes req on conn and reads the one response.
func exchangeOn(conn transport.Stream, req *protocol.Request) (*protocol.Response, error) {
	if err := protocol.WriteRequest(conn, req); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(conn))
	if err == nil && resp == nil {
		err = errors.New("the worker closed the connection")
	}
	return resp, err
}

// workerCapabilities asks a session's worker which optional protocol
// features it supports. A worker may run an older binary than the daemon
// (one whose handoff failed, or that predates handoff); one too old to
// answer Hello supports none.
func workerCapabilities(socket string) ([]string, error) {
	conn, err := transport.DialUnix(socket, workerDialTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	resp, err := exchangeOn(conn, &protocol.Request{Hello: protocol.NewHello("agentd " + Version)})
	if err != nil || resp.Welcome == nil {
		// A worker that predates Hello answers it with an error, or
		// drops the connection if it cannot decode it.
		return nil, nil
	}
	return resp.Welcome.Capabilities, nil
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
