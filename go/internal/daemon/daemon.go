// Package daemon implements `agentd serve`: the long-lived process that owns
// the session registry, spawns one session worker per session, and serves the
// agent protocol on the runtime root's Unix socket.
//
// Ownership model. A session belongs to the daemon's state (SQLite plus the
// worker process), never to a client connection. Each session's PTY lives in
// its own worker process (internal/worker), started in a new process session
// so it survives the daemon exiting or restarting; a restarted daemon finds
// running workers again through state.db and their per-session sockets.
//
// Goroutines, and what ends them:
//   - Serve's accept loop: ends when the listener is closed at shutdown.
//   - One handler per client connection: ends when its request is answered,
//     or for attach when either side of the proxy closes. Shutdown closes
//     every tracked connection, which unblocks all handlers.
//   - One extra goroutine per attach proxy (client -> worker direction).
//   - One supervisor per worker this daemon spawned: blocks in cmd.Wait, so
//     workers are reaped, and records a failure if the worker died without
//     recording its own outcome. It lives exactly as long as the worker; if
//     the daemon exits first the worker is re-parented and keeps running.
//     Once Serve has returned it no longer writes state, since the next
//     daemon owns reconciliation from then on.
//
// Backpressure. The daemon never buffers PTY output: attach is a byte pipe
// between the client socket and the worker socket. A slow client blocks only
// its own proxy, which blocks only its own writer in the worker, where the
// per-attachment fan-out queue applies the slow-consumer policy (see
// internal/worker/broadcast.go).
package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/transport"
)

// Version is reported by GetDaemonInfo and daemon status.
var Version = "0.1.0"

const (
	// connectionDrainTimeout bounds how long shutdown waits for handlers
	// after their connections have been closed.
	connectionDrainTimeout = 2 * time.Second
	// firstRequestTimeout bounds how long a new connection may take to send
	// its first frame.
	firstRequestTimeout = 30 * time.Second
	// defaultLockWait is how long a starting daemon waits for a previous one
	// to finish draining after a restart.
	defaultLockWait = 5 * time.Second
)

type Server struct {
	paths  *paths.AppPaths
	db     *db.Database
	config *Config
	// workerBin is the executable started as `<workerBin> session-worker`.
	workerBin string
	lockWait  time.Duration

	// remoteAddr is where the QUIC listener is bound, if remote access is
	// enabled and it started.
	remoteAddr atomic.Pointer[string]
	// remoteErr is why a configured QUIC listener is not bound (yet).
	remoteErr atomic.Pointer[string]

	// createMu serialises session name allocation and row insertion.
	createMu sync.Mutex

	shutdownOnce sync.Once
	shutdown     chan struct{}

	// connsMu guards conns, listeners and closing, and orders every
	// handlers.Add before shutdown's handlers.Wait.
	connsMu   sync.Mutex
	conns     map[transport.Stream]struct{}
	listeners []transport.Listener
	// closing is set under connsMu once shutdown starts, so no connection
	// accepted afterwards is left untracked.
	closing  bool
	handlers sync.WaitGroup
}

// New opens the state database and config under p. workerBin is normally
// os.Executable(); tests point it at a freshly built agentd.
func New(p *paths.AppPaths, workerBin string) (*Server, error) {
	if err := p.EnsureLayout(); err != nil {
		return nil, err
	}
	store, err := db.Open(p.Database)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(p.Config)
	if err != nil {
		return nil, err
	}
	return &Server{
		paths:     p,
		db:        store,
		config:    cfg,
		workerBin: workerBin,
		lockWait:  defaultLockWait,
		shutdown:  make(chan struct{}),
		conns:     make(map[transport.Stream]struct{}),
	}, nil
}

