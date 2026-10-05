// Package worker runs a single agent session: it owns the PTY, the shadow
// terminal used for reattach snapshots, and the per-session Unix socket the
// daemon proxies client requests through.
//
// A worker can hand its session over to a new agentd executable without
// stopping the agent (see handoff.go): it re-executes itself in place,
// keeping its pid, the PTY and its socket, and the new image restores the
// terminal state and carries on.
//
// Goroutines, and what ends them:
//   - The main goroutine (run): waits for the session to end or for a
//     handoff request, which it carries out itself.
//   - The owner goroutine: all session state (owner.go). Stopped last.
//   - The pump: reads the PTY (pump.go). Ends at EOF, or when stopped by a
//     handoff or shutdown.
//   - The agent watcher: waits for the agent to exit and reaps it, unless
//     a handoff holds reapMu.
//   - The input writer (input.go), the accept loop, and one handler per
//     connection.
package worker

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

const (
	defaultPtyRows      uint16 = 48
	defaultPtyCols      uint16 = 160
	maxScrollbackBytes  uint   = 10_000_000
	shutdownGrace              = 2 * time.Second
	firstRequestTimeout        = 30 * time.Second
	pumpDrainTimeout           = 250 * time.Millisecond
)

// agentKillGrace is how long a killed agent gets between SIGTERM and SIGKILL.
// A variable so tests can shorten it.
var agentKillGrace = 5 * time.Second

type Args struct {
	SessionID string
	// Cwd is where the agent process is started. It must exist; the worker
	// does not create it and does not care whether it is a git checkout.
	Cwd string
	// CreatedAt identifies the incarnation of the session this worker was
	// started for (see db.InsertSession).
	CreatedAt string
	// UID is that incarnation's UID (session.Record.UID). Attach streams
	// that expect another incarnation are refused, and attach ids are
	// numbered per UID.
	UID       string
	AgentName string
	Command   string
	Model     string
	Args      []string
}

type sessionEnded struct {
	status   session.Status
	exitCode *int32
	err      *string
}

type endedSignal struct {
	ch   chan struct{}
	once sync.Once
	info *sessionEnded
}

func (e *endedSignal) fire(info *sessionEnded) {
	e.once.Do(func() {
		e.info = info
		close(e.ch)
	})
}

type runtime struct {
	args      Args
	sessionID string
	uid       string
	paths     *paths.AppPaths
	db        *db.Database
	// attachSeqNext..attachSeqLast are the attachment numbers this worker
	// has reserved and not yet used (nextAttachSeq); zero before the first
	// reservation. Guarded by attachSeqMu, since attaches are served
	// concurrently.
	attachSeqMu                  sync.Mutex
	attachSeqNext, attachSeqLast uint64
	owner                        *owner
	ended                        *endedSignal
	// handlers counts connection handlers, so shutdown and handoffs can
	// wait for them.
	handlers tracker
	// agentPID leads the agent's process group (pty.Start puts the agent in
	// its own session). The agent is this process's child, across handoffs
	// too: an exec keeps the pid.
	agentPID int
	// killed is set once a kill has been requested, so the exit is recorded
	// as a deliberate stop rather than a failure.
	killed atomic.Bool

	ptmx       *os.File
	socketPath string
	ownSocket  os.FileInfo

	// listener, acceptDone and pump belong to the main goroutine, which
	// stops and restarts them around a handoff.
	listener   *transport.UnixListener
	acceptDone chan struct{}
	pump       *pump
	// ptyEOF is closed once the PTY reports end of file.
	ptyEOF chan struct{}

	// handoffs carries handoff requests from connection handlers to the
	// main goroutine. One may wait; a second is refused.
	handoffs chan *handoffRequest
	// handoffCount counts the handoffs this session's worker has been
	// through, across images.
	handoffCount uint32
	// reapMu keeps the agent from being reaped while a handoff is under
	// way: the new image must find it unreaped to collect its exit status.
	// The agent watcher holds it while reaping; a handoff holds it from its
	// start until the exec, or until it gives up.
	reapMu sync.Mutex
	// agentExited is set by the watcher once the agent has exited (before
	// it is reaped).
	agentExited atomic.Bool
	// noHandoff, when set, says why this worker cannot hand off.
	noHandoff atomic.Pointer[string]
	// startedAt is when the worker started (the first image, across
	// handoffs), for its uptime in stats.
	startedAt time.Time
	// recorder writes events and activity to state.db (activity.go).
	recorder *recorder
}

