package daemon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

const handoffAgents = `
[agents.counter]
command = "/bin/sh"
args = ["-c", '''stty -echo; i=0; while [ ! -e stop ]; do echo "n $i"; i=$((i+1)); case $i in *00) sleep 0.01;; esac; done; echo "stopped at $i"; while :; do sleep 1; done''']

[agents.stall]
command = "/bin/sh"
args = ["-c", '''stty raw -echo; echo stalled; sleep 6; echo resumed; head -c 1048589 | tail -c 13; exec cat''']
`

// writeScript writes an executable shell script standing in for a broken
// agentd binary.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A handoff to an executable that cannot take over is refused before
// anything is disturbed: the session, its attachments and its agent carry
// on as if nothing happened.
func TestHandoffToABadBinaryKeepsTheSession(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	c := h.attachRestartable(id)
	before := h.session(id)

	for name, exe := range map[string]string{
		"missing":             filepath.Join(t.TempDir(), "agentd"),
		"exits at once":       writeScript(t, "crashing", "exit 1"),
		"not agentd":          writeScript(t, "other", "echo agentd 0.0.1"),
		"a directory":         h.cwd,
		"newer handoff only":  writeScript(t, "future", `echo "agentd session-worker handoff 99"`),
		"hangs on the probe?": writeScript(t, "slow", "exit 3"),
	} {
		results := h.handoff(exe)
		if len(results) != 1 || results[0].HandedOff != nil || results[0].Lost || results[0].Err == nil {
			t.Fatalf("%s: results = %+v", name, results)
		}
		if !strings.Contains(results[0].Err.Error(), "keeps running") {
			t.Fatalf("%s: error does not say the session keeps running: %v", name, results[0].Err)
		}
	}
	// The attachment was never interrupted.
	c.input("still-here\n")
	c.expectOutput("got:still-here")
	if after := h.session(id); after.Status != session.StatusRunning || *after.WorkerPID != *before.WorkerPID {
		t.Fatalf("record = %+v", after)
	}
}

// A handoff that gets as far as stopping the session's I/O and then cannot
// finish (here: the agent is not reading its input, which must not be lost)
// puts everything back: clients reattach to the same image, output flows,
// and the queued input is delivered once the agent reads again.
func TestFailedHandoffRestoresTheSession(t *testing.T) {
	h := newHarnessWithConfig(t, handoffAgents)
	if resp := h.create("stall", "stall", h.cwd); resp.CreateSession == nil {
		t.Fatalf("create: %v", resp.Error)
	}
	c := h.attachRestartable("stall")
	h.eventually("the agent to stall", func() bool { return strings.Contains(h.history("stall"), "stalled") })
	// Far more input than the kernel buffers for a PTY (about 4 KiB on
	// macOS; Linux keeps up to 64 KiB in the PTY's buffer besides the line
	// discipline's 4 KiB), so the input writer is stuck in a write until
	// the agent reads.
	chunk := strings.Repeat("x", 16<<10)
	for range 64 {
		h.sendInput("stall", chunk)
	}
	h.sendInput("stall", "end-of-input\n")

	results := h.handoff(nextBinary(t))
	if len(results) != 1 || results[0].HandedOff != nil || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "not reading its input") {
		t.Fatalf("results = %+v", results)
	}
	c.expectRestarting()

	// The old image serves again: a new attachment, output once the agent
	// wakes up, and every byte of the input that was queued: the agent
	// reads exactly the 1 MiB and the marker after it, and prints the
	// marker.
	c2 := h.attachRestartable("stall")
	c2.expectOutput("resumed")
	c2.expectOutput("end-of-input")
	if rec := h.session("stall"); rec.Status != session.StatusRunning {
		t.Fatalf("status = %s", rec.Status)
	}
}

// If the new image dies after the exec, nothing can bring the old one back:
// the session is lost, and that is reported and recorded rather than left
// looking alive. The agent is hung up with its terminal, not orphaned.
func TestHandoffToABinaryThatDiesAfterExec(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	rec := h.session(id)
	// Passes the probe, then exits instead of resuming.
	exe := writeScript(t, "liar", `if [ "$2" = --handoff-probe ]; then echo "agentd session-worker handoff 1"; exit 0; fi; exit 1`)

	results := h.handoff(exe)
	if len(results) != 1 || !results[0].Lost || results[0].HandedOff != nil {
		t.Fatalf("results = %+v", results)
	}
	// The daemon that spawned the worker reaps it and records why.
	h.eventually("the lost session to be recorded", func() bool {
		return h.session(id).Status == session.StatusFailed
	})
	if !waitForExit(int(*rec.AgentPID), testTimeout) {
		t.Fatal("the agent outlived its worker's failed handoff")
	}
}

