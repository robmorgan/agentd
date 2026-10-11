package worker

// Live handoff: replacing a session worker's executable without stopping
// its agent.
//
// The worker re-executes itself in place (execve) with the new executable.
// The pid stays the same, so the agent stays its child and its exit status
// can still be collected with wait4; the PTY master, the listening socket,
// and the connection that asked for the handoff are inherited by clearing
// close-on-exec on them, and everything else the new image needs (the
// shadow terminal's exact state and the session's identity; attachment
// numbering lives in state.db)
// travels in an unlinked temporary file whose descriptor is inherited too.
//
// The alternative, starting a separate new worker and passing it the PTY
// and socket with SCM_RIGHTS (see fdpass_test.go), was prototyped and rejected:
// the agent would not be the new worker's child, so its exit status would be
// lost (only the old worker, or init after it exits, can reap it), and the
// new worker would have to poll for the exit (pidfd or kqueue) and could
// never learn the exit code; the session would also have two workers for a
// while, both able to read the PTY. Exec in place has none of those
// problems. Its cost is that a new image that fails after the exec cannot
// fall back to the old one, so the old image checks the new executable first
// (probeHandoff) and the new image falls back to an inexact terminal
// restore rather than give up.
//
// Sequence in the old image (rt.handoff, on the main goroutine):
//
//  1. Probe the new executable: it must run and speak this handoff format.
//  2. Take reapMu, so the agent cannot be reaped until the exec; give up if
//     it has already exited.
//  3. Stop accepting connections, keeping a duplicate of the listening
//     socket: new connections queue in its backlog for the new image, so the
//     socket never refuses a connection and the session never looks dead.
//  4. Tell every attachment the session is restarting (SessionRestarting,
//     for clients that support it) and wait for every connection handler.
//  5. Stop the PTY pump. From here on the agent's output stays in the
//     kernel's PTY buffer (an agent that fills it blocks in write), so
//     nothing is read that the snapshot would miss.
//  6. Wait for queued input to reach the PTY.
//  7. Snapshot the shadow terminal on the owner goroutine and exec.
//
// Any failure before the exec, including the exec itself, undoes these
// steps in reverse and the old image carries on; the requester gets an
// Error. After the exec the new image (Resume) restores the state, starts
// serving, and answers the requester with HandedOff.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// handoffVersion is the format of handoffState. A new image refuses a state
// it does not know, which the old image avoids by probing first.
const handoffVersion = 1

// handoffProbeOutput is what `agentd session-worker --handoff-probe` prints:
// the handoff formats the binary can resume from.
var handoffProbeOutput = fmt.Sprintf("agentd session-worker handoff %d", handoffVersion)

const (
	probeTimeout = 10 * time.Second
	// handoffHandlersTimeout bounds the wait for connection handlers once
	// attachments were told to restart. Attach handlers get shutdownGrace
	// to deliver their last frames.
	handoffHandlersTimeout = shutdownGrace + time.Second
	// inputDrainTimeout bounds the wait for queued input to reach an agent
	// that is not reading it.
	inputDrainTimeout = 2 * time.Second
)

// HandoffProbe answers `agentd session-worker --handoff-probe`.
func HandoffProbe() string { return handoffProbeOutput }

// handoffState is what the old image hands the new one, besides the
// inherited descriptors whose numbers it records.
type handoffState struct {
	Version  int
	Args     Args
	AgentPID int
	Handoffs uint32
	// StartedAt is when the first image started, for stats.
	StartedAt time.Time
	// Killed carries a kill requested too late to call the handoff off
	// (a SIGTERM to the worker just before the exec).
	Killed bool

	Terminal terminalHandoff
	Geometry protocol.Geometry
	// Status carries the program status records (OSC 7501): a blocked
	// agent must stay blocked across a handoff, since the program will
	// not re-report on its own. Nil from an older image.
	Status *statusHandoff

	// Inherited descriptors.
	PTYFD      int
	ListenerFD int
	RequestFD  int
}

type handoffRequest struct {
	conn transport.Stream
	exe  string
}