// terminateAgent asks the agent's whole process group to exit and escalates
// to SIGKILL after agentKillGrace. The normal child-exit path then writes the
// logs, records the final state, and ends every attachment. Safe to call more
// than once and from any goroutine.
func (rt *runtime) terminateAgent() {
	if !rt.killed.CompareAndSwap(false, true) {
		return
	}
	signalGroup(rt.agentPID, syscall.SIGTERM)
	grace := agentKillGrace
	go func() {
		select {
		case <-rt.ended.ch:
		case <-time.After(grace):
			signalGroup(rt.agentPID, syscall.SIGKILL)
		}
	}()
}

// ptyHint explains running out of pseudo-terminals, which caps how many
// sessions a machine can run: macOS allows kern.tty.ptmx_max of them (511
// by default, for all programs together), Linux kernel.pty.max (4096).
func ptyHint(err error) string {
	if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENOSPC) {
		return " (the system may have run out of pseudo-terminals: see sysctl kern.tty.ptmx_max on macOS or kernel.pty.max on Linux)"
	}
	return ""
}

func signalGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// Run starts a session: it spawns the agent in a new PTY and serves the
// session until the agent exits.
func Run(args Args) error {
	startedAt := time.Now()
	p, err := paths.Discover()
	if err != nil {
		return err
	}
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	if args.UID == "" {
		return errors.New("session worker needs the session's UID")
	}
	store, err := db.Open(p.Database)
	if err != nil {
		return err
	}
	// Until the agent is running, record any failure on the session so the
	// daemon (and `agent ls`) can report it rather than timing out.
	fail := func(err error) error {
		_ = store.MarkFailed(args.SessionID, err.Error())
		return err
	}
	socketPath := p.SessionSocketPath(args.SessionID)
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("failed to remove %s: %w", socketPath, err))
	}

	if info, err := os.Stat(args.Cwd); err != nil {
		return fail(fmt.Errorf("working directory `%s` does not exist", args.Cwd))
	} else if !info.IsDir() {
		return fail(fmt.Errorf("working directory `%s` is not a directory", args.Cwd))
	}
	cmd := exec.Command(args.Command, args.Args...)
	cmd.Dir = args.Cwd
	cmd.Env = append(os.Environ(),
		"AGENTD_SESSION_ID="+args.SessionID,
		"AGENTD_SESSION_NAME="+args.SessionID,
		"AGENTD_CWD="+args.Cwd,
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: defaultPtyRows, Cols: defaultPtyCols})
	if err != nil {
		return fail(fmt.Errorf("failed to spawn agent process: %w%s", err, ptyHint(err)))
	}
	defer ptmx.Close()
	agentPID := cmd.Process.Pid
	// The agent watcher reaps the agent with wait4 rather than through
	// cmd.Wait (see watchAgent).
	_ = cmd.Process.Release()

	terminal, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
	if err != nil {
		signalGroup(agentPID, syscall.SIGKILL)
		return fail(err)
	}
	defer func() { terminal.close() }()

	// Unlink the socket ourselves, and only while the path is still ours:
	// after the session is removed its name may be reused, and a new
	// worker's socket may already sit at the same path.
	listener, err := transport.ListenUnix(socketPath, transport.UnixOptions{KeepSocketOnClose: true})
	if err != nil {
		signalGroup(agentPID, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to bind worker socket: %w", err))
	}
	ownSocket, err := os.Stat(socketPath)
	if err != nil {
		listener.Close()
		signalGroup(agentPID, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to stat worker socket: %w", err))
	}
	rt := newRuntime(args, p, store, agentPID, ptmx, socketPath, ownSocket, listener)
	rt.startedAt = startedAt
	if err := store.MarkRunning(args.SessionID, args.CreatedAt, os.Getpid(), agentPID); err != nil {
		listener.Close()
		rt.removeOwnSocket()
		signalGroup(agentPID, syscall.SIGKILL)
		if errors.Is(err, db.ErrNotCreating) {
			// The daemon gave up on this worker or the session was
			// removed while it started; the row is not ours to touch.
			return fmt.Errorf("session %s: %w", args.SessionID, err)
		}
		return fail(err)
	}

	state := newOwnerState(args.SessionID, ptmx, terminal)
	state.activity = newActivityTracker(rt.recorder, agentPID, time.Now())
	state.owner = rt.owner
	return rt.run(state, nil)
}

func newRuntime(args Args, p *paths.AppPaths, store *db.Database, agentPID int, ptmx *os.File,
	socketPath string, ownSocket os.FileInfo, listener *transport.UnixListener) *runtime {
	return &runtime{
		args:       args,
		sessionID:  args.SessionID,
		uid:        args.UID,
		paths:      p,
		db:         store,
		owner:      newOwner(),
		ended:      &endedSignal{ch: make(chan struct{})},
		agentPID:   agentPID,
		ptmx:       ptmx,
		socketPath: socketPath,
		ownSocket:  ownSocket,
		listener:   listener,
		ptyEOF:     make(chan struct{}),
		handoffs:   make(chan *handoffRequest, 1),
		recorder:   newRecorder(store, args.SessionID, os.Getpid()),
	}
}

func newOwnerState(sessionID string, ptmx *os.File, terminal *terminalState) *ownerState {
	return &ownerState{
		sessionID:   sessionID,
		ptmx:        ptmx,
		terminal:    terminal,
		geometry:    protocol.Geometry{Cols: defaultPtyCols, Rows: defaultPtyRows},
		attachments: make(map[string]*ownerAttachment),
		output:      newBroadcaster(),
		input:       newPTYInput(ptmx),
		restart:     make(chan struct{}),
	}
}

func (rt *runtime) removeOwnSocket() {
	if cur, err := os.Stat(rt.socketPath); err == nil && os.SameFile(cur, rt.ownSocket) {
		_ = os.Remove(rt.socketPath)
	}
}

// run serves the session until the agent exits, carrying out handoffs on
// the way. ready, if set, runs once everything is serving. It returns only
// when the session has ended (a successful handoff never returns: the
// process is the new image by then).
func (rt *runtime) run(state *ownerState, ready func()) error {
	// SIGTERM and SIGINT stop the session like a kill request.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			rt.terminateAgent()
		case <-rt.ended.ch:
		}
	}()

	ownerDone := make(chan struct{})
	go func() {
		rt.owner.run(state)
		close(ownerDone)
	}()
	if err := rt.startPump(); err != nil {
		// Without a pump the agent's output would never be read; stop
		// the session rather than leave it hanging.
		fmt.Fprintf(os.Stderr, "session worker: %v\n", err)
		rt.terminateAgent()
	}
	go rt.watchAgent()
	// Drives activity detection (idleness, stalls, the foreground
	// process) until the session ends.
	go func() {
		ticker := time.NewTicker(activityTick)
		defer ticker.Stop()
		for {
			select {
			case <-rt.ended.ch:
				return
			case <-ticker.C:
				rt.owner.post(func(s *ownerState) { s.tickActivity() })
			}
		}
	}()
	// Reserve the first block of attachment numbers now, so even the first
	// attach does not wait on a database write. A failure here is retried
	// by that attach.
	if err := rt.reserveAttachSeq(); err != nil {
		fmt.Fprintf(os.Stderr, "session worker: reserving attachment numbers: %v\n", err)
	}
	rt.startAccepting()
	if ready != nil {
		ready()
	}

	for done := false; !done; {
		select {
		case <-rt.ended.ch:
			done = true
		case req := <-rt.handoffs:
			// Returns only if the handoff did not happen.
			rt.handoff(req, state)
		}
	}

	rt.listener.Close()
	// Every handler Serve started is counted in rt.handlers once it
	// returns.
	<-rt.acceptDone
	rt.removeOwnSocket()
	// A handoff requested meanwhile will not happen.
	select {
	case req := <-rt.handoffs:
		req.refuse(errors.New("the session has ended"))
	default:
	}

	// Give attached clients a moment to receive their SessionEnded frame.
	rt.handlers.waitIdle(shutdownGrace)
	rt.owner.stop()
	<-ownerDone
	if rt.pump != nil {
		rt.pump.stop()
	}
	// Only the owner goroutine enqueues input, so the queue can close now.
	state.input.close()
	return nil
}