// Repeated handoffs while the agent writes output nonstop and a client stays
// attached: every line the agent wrote is in the shadow terminal exactly
// once and in order (output written while the PTY was not being read waited
// in the kernel for the next image), and the client followed every restart.
func TestRepeatedHandoffsLoseNoOutput(t *testing.T) {
	h := newHarnessWithConfig(t, handoffAgents)
	if resp := h.create("counter", "counter", h.cwd); resp.CreateSession == nil {
		t.Fatalf("create: %v", resp.Error)
	}
	pid := *h.session("counter").WorkerPID

	// The follower reattaches whenever the worker restarts, as the CLI
	// does, until the session ends. It owns its connections; the test
	// waits for it at the end.
	restarts := make(chan int, 1)
	go func() {
		n := 0
		defer func() { restarts <- n }()
		for {
			conn, err := transport.DialUnix(h.paths.Socket, testTimeout)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			r := bufio.NewReader(conn)
			protocol.WriteRequest(conn, &protocol.Request{AttachSession: &protocol.AttachSession{
				SessionID: "counter", Kind: session.AttachmentAttach, Geometry: defaultGeometry,
				Features: []string{protocol.CapSessionRestart},
			}})
			restarted := false
			for !restarted {
				conn.SetReadDeadline(time.Now().Add(testTimeout))
				resp, err := protocol.ReadResponse(r)
				switch {
				case err != nil || resp == nil:
					t.Errorf("attach stream ended without a final frame: %v", err)
					conn.Close()
					return
				case resp.SessionRestarting != nil:
					n++
					restarted = true
				case resp.Attached == nil && resp.PtyOutput == nil:
					conn.Close()
					return // the session ended
				}
			}
			conn.Close()
		}
	}()

	next := nextBinary(t)
	for i := range 5 {
		exe := next
		if i%2 == 1 {
			exe = workerBin
		}
		time.Sleep(100 * time.Millisecond)
		results := h.handoff(exe)
		if len(results) != 1 || results[0].HandedOff == nil || results[0].HandedOff.WorkerPID != pid || results[0].HandedOff.Handoffs != uint32(i+1) {
			t.Fatalf("handoff %d: %+v", i+1, results)
		}
	}
	if err := os.WriteFile(filepath.Join(h.cwd, "stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var history string
	h.eventually("the counter to stop", func() bool {
		history = h.history("counter")
		return strings.Contains(history, "stopped at")
	})

	// Scrollback is bounded, so the oldest lines may have scrolled out;
	// what is there must be consecutive and end where the agent stopped.
	var stoppedAt int
	if _, err := fmt.Sscanf(history[strings.Index(history, "stopped at"):], "stopped at %d", &stoppedAt); err != nil {
		t.Fatal(err)
	}
	expect, first := -1, -1
	for _, line := range strings.Split(history, "\n") {
		line = strings.TrimSpace(line)
		var n int
		if _, err := fmt.Sscanf(line, "n %d", &n); err != nil || line != fmt.Sprintf("n %d", n) {
			continue
		}
		if expect >= 0 && n != expect {
			t.Fatalf("line %d follows line %d: output was lost or duplicated", n, expect-1)
		}
		if first < 0 {
			first = n
		}
		expect = n + 1
	}
	if expect != stoppedAt {
		t.Fatalf("last line %d, but the agent stopped at %d", expect-1, stoppedAt)
	}
	t.Logf("lines %d-%d of %d retained, consecutive, across 5 handoffs", first, expect-1, stoppedAt)

	if _, err := h.srv.killSession("counter", false); err != nil {
		t.Fatal(err)
	}
	if n := <-restarts; n != 5 {
		t.Fatalf("the attached client followed %d of 5 restarts", n)
	}
}

// An exec that fails (here the executable vanishes between the probe and
// the exec) leaves the old image running: it takes its socket, pump and
// clients back, and the session carries on as before.
func TestHandoffExecFailureKeepsTheSession(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	c := h.attachRestartable(id)
	c.input("before\n")
	c.expectOutput("got:before")
	before := h.session(id)
	exe := writeScript(t, "vanishing", `echo "agentd session-worker handoff 1"; rm -f "$0"`)

	results := h.handoff(exe)
	if len(results) != 1 || results[0].HandedOff != nil || results[0].Lost || results[0].Err == nil ||
		!strings.Contains(results[0].Err.Error(), "failed to execute") {
		t.Fatalf("results = %+v", results)
	}
	c.expectRestarting()
	c2 := h.attachRestartable(id)
	if c2.attachID != "attach-2" || !strings.Contains(string(c2.snapshot), "got:before") {
		t.Fatalf("reattach after the failed exec: %s, %q", c2.attachID, c2.snapshot)
	}
	c2.input("after\n")
	c2.expectOutput("got:after")
	if after := h.session(id); after.Status != session.StatusRunning || *after.WorkerPID != *before.WorkerPID {
		t.Fatalf("record = %+v", after)
	}
	// And it can still hand off for real.
	if results := h.handoff(nextBinary(t)); len(results) != 1 || results[0].HandedOff == nil {
		t.Fatalf("handoff after a failed one: %+v", results)
	}
}
