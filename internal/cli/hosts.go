package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// localHostName names this machine's own daemon wherever hosts are listed
// or chosen. It cannot be used for a remote host, and neither can
// autoHostName, which asks `agent new` to choose a host.
const (
	localHostName = "local"
	autoHostName  = "auto"
)

// probeTimeout bounds one host probe: connecting, the handshake, and
// listing its sessions. Hosts are probed in parallel, so a whole listing
// takes about this long when a host is down.
const probeTimeout = 4 * time.Second

// Host states, as listed by `agent host ls`.
const (
	hostOnline       = "online"
	hostOffline      = "offline"
	hostNotRunning   = "not running"
	hostUnauthorized = "unauthorized"
	hostKeyChanged   = "key changed"
	hostError        = "error"
)

// hostProbe is what probing one daemon found.
type hostProbe struct {
	name    string // localHostName for this machine's daemon
	address string
	host    *transport.Host // nil for the local daemon
	status  string
	err     error
	// latency is the round trip of one request on an established
	// connection, so it measures the network and the daemon, not the
	// handshake.
	latency    time.Duration
	welcome    *protocol.Welcome
	sessions   []session.Record
	workspaces []string // only when asked for
}

func (h *hostProbe) online() bool { return h.status == hostOnline }

// running counts the host's running sessions.
func (h *hostProbe) running() int {
	n := 0
	for _, s := range h.sessions {
		if s.Status == session.StatusRunning {
			n++
		}
	}
	return n
}

// hasAgent reports whether the host can run agent ("" means its default).
func (h *hostProbe) hasAgent(agent string) bool {
	return h.welcome != nil && (agent == "" || slices.Contains(h.welcome.Host.Agents, agent))
}

// probeHosts probes the local daemon (only if it is running: listing hosts
// never starts one) and every host in hosts.toml, in parallel, and returns
// them local first, then in file order. withWorkspaces also lists each
// host's workspaces.
func (a *app) probeHosts(withWorkspaces bool) ([]*hostProbe, error) {
	hosts, err := transport.ReadHosts(a.paths.HostsPath())
	if err != nil {
		return nil, err
	}
	probes := []*hostProbe{{name: localHostName, address: a.paths.Socket}}
	for i := range hosts {
		probes = append(probes, &hostProbe{name: hosts[i].Name, address: hosts[i].Address, host: &hosts[i]})
	}
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Go(func() { a.probe(p, withWorkspaces) })
	}
	wg.Wait()
	return probes, nil
}