func (rt *runtime) startPump() error {
	p, err := startPump(rt.ptmx, rt.owner, rt.ptyEOF)
	if err != nil {
		return err
	}
	rt.pump = p
	return nil
}

func (rt *runtime) startAccepting() {
	done := make(chan struct{})
	rt.acceptDone = done
	l := rt.listener
	go func() {
		defer close(done)
		transport.Serve(l, "session worker", &rt.handlers, func(conn transport.Stream) {
			keep, err := rt.handleConnection(conn)
			if err != nil {
				fmt.Fprintf(os.Stderr, "session worker connection error: %v\n", err)
			}
			if !keep {
				conn.Close()
			}
		})
	}()
}

// watchAgent waits for the agent to exit, reaps it, and ends the session.
//
// It waits without reaping first and then reaps under reapMu, so a handoff
// in progress (which holds reapMu) either sees that the agent exited and
// gives up, or execs with the agent still unreaped, for the new image to
// reap. The exit status is never lost in between.
func (rt *runtime) watchAgent() {
	coordinated := true
	if err := waitExited(rt.agentPID); err != nil {
		reason := fmt.Sprintf("cannot watch the agent without reaping it: %v", err)
		fmt.Fprintf(os.Stderr, "session worker: %s\n", reason)
		rt.noHandoff.Store(&reason)
		coordinated = false
	}
	rt.agentExited.Store(true)
	if coordinated {
		rt.reapMu.Lock()
	}
	code := reapAgent(rt.agentPID)
	if coordinated {
		rt.reapMu.Unlock()
	}
	// Let the pump queue the agent's last output before the exit is
	// handled, so the saved history and SessionEnded include it. The pump
	// ends once nothing holds the PTY open; a lingering descendant may
	// keep it open, hence the bound.
	select {
	case <-rt.ptyEOF:
	case <-time.After(pumpDrainTimeout):
	}
	rt.owner.post(func(s *ownerState) { rt.onChildExited(s, code) })
}

