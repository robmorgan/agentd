package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/repo"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

const (
	workerReadyTimeout = 5 * time.Second
	// workerStopTimeout covers the worker's own SIGTERM->SIGKILL grace for
	// the agent (worker.agentKillGrace, 5s) plus time to write logs and exit.
	workerStopTimeout = 8 * time.Second
	workerKillTimeout = 2 * time.Second
	workerDialTimeout = 2 * time.Second
	pollInterval      = 20 * time.Millisecond
)

var (
	nameAdjectives = []string{"brisk", "calm", "clever", "curious", "gentle", "nimble", "quiet", "steady", "swift", "wrinkly"}
	nameAnimals    = []string{"badgers", "bears", "foxes", "geckos", "otters", "pandas", "ravens", "tigers", "whales", "wolves"}
)

type liveness int

const (
	workerLive    liveness = iota
	workerGone             // nothing listens on the socket: the worker is dead
	workerUnknown          // the probe itself failed (fd exhaustion, timeout)
)

// workerState probes a session's worker by connecting to its socket. This
// is the liveness test for sessions: unlike a stored pid it cannot be fooled
// by pid reuse, and unlike the socket file existing it cannot be fooled by a
// worker that died without cleaning up. Only a refused connection or a
// missing socket counts as proof of death; any other failure is unknown, so
// a probe that fails under load never condemns a healthy session.
func (s *Server) workerState(id string) (liveness, error) {
	conn, err := transport.DialUnix(s.paths.SessionSocketPath(id), workerDialTimeout)
	switch {
	case err == nil:
		conn.Close()
		return workerLive, nil
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOENT):
		return workerGone, nil
	}
	return workerUnknown, err
}

func (s *Server) workerAnswers(id string) bool {
	state, _ := s.workerState(id)
	return state == workerLive
}

// alive reports whether a running record still has a live worker, counting
// a worker that could not be probed as live.
func (s *Server) alive(rec *session.Record) bool {
	if rec.Status != session.StatusRunning {
		return false
	}
	state, _ := s.workerState(rec.SessionID)
	return state != workerGone
}

// refresh downgrades a running record whose worker has vanished, e.g. one
// that was SIGKILLed while no daemon was supervising it.
func (s *Server) refresh(rec *session.Record) (*session.Record, error) {
	if rec.Status != session.StatusRunning {
		return rec, nil
	}
	if state, _ := s.workerState(rec.SessionID); state != workerGone {
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
			switch state, _ := s.workerState(rec.SessionID); state {
			case workerGone:
				if err := s.db.MarkUnknownRecovered(rec.SessionID); err != nil {
					return err
				}
			case workerLive:
				if err := s.db.MarkRecovered(rec.SessionID); err != nil {
					return err
				}
			}
		case session.StatusCreating:
			// The daemon that was creating it is gone. If its worker is
			// still starting, MarkRunning will now refuse it and it exits.
			if err := s.db.MarkFailedIfActive(rec.SessionID, "agentd stopped while the session was starting"); err != nil {
				return err
			}
		}
	}
	return nil
}

// allocateSession picks the session's name and inserts its row. If a
// session was already created with token, it inserts nothing and returns
// that session as replay instead.
func (s *Server) allocateSession(name *string, agent string, model *string, cwd string, workspace *string, base repo.Base, uid, token string) (id, createdAt string, replay *session.Record, err error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	if token != "" {
		rec, err := s.db.SessionByCreateToken(token)
		if err != nil || rec != nil {
			return "", "", rec, err
		}
	}

	if name != nil {
		id = *name
		existing, err := s.db.GetSession(id)
		if err != nil {
			return "", "", nil, err
		}
		if existing != nil {
			return "", "", nil, fmt.Errorf("session `%s` already exists", id)
		}
	} else {
		for _, candidate := range generatedNames() {
			existing, err := s.db.GetSession(candidate)
			if err != nil {
				return "", "", nil, err
			}
			if existing == nil {
				id = candidate
				break
			}
		}
		if id == "" {
			return "", "", nil, errors.New("failed to allocate a unique session name")
		}
	}
	createdAt, err = s.db.InsertSession(db.NewSession{
		SessionID: id, UID: uid, Agent: agent, Model: model, Mode: session.ModeExecute, Cwd: cwd, Workspace: workspace,
		CreateToken: token,
		GitBase:     base.Commit, GitBaseBranch: base.Branch,
	})
	return id, createdAt, nil, err
}

// generatedNames yields candidate names for an unnamed session: every
// adjective-animal pair in random order, then numbered variants, so that
// allocation keeps succeeding however many sessions are retained.
func generatedNames() []string {
	var names []string
	for _, a := range nameAdjectives {
		for _, b := range nameAnimals {
			names = append(names, a+"-"+b)
		}
	}
	rand.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	for range 64 {
		names = append(names, fmt.Sprintf("%s-%d", names[rand.IntN(100)], 2+rand.IntN(9998)))
	}
	return names
}

