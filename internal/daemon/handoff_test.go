package daemon

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// These tests hand real session workers over to another agentd executable
// (a copy of the test build) while their agents run, and check that nothing
// about the session is lost on the way.

var (
	nextBinOnce sync.Once
	nextBinPath string
	nextBinErr  error
)

// nextBinary is a second agentd executable, standing in for an upgrade.
func nextBinary(t *testing.T) string {
	t.Helper()
	nextBinOnce.Do(func() {
		data, err := os.ReadFile(workerBin)
		if err != nil {
			nextBinErr = err
			return
		}
		nextBinPath = filepath.Join(filepath.Dir(workerBin), "agentd-next")
		nextBinErr = os.WriteFile(nextBinPath, data, 0o755)
	})
	if nextBinErr != nil {
		t.Fatal(nextBinErr)
	}
	return nextBinPath
}

// attachRestartable attaches as the CLI does when the daemon supports
// CapSessionRestart.
func (h *harness) attachRestartable(id string) *client {
	h.t.Helper()
	c := &client{t: h.t, conn: h.dial()}
	c.r = bufio.NewReader(c.conn)
	h.t.Cleanup(func() { c.conn.Close() })
	c.send(&protocol.Request{AttachSession: &protocol.AttachSession{
		SessionID: id, Kind: session.AttachmentAttach, Geometry: defaultGeometry,
		Features: []string{protocol.CapSessionRestart},
	}})
	resp := c.read()
	if resp.Attached == nil {
		h.t.Fatalf("attach: unexpected response %#v (error %v)", resp, resp.Error)
	}
	c.attachID = resp.Attached.AttachID
	c.snapshot = resp.Attached.Snapshot
	return c
}

// expectRestarting reads output until the worker says it is restarting.
func (c *client) expectRestarting() {
	c.t.Helper()
	resp := c.expectEnd()
	if resp.SessionRestarting == nil {
		c.t.Fatalf("expected SessionRestarting, got %#v (error %v)", resp, resp.Error)
	}
}

func (h *harness) handoff(exe string) []HandoffResult {
	h.t.Helper()
	results, err := HandoffSessions(h.paths, exe)
	if err != nil {
		h.t.Fatal(err)
	}
	return results
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

func TestLiveHandoffKeepsTheSession(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("live")
	c := h.attachRestartable(id)
	c.input("before\n")
	c.expectOutput("got:before")
	before := h.session(id)
	vtBefore, plainBefore := h.historyVT(id), h.history(id)

	next := nextBinary(t)
	results := h.handoff(next)
	if len(results) != 1 || results[0].HandedOff == nil {
		t.Fatalf("handoff results = %+v", results)
	}
	got := results[0].HandedOff
	if got.SessionID != id || got.WorkerPID != *before.WorkerPID || got.Handoffs != 1 || !sameFile(got.Executable, next) {
		t.Fatalf("handed off = %+v, want pid %d running %s", got, *before.WorkerPID, next)
	}
	c.expectRestarting()

	after := h.session(id)
	if after.Status != session.StatusRunning || *after.WorkerPID != *before.WorkerPID || *after.AgentPID != *before.AgentPID {
		t.Fatalf("record after handoff = %+v", after)
	}
	if !processExists(int(*before.AgentPID)) {
		t.Fatal("the agent died in the handoff")
	}
	// The new image's terminal is the old one's, byte for byte (the agent
	// is idle, waiting for input).
	if vt, plain := h.historyVT(id), h.history(id); vt != vtBefore || plain != plainBefore {
		t.Fatalf("terminal changed in the handoff:\nVT before %q\nVT after  %q", vtBefore, vt)
	}

	// The new image has the old one's screen, keeps counting attachments,
	// and talks to the same agent.
	c2 := h.attachRestartable(id)
	if !bytes.Contains(c2.snapshot, []byte("got:before")) {
		t.Fatalf("snapshot after handoff lost earlier output: %q", c2.snapshot)
	}
	if c2.attachID != "attach-2" {
		t.Fatalf("attach id after handoff = %s, want attach-2", c2.attachID)
	}
	c2.input("after\n")
	c2.expectOutput("got:after")
	h.sendInput(id, "via-send-input\n")
	c2.expectOutput("got:via-send-input")

	// The new image collects the agent's exit status, though the old one
	// started it.
	c2.input("quit\n")
	end := c2.expectEnd()
	if end.SessionEnded == nil || end.SessionEnded.Status != session.StatusFailed || deref(end.SessionEnded.Error) != "agent exited with code 3" {
		t.Fatalf("end = %#v (%v)\n%s", end.SessionEnded, deref(end.SessionEnded.Error), workerLog(h, id))
	}
	if hist := h.history(id); !strings.Contains(hist, "got:before") || !strings.Contains(hist, "got:after") {
		t.Fatalf("history = %q", hist)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func workerLog(h *harness, id string) string {
	data, _ := os.ReadFile(h.paths.WorkerLogPath(id))
	return string(data)
}

func (h *harness) historyVT(id string) string {
	h.t.Helper()
	resp := h.request(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: id, VT: true}})
	if resp.History == nil {
		h.t.Fatalf("history %s: %#v", id, resp.Error)
	}
	return resp.History.Data
}