// reapAgent collects the agent's exit status: its exit code, or nil if it
// was killed by a signal (or could not be reaped).
func reapAgent(pid int) *int32 {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "session worker: failed to reap the agent: %v\n", err)
			return nil
		}
		break
	}
	if !ws.Exited() {
		return nil
	}
	code := int32(ws.ExitStatus())
	return &code
}

func (rt *runtime) onChildExited(s *ownerState, exitCode *int32) {
	if rt.killed.Load() {
		// The agent exited, but a descendant that ignored SIGTERM may
		// still be running (and changing the working directory). A kill
		// stops the whole process group, so take the rest down now. The
		// group id cannot be reused while any member is alive.
		_ = syscall.Kill(-rt.agentPID, syscall.SIGKILL)
	}
	plain, _ := s.history(false)
	vt, _ := s.history(true)
	if err := os.WriteFile(rt.paths.RenderedLogPath(rt.sessionID), []byte(plain), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write rendered history for %s: %v\n", rt.sessionID, err)
	}
	if err := os.WriteFile(rt.paths.LogPath(rt.sessionID), []byte(vt), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write VT history for %s: %v\n", rt.sessionID, err)
	}
	// Events the agent caused before exiting come before its end.
	s.activity = nil
	rt.recorder.close()
	if err := rt.finalizeExit(exitCode); err != nil {
		fmt.Fprintf(os.Stderr, "failed to finalize %s: %v\n", rt.sessionID, err)
	}
	rec, err := rt.db.GetSession(rt.sessionID)
	if err != nil || rec == nil {
		fmt.Fprintf(os.Stderr, "missing session after worker exit: %v\n", err)
		rt.ended.fire(nil)
		return
	}
	rt.ended.fire(&sessionEnded{status: rec.Status, exitCode: rec.ExitCode, err: rec.Error})
}