func (a *app) probe(p *hostProbe, withWorkspaces bool) {
	c := &client{paths: a.paths, host: p.host}
	defer c.close()
	if p.host == nil && !c.localDaemonAnswers() {
		p.status = hostNotRunning
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	p.err = func() error {
		cs, err := c.controlStream(ctx)
		if err != nil {
			return err
		}
		p.welcome = cs.welcome
		start := time.Now()
		resp, err := cs.roundTrip(ctx, &protocol.Request{ListSessions: protocol.Empty})
		p.latency = time.Since(start)
		if err != nil {
			return err
		}
		if resp.Sessions == nil {
			return fmt.Errorf("unexpected response: %s", describeResponse(resp))
		}
		p.sessions = *resp.Sessions
		if !withWorkspaces {
			return nil
		}
		resp, err = cs.roundTrip(ctx, &protocol.Request{ListWorkspaces: protocol.Empty})
		if err != nil {
			return err
		}
		if resp.Workspaces != nil {
			for _, w := range *resp.Workspaces {
				p.workspaces = append(p.workspaces, w.Name)
			}
		}
		return nil
	}()
	p.status = classifyProbe(p.err)
}

// classifyProbe turns a probe's error into a host state.
func classifyProbe(err error) string {
	var changed *transport.KeyChangedError
	switch {
	case err == nil:
		return hostOnline
	case errors.As(err, &changed):
		return hostKeyChanged
	case transport.IsKeyRefused(err):
		return hostUnauthorized
	case transport.IsConnectionLost(err), errors.Is(err, context.DeadlineExceeded):
		return hostOffline
	}
	return hostError
}

// renderHosts writes the host table of `agent host ls`.
func renderHosts(w io.Writer, probes []*hostProbe) {
	rows := [][]string{{"NAME", "ADDRESS", "STATUS", "LATENCY", "SESSIONS", "CPUS", "AGENTS", "VERSION"}}
	for _, p := range probes {
		row := []string{p.name, p.address, p.status, "-", "-", "-", "-", "-"}
		if p.name == localHostName {
			row[1] = "(this machine)"
		}
		if p.online() {
			h := p.welcome.Host
			row[3] = formatLatency(p.latency)
			row[4] = fmt.Sprintf("%d/%d", p.running(), len(p.sessions))
			row[5] = fmt.Sprint(h.CPUs)
			row[6] = strings.Join(h.Agents, ",")
			row[7] = p.welcome.DaemonVersion
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			fmt.Fprintf(&line, "%-*s", widths[i], escapeControls(cell))
		}
		fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
	for _, p := range probes {
		if p.err != nil && p.status != hostOffline {
			fmt.Fprintf(w, "%s: %v\n", p.name, p.err)
		}
	}
}

func formatLatency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// renderHostInfo writes `agent host info`: the machine and daemon behind a
// host.
func renderHostInfo(w io.Writer, p *hostProbe) {
	fmt.Fprintf(w, "name: %s\n", p.name)
	if p.host != nil {
		fmt.Fprintf(w, "address: %s\n", p.host.Address)
		fmt.Fprintf(w, "fingerprint: %s\n", p.host.Fingerprint)
	} else {
		fmt.Fprintf(w, "socket: %s\n", p.address)
	}
	fmt.Fprintf(w, "status: %s\n", p.status)
	if p.err != nil {
		fmt.Fprintf(w, "error: %v\n", p.err)
	}
	if !p.online() {
		return
	}
	h := p.welcome.Host
	fmt.Fprintf(w, "latency: %s\n", formatLatency(p.latency))
	fmt.Fprintf(w, "hostname: %s\n", escapeControls(h.Name))
	fmt.Fprintf(w, "platform: %s/%s\n", escapeControls(h.OS), escapeControls(h.Arch))
	fmt.Fprintf(w, "cpus: %d\n", h.CPUs)
	if h.MemoryBytes > 0 {
		fmt.Fprintf(w, "memory: %s\n", formatBytes(h.MemoryBytes))
	}
	fmt.Fprintf(w, "agents: %s\n", escapeControls(strings.Join(h.Agents, ", ")))
	fmt.Fprintf(w, "default_agent: %s\n", escapeControls(h.DefaultAgent))
	fmt.Fprintf(w, "sessions: %d running, %d total\n", p.running(), len(p.sessions))
	fmt.Fprintf(w, "daemon_version: %s\n", escapeControls(p.welcome.DaemonVersion))
	fmt.Fprintf(w, "protocol_version: %d\n", p.welcome.Version)
	fmt.Fprintf(w, "capabilities: %s\n", escapeControls(strings.Join(p.welcome.Capabilities, ", ")))
}

// placement is where `agent new --host auto` runs a session, and why.
type placement struct {
	probe  *hostProbe
	reason string
}

// choosePlacement picks a host for a new session: one that is online, has
// the agent, and has the workspace when one is named, with the fewest
// running sessions per CPU; ties go to this machine, then to the host
// listed first. It is a hint, not a scheduler: it knows nothing about what
// the sessions are doing.
func choosePlacement(probes []*hostProbe, agent, workspace string) (*placement, error) {
	var candidates []*hostProbe
	var skipped []string
	for _, p := range probes {
		switch {
		case !p.online():
			skipped = append(skipped, fmt.Sprintf("%s is %s", p.name, p.status))
		case !p.hasAgent(agent):
			skipped = append(skipped, fmt.Sprintf("%s has no agent `%s`", p.name, agent))
		case workspace != "" && !slices.Contains(p.workspaces, workspace):
			skipped = append(skipped, fmt.Sprintf("%s has no workspace `%s`", p.name, workspace))
		default:
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no host can run this session: %s", strings.Join(skipped, "; "))
	}
	load := func(p *hostProbe) float64 { return float64(p.running()) / float64(max(p.welcome.Host.CPUs, 1)) }
	best := slices.MinFunc(candidates, func(x, y *hostProbe) int { return cmp.Compare(load(x), load(y)) })
	return &placement{
		probe: best,
		reason: fmt.Sprintf("%d running on %d CPUs, the lightest of %d %s", best.running(), best.welcome.Host.CPUs,
			len(candidates), plural(len(candidates), "candidate", "candidates")),
	}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