// refuse answers a handoff request that did not happen. The session keeps
// running in this image.
func (r *handoffRequest) refuse(err error) {
	self, _ := os.Executable()
	r.conn.SetWriteDeadline(time.Now().Add(shutdownGrace))
	_ = protocol.WriteResponse(r.conn, protocol.ErrorResponsef("handoff to %s failed: %v; the session keeps running on %s", r.exe, err, self))
	r.conn.Close()
}

// handoff hands the session to req.exe, or reports why it could not. It
// returns only if the handoff did not happen, with the session running as
// before.
func (rt *runtime) handoff(req *handoffRequest, state *ownerState) {
	err := rt.tryHandoff(req, state)
	fmt.Fprintf(os.Stderr, "session worker: handoff to %s failed: %v\n", req.exe, err)
	req.refuse(err)
}

func (rt *runtime) tryHandoff(req *handoffRequest, state *ownerState) (err error) {
	if reason := rt.noHandoff.Load(); reason != nil {
		return errors.New(*reason)
	}
	if rt.killed.Load() {
		return errors.New("the session is being stopped")
	}
	if rt.pump == nil {
		return errors.New("the session's output is not being read")
	}
	if !filepath.IsAbs(req.exe) {
		return errors.New("the executable must be an absolute path")
	}
	if err := probeHandoff(req.exe); err != nil {
		return err
	}

	// Each step that changes something pushes its undo; on failure they run
	// in reverse.
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()

	rt.reapMu.Lock()
	undo = append(undo, rt.reapMu.Unlock)
	if rt.agentExited.Load() {
		return errors.New("the agent has exited")
	}

	listenerFile, err := rt.listener.File()
	if err != nil {
		return fmt.Errorf("failed to duplicate the session socket: %w", err)
	}
	rt.listener.Close()
	<-rt.acceptDone
	undo = append(undo, func() {
		rt.listener = rt.relisten(listenerFile)
		listenerFile.Close()
		rt.startAccepting()
	})

	_ = rt.owner.do(func(s *ownerState) error { close(s.restart); return nil })
	undo = append(undo, func() {
		_ = rt.owner.do(func(s *ownerState) error { s.restart = make(chan struct{}); return nil })
	})
	if !rt.handlers.waitIdle(handoffHandlersTimeout) {
		return errors.New("connections to the session did not finish in time")
	}
	if rt.killed.Load() {
		return errors.New("the session is being stopped")
	}

	rt.pump.stop()
	undo = append(undo, func() {
		if err := rt.startPump(); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: %v\n", err)
			rt.terminateAgent()
		}
	})

	if !waitInputDrained(state.input, inputDrainTimeout) {
		return errors.New("the agent is not reading its input")
	}

	var st *handoffState
	if err := rt.owner.do(func(s *ownerState) error {
		term, err := s.terminal.handoff()
		if err != nil {
			return err
		}
		st = &handoffState{
			Version:   handoffVersion,
			Args:      rt.args,
			AgentPID:  rt.agentPID,
			Handoffs:  rt.handoffCount + 1,
			StartedAt: rt.startedAt,
			Terminal:  *term,
			Geometry:  s.geometry,
			Status:    s.status.handoff(),
		}
		if s.activity != nil {
			s.activity.exportAlertMemory(st.Status)
		}
		return nil
	}); err != nil {
		return err
	}
	if rt.killed.Load() {
		return errors.New("the session is being stopped")
	}
	return rt.execHandoff(req, st, listenerFile)
}

// relisten serves the socket again from the duplicate kept for the exec.
// If that fails, it binds the path again; clients that connected meanwhile
// lose their connection, but the session stays reachable.
func (rt *runtime) relisten(f *os.File) *transport.UnixListener {
	l, err := transport.UnixListenerFromFile(f, rt.socketPath)
	if err == nil {
		return l
	}
	fmt.Fprintf(os.Stderr, "session worker: %v; binding the socket again\n", err)
	_ = os.Remove(rt.socketPath)
	l, err = transport.ListenUnix(rt.socketPath, transport.UnixOptions{KeepSocketOnClose: true})
	if err != nil {
		// Unreachable sessions cannot be managed: end this one.
		fmt.Fprintf(os.Stderr, "session worker: failed to bind %s again: %v; stopping the session\n", rt.socketPath, err)
		rt.terminateAgent()
		return rt.listener
	}
	if info, err := os.Stat(rt.socketPath); err == nil {
		rt.ownSocket = info
	}
	return l
}

