package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

func onlineProbe(name string, cpus uint32, running int, agents []string, workspaces ...string) *hostProbe {
	p := &hostProbe{name: name, status: hostOnline, workspaces: workspaces, latency: 1500 * time.Microsecond,
		welcome: &protocol.Welcome{Version: 1, DaemonVersion: "0.1.0",
			Host: protocol.HostInfo{CPUs: cpus, Agents: agents, DefaultAgent: agents[0]}}}
	if name != localHostName {
		p.host = &transport.Host{Name: name, Address: name + ":7433"}
	}
	for i := 0; i < running; i++ {
		p.sessions = append(p.sessions, session.Record{SessionID: fmt.Sprint(i), Status: session.StatusRunning})
	}
	p.sessions = append(p.sessions, session.Record{SessionID: "old", Status: session.StatusExited})
	return p
}

func TestChoosePlacement(t *testing.T) {
	local := onlineProbe(localHostName, 8, 4, []string{"claude", "codex"}, "mono")
	devbox := onlineProbe("devbox", 32, 4, []string{"claude"}, "mono")
	gpu := onlineProbe("gpu", 64, 0, []string{"codex"})
	down := &hostProbe{name: "down", status: hostOffline}

	p, err := choosePlacement([]*hostProbe{local, devbox, gpu, down}, "claude", "mono")
	if err != nil || p.probe != devbox || !strings.Contains(p.reason, "4 running on 32 CPUs") {
		t.Fatalf("claude in mono: %+v, %v", p, err)
	}
	// The lightest host wins when it has what the session needs.
	if p, _ := choosePlacement([]*hostProbe{local, devbox, gpu, down}, "codex", ""); p.probe != gpu {
		t.Fatalf("codex: %s", p.probe.name)
	}
	// "" is each host's default agent; ties go to the host listed first.
	if p, _ := choosePlacement([]*hostProbe{onlineProbe(localHostName, 4, 1, []string{"sh"}), onlineProbe("b", 4, 1, []string{"sh"})}, "", ""); p.probe.name != localHostName {
		t.Fatalf("tie: %s", p.probe.name)
	}
	_, err = choosePlacement([]*hostProbe{gpu, down}, "claude", "")
	if err == nil || !strings.Contains(err.Error(), "gpu has no agent `claude`") || !strings.Contains(err.Error(), "down is offline") {
		t.Fatalf("no candidate: %v", err)
	}
	if _, err := choosePlacement([]*hostProbe{gpu}, "codex", "mono"); err == nil || !strings.Contains(err.Error(), "no workspace `mono`") {
		t.Fatalf("missing workspace: %v", err)
	}
}

func TestClassifyProbe(t *testing.T) {
	changed := &hostKeyChangedError{changed: &transport.KeyChangedError{Pinned: "a", Presented: "b"}, msg: "changed"}
	for err, want := range map[error]string{
		nil:                          hostOnline,
		changed:                      hostKeyChanged,
		fmt.Errorf("x: %w", changed): hostKeyChanged,
		context.DeadlineExceeded:     hostOffline,
		fmt.Errorf("dial: %w", context.DeadlineExceeded): hostOffline,
		errors.New("unexpected response"):                hostError,
	} {
		if got := classifyProbe(err); got != want {
			t.Errorf("classifyProbe(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestRenderHosts(t *testing.T) {
	var out bytes.Buffer
	renderHosts(&out, []*hostProbe{
		onlineProbe(localHostName, 8, 2, []string{"claude", "codex"}),
		{name: "gpu", address: "10.0.0.9:7433", status: hostOffline, err: context.DeadlineExceeded},
		{name: "new", address: "10.0.0.7:7433", status: hostUnauthorized, err: errors.New("refused")},
	})
	got := out.String()
	for _, want := range []string{
		"NAME   ADDRESS         STATUS        LATENCY  SESSIONS  CPUS  AGENTS        VERSION",
		"local  (this machine)  online        1.5ms    2/3       8     claude,codex  0.1.0",
		"gpu    10.0.0.9:7433   offline       -",
		"new: refused",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	// An offline host's timeout is its status, not an error line.
	if strings.Contains(got, "gpu:") {
		t.Errorf("offline host has an error line:\n%s", got)
	}
}

func TestReservedHostNames(t *testing.T) {
	for _, name := range []string{localHostName, autoHostName} {
		if err := transport.ValidHostName(name); err == nil {
			t.Errorf("%s accepted as a host name", name)
		}
	}
	a := &app{paths: testClient(t).paths, host: autoHostName}
	if _, err := a.connect(nil, "", nil); err == nil || !strings.Contains(err.Error(), "only chooses a host for `agent new`") {
		t.Fatalf("--host auto: %v", err)
	}
}