func (rt *runtime) finalizeExit(exitCode *int32) error {
	rec, err := rt.db.GetSession(rt.sessionID)
	if err != nil || rec == nil {
		return err
	}
	if rec.Status == session.StatusFailed {
		return nil
	}
	if rt.killed.Load() {
		return rt.db.MarkKilled(rt.sessionID, exitCode)
	}
	if exitCode != nil && *exitCode == 0 {
		return rt.db.MarkExited(rt.sessionID, exitCode)
	}
	msg := "agent exited unexpectedly"
	if exitCode != nil {
		msg = fmt.Sprintf("agent exited with code %d", *exitCode)
	}
	return rt.db.MarkFailed(rt.sessionID, msg)
}

func (rt *runtime) endedResponse() *protocol.Response {
	info := rt.ended.info
	if info == nil {
		return protocol.EndOfStreamResponse()
	}
	return &protocol.Response{SessionEnded: &protocol.SessionEnded{
		SessionID: rt.sessionID,
		Status:    info.status,
		ExitCode:  info.exitCode,
		Error:     info.err,
	}}
}

// handleConnection serves one connection. keep reports that the connection
// was handed to someone else (a handoff request, answered by the main
// goroutine or the next image), so the caller must not close it.
func (rt *runtime) handleConnection(conn transport.Stream) (keep bool, err error) {
	reader := bufio.NewReader(conn)
	// A peer that connects and never sends a request must not hold a
	// handler forever.
	conn.SetReadDeadline(time.Now().Add(firstRequestTimeout))
	req, err := protocol.ReadRequest(reader)
	conn.SetReadDeadline(time.Time{})
	if err != nil || req == nil {
		return false, err
	}

	reply := func(resp *protocol.Response) error { return protocol.WriteResponse(conn, resp) }
	fail := func(err error) error {
		if err == nil {
			return nil
		}
		_ = reply(protocol.ErrorResponsef("%v", err))
		return err
	}

	switch {
	case req.Hello != nil:
		// The daemon asks which features this worker (which may run an
		// older binary than the daemon) supports: the attach-stream
		// features, before it forwards an AttachSession that lists some,
		// and live handoff, before an upgrade.
		version, err := protocol.NegotiateVersion(req.Hello.MinVersion, req.Hello.MaxVersion)
		if err != nil {
			return false, fail(err)
		}
		return false, reply(&protocol.Response{Welcome: &protocol.Welcome{
			Version: version, Capabilities: append(protocol.AttachCapabilities(), protocol.CapWorkerHandoff),
		}})
	case req.HandoffSession != nil:
		h := req.HandoffSession
		if h.SessionID != rt.sessionID {
			return false, fail(fmt.Errorf("this worker runs session `%s`, not `%s`", rt.sessionID, h.SessionID))
		}
		select {
		case rt.handoffs <- &handoffRequest{conn: conn, exe: h.Executable}:
			return true, nil
		default:
			return false, fail(errors.New("a handoff is already in progress"))
		}
	case req.AttachSession != nil:
		return false, rt.serveAttach(conn, reader, req.AttachSession)
	case req.SendInput != nil, req.AttachInput != nil:
		data := []byte(nil)
		if req.SendInput != nil {
			data = req.SendInput.Data
		} else {
			data = req.AttachInput.Data
		}
		if err := rt.owner.do(func(s *ownerState) error { return s.writeInput(data) }); err != nil {
			return false, fail(err)
		}
		return false, reply(&protocol.Response{InputAccepted: protocol.Empty})
	case req.ListAttachments != nil:
		var attachments []session.AttachmentRecord
		_ = rt.owner.do(func(s *ownerState) error { attachments = s.listAttachments(); return nil })
		return false, reply(&protocol.Response{Attachments: &attachments})
	case req.DetachAttachment != nil:
		id := req.DetachAttachment.AttachID
		if err := rt.owner.do(func(s *ownerState) error { return s.detachAttachment(id) }); err != nil {
			return false, fail(err)
		}
		return false, reply(protocol.OkResponse())
	case req.DetachSession != nil:
		if !req.DetachSession.All {
			return false, fail(errors.New("worker detach requires all=true"))
		}
		_ = rt.owner.do(func(s *ownerState) error { s.detachAll(); return nil })
		return false, reply(protocol.OkResponse())
	case req.GetHistory != nil:
		vt := req.GetHistory.VT
		var data string
		if err := rt.owner.do(func(s *ownerState) error {
			var err error
			data, err = s.history(vt)
			return err
		}); err != nil {
			return false, fail(err)
		}
		return false, reply(&protocol.Response{History: &protocol.History{Data: data}})
	case req.KillSession != nil:
		rt.terminateAgent()
		return false, reply(protocol.OkResponse())
	case req.GetSessionStats != nil:
		stats, err := rt.stats(req.GetSessionStats.Snapshot)
		if err != nil {
			return false, fail(err)
		}
		return false, protocol.WriteResponseWith(conn, protocol.OwnFeatures(), &protocol.Response{SessionStats: stats})
	default:
		return false, reply(protocol.ErrorResponsef("unsupported worker request"))
	}
}