// waitInputDrained waits until every queued write has reached the PTY. The
// writer is then idle, blocked on its channel rather than in a write, so an
// exec cannot cut a write short.
func waitInputDrained(in *ptyInput, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for in.queued.Load() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// execHandoff writes the state, makes the descriptors inheritable and
// executes the new image. It returns only if the exec failed, having made
// the descriptors close-on-exec again.
func (rt *runtime) execHandoff(req *handoffRequest, st *handoffState, listenerFile *os.File) error {
	stateFile, err := os.CreateTemp(rt.paths.SessionsDir, "handoff-*")
	if err != nil {
		return fmt.Errorf("failed to create the handoff state file: %w", err)
	}
	defer stateFile.Close()
	// Unlinked at once: only the inherited descriptor refers to it, so it
	// disappears with the new image's read, or with this process.
	_ = os.Remove(stateFile.Name())

	uc, ok := req.conn.(*net.UnixConn)
	if !ok {
		return errors.New("the handoff request did not come over a Unix socket")
	}
	requestFile, err := uc.File()
	if err != nil {
		return fmt.Errorf("failed to duplicate the request connection: %w", err)
	}
	defer requestFile.Close()

	st.Killed = rt.killed.Load()
	st.PTYFD = rawFD(rt.ptmx)
	st.ListenerFD = rawFD(listenerFile)
	st.RequestFD = rawFD(requestFile)
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := stateFile.Write(data); err != nil {
		return fmt.Errorf("failed to write the handoff state: %w", err)
	}
	if _, err := stateFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	fds := []int{st.PTYFD, st.ListenerFD, st.RequestFD, rawFD(stateFile)}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			for _, fd := range fds {
				syscall.CloseOnExec(fd)
			}
			return fmt.Errorf("failed to pass descriptor %d: %w", fd, err)
		}
	}
	argv := []string{req.exe, "session-worker", "--resume-fd", strconv.Itoa(rawFD(stateFile))}
	fmt.Fprintf(os.Stderr, "session worker: handing session %s off to %s\n", rt.sessionID, req.exe)
	err = syscall.Exec(req.exe, argv, os.Environ())
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
	}
	return fmt.Errorf("failed to execute %s: %w", req.exe, err)
}

// rawFD is f's descriptor. Unlike f.Fd it leaves the descriptor's blocking
// mode alone (it may be shared with a socket the runtime poller serves).
func rawFD(f *os.File) int {
	fd := -1
	if rc, err := f.SyscallConn(); err == nil {
		_ = rc.Control(func(v uintptr) { fd = int(v) })
	}
	return fd
}

// probeHandoff checks that exe runs here and can resume from this image's
// handoff format, before anything is disturbed. After the exec there is no
// going back, so a missing binary, one for another architecture, one that
// crashes at start, or one too old to resume, is caught here instead.
func probeHandoff(exe string) error {
	if exe == "" {
		return errors.New("no executable given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "session-worker", "--handoff-probe")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s does not run here: %v %s", exe, err, strings.TrimSpace(out.String()))
	}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == handoffProbeOutput {
			return nil
		}
	}
	return fmt.Errorf("%s cannot resume this session (it does not support handoff format %d)", exe, handoffVersion)
}

