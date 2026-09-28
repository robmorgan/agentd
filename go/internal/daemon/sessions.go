package daemon

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

const (
	workerReadyTimeout = 5 * time.Second
	// workerStopTimeout covers the worker's own SIGTERM->SIGKILL grace for
	// the agent (5s) plus time to write logs and exit.
	workerStopTimeout = 8 * time.Second
	workerKillTimeout = 2 * time.Second
	pollInterval      = 20 * time.Millisecond

	sessionNameRules = "use 1-64 lowercase letters, numbers, and single hyphens"
)

var (
	nameAdjectives = []string{"brisk", "calm", "clever", "curious", "gentle", "nimble", "quiet", "steady", "swift", "wrinkly"}
	nameAnimals    = []string{"badgers", "bears", "foxes", "geckos", "otters", "pandas", "ravens", "tigers", "whales", "wolves"}
)

func validSessionName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	lastHyphen := false
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			lastHyphen = false
		case c == '-' && !lastHyphen:
			lastHyphen = true
		default:
			return false
		}
	}
	return true
}

func (s *Server) workerLogPath(sessionID string) string {
	return filepath.Join(s.paths.LogsDir, sessionID+".worker.log")
}

// alive reports whether a running record still has a live worker. A worker
// removes its socket only after recording its outcome, so a running row with
// a live pid and a socket is genuinely live.
func (s *Server) alive(rec *session.Record) bool {
	if rec.Status != session.StatusRunning || rec.WorkerPID == nil || !processExists(int(*rec.WorkerPID)) {
		return false
	}
	_, err := os.Stat(s.paths.SessionSocketPath(rec.SessionID))
	return err == nil
}

// refresh downgrades a running record whose worker has vanished, e.g. one
// that was SIGKILLed while no daemon was supervising it.
func (s *Server) refresh(rec *session.Record) (*session.Record, error) {
	if rec.Status != session.StatusRunning || s.alive(rec) {
		return rec, nil
	}
	if err := s.db.MarkUnknownRecovered(rec.SessionID); err != nil {
		return nil, err
	}
	fresh, err := s.db.GetSession(rec.SessionID)
	if err != nil || fresh == nil {
		return rec, err
	}
	return fresh, nil
}

func (s *Server) getSession(id string) (*session.Record, error) {
	rec, err := s.db.GetSession(id)
	if err != nil || rec == nil {
		return rec, err
	}
	return s.refresh(rec)
}