// Shutdown asks Serve to stop. Running sessions are not affected.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// Serve binds the daemon socket and serves until ctx is cancelled or a
// shutdown request arrives. It returns once the socket and pid file are gone
// and connection handlers have finished.
func (s *Server) Serve(ctx context.Context) error {
	lock, err := acquireLock(s.paths.LockPath(), s.lockWait)
	if err != nil {
		return err
	}
	defer releaseLock(lock)

	// Holding the lock, any socket left at the path belongs to a daemon that
	// is gone.
	if err := os.Remove(s.paths.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove stale agentd socket: %w", err)
	}
	if err := s.reconcileSessions(); err != nil {
		return err
	}

	listener, err := transport.ListenUnix(s.paths.Socket, transport.UnixOptions{})
	if err != nil {
		return fmt.Errorf("failed to bind agentd socket: %w", err)
	}
	// The pid file is informational (agent daemon info); the lock, not the
	// pid file, decides whether a daemon is running.
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(s.paths.PIDFile, []byte(pid), 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("failed to write %s: %w", s.paths.PIDFile, err)
	}

	s.startRemote()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		s.serveListener(listener)
	}()

	select {
	case <-ctx.Done():
	case <-s.shutdown:
	}
	s.Shutdown()

	// Remove the pid file before closing the listener (which unlinks the
	// socket): a client that finds the socket gone starts a replacement
	// daemon, which then waits for our lock while we drain.
	if data, err := os.ReadFile(s.paths.PIDFile); err == nil && strings.TrimSpace(string(data)) == pid {
		_ = os.Remove(s.paths.PIDFile)
	}
	s.closeListeners()
	<-acceptDone
	s.closeConnections()
	drained := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(connectionDrainTimeout):
	}

	// The socket and pid file were removed above. Removing the socket path
	// again here could delete the socket of a daemon started since, which
	// is waiting for the lock released when Serve returns.
	return nil
}

// remoteRetryInterval is how often a QUIC listener that failed to bind is
// retried. Tests shorten it.
var remoteRetryInterval = 5 * time.Second

// startRemote starts the QUIC listener when remote access is configured. A
// failure is logged and local service carries on: a bad remote setting must
// not lock the user out of their local sessions.
//
// Binding is retried until it succeeds or the daemon shuts down, since the
// listen address may not exist yet: a daemon started at login can come up
// before Tailscale or WireGuard has configured its interface. The retry
// goroutine is the only one started here and ends with the daemon.
func (s *Server) startRemote() {
	addr := s.config.Remote.Listen
	if addr == "" {
		return
	}
	id, err := transport.LoadOrCreateIdentity(s.paths.RemoteKeyPath())
	if err != nil {
		s.setRemoteError(err)
		fmt.Fprintf(os.Stderr, "agentd: remote access disabled: %v\n", err)
		return
	}
	// Read once here, in Serve, rather than from the retry goroutine.
	retry := remoteRetryInterval
	authorized := s.paths.AuthorizedClientsPath()
	opts := transport.QUICOptions{
		Authorized: func(fp string) bool {
			ok, err := transport.IsAuthorized(authorized, fp)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agentd: reading %s: %v\n", authorized, err)
			}
			return ok
		},
		OnConnect: func(fp string, remote net.Addr) {
			fmt.Fprintf(os.Stderr, "agentd: remote client %s connected from %s\n", fp, remote)
		},
	}
	listen := func() bool {
		l, err := transport.ListenQUIC(addr, id, opts)
		if err != nil {
			// Logged only when the reason changes, not on every retry.
			if prev := s.remoteErr.Load(); prev == nil || *prev != err.Error() {
				fmt.Fprintf(os.Stderr, "agentd: remote access not available yet (retrying every %s): %v\n", retry, err)
			}
			s.setRemoteError(err)
			return false
		}
		bound := l.Addr()
		s.remoteAddr.Store(&bound)
		s.remoteErr.Store(nil)
		fmt.Fprintf(os.Stderr, "agentd: accepting remote clients over QUIC on %s (key %s)\n", bound, id.Fingerprint)
		go s.serveListener(l)
		return true
	}
	if listen() {
		return
	}
	go func() {
		ticker := time.NewTicker(retry)
		defer ticker.Stop()
		for {
			select {
			case <-s.shutdown:
				return
			case <-ticker.C:
				if listen() {
					return
				}
			}
		}
	}()
}

func (s *Server) setRemoteError(err error) {
	msg := err.Error()
	s.remoteErr.Store(&msg)
}

// RemoteAddr is the address the QUIC listener is bound to, or "" when
// remote access is off or not listening yet.
func (s *Server) RemoteAddr() string {
	if addr := s.remoteAddr.Load(); addr != nil {
		return *addr
	}
	return ""
}