// createSession creates a session and waits for its worker. A request
// whose token already created a session (a client retrying after it lost
// its connection) is answered with that session instead; see replay.go.
func (s *Server) createSession(req *protocol.CreateSession) (*session.CreateResult, error) {
	if req.Token != "" {
		// Checked before anything else, so a retry gets the original
		// answer even if, say, the workspace it named is gone since.
		rec, err := s.db.SessionByCreateToken(req.Token)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			if req.Name != nil && strings.TrimSpace(*req.Name) != "" && strings.TrimSpace(*req.Name) != rec.SessionID {
				return nil, fmt.Errorf("request token %q was already used to create session `%s`", req.Token, rec.SessionID)
			}
			return s.awaitCreated(rec)
		}
	}
	var name *string
	if req.Name != nil {
		if trimmed := strings.TrimSpace(*req.Name); trimmed != "" {
			if !session.ValidName(trimmed) {
				return nil, fmt.Errorf("invalid session name `%s`: %s", trimmed, session.NameRules)
			}
			name = &trimmed
		}
	}
	// A client that does not know this daemon's configuration (a remote
	// `agent --host`) leaves the agent empty to get the daemon's default.
	if req.Agent == "" {
		req.Agent = s.config.DefaultAgent
	}
	agent, err := s.config.RequireAgent(req.Agent, s.paths.Config)
	if err != nil {
		return nil, err
	}
	cwd, workspace, err := s.resolveCwd(req)
	if err != nil {
		return nil, err
	}

	// What the agent produces is measured from the repository's HEAD now.
	// The daemon only reads it; it never creates branches or worktrees.
	base := probeBase(cwd)
	uid := db.NewUID()
	id, createdAt, replay, err := s.allocateSession(name, req.Agent, req.Model, cwd, workspace, base, uid, req.Token)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return s.awaitCreated(replay)
	}
	fail := func(err error) (*session.CreateResult, error) {
		_ = s.db.MarkFailedIfActive(id, err.Error())
		return nil, err
	}

	// Checked here as well as in the worker so a bad cwd is refused before
	// any process is spawned. The record is kept, failed, so `agent ls`
	// shows what happened.
	if info, err := os.Stat(cwd); err != nil {
		return fail(fmt.Errorf("working directory `%s` does not exist", cwd))
	} else if !info.IsDir() {
		return fail(fmt.Errorf("working directory `%s` is not a directory", cwd))
	}

	args := []string{
		"session-worker",
		"--session-id", id,
		"--cwd", cwd,
		"--created-at", createdAt,
		"--session-uid", uid,
		"--agent-name", req.Agent,
		"--command", agent.Command,
	}
	if req.Model != nil {
		args = append(args, "--model", *req.Model)
	}
	launchArgs := append([]string(nil), agent.Args...)
	if req.Model != nil && agent.Flag() != "" {
		launchArgs = append(launchArgs, agent.Flag(), *req.Model)
	}
	for _, a := range launchArgs {
		args = append(args, "--arg", a)
	}

	cmd, exited, err := s.spawnWorker(id, args)
	if err != nil {
		return fail(err)
	}
	err = s.waitWorkerReady(id, cmd, exited)
	s.events.notify()
	if err != nil {
		return nil, err
	}
	return &session.CreateResult{
		SessionID: id, UID: uid, Cwd: cwd, Status: session.StatusRunning, Mode: session.ModeExecute,
	}, nil
}

// awaitCreated answers a retried CreateSession with the session the first
// attempt created, waiting for it to finish starting if it still is. The
// answer is what the first attempt answered, or would have: the session if
// it started (it may have finished since), the error if it failed.
func (s *Server) awaitCreated(rec *session.Record) (*session.CreateResult, error) {
	deadline := time.Now().Add(workerReadyTimeout + time.Second)
	for rec.Status == session.StatusCreating && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		fresh, err := s.db.GetSession(rec.SessionID)
		if err != nil {
			return nil, err
		}
		if fresh == nil || fresh.UID != rec.UID {
			return nil, fmt.Errorf("session `%s` was removed while it was starting", rec.SessionID)
		}
		rec = fresh
	}
	switch rec.Status {
	case session.StatusCreating:
		return nil, errors.New("timed out waiting for session worker to start")
	case session.StatusFailed:
		if rec.Error != nil {
			return nil, errors.New(*rec.Error)
		}
		return nil, errors.New("session worker exited before becoming ready")
	}
	return &session.CreateResult{
		SessionID: rec.SessionID, UID: rec.UID, Cwd: rec.Cwd, Status: rec.Status, Mode: rec.Mode,
	}, nil
}

// spawnWorker starts a session worker in its own process session, so it is
// not tied to the daemon's lifetime or terminal, and starts its supervisor.
// The returned channel closes when the worker exits.
func (s *Server) spawnWorker(id string, args []string) (*exec.Cmd, <-chan struct{}, error) {
	logFile, err := os.OpenFile(s.paths.WorkerLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open worker log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(s.workerBin, args...)
	cmd.Env = append(os.Environ(), "AGENTD_DIR="+s.paths.Root)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to spawn session worker: %w", err)
	}

	workerPID := cmd.Process.Pid
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
		// this only catches crashes and SIGKILL. The update is keyed on
		// this worker's pid, so it cannot touch a newer session that has
		// since reused the name.
		if err := s.db.MarkWorkerLost(id, workerPID, "session worker exited unexpectedly"); err != nil {
			fmt.Fprintf(os.Stderr, "agentd: failed to record worker exit for %s: %v\n", id, err)
		}
	}()
	return cmd, exited, nil
}