// Resume is the new image's side of a handoff: `agentd session-worker
// --resume-fd FD`, executed in place of the old image with the state file
// at FD. It restores the session and serves it until the agent exits.
//
// If the state cannot be used at all, the session cannot be served: it is
// recorded failed and the agent stopped, rather than left running without a
// worker. A terminal snapshot this build cannot decode is not such a case;
// the terminal is rebuilt from its VT rendering instead.
func Resume(stateFD int) error {
	stateFile := os.NewFile(uintptr(stateFD), "handoff-state")
	data, err := io.ReadAll(stateFile)
	stateFile.Close()
	var st handoffState
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err == nil && st.Version != handoffVersion {
		err = fmt.Errorf("unsupported handoff format %d", st.Version)
	}
	if err != nil {
		// Without the state there is nothing to go on: not even which
		// session this was. The inherited PTY closes when this process
		// exits, which hangs up the agent.
		return fmt.Errorf("failed to read the handoff state: %w", err)
	}
	for _, fd := range []int{st.PTYFD, st.ListenerFD, st.RequestFD} {
		syscall.CloseOnExec(fd)
	}
	requestFile := os.NewFile(uintptr(st.RequestFD), "handoff-request")
	request, reqErr := net.FileConn(requestFile)
	requestFile.Close()
	if reqErr != nil {
		request = nil
	}
	answer := func(resp *protocol.Response) {
		if request == nil {
			return
		}
		request.SetWriteDeadline(time.Now().Add(shutdownGrace))
		_ = protocol.WriteResponse(request, resp)
		request.Close()
	}

	rt, state, err := resumeRuntime(&st)
	if err != nil {
		err = fmt.Errorf("handoff failed: %w", err)
		answer(protocol.ErrorResponsef("%v; the session was stopped", err))
		if p, perr := paths.Discover(); perr == nil {
			if store, derr := db.Open(p.Database); derr == nil {
				_ = store.MarkFailed(st.Args.SessionID, err.Error())
			}
		}
		signalGroup(st.AgentPID, syscall.SIGKILL)
		return err
	}
	if reqErr != nil {
		fmt.Fprintf(os.Stderr, "session worker: lost the handoff request connection: %v\n", reqErr)
	}
	exe, _ := os.Executable()
	fmt.Fprintf(os.Stderr, "session worker: resumed session %s on %s (handoff %d)\n", rt.sessionID, exe, rt.handoffCount)
	defer state.terminal.close()
	defer rt.ptmx.Close()
	return rt.run(state, func() {
		answer(&protocol.Response{HandedOff: &protocol.HandedOff{
			SessionID: rt.sessionID, WorkerPID: uint32(os.Getpid()), Executable: exe, Handoffs: rt.handoffCount,
		}})
	})
}

func resumeRuntime(st *handoffState) (*runtime, *ownerState, error) {
	ptmx := os.NewFile(uintptr(st.PTYFD), "/dev/ptmx")
	listenerFile := os.NewFile(uintptr(st.ListenerFD), "session-socket")
	defer listenerFile.Close()
	p, err := paths.Discover()
	if err != nil {
		return nil, nil, err
	}
	store, err := db.Open(p.Database)
	if err != nil {
		return nil, nil, err
	}
	socketPath := p.SessionSocketPath(st.Args.SessionID)
	listener, err := transport.UnixListenerFromFile(listenerFile, socketPath)
	if err != nil {
		return nil, nil, err
	}
	ownSocket, err := os.Stat(socketPath)
	if err != nil {
		listener.Close()
		return nil, nil, fmt.Errorf("the session socket is gone: %w", err)
	}

	terminal, inexact, err := restoreTerminal(&st.Terminal)
	if err != nil {
		listener.Close()
		return nil, nil, err
	}
	if inexact != nil {
		fmt.Fprintf(os.Stderr, "session worker: restored the terminal from its text rendering, not exactly: %v\n", inexact)
	}

	if err := store.MarkResumed(st.Args.SessionID, st.Args.CreatedAt, os.Getpid()); err != nil {
		// The old image would have carried on regardless; so does this one.
		fmt.Fprintf(os.Stderr, "session worker: %v\n", err)
	}

	rt := newRuntime(st.Args, p, store, st.AgentPID, ptmx, socketPath, ownSocket, listener)
	rt.handoffCount = st.Handoffs
	rt.startedAt = st.StartedAt
	if st.Killed {
		// Signal the agent again and arm the SIGKILL escalation, which
		// did not survive the exec.
		rt.terminateAgent()
	}
	state := newOwnerState(st.Args.SessionID, ptmx, terminal)
	// What the heuristics judged (idle, stalled) is not carried over: that
	// tracker starts afresh, as for a new attachment of interest. The
	// program status records are: the program will not re-report them.
	state.activity = newActivityTracker(rt.recorder, st.AgentPID, time.Now())
	state.status = restoreStatusTracker(st.Status)
	state.activity.setNative(time.Now(), state.status.derive(), false, false)
	// The previous image's alert memory comes along: an already-alerted
	// question stays quiet, and one its rate floor had deferred still
	// differs from the memory, so the tick retry records it here.
	state.activity.restoreAlertMemory(st.Status)
	state.owner = rt.owner
	state.geometry = st.Geometry
	return rt, state, nil
}
