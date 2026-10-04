package daemon

import (
	"bufio"
	"runtime"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
)

// statsSupported reports whether procstat has an OS reader here.
func statsSupported() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

func checkProcess(t *testing.T, what string, p protocol.ProcessStats, goProcess bool) {
	t.Helper()
	if p.PID == 0 {
		t.Errorf("%s: no pid", what)
	}
	if statsSupported() {
		if p.RSSBytes < 1<<20 && goProcess || p.RSSBytes == 0 || p.Threads == 0 || p.OpenFDs == 0 {
			t.Errorf("%s: implausible OS numbers %+v", what, p)
		}
		if p.PrivateBytes == 0 {
			t.Errorf("%s: no private memory %+v", what, p)
		}
	}
	if goProcess && (p.Goroutines == 0 || p.GoHeapBytes == 0 || p.GoRuntimeBytes < p.GoHeapBytes || p.UptimeNanos == 0) {
		t.Errorf("%s: implausible Go runtime numbers %+v", what, p)
	}
	if !goProcess && (p.Goroutines != 0 || p.GoHeapBytes != 0) {
		t.Errorf("%s: Go numbers for a non-Go process %+v", what, p)
	}
}

func TestSessionStats(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("stats")
	rec := h.session(id)

	c := h.attach(id)
	c.input("hello\n")
	c.expectOutput("got:hello")

	resp := h.request(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: id}})
	if resp.SessionStats == nil {
		t.Fatalf("stats: %#v", resp.Error)
	}
	s := resp.SessionStats
	t.Logf("%+v", s)
	if s.SessionID != id || s.Snapshot != nil {
		t.Errorf("stats = %+v", s)
	}
	if s.Worker.PID != *rec.WorkerPID || s.Agent.PID != *rec.AgentPID {
		t.Errorf("pids = %d/%d, want %d/%d", s.Worker.PID, s.Agent.PID, *rec.WorkerPID, *rec.AgentPID)
	}
	checkProcess(t, "worker", s.Worker, true)
	checkProcess(t, "agent", s.Agent, false)
	if s.Cols != defaultGeometry.Cols || s.Rows != defaultGeometry.Rows || s.Attachments != 1 ||
		s.OutputBytes < uint64(len("ready\r\ngot:hello")) || s.ScrollbackLimitBytes == 0 {
		t.Errorf("terminal stats = %+v", s)
	}

	// A one-shot stream gets the base encoding; a control stream that
	// negotiated it also gets the terminal's memory.
	if s.Terminal != nil {
		t.Errorf("terminal memory sent to a client that did not ask: %+v", s.Terminal)
	}
	cc := h.control()
	cc.send(1, &protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: id}})
	if _, r := cc.read(); r.SessionStats == nil || r.SessionStats.Terminal == nil || r.SessionStats.Terminal.Pages == 0 || r.SessionStats.Terminal.ResidentBytes == 0 {
		t.Errorf("stats on a control stream = %#v", r)
	}

	resp = h.request(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: id, Snapshot: true}})
	if resp.SessionStats == nil || resp.SessionStats.Snapshot == nil {
		t.Fatalf("stats with snapshot: %#v", resp)
	}
	snap := resp.SessionStats.Snapshot
	if snap.Bytes == 0 || snap.FormatNanos == 0 || snap.RestoreNanos == 0 {
		t.Errorf("snapshot stats = %+v", snap)
	}

	wantError(t, h.request(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: "missing"}}), "not found")
	wantError(t, h.request(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: "../x"}}), "not found")
}

func TestDaemonStats(t *testing.T) {
	h := newHarness(t)
	resp := h.request(&protocol.Request{GetDaemonStats: protocol.Empty})
	if resp.DaemonStats == nil {
		t.Fatalf("daemon stats: %#v", resp.Error)
	}
	// The test binary is the daemon here.
	checkProcess(t, "daemon", resp.DaemonStats.Daemon, true)
	if resp.DaemonStats.OpenStreams < 1 {
		t.Errorf("open streams = %d; this request's stream is one", resp.DaemonStats.OpenStreams)
	}
}

func TestStatsAreAdvertised(t *testing.T) {
	h := newHarness(t)
	conn := h.dial()
	defer conn.Close()
	if err := protocol.WriteRequest(conn, &protocol.Request{Hello: protocol.NewHello("test")}); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	resp := c.read()
	if resp.Welcome == nil || !protocol.HasCapability(resp.Welcome.Capabilities, protocol.CapRuntimeStats) {
		t.Fatalf("welcome = %#v", resp)
	}
}