// remoteError is why remote access is configured but not listening, or "".
func (s *Server) remoteError() string {
	if msg := s.remoteErr.Load(); msg != nil {
		return *msg
	}
	return ""
}

// serveListener serves streams from l until it is closed. Serve runs the
// Unix socket through it; further transports (and tests) add theirs the same
// way and are closed along with it at shutdown.
func (s *Server) serveListener(l transport.Listener) {
	s.connsMu.Lock()
	if s.closing {
		s.connsMu.Unlock()
		l.Close()
		return
	}
	s.listeners = append(s.listeners, l)
	s.connsMu.Unlock()
	// Handlers are counted in track, under connsMu, rather than by Serve:
	// that orders them before shutdown's Wait for every listener, including
	// ones still winding down.
	transport.Serve(l, "agentd", nil, s.handleStream)
}

func (s *Server) handleStream(stream transport.Stream) {
	if !s.track(stream) {
		stream.Close()
		return
	}
	defer s.handlers.Done()
	defer s.untrack(stream)
	if err := s.handleConnection(stream); err != nil && !isDisconnect(err) {
		fmt.Fprintf(os.Stderr, "agentd: connection error: %v\n", err)
	}
}

// track registers a stream and counts its handler, unless shutdown has
// begun. Counting under connsMu orders it before shutdown's handlers.Wait.
func (s *Server) track(stream transport.Stream) bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.closing {
		return false
	}
	s.conns[stream] = struct{}{}
	s.handlers.Add(1)
	return true
}

func (s *Server) untrack(stream transport.Stream) {
	s.connsMu.Lock()
	delete(s.conns, stream)
	s.connsMu.Unlock()
	stream.Close()
}

// closeListeners stops every listener; their serveListener calls return.
func (s *Server) closeListeners() {
	s.connsMu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.connsMu.Unlock()
	for _, l := range listeners {
		l.Close()
	}
}

func (s *Server) closeConnections() {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	s.closing = true
	for stream := range s.conns {
		stream.Close()
	}
}

func (s *Server) handleConnection(conn transport.Stream) error {
	reader := bufio.NewReader(conn)
	// A client that connects and never sends a request must not hold a
	// handler (and a file descriptor) until shutdown.
	conn.SetReadDeadline(time.Now().Add(firstRequestTimeout))
	req, mgmt, err := protocol.ReadIncoming(reader)
	conn.SetReadDeadline(time.Time{})
	var versionErr *protocol.VersionError
	if errors.As(err, &versionErr) {
		// Answer in the client's own framing so it can say why it was
		// refused instead of reporting a dropped connection.
		return protocol.WriteErrorAtVersion(conn, versionErr.Version, fmt.Sprintf(
			"agentd speaks protocol version %d but this client sent version %d; upgrade the agent CLI or the daemon so they match",
			protocol.ProtocolVersion, versionErr.Version))
	}
	var decodeErr *protocol.DecodeError
	if errors.As(err, &decodeErr) {
		msg := fmt.Sprintf("agentd could not decode the request: %v", decodeErr.Err)
		if decodeErr.Version == protocol.DaemonManagementVersion {
			return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Error: &protocol.ErrorResponse{Message: msg}})
		}
		return protocol.WriteResponse(conn, protocol.ErrorResponsef("%s", msg))
	}
	if err != nil {
		return err
	}
	switch {
	case mgmt != nil:
		return s.handleManagement(conn, mgmt)
	case req != nil:
		return s.handleRequest(conn, reader, req)
	}
	return nil
}

func (s *Server) handleManagement(conn transport.Stream, req *protocol.ManagementRequest) error {
	running, err := s.hasRunningSessions()
	if err != nil {
		return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Error: &protocol.ErrorResponse{Message: err.Error()}})
	}
	switch {
	case req.Status != nil:
		return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Status: &protocol.ManagementStatus{
			DaemonVersion:   Version,
			ProtocolVersion: protocol.ProtocolVersion,
			PID:             uint32(os.Getpid()),
			Root:            s.paths.Root,
			Socket:          s.paths.Socket,
			RunningSessions: running,
			Remote:          s.RemoteAddr(),
			RemoteError:     s.remoteError(),
		}})
	case req.Shutdown != nil:
		if running && !req.Shutdown.Force {
			return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Shutdown: &protocol.ManagementShutdownResult{
				Stopped: false, RunningSessions: true, Message: "cannot shut down agentd while sessions are running",
			}})
		}
		err := protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Shutdown: &protocol.ManagementShutdownResult{
			Stopped: true, RunningSessions: running, Message: "agentd stopping",
		}})
		s.Shutdown()
		return err
	}
	return nil
}