func (s *Server) waitWorkerReady(id string, cmd *exec.Cmd, exited <-chan struct{}) error {
	deadline := time.Now().Add(workerReadyTimeout)
	workerGone := false
	for {
		rec, err := s.db.GetSession(id)
		if err != nil {
			return err
		}
		if rec == nil {
			_ = cmd.Process.Kill()
			return errors.New("session was removed while it was starting")
		}
		switch rec.Status {
		case session.StatusRunning:
			if s.workerAnswers(id) {
				return nil
			}
		case session.StatusFailed, session.StatusExited:
			if rec.Error != nil {
				return errors.New(*rec.Error)
			}
			return errors.New("session worker exited before becoming ready")
		}
		if workerGone {
			// The worker exited without recording a failure (it crashed).
			_ = s.db.MarkFailedIfActive(id, "session worker exited before becoming ready")
			return fmt.Errorf("session worker exited before becoming ready; see %s", s.paths.WorkerLogPath(id))
		}
		select {
		case <-exited:
			// Look at the state once more: the worker may have recorded
			// why it stopped.
			workerGone = true
			continue
		default:
		}
		if time.Now().After(deadline) {
			// Give up on this worker. Its MarkRunning would now be
			// refused anyway, but don't leave it running.
			_ = s.db.MarkFailedIfActive(id, "timed out waiting for session worker to start")
			_ = cmd.Process.Kill()
			return errors.New("timed out waiting for session worker to start")
		}
		time.Sleep(pollInterval)
	}
}

// killSession stops a live session through its worker, which stops the
// agent's process group gracefully, and with remove deletes its record and
// logs.
func (s *Server) killSession(id string, remove bool) (*protocol.KillSessionResult, error) {
	rec, err := s.getSession(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("session `%s` not found", id)
	}
	state, probeErr := s.workerState(id)
	if rec.Status == session.StatusRunning && state == workerUnknown {
		// Refuse rather than guess: removing a session whose worker may be
		// alive would orphan its agent.
		return nil, fmt.Errorf("could not reach session `%s`: %v", id, probeErr)
	}
	wasRunning := rec.Status == session.StatusRunning && state == workerLive
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
			s.paths.LogPath(id), s.paths.RenderedLogPath(id), s.paths.WorkerLogPath(id), s.paths.SessionSocketPath(id),
		} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("failed to remove %s: %w", path, err)
			}
		}
		if err := s.db.DeleteSession(id); err != nil {
			return nil, err
		}
	}
	s.events.notify()
	return &protocol.KillSessionResult{Removed: remove, WasRunning: wasRunning}, nil
}

// stopWorker asks the worker, over its socket, to stop the agent and waits
// for the session to record its end. Signals are the fallback for a wedged
// worker only, and only while its socket still answers, which proves the
// pids it recorded still belong to it.
func (s *Server) stopWorker(rec *session.Record) error {
	id := rec.SessionID
	if err := s.requestWorkerKill(id); err == nil && s.waitStopped(id, workerStopTimeout) {
		return nil
	}
	if !s.workerAnswers(id) {
		// It went away on its own (or crashed); whatever it recorded
		// stands, and a crash is marked by its supervisor or on refresh.
		_ = s.db.MarkExitedIfActive(id)
		return nil
	}
	if rec.AgentPID != nil {
		if pid := int(*rec.AgentPID); pid > 1 {
			if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}
	if rec.WorkerPID != nil {
		if pid := int(*rec.WorkerPID); pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	deadline := time.Now().Add(workerKillTimeout)
	for s.workerAnswers(id) {
		if time.Now().After(deadline) {
			return fmt.Errorf("session `%s` did not stop", id)
		}
		time.Sleep(pollInterval)
	}
	return s.db.MarkExitedIfActive(id)
}

func (s *Server) requestWorkerKill(id string) error {
	conn, err := transport.DialUnix(s.paths.SessionSocketPath(id), workerDialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	if err := protocol.WriteRequest(conn, &protocol.Request{KillSession: &protocol.KillSession{SessionID: id}}); err != nil {
		return err
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(conn))
	switch {
	case err != nil:
		return err
	case resp == nil:
		return errors.New("worker closed the connection")
	case resp.Error != nil:
		return errors.New(resp.Error.Message)
	}
	return nil
}

// waitStopped waits for a session to leave the running state and for its
// worker to stop listening. The worker records its outcome before it closes
// its socket, so waiting for the status alone would let a removed name be
// reused while the old worker is still cleaning up.
func (s *Server) waitStopped(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := s.db.GetSession(id)
		if err == nil && (rec == nil || rec.Status != session.StatusRunning) {
			if state, _ := s.workerState(id); state == workerGone {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}
