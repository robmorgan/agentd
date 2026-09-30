package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/transport"
)

// client reaches one daemon: the local one over its Unix socket, or a
// remote one over QUIC (agent --host NAME).
//
// The protocol's unit is one stream per request or attach session, ended
// by half-closing. Locally that is one Unix connection; remotely it is one
// QUIC stream on a connection the process opens once, lazily, and reuses.
type client struct {
	paths *paths.AppPaths
	// host is the remote daemon, or nil for the local one.
	host *transport.Host

	mu   sync.Mutex
	quic *transport.QUICClient
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
		return nil, fmt.Errorf("the key of host `%s` (%s) has changed!\n  pinned:    %s\n  presented: %s\n"+
			"This could mean someone is intercepting the connection, or the daemon's key was recreated.\n"+
			"If you trust the new key, run `agent host rm %s` and add the host again.",
			c.host.Name, c.host.Address, c.host.Fingerprint, changed.Presented, c.host.Name)
	case err != nil:
		return nil, fmt.Errorf("could not reach agentd on `%s` (%s): %w", c.host.Name, c.host.Address, err)
	}
	c.quic = conn
	return conn, nil
}

// close ends the remote connection, if any, so the daemon sees this client
// go away at once rather than after an idle timeout.
func (c *client) close() {
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
func (c *client) request(req *protocol.Request, timeout time.Duration) (*protocol.Response, error) {
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

func (c *client) ensureCompatible() error {
	info, err := c.daemonInfo()
	if err != nil {
		return err
	}
	if info.ProtocolVersion != protocol.ProtocolVersion {
		return fmt.Errorf("agentd `%s` speaks protocol %d but agent `%s` needs protocol %d; %s",
			info.DaemonVersion, info.ProtocolVersion, Version, protocol.ProtocolVersion, upgradeHint)
	}
	return nil
}

func incompatibleDaemonMessage() string {
	return fmt.Sprintf("agentd does not speak agent protocol %d; %s", protocol.ProtocolVersion, upgradeHint)
}

func (c *client) daemonInfo() (*protocol.DaemonInfo, error) {
	resp, err := c.request(&protocol.Request{GetDaemonInfo: protocol.Empty}, c.controlTimeout())
	switch {
	case err != nil && c.host != nil:
		// Remote connect errors already say what to do.
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%s: %w", incompatibleDaemonMessage(), err)
	case resp.Error != nil:
		return nil, errors.New(resp.Error.Message)
	case resp.DaemonInfo == nil:
		return nil, fmt.Errorf("unexpected response: %s", describeResponse(resp))
	}
	return resp.DaemonInfo, nil
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
	}
	return "unknown"
}