// attachIDBlock is how many attachment numbers a worker reserves in
// state.db at once (db.ReserveAttachIDs), so attaching rarely waits on a
// database write.
const attachIDBlock = 16

// nextAttachSeq numbers a new attachment from the worker's reserved block,
// reserving another when it runs out.
func (rt *runtime) nextAttachSeq() (uint64, error) {
	rt.attachSeqMu.Lock()
	defer rt.attachSeqMu.Unlock()
	if err := rt.reserveAttachSeqLocked(); err != nil {
		return 0, err
	}
	seq := rt.attachSeqNext
	rt.attachSeqNext++
	return seq, nil
}

// reserveAttachSeq makes sure a block is reserved, without using a number
// from it.
func (rt *runtime) reserveAttachSeq() error {
	rt.attachSeqMu.Lock()
	defer rt.attachSeqMu.Unlock()
	return rt.reserveAttachSeqLocked()
}

func (rt *runtime) reserveAttachSeqLocked() error {
	if rt.attachSeqNext != 0 && rt.attachSeqNext <= rt.attachSeqLast {
		return nil
	}
	first, err := rt.db.ReserveAttachIDs(rt.sessionID, rt.uid, attachIDBlock)
	if err != nil {
		return err
	}
	rt.attachSeqNext, rt.attachSeqLast = first, first+attachIDBlock-1
	return nil
}

