// Package worker runs a single agent session: it owns the PTY, the shadow
// terminal used for reattach snapshots, and the per-session Unix socket the
// daemon proxies client requests through.
package worker

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
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

func signalGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

func Run(args Args) error {
	p, err := paths.Discover()
	if err != nil {
		return err
	}
	if err := p.EnsureLayout(); err != nil {
		return err
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
	// AGENTD_WORKSPACE is an alias of AGENTD_CWD kept for one release so agent
	// instructions written against the Rust daemon keep working.
	cmd.Env = append(os.Environ(),
		"AGENTD_SESSION_ID="+args.SessionID,
		"AGENTD_SESSION_NAME="+args.SessionID,
		"AGENTD_SOCKET="+p.Socket,
		"AGENTD_CWD="+args.Cwd,
		"AGENTD_WORKSPACE="+args.Cwd,
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: defaultPtyRows, Cols: defaultPtyCols})
	if err != nil {
		return fail(fmt.Errorf("failed to spawn agent process: %w", err))
	}
	defer ptmx.Close()

	terminal, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
	if err != nil {
		_ = cmd.Process.Kill()
		return fail(err)
	}
	defer terminal.close()

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to bind worker socket: %w", err))
	}
	// Unlink the socket ourselves, and only while the path is still ours:
	// after the session is removed its name may be reused, and a new
	// worker's socket may already sit at the same path.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
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
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		removeOwnSocket()
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		return fail(fmt.Errorf("failed to restrict worker socket: %w", err))
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
		paths:     p,
		db:        store,
		owner:     newOwner(),
		ended:     &endedSignal{ch: make(chan struct{})},
		agentPID:  cmd.Process.Pid,
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
	}
	state.nextAttachOrdinal = 1

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

	go func() {
		backoff := time.Duration(0)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				// Temporary failures such as running out of file
				// descriptors must not stop the worker accepting for good.
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				fmt.Fprintf(os.Stderr, "session worker: accept: %v; retrying in %v\n", err, backoff)
				time.Sleep(backoff)
				continue
			}
			backoff = 0
			rt.conns.Add(1)
			go func() {
				defer rt.conns.Done()
				defer conn.Close()
				if err := rt.handleConnection(conn); err != nil {
					fmt.Fprintf(os.Stderr, "session worker connection error: %v\n", err)
				}
			}()
		}
	}()

	<-rt.ended.ch
	listener.Close()
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
	if rt.killed.Load() || (exitCode != nil && *exitCode == 0) {
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

func (rt *runtime) handleConnection(conn net.Conn) error {
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
	case req.AttachSession != nil:
		return rt.serveAttach(conn, reader, req.AttachSession.Kind, req.AttachSession.Geometry)
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
	default:
		return reply(protocol.ErrorResponsef("unsupported worker request"))
	}
}

func (rt *runtime) serveAttach(conn net.Conn, reader *bufio.Reader, kind session.AttachmentKind, g protocol.Geometry) error {
	var att *attachResult
	if err := rt.owner.do(func(s *ownerState) error {
		var err error
		att, err = s.attach(kind, g)
		return err
	}); err != nil {
		_ = protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
		return err
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

	if err := protocol.WriteResponse(conn, &protocol.Response{Attached: &protocol.Attached{
		AttachID: att.attachID, Snapshot: att.snapshot,
	}}); err != nil {
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
			if err := protocol.WriteResponse(conn, &protocol.Response{PtyOutput: &protocol.Bytes{Data: data}}); err != nil {
				return err
			}
		case <-att.detach:
			break loop
		case <-rt.ended.ch:
			// Flush output already queued for this client so the tail of
			// the session is not lost behind SessionEnded.
			if err := drainOutput(conn, att.sub); err != nil {
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
					return err
				}
			case in.req.AttachSnapshot != nil:
				// Output is published on the owner goroutine, so discarding
				// what is queued for this client and taking the snapshot
				// there makes the snapshot an exact boundary: everything
				// before it is in the snapshot, everything after follows it.
				var snapshot []byte
				if err := rt.owner.do(func(s *ownerState) error {
					discardQueued(att.sub)
					var err error
					snapshot, err = s.snapshot()
					return err
				}); err != nil {
					return err
				}
				if err := protocol.WriteResponse(conn, &protocol.Response{AttachSnapshot: &protocol.Bytes{Data: snapshot}}); err != nil {
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

func drainOutput(conn net.Conn, sub *subscriber) error {
	for {
		select {
		case data := <-sub.ch:
			if err := protocol.WriteResponse(conn, &protocol.Response{PtyOutput: &protocol.Bytes{Data: data}}); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func discardQueued(sub *subscriber) {
	for {
		select {
		case <-sub.ch:
		default:
			return
		}
	}
}
