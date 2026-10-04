package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// client reaches one daemon: the local one over its Unix socket, or a
// remote one over QUIC (agent --host NAME).
//
// The protocol's unit is a stream: the control stream that carries every
// one-shot request, one per attach session, one per transfer. Locally a
// stream is one Unix connection; remotely it is one QUIC stream on a
// connection the process opens once, lazily, and reuses.
type client struct {
	paths *paths.AppPaths
	// host is the remote daemon, or nil for the local one.
	host *transport.Host

	mu   sync.Mutex
	quic *transport.QUICClient

	// controlMu guards control, the stream one-shot requests share; see
	// controlStream. It is separate from mu because opening a control
	// stream may dial the remote connection, which takes mu.
	controlMu sync.Mutex
	control   *controlStream
}

const (
	localConnectTimeout = 2 * time.Second
	// daemonStartTimeout is longer than the daemon's own 5s wait for
	// agentd.lock, so a restart that has to wait out the previous daemon
	// still counts as a successful start.
	daemonStartTimeout = 7 * time.Second
	daemonStopTimeout  = 5 * time.Second
	pollInterval       = 100 * time.Millisecond

	upgradeHint = "run `agent daemon upgrade` (set AGENTD_BIN to choose the daemon binary)"
)

func (c *client) remoteName() string {
	if c.host == nil {
		return ""
	}
	return c.host.Name
}

// controlTimeout bounds a control request: the local daemon answers at
// once, while a remote one may first need a QUIC handshake over a slow link.
func (c *client) controlTimeout() time.Duration {
	if c.host != nil {
		return 15 * time.Second
	}
	return 250 * time.Millisecond
}

// requestTimeout bounds the requests the picker and the overlay make while
// the user waits on them.
func (c *client) requestTimeout() time.Duration {
	if c.host != nil {
		return 15 * time.Second
	}
	return 5 * time.Second
}

// open opens a stream to the daemon.
func (c *client) open(ctx context.Context) (transport.Stream, error) {
	if c.host == nil {
		s, err := transport.DialUnix(c.paths.Socket, localConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to %s: %w", c.paths.Socket, err)
		}
		return s, nil
	}
	conn, err := c.remoteConnection(ctx)
	if err != nil {
		return nil, err
	}
	return conn.OpenStream(ctx)
}

func (c *client) remoteConnection(ctx context.Context) (*transport.QUICClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quic != nil {
		return c.quic, nil
	}
	id, err := loadClientIdentity(c.paths)
	if err != nil {
		return nil, err
	}
	conn, err := transport.DialQUIC(ctx, c.host.Address, id, c.host.Fingerprint)
	var changed *transport.KeyChangedError
	switch {
	case errors.As(err, &changed):
		return nil, &hostKeyChangedError{changed: changed, msg: fmt.Sprintf("the key of host `%s` (%s) has changed!\n  pinned:    %s\n  presented: %s\n"+
			"This could mean someone is intercepting the connection, or the daemon's key was recreated.\n"+
			"If you trust the new key, run `agent host rm %s` and add the host again.",
			c.host.Name, c.host.Address, c.host.Fingerprint, changed.Presented, c.host.Name)}
	case err != nil:
		return nil, fmt.Errorf("could not reach agentd on `%s` (%s): %w", c.host.Name, c.host.Address, err)
	}
	c.quic = conn
	return conn, nil
}

// hostKeyChangedError explains a changed host key to the user, and still
// unwraps to the *transport.KeyChangedError.
type hostKeyChangedError struct {
	changed *transport.KeyChangedError
	msg     string
}

func (e *hostKeyChangedError) Error() string { return e.msg }
func (e *hostKeyChangedError) Unwrap() error { return e.changed }

// close ends the control stream and the remote connection, if any, so the
// daemon sees this client go away at once rather than after an idle
// timeout.
func (c *client) close() {
	c.controlMu.Lock()
	if c.control != nil {
		c.control.close()
		c.control = nil
	}
	c.controlMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quic != nil {
		c.quic.Close()
		c.quic = nil
	}
}

// explain adds what to do to an error from a remote daemon that refused
// this machine's key. With TLS 1.3 the client finishes its side of the
// handshake before the daemon judges its certificate, so the refusal can
// surface on the first stream rather than at connect; this runs on a
// command's final error so every path gets the hint.
func (c *client) explain(err error) error {
	if c.host == nil || !transport.IsKeyRefused(err) {
		return err
	}
	fp := "<this machine's key>"
	if id, idErr := loadClientIdentity(c.paths); idErr == nil {
		fp = id.Fingerprint
	}
	return fmt.Errorf("%w\nagentd on `%s` (%s) refused this machine's key. If it is not authorized yet, run on `%s`:\n  agentd remote authorize %s %s",
		err, c.host.Name, c.host.Address, c.host.Name, fp, localHostname())
}

