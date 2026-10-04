// Package worker runs a single agent session: it owns the PTY, the shadow
// terminal used for reattach snapshots, and the per-session Unix socket the
// daemon proxies client requests through.
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
	sessionID string
	uid       string
	paths     *paths.AppPaths
	db        *db.Database
	owner     *owner
	ended     *endedSignal
	conns     sync.WaitGroup
	// agentPID leads the agent's process group (pty.Start puts the agent in
	// its own session).
	agentPID int
	// killed is set once a kill has been requested, so the exit is recorded
	// as a deliberate stop rather than a failure.
	killed atomic.Bool
	// startedAt is when the worker started, for its uptime in stats.
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

	terminal, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
	if err != nil {
		_ = cmd.Process.Kill()
		return fail(err)
	}
	defer terminal.close()

	// Unlink the socket ourselves, and only while the path is still ours:
	// after the session is removed its name may be reused, and a new
	// worker's socket may already sit at the same path.
	listener, err := transport.ListenUnix(socketPath, transport.UnixOptions{KeepSocketOnClose: true})
	if err != nil {
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to bind worker socket: %w", err))
	}
	ownSocket, err := os.Stat(socketPath)
	if err != nil {
		listener.Close()
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to stat worker socket: %w", err))
	}
	removeOwnSocket := func() {
		if cur, err := os.Stat(socketPath); err == nil && os.SameFile(cur, ownSocket) {
			_ = os.Remove(socketPath)
		}
	}
	if err := store.MarkRunning(args.SessionID, args.CreatedAt, os.Getpid(), cmd.Process.Pid); err != nil {
		listener.Close()
		removeOwnSocket()
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, db.ErrNotCreating) {
			// The daemon gave up on this worker or the session was
			// removed while it started; the row is not ours to touch.
			return fmt.Errorf("session %s: %w", args.SessionID, err)
		}
		return fail(err)
	}

	rt := &runtime{
		sessionID: args.SessionID,
		uid:       args.UID,
		paths:     p,
		db:        store,
		owner:     newOwner(),
		ended:     &endedSignal{ch: make(chan struct{})},
		agentPID:  cmd.Process.Pid,
		startedAt: startedAt,
		recorder:  newRecorder(store, args.SessionID, os.Getpid()),
	}

	// The daemon stops a session by sending the worker SIGTERM.
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

	state := &ownerState{
		sessionID:   args.SessionID,
		ptmx:        ptmx,
		terminal:    terminal,
		geometry:    protocol.Geometry{Cols: defaultPtyCols, Rows: defaultPtyRows},
		attachments: make(map[string]*ownerAttachment),
		output:      newBroadcaster(),
		input:       newPTYInput(ptmx),
		activity:    newActivityTracker(rt.recorder, cmd.Process.Pid, time.Now()),
	}

	ownerDone := make(chan struct{})
	go func() {
		rt.owner.run(state)
		close(ownerDone)
	}()
	pumpDone := make(chan struct{})
	go func() {
		pumpPty(ptmx, rt.owner)
		close(pumpDone)
	}()
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
	go func() {
		err := cmd.Wait()
		// Let the pump queue the agent's last output before the exit is
		// handled, so the saved history and SessionEnded include it. The
		// pump ends once nothing holds the PTY open; a lingering
		// descendant may keep it open, hence the bound.
		select {
		case <-pumpDone:
		case <-time.After(pumpDrainTimeout):
		}
		var code *int32
		if err == nil {
			zero := int32(0)
			code = &zero
		} else if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() >= 0 {
			c := int32(exitErr.ExitCode())
			code = &c
		}
		rt.owner.post(func(s *ownerState) { rt.onChildExited(s, code) })
	}()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		transport.Serve(listener, "session worker", &rt.conns, func(conn transport.Stream) {
			defer conn.Close()
			if err := rt.handleConnection(conn); err != nil {
				fmt.Fprintf(os.Stderr, "session worker connection error: %v\n", err)
			}
		})
	}()

	<-rt.ended.ch
	listener.Close()
	// Every handler Serve started is counted in rt.conns once it returns.
	<-acceptDone
	removeOwnSocket()

	// Give attached clients a moment to receive their SessionEnded frame.
	finished := make(chan struct{})
	go func() {
		rt.conns.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(shutdownGrace):
	}
	rt.owner.stop()
	<-ownerDone
	// Only the owner goroutine enqueues input, so the queue can close now.
	state.input.close()
	return nil
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