func (s *Server) listSessions() ([]session.Record, error) {
	recs, err := s.db.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]session.Record, 0, len(recs))
	for i := range recs {
		rec, err := s.refresh(&recs[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

func (s *Server) hasRunningSessions() (bool, error) {
	recs, err := s.db.ListSessions()
	if err != nil {
		return false, err
	}
	for i := range recs {
		if s.alive(&recs[i]) {
			return true, nil
		}
	}
	return false, nil
}

// reconcileSessions runs at startup. Running sessions whose worker survived a
// previous daemon stay running and are reachable again through their
// sockets; the rest are marked lost.
func (s *Server) reconcileSessions() error {
	recs, err := s.db.ListSessions()
	if err != nil {
		return err
	}
	for i := range recs {
		rec := &recs[i]
		switch rec.Status {
		case session.StatusRunning:
			if _, err := s.refresh(rec); err != nil {
				return err
			}
		case session.StatusCreating:
			if rec.WorkerPID == nil {
				if err := s.db.MarkFailedIfActive(rec.SessionID, "agentd stopped while the session was starting"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) allocateSession(name *string, agent string, model *string, cwd string) (string, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	id := ""
	if name != nil {
		id = *name
		existing, err := s.db.GetSession(id)
		if err != nil {
			return "", err
		}
		if existing != nil {
			return "", fmt.Errorf("session `%s` already exists", id)
		}
	} else {
		for range 16 {
			candidate := nameAdjectives[rand.IntN(len(nameAdjectives))] + "-" + nameAnimals[rand.IntN(len(nameAnimals))]
			existing, err := s.db.GetSession(candidate)
			if err != nil {
				return "", err
			}
			if existing == nil {
				id = candidate
				break
			}
		}
		if id == "" {
			return "", errors.New("failed to allocate a unique session name")
		}
	}
	return id, s.db.InsertSession(db.NewSession{
		SessionID: id, Agent: agent, Model: model, Mode: session.ModeExecute, Cwd: cwd,
	})
}

func (s *Server) createSession(req *protocol.CreateSession) (*session.CreateResult, error) {
	var name *string
	if req.Name != nil {
		if trimmed := strings.TrimSpace(*req.Name); trimmed != "" {
			if !validSessionName(trimmed) {
				return nil, fmt.Errorf("invalid session name `%s`: %s", trimmed, sessionNameRules)
			}
			name = &trimmed
		}
	}
	agent, err := s.config.requireAgent(req.Agent, s.paths.Config)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(req.Cwd) {
		return nil, fmt.Errorf("working directory `%s` must be an absolute path", req.Cwd)
	}

	id, err := s.allocateSession(name, req.Agent, req.Model, req.Cwd)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*session.CreateResult, error) {
		_ = s.db.MarkFailedIfActive(id, err.Error())
		return nil, err
	}

	// Checked here as well as in the worker so a bad cwd is refused before
	// any process is spawned. The record is kept, failed, so `agent ls`
	// shows what happened.
	if info, err := os.Stat(req.Cwd); err != nil {
		return fail(fmt.Errorf("working directory `%s` does not exist", req.Cwd))
	} else if !info.IsDir() {
		return fail(fmt.Errorf("working directory `%s` is not a directory", req.Cwd))
	}

	args := []string{
		"session-worker",
		"--session-id", id,
		"--cwd", req.Cwd,
		"--agent-name", req.Agent,
		"--command", agent.Command,
	}
	if req.Model != nil {
		args = append(args, "--model", *req.Model)
	}
	launchArgs := append([]string(nil), agent.Args...)
	if req.Model != nil && agent.modelFlag() != "" {
		launchArgs = append(launchArgs, agent.modelFlag(), *req.Model)
	}
	for _, a := range launchArgs {
		args = append(args, "--arg", a)
	}

	exited, err := s.spawnWorker(id, args)
	if err != nil {
		return fail(err)
	}
	if err := s.waitWorkerReady(id, exited); err != nil {
		return nil, err
	}
	return &session.CreateResult{
		SessionID: id, Cwd: req.Cwd, Status: session.StatusRunning, Mode: session.ModeExecute,
	}, nil
}

// spawnWorker starts a session worker in its own process session, so it is
// not tied to the daemon's lifetime or terminal, and starts its supervisor.
// The returned channel closes when the worker exits.
func (s *Server) spawnWorker(id string, args []string) (<-chan struct{}, error) {
	logFile, err := os.OpenFile(s.workerLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open worker log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(s.workerBin, args...)
	cmd.Env = append(os.Environ(), "AGENTD_DIR="+s.paths.Root)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn session worker: %w", err)
	}

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
		select {
		case <-s.shutdown:
			return
		default:
		}
		// A worker that exits cleanly has already recorded the outcome;
		// this only catches crashes and SIGKILL.
		if err := s.db.MarkFailedIfActive(id, "session worker exited unexpectedly"); err != nil {
			fmt.Fprintf(os.Stderr, "agentd: failed to record worker exit for %s: %v\n", id, err)
		}
		_ = os.Remove(s.paths.SessionSocketPath(id))
	}()
	return exited, nil
}

func (s *Server) waitWorkerReady(id string, exited <-chan struct{}) error {
	deadline := time.Now().Add(workerReadyTimeout)
	for {
		rec, err := s.db.GetSession(id)
		if err != nil {
			return err
		}
		if rec == nil {
			return errors.New("session missing after worker spawn")
		}
		switch rec.Status {
		case session.StatusRunning:
			if rec.WorkerPID != nil {
				if _, err := os.Stat(s.paths.SessionSocketPath(id)); err == nil {
					return nil
				}
			}
		case session.StatusFailed, session.StatusExited:
			if rec.Error != nil {
				return errors.New(*rec.Error)
			}
			return errors.New("session worker exited before becoming ready")
		}
		select {
		case <-exited:
			// Let the loop observe the final state the worker (or its
			// supervisor) recorded.
			exited = nil
			continue
		default:
		}
		if time.Now().After(deadline) {
			if rec.WorkerPID != nil {
				_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
			}
			_ = s.db.MarkFailedIfActive(id, "timed out waiting for session worker to start")
			return errors.New("timed out waiting for session worker to start")
		}
		time.Sleep(pollInterval)
	}
}

// killSession stops a live session through its worker (SIGTERM, which the
// worker turns into a graceful stop of the agent) and, with remove, deletes
// its record and logs.
func (s *Server) killSession(id string, remove bool) (*protocol.KillSessionResult, error) {
	rec, err := s.getSession(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("session `%s` not found", id)
	}
	wasRunning := s.alive(rec)
	if !wasRunning && !remove {
		return nil, fmt.Errorf("session `%s` is not running", id)
	}
	if wasRunning {
		if err := s.stopWorker(rec); err != nil {
			return nil, err
		}
	}
	if remove {
		for _, path := range []string{
			s.paths.LogPath(id), s.paths.RenderedLogPath(id), s.workerLogPath(id), s.paths.SessionSocketPath(id),
		} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("failed to remove %s: %w", path, err)
			}
		}
		if err := s.db.DeleteSession(id); err != nil {
			return nil, err
		}
	}
	return &protocol.KillSessionResult{Removed: remove, WasRunning: wasRunning}, nil
}

func (s *Server) stopWorker(rec *session.Record) error {
	workerPID := int(*rec.WorkerPID)
	if err := syscall.Kill(workerPID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to send SIGTERM to session `%s`: %w", rec.SessionID, err)
	}
	if waitForExit(workerPID, workerStopTimeout) {
		return nil
	}
	// The worker is wedged. Take the agent's process group down with it and
	// record the stop ourselves.
	if rec.AgentPID != nil {
		_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	}
	_ = syscall.Kill(workerPID, syscall.SIGKILL)
	if !waitForExit(workerPID, workerKillTimeout) {
		return fmt.Errorf("session `%s` did not exit after SIGTERM and SIGKILL", rec.SessionID)
	}
	return s.db.MarkExitedIfActive(rec.SessionID)
}

func (s *Server) runtimeSocket(id string) (string, error) {
	rec, err := s.getSession(id)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", fmt.Errorf("session `%s` not found", id)
	}
	if rec.Status != session.StatusRunning {
		return "", fmt.Errorf("session `%s` is not running", id)
	}
	socket := s.paths.SessionSocketPath(id)
	if _, err := os.Stat(socket); err != nil {
		return "", fmt.Errorf("session `%s` does not have a live runtime socket", id)
	}
	return socket, nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processExists(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
	return true
}