// loadClientIdentity loads remote/client.key, creating it (private, in a
// private directory) on first use.
func loadClientIdentity(p *paths.AppPaths) (*transport.Identity, error) {
	path := p.ClientKeyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to restrict %s: %w", filepath.Dir(path), err)
	}
	id, err := transport.LoadOrCreateIdentity(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", path, err)
	}
	return id, nil
}

func localHostname() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "this-machine"
}

// request sends one request and reads its response. With a non-zero
// timeout the whole exchange, connecting included, must finish in time.
//
// One-shot requests share the control stream; an attach, or a transfer
// such as history, gets a stream of its own (see protocol.StreamRole).
func (c *client) request(req *protocol.Request, timeout time.Duration) (*protocol.Response, error) {
	ctx, cancel := contextFor(timeout)
	defer cancel()
	if req.Role() == protocol.RoleRequest {
		return c.controlRequest(ctx, req)
	}
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if deadline, ok := ctx.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	if err := protocol.WriteRequest(s, req); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(s))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("agentd closed the connection")
	}
	return resp, nil
}

// controlRequest sends req on the control stream. A stream that turns out
// to have ended before req was written (the daemon restarted since it was
// opened, say) is replaced once.
//
// A request that was sent is repeated only when that is safe: a read, or a
// request with side effects that carries a request token, which the daemon
// answers with its original answer if it already acted on it (see
// protocol.CapRequestTokens). Such a request is sent again, on a new
// connection, when the connection to a remote daemon was lost, backing off
// like an attachment's reconnect, until it is answered, fails for another
// reason, or ctx (or, without a deadline, retryBudget) runs out. Requests
// without a token (send-input, detach) are never repeated, since the daemon
// may have acted on them.
func (c *client) controlRequest(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if slot := req.TokenSlot(); slot != nil && *slot == "" {
		// Set once, so every attempt carries the same token. It is only
		// sent if the daemon supports tokens.
		*slot = newRequestToken()
	}
	start := time.Now()
	delay := time.Duration(0)
	// sent is set once req has been written: only then is a lost
	// connection retried. A daemon that was never reached fails at once.
	sent := false
	for attempt := 0; ; attempt++ {
		cs, err := c.controlStream(ctx)
		if err == nil {
			var resp *protocol.Response
			if resp, err = cs.roundTrip(ctx, req); err == nil {
				return resp, nil
			}
			var se *sentError
			if errors.As(err, &se) {
				if !c.mayResend(cs, req) {
					return nil, se.err
				}
				sent = true
				err = se.err
			} else if attempt == 0 {
				continue // the stream had ended before req was written
			}
		}
		if !sent || !c.mayRetry(ctx, err, start) {
			return nil, err
		}
		// The connection is dead: the next attempt dials a new one.
		c.close()
		delay = min(max(2*delay, reconnectFirstRetry), reconnectMaxRetry)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, err
		}
	}
}

// retryBudget bounds how long a request without its own deadline keeps
// retrying after lost connections.
const retryBudget = time.Minute

// mayResend reports whether req may be sent again after it was sent on cs:
// a read always, a request with side effects only with a token the daemon
// understands.
func (c *client) mayResend(cs *controlStream, req *protocol.Request) bool {
	if req.TokenSlot() != nil {
		return cs.features.Has(protocol.CapRequestTokens)
	}
	return isRead(req)
}

// mayRetry reports whether a request that failed with err is worth another
// attempt: only a remote one whose connection was lost, within its time.
func (c *client) mayRetry(ctx context.Context, err error, start time.Time) bool {
	if c.host == nil || ctx.Err() != nil || !transport.IsConnectionLost(err) {
		return false
	}
	if _, ok := ctx.Deadline(); !ok && time.Since(start) > retryBudget {
		return false
	}
	return true
}

// isRead reports whether req only reads state, so sending it twice is
// harmless.
func isRead(req *protocol.Request) bool {
	switch {
	case req.GetDaemonInfo != nil, req.GetSession != nil, req.ListSessions != nil,
		req.ListAttachments != nil, req.ListWorkspaces != nil:
		return true
	}
	return false
}