func (rt *runtime) serveAttach(conn transport.Stream, reader *bufio.Reader, req *protocol.AttachSession) error {
	refuse := func(err error) error {
		_ = protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
		return err
	}
	features := protocol.IntersectCapabilities(req.Features, protocol.AttachCapabilities())
	expect := ""
	if slices.Contains(features, protocol.CapSessionUID) {
		expect = req.ExpectUID
	}
	if expect != "" && expect != rt.uid {
		return refuse(errors.New(protocol.SessionReplacedMessage(rt.sessionID)))
	}
	// A replaced id is only meaningful within the incarnation the client
	// knew, which ExpectUID has just confirmed.
	replaces := ""
	if expect != "" && slices.Contains(features, protocol.CapAttachReplace) {
		replaces = req.Replaces
	}
	resync := slices.Contains(features, protocol.CapAttachResync)
	// A client that asked for CapSessionRestart is told when the worker
	// restarts, and attaches again; any other is simply detached.
	canRestart := slices.Contains(features, protocol.CapSessionRestart)
	// With CapAttachScrollback the client caps the scrollback in its first
	// snapshot, and the snapshots that repaint its screen later carry none:
	// its terminal already holds what scrolled off before them.
	initial, repaint := allScrollback, allScrollback
	if slices.Contains(features, protocol.CapAttachScrollback) {
		initial, repaint = scrollbackRows(req.ScrollbackRows), 0
	}

	seq, err := rt.nextAttachSeq()
	if errors.Is(err, db.ErrNoSuchIncarnation) {
		return refuse(fmt.Errorf("session `%s` is being removed", rt.sessionID))
	} else if err != nil {
		return refuse(fmt.Errorf("failed to number the attachment: %w", err))
	}
	var att *attachResult
	if err := rt.owner.doAtGround(func(s *ownerState) error {
		var err error
		att, err = s.attach(fmt.Sprintf("%s-%d", req.Kind, seq), req.Kind, req.Geometry, replaces, initial)
		return err
	}); err != nil {
		return refuse(err)
	}
	defer rt.owner.post(func(s *ownerState) {
		s.output.unsubscribe(att.sub)
		s.removeAttachment(att.attachID)
	})

	// A client that stops reading blocks this handler in a write, where it
	// cannot notice a detach, a restart or the session ending. Once one
	// happens, give the client shutdownGrace to take its final frames, then
	// fail the blocked write so the attachment is released.
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-att.detach:
		case <-att.restart:
		case <-rt.ended.ch:
		case <-handlerDone:
			return
		}
		conn.SetWriteDeadline(time.Now().Add(shutdownGrace))
	}()

	attached := &protocol.Attached{AttachID: att.attachID, Snapshot: att.snapshot, Features: features}
	if slices.Contains(features, protocol.CapSessionUID) {
		attached.SessionUID = rt.uid
	}
	if err := protocol.WriteResponse(conn, &protocol.Response{Attached: attached}); err != nil {
		return err
	}

	type incoming struct {
		req *protocol.Request
		err error
	}
	// The reader goroutine exits when conn is closed (handleConnection's
	// deferred Close) or the peer hangs up; stop keeps it from blocking on
	// requests once this loop has returned.
	requests := make(chan incoming, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			req, err := protocol.ReadRequest(reader)
			select {
			case requests <- incoming{req, err}:
			case <-stop:
				return
			}
			if err != nil || req == nil {
				return
			}
		}
	}()

	final := protocol.EndOfStreamResponse()