func (rt *runtime) handleConnection(conn transport.Stream) error {
	reader := bufio.NewReader(conn)
	// A peer that connects and never sends a request must not hold a
	// handler forever.
	conn.SetReadDeadline(time.Now().Add(firstRequestTimeout))
	req, err := protocol.ReadRequest(reader)
	conn.SetReadDeadline(time.Time{})
	if err != nil || req == nil {
		return err
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
		// The daemon asks which attach-stream features this worker
		// supports before it forwards an AttachSession that lists some: a
		// worker started by an older daemon build may support fewer.
		version, err := protocol.NegotiateVersion(req.Hello.MinVersion, req.Hello.MaxVersion)
		if err != nil {
			return fail(err)
		}
		return reply(&protocol.Response{Welcome: &protocol.Welcome{
			Version: version, Capabilities: protocol.AttachCapabilities(),
		}})
	case req.AttachSession != nil:
		return rt.serveAttach(conn, reader, req.AttachSession)
	case req.SendInput != nil, req.AttachInput != nil:
		data := []byte(nil)
		if req.SendInput != nil {
			data = req.SendInput.Data
		} else {
			data = req.AttachInput.Data
		}
		if err := rt.owner.do(func(s *ownerState) error { return s.writeInput(data) }); err != nil {
			return fail(err)
		}
		return reply(&protocol.Response{InputAccepted: protocol.Empty})
	case req.ListAttachments != nil:
		var attachments []session.AttachmentRecord
		_ = rt.owner.do(func(s *ownerState) error { attachments = s.listAttachments(); return nil })
		return reply(&protocol.Response{Attachments: &attachments})
	case req.DetachAttachment != nil:
		id := req.DetachAttachment.AttachID
		if err := rt.owner.do(func(s *ownerState) error { return s.detachAttachment(id) }); err != nil {
			return fail(err)
		}
		return reply(protocol.OkResponse())
	case req.DetachSession != nil:
		if !req.DetachSession.All {
			return fail(errors.New("worker detach requires all=true"))
		}
		_ = rt.owner.do(func(s *ownerState) error { s.detachAll(); return nil })
		return reply(protocol.OkResponse())
	case req.GetHistory != nil:
		vt := req.GetHistory.VT
		var data string
		if err := rt.owner.do(func(s *ownerState) error {
			var err error
			data, err = s.history(vt)
			return err
		}); err != nil {
			return fail(err)
		}
		return reply(&protocol.Response{History: &protocol.History{Data: data}})
	case req.KillSession != nil:
		rt.terminateAgent()
		return reply(protocol.OkResponse())
	case req.GetSessionStats != nil:
		stats, err := rt.stats(req.GetSessionStats.Snapshot)
		if err != nil {
			return fail(err)
		}
		return reply(&protocol.Response{SessionStats: stats})
	default:
		return reply(protocol.ErrorResponsef("unsupported worker request"))
	}
}

// serveAttach serves one attach stream.
//
// The attachment's life: AttachSession numbers it (kind-N, N counted per
// session incarnation in state.db, so an id is never reused within an
// incarnation, not even by a later worker), takes its snapshot and
// subscribes it to output in one step on the owner goroutine, and answers
// Attached. It then lives until the client half-closes the stream (detach
// or disconnect), a DetachAttachment or DetachSession names it, a later
// attach replaces it, the session ends, or a write to the client fails.
// Whatever ends it, it is unsubscribed and removed from the list on the
// owner goroutine as this returns.
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

	seq, err := rt.db.NextAttachID(rt.sessionID, rt.uid)
	if errors.Is(err, db.ErrNoSuchIncarnation) {
		return refuse(fmt.Errorf("session `%s` is being removed", rt.sessionID))
	} else if err != nil {
		return refuse(fmt.Errorf("failed to number the attachment: %w", err))
	}
	var att *attachResult
	if err := rt.owner.do(func(s *ownerState) error {
		var err error
		att, err = s.attach(fmt.Sprintf("%s-%d", req.Kind, seq), req.Kind, req.Geometry, replaces)
		return err
	}); err != nil {
		return refuse(err)
	}
	defer rt.owner.post(func(s *ownerState) {
		s.output.unsubscribe(att.sub)
		s.removeAttachment(att.attachID)
	})

	// A client that stops reading blocks this handler in a write, where it
	// cannot notice a detach or the session ending. Once either happens,
	// give the client shutdownGrace to take its final frames, then fail the
	// blocked write so the attachment is released.
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-att.detach:
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
				if err := rt.sendSnapshot(conn, att.sub, true); err != nil {
					return err
				}
				continue
			}
			if err := protocol.WriteResponse(conn, &protocol.Response{PtyOutput: &protocol.Bytes{Data: data}}); err != nil {
				return err
			}
		case <-att.detach:
			break loop
		case <-rt.ended.ch:
			// Flush output already queued for this client so the tail of
			// the session is not lost behind SessionEnded; a client that
			// lagged gets the final screen instead.
			if resync && att.sub.lagged.Load() {
				if err := rt.sendSnapshot(conn, att.sub, true); err != nil {
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
				if err := rt.sendSnapshot(conn, att.sub, false); err != nil {
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
// before it is in the snapshot, everything after follows it.
func (rt *runtime) sendSnapshot(conn transport.Stream, sub *subscriber, resync bool) error {
	var snapshot []byte
	if err := rt.owner.do(func(s *ownerState) error {
		sub.discard()
		var err error
		snapshot, err = s.snapshot()
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