// newRequestToken returns a random request token.
func newRequestToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// controlStream returns the open control stream, opening one (and with it
// the handshake) if there is none or it has ended.
func (c *client) controlStream(ctx context.Context) (*controlStream, error) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if c.control != nil && !c.control.failed() {
		return c.control, nil
	}
	if c.control != nil {
		c.control.close()
		c.control = nil
	}
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := openControl(ctx, s)
	if err != nil {
		return nil, err
	}
	c.control = cs
	return cs, nil
}

func contextFor(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithCancel(context.Background())
}

// call sends a request whose only expected answers are want or an error.
func (c *client) call(req *protocol.Request, timeout time.Duration, want func(*protocol.Response) bool) (*protocol.Response, error) {
	resp, err := c.request(req, timeout)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	if !want(resp) {
		return nil, fmt.Errorf("unexpected response: %s", describeResponse(resp))
	}
	return resp, nil
}

// manage sends one daemon management request.
func (c *client) manage(req *protocol.ManagementRequest, timeout time.Duration) (*protocol.ManagementResponse, error) {
	ctx, cancel := contextFor(timeout)
	defer cancel()
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if deadline, ok := ctx.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	if err := protocol.WriteManagementRequest(s, req); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadManagementResponse(bufio.NewReader(s))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("agentd closed the management connection")
	}
	return resp, nil
}

// prepareDaemon makes sure a daemon is there for a command. Every command
// goes through the daemon, which alone owns session state; with no
// compatible daemon a command fails and names the fix. `agent daemon`
// subcommands only need something to talk to, since they exist to repair a
// missing or mismatched daemon; `agent daemon upgrade` needs nothing. A
// remote daemon is never started, restarted or replaced from here.
func (c *client) prepareDaemon(daemonCommand string) error {
	if c.host != nil {
		switch daemonCommand {
		case "restart", "upgrade":
			return fmt.Errorf("`agent daemon %s` only manages the local daemon; run `agentd` on `%s` instead", daemonCommand, c.host.Name)
		case "":
			return c.ensureCompatible()
		}
		return nil
	}
	switch daemonCommand {
	case "upgrade":
		return nil
	case "":
		return c.ensureDaemon()
	}
	if !c.localDaemonAnswers() {
		return c.spawnDaemon()
	}
	return nil
}

func (c *client) localDaemonAnswers() bool {
	s, err := transport.DialUnix(c.paths.Socket, localConnectTimeout)
	if err != nil {
		return false
	}
	s.Close()
	return true
}

func (c *client) ensureDaemon() error {
	if !c.localDaemonAnswers() {
		if err := c.spawnDaemon(); err != nil {
			return err
		}
	}
	return c.ensureCompatible()
}

func (c *client) spawnDaemon() error {
	exe, err := daemonExecutable()
	if err != nil {
		return err
	}
	return c.startDaemon(exe, daemonStartTimeout)
}

// startDaemon starts `agentd serve --daemonize` and waits for its socket to
// answer.
//
// The daemon holds an exclusive flock on agentd.lock for its whole life,
// waits up to 5s for it on startup (so a restart can hand over), and removes
// a stale socket itself while holding the lock. So the CLI never deletes the
// socket or pid file and never judges liveness from the pid file (a recycled
// pid would make that judgement wrong forever); if another daemon wins the
// race, its socket answering is just as good.
func (c *client) startDaemon(exe string, timeout time.Duration) error {
	cmd := exec.Command(exe, "serve", "--daemonize")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start agentd: %w", err)
	}
	// daemonize forks and exits at once; reap it so it is not left a zombie.
	go cmd.Wait()
	deadline := time.Now().Add(timeout)
	for {
		if c.localDaemonAnswers() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for agentd to start; see %s", c.paths.DaemonLogPath())
		}
		time.Sleep(pollInterval)
	}
}

// daemonExecutable is the daemon started by `serve --daemonize` and
// `upgrade`: $AGENTD_BIN when set and non-empty (for example go/bin/agentd
// during development), otherwise the agentd next to this executable.
func daemonExecutable() (string, error) {
	exe, err := os.Executable()
	return daemonExecutableFrom(os.Getenv("AGENTD_BIN"), exe, err)
}

func daemonExecutableFrom(agentdBin, exe string, exeErr error) (string, error) {
	if agentdBin != "" {
		return agentdBin, nil
	}
	if exeErr != nil {
		return "", fmt.Errorf("failed to resolve current executable: %w", exeErr)
	}
	return filepath.Join(filepath.Dir(exe), "agentd"), nil
}

// ensureCompatible performs the handshake, which agrees on a protocol
// version, and keeps the control stream it opens for the requests that
// follow.
func (c *client) ensureCompatible() error {
	_, err := c.welcome()
	return err
}