func (s *Server) handleRequest(conn transport.Stream, reader *bufio.Reader, req *protocol.Request) error {
	reply := func(resp *protocol.Response) error { return protocol.WriteResponse(conn, resp) }
	replyErr := func(err error) error { return reply(protocol.ErrorResponsef("%v", err)) }

	// Session ids become socket and log paths, so anything that could not
	// have been created as a session name (e.g. "../x") is refused here,
	// before any path is built from it.
	if id, ok := requestSessionID(req); ok && !validSessionName(id) {
		return reply(protocol.ErrorResponsef("session `%s` not found", id))
	}

	switch {
	case req.GetDaemonInfo != nil:
		return reply(&protocol.Response{DaemonInfo: &protocol.DaemonInfo{
			DaemonVersion: Version, ProtocolVersion: protocol.ProtocolVersion,
		}})
	case req.ShutdownDaemon != nil:
		running, err := s.hasRunningSessions()
		if err != nil {
			return replyErr(err)
		}
		if running {
			return reply(protocol.ErrorResponsef("cannot shut down agentd while sessions are running"))
		}
		err = reply(protocol.OkResponse())
		s.Shutdown()
		return err
	case req.CreateSession != nil:
		result, err := s.createSession(req.CreateSession)
		if err != nil {
			return replyErr(err)
		}
		return reply(&protocol.Response{CreateSession: result})
	case req.KillSession != nil:
		k := req.KillSession
		result, err := s.killSession(k.SessionID, k.Remove)
		if err != nil {
			return replyErr(err)
		}
		return reply(&protocol.Response{KillSession: result})
	case req.AttachSession != nil:
		return s.proxyAttach(conn, reader, req.AttachSession)
	case req.AttachSnapshot != nil:
		return reply(protocol.ErrorResponsef("attach snapshot requests are only valid during an active attach"))
	case req.AttachInput != nil:
		return reply(protocol.ErrorResponsef("attach_input is only valid during an attached session"))
	case req.AttachResize != nil:
		return reply(protocol.ErrorResponsef("attach_resize is only valid during an attached session"))
	case req.DetachSession != nil:
		return s.proxyRequest(conn, req.DetachSession.SessionID, req)
	case req.DetachAttachment != nil:
		return s.proxyRequest(conn, req.DetachAttachment.SessionID, req)
	case req.SendInput != nil:
		return s.proxyRequest(conn, req.SendInput.SessionID, req)
	case req.ListAttachments != nil:
		return s.proxyRequest(conn, req.ListAttachments.SessionID, req)
	case req.GetHistory != nil:
		return s.history(conn, req)
	case req.GetSession != nil:
		rec, err := s.getSession(req.GetSession.SessionID)
		if err != nil {
			return replyErr(err)
		}
		if rec == nil {
			return reply(protocol.ErrorResponsef("session `%s` not found", req.GetSession.SessionID))
		}
		return reply(&protocol.Response{Session: rec})
	case req.ListSessions != nil:
		recs, err := s.listSessions()
		if err != nil {
			return replyErr(err)
		}
		return reply(&protocol.Response{Sessions: &recs})
	}
	return reply(protocol.ErrorResponsef("unsupported request"))
}

// requestSessionID returns the existing session a request refers to.
func requestSessionID(req *protocol.Request) (string, bool) {
	switch {
	case req.KillSession != nil:
		return req.KillSession.SessionID, true
	case req.AttachSession != nil:
		return req.AttachSession.SessionID, true
	case req.DetachSession != nil:
		return req.DetachSession.SessionID, true
	case req.DetachAttachment != nil:
		return req.DetachAttachment.SessionID, true
	case req.SendInput != nil:
		return req.SendInput.SessionID, true
	case req.GetSession != nil:
		return req.GetSession.SessionID, true
	case req.ListAttachments != nil:
		return req.ListAttachments.SessionID, true
	case req.GetHistory != nil:
		return req.GetHistory.SessionID, true
	}
	return "", false
}

func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset")
}