loop:
	for {
		select {
		case data := <-att.sub.ch:
			att.sub.took(data)
			// Output was dropped while this client lagged, so what is
			// queued no longer continues its screen. Getting here means
			// the previous write went through: the client is draining,
			// and a snapshot taken now is as fresh as it can use.
			if resync && att.sub.lagged.Load() {
				if err := rt.sendSnapshot(conn, att.sub, true, repaint); err != nil {
					return err
				}
				continue
			}
			if err := protocol.WriteResponse(conn, &protocol.Response{PtyOutput: &protocol.Bytes{Data: data}}); err != nil {
				return err
			}
		case <-att.detach:
			break loop
		case <-att.restart:
			// The client repaints from a fresh snapshot when it attaches
			// again, but flush what it was sent so far anyway.
			if err := drainOutput(conn, att.sub); err != nil {
				return err
			}
			if canRestart {
				final = &protocol.Response{SessionRestarting: &protocol.SessionRestarting{
					SessionID: rt.sessionID, Reason: "the session worker is restarting",
				}}
			}
			break loop
		case <-rt.ended.ch:
			// Flush output already queued for this client so the tail of
			// the session is not lost behind SessionEnded; a client that
			// lagged gets the final screen instead.
			if resync && att.sub.lagged.Load() {
				if err := rt.sendSnapshot(conn, att.sub, true, repaint); err != nil {
					return err
				}
			} else if err := drainOutput(conn, att.sub); err != nil {
				return err
			}
			final = rt.endedResponse()
			break loop
		case in := <-requests:
			if in.err != nil {
				return in.err
			}
			if in.req == nil {
				break loop
			}
			switch {
			case in.req.AttachResize != nil:
				ng := *in.req.AttachResize
				if err := rt.owner.do(func(s *ownerState) error { return s.resize(ng) }); err != nil {
					return err
				}
			case in.req.AttachInput != nil:
				data := in.req.AttachInput.Data
				if err := rt.owner.do(func(s *ownerState) error { return s.writeInput(data) }); err != nil {
					if errors.Is(err, errOwnerStopped) {
						return err
					}
					// The agent has stopped reading its input. Dropping
					// keystrokes silently would be worse than ending the
					// attachment with the reason.
					final = protocol.ErrorResponsef("%v", err)
					break loop
				}
			case in.req.AttachSnapshot != nil:
				if err := rt.sendSnapshot(conn, att.sub, false, repaint); err != nil {
					return err
				}
			default:
				final = protocol.ErrorResponsef("unexpected request during attach")
				break loop
			}
		}
	}

	return protocol.WriteResponse(conn, final)
}

// sendSnapshot sends the client a snapshot of the screen: AttachSnapshot
// when it asked for one, AttachResync when it lagged. Output is published
// on the owner goroutine, so discarding what is queued for this client and
// taking the snapshot there makes the snapshot an exact boundary: everything
// before it is in the snapshot, everything after follows it. doAtGround
// keeps that boundary outside escape sequences. The snapshot has up to
// scrollback rows of scrollback (allScrollback for all).
func (rt *runtime) sendSnapshot(conn transport.Stream, sub *subscriber, resync bool, scrollback int) error {
	var snapshot []byte
	if err := rt.owner.doAtGround(func(s *ownerState) error {
		sub.discard()
		var err error
		snapshot, err = s.snapshot(scrollback)
		return err
	}); err != nil {
		return err
	}
	resp := &protocol.Response{AttachSnapshot: &protocol.Bytes{Data: snapshot}}
	if resync {
		resp = &protocol.Response{AttachResync: &protocol.Bytes{Data: snapshot}}
	}
	return protocol.WriteResponse(conn, resp)
}

// scrollbackRows converts AttachSession.ScrollbackRows to snapshot's
// argument.
func scrollbackRows(rows uint32) int {
	if rows == protocol.AllScrollbackRows {
		return allScrollback
	}
	return int(rows)
}

func drainOutput(conn transport.Stream, sub *subscriber) error {
	for {
		data, ok := sub.next()
		if !ok {
			return nil
		}
		if err := protocol.WriteResponse(conn, &protocol.Response{PtyOutput: &protocol.Bytes{Data: data}}); err != nil {
			return err
		}
	}
}

// tracker counts running connection handlers (a transport.Counter) and lets
// the main goroutine wait, with a bound, for there to be none. Unlike a
// sync.WaitGroup it can be waited on again after a wait timed out, which a
// handoff that gives up needs.
type tracker struct {
	mu sync.Mutex
	n  int
	// idle is closed while n is zero; nil means "not created yet".
	idle chan struct{}
}

func (t *tracker) Add(delta int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == 0 && delta > 0 {
		t.idle = make(chan struct{})
	}
	t.n += delta
	if t.n == 0 && t.idle != nil {
		close(t.idle)
	}
}

func (t *tracker) Done() { t.Add(-1) }

// waitIdle waits up to timeout for every handler to return and reports
// whether they did.
func (t *tracker) waitIdle(timeout time.Duration) bool {
	t.mu.Lock()
	if t.n == 0 {
		t.mu.Unlock()
		return true
	}
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-time.After(timeout):
		return false
	}
}