func incompatibleDaemonMessage() string {
	return fmt.Sprintf("agentd does not speak agent protocol %d; %s", protocol.ProtocolVersion, upgradeHint)
}

// welcome is the daemon's answer to this client's Hello.
func (c *client) welcome() (*protocol.Welcome, error) {
	ctx, cancel := contextFor(c.controlTimeout())
	defer cancel()
	cs, err := c.controlStream(ctx)
	switch {
	case err != nil && c.host != nil:
		// Remote connect errors already say what to do.
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%s: %w", incompatibleDaemonMessage(), err)
	}
	return cs.welcome, nil
}

func (c *client) managementStatus() (*protocol.ManagementStatus, error) {
	resp, err := c.manage(&protocol.ManagementRequest{Status: protocol.Empty}, c.controlTimeout())
	switch {
	case err != nil:
		return nil, fmt.Errorf("daemon management status request failed: %w", err)
	case resp.Error != nil:
		return nil, errors.New(resp.Error.Message)
	case resp.Status == nil:
		return nil, errors.New("unexpected daemon management response")
	}
	return resp.Status, nil
}

func (c *client) shutdownDaemon(force bool) error {
	resp, err := c.manage(&protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: force}}, 250*time.Millisecond)
	switch {
	case err != nil:
		return fmt.Errorf("daemon management shutdown request failed: %w", err)
	case resp.Error != nil:
		return errors.New(resp.Error.Message)
	case resp.Shutdown == nil:
		return errors.New("unexpected daemon management response")
	case !resp.Shutdown.Stopped:
		return errors.New(resp.Shutdown.Message)
	}
	deadline := time.Now().Add(daemonStopTimeout)
	for c.localDaemonAnswers() {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for agentd to stop")
		}
		time.Sleep(pollInterval)
	}
	return nil
}

// restartDaemon replaces the local daemon. Sessions run in their own worker
// processes and survive it, so a restart is always safe; the new daemon
// picks them up from state.db.
func (c *client) restartDaemon() error {
	if c.localDaemonAnswers() {
		if err := c.shutdownDaemon(true); err != nil {
			return err
		}
	}
	if err := c.spawnDaemon(); err != nil {
		return err
	}
	return c.ensureCompatible()
}

func (c *client) upgradeDaemon() error {
	current, err := c.managementStatus()
	if err != nil {
		return err
	}
	fmt.Printf("✓ Current daemon `%s`\n", current.DaemonVersion)
	fmt.Printf("✓ Current client `%s`\n", Version)
	fmt.Println("✓ Restarting daemon to upgrade")
	exe, err := daemonExecutable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "upgrade")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() >= 0 {
				return fmt.Errorf("agentd upgrade exited with status %d", exit.ExitCode())
			}
			return errors.New("agentd upgrade terminated by signal")
		}
		return fmt.Errorf("failed to run agentd upgrade: %w", err)
	}
	if err := c.ensureCompatible(); err != nil {
		return err
	}
	upgraded, err := c.managementStatus()
	if err != nil {
		return err
	}
	fmt.Printf("✓ Upgraded daemon `%s`\n", upgraded.DaemonVersion)
	return nil
}

// describeResponse names a response's kind, for errors about unexpected
// ones.
func describeResponse(r *protocol.Response) string {
	switch {
	case r.DaemonInfo != nil:
		return "DaemonInfo"
	case r.CreateSession != nil:
		return "CreateSession"
	case r.KillSession != nil:
		return "KillSession"
	case r.Attached != nil:
		return "Attached"
	case r.AttachSnapshot != nil:
		return "AttachSnapshot"
	case r.SessionEnded != nil:
		return "SessionEnded"
	case r.InputAccepted != nil:
		return "InputAccepted"
	case r.Session != nil:
		return "Session"
	case r.Sessions != nil:
		return "Sessions"
	case r.Attachments != nil:
		return "Attachments"
	case r.History != nil:
		return "History"
	case r.PtyOutput != nil:
		return "PtyOutput"
	case r.EndOfStream != nil:
		return "EndOfStream"
	case r.Error != nil:
		return "Error"
	case r.Ok != nil:
		return "Ok"
	case r.Workspaces != nil:
		return "Workspaces"
	case r.Workspace != nil:
		return "Workspace"
	case r.Welcome != nil:
		return "Welcome"
	case r.GitState != nil:
		return "GitState"
	case r.Artifacts != nil:
		return "Artifacts"
	case r.ArtifactChunk != nil:
		return "ArtifactChunk"
	case r.Event != nil:
		return "Event"
	case r.Events != nil:
		return "Events"
	}
	return "unknown"
}
