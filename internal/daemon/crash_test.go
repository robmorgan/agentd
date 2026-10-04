package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// Crash recovery: the daemon or a worker dies without warning (SIGKILL), at
// any point, and the next daemon must find every session either still
// running and fully usable, or recorded as ended, never something in
// between, and never leave an agent running unsupervised.

// startDaemonProcess runs `agentd serve` on the harness root as a separate
// process, which a test can SIGKILL, in place of the harness's in-process
// daemon.
func (h *harness) startDaemonProcess() *exec.Cmd {
	h.t.Helper()
	h.stop()
	log, err := os.OpenFile(filepath.Join(h.paths.Root, "serve.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(workerBin, "serve")
	cmd.Env = append(os.Environ(), "AGENTD_DIR="+h.paths.Root)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	h.eventually("the daemon process to serve", func() bool {
		conn, err := transport.DialUnix(h.paths.Socket, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		st := h.management(&protocol.ManagementRequest{Status: protocol.Empty}).Status
		return st != nil && int(st.PID) == cmd.Process.Pid
	})
	return cmd
}

func sigkill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
}

// A daemon killed outright, with sessions running and a client attached,
// takes nothing with it: the next daemon adopts the sessions, and attach,
// send-input and kill all work on them.
func TestDaemonSIGKILLWithLiveSessions(t *testing.T) {
	h := newHarness(t)
	daemon := h.startDaemonProcess()
	a := h.mustCreate("alpha")
	b := h.mustCreate("beta")
	c := h.attach(a)
	c.input("before\n")
	c.expectOutput("got:before")
	pids := map[string]*session.Record{a: h.session(a), b: h.session(b)}

	sigkill(t, daemon)
	c.expectClosed()
	for id, rec := range pids {
		if !processExists(int(*rec.WorkerPID)) || !processExists(int(*rec.AgentPID)) {
			t.Fatalf("%s died with the daemon", id)
		}
	}

	// The lock died with the process and the stale socket is replaced.
	h.start()
	for id, rec := range pids {
		if got := h.session(id); got.Status != session.StatusRunning || *got.WorkerPID != *rec.WorkerPID {
			t.Fatalf("%s after the crash: %+v", id, got)
		}
	}
	c2 := h.attach(a)
	if !bytes.Contains(c2.snapshot, []byte("got:before")) {
		t.Fatalf("snapshot after the crash: %q", c2.snapshot)
	}
	h.sendInput(a, "after\n")
	c2.expectOutput("got:after")
	if resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: b}}); resp.KillSession == nil || !resp.KillSession.WasRunning {
		t.Fatalf("kill after the crash: %#v (%v)", resp, resp.Error)
	}
	if st := h.session(b).Status; st != session.StatusExited {
		t.Fatalf("killed session status = %s", st)
	}
	if !waitForExit(int(*pids[b].AgentPID), testTimeout) {
		t.Fatal("kill did not stop the adopted agent")
	}
}

// A worker killed outright hangs up its PTY: the kernel sends the agent
// (whose controlling terminal it was) SIGHUP, so the agent is not left
// orphaned, and the session is recorded failed.
//
// An agent that ignores SIGHUP would survive, reparented to init, with a
// dead terminal. agentd deliberately does not then signal the pid it
// recorded: by the time anyone looks, that pid may belong to an unrelated
// process (see TestRecycledPidsAreNeverSignalled).
func TestWorkerSIGKILLHangsUpTheAgent(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	rec := h.session(id)
	if err := syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.eventually("the crash to be recorded", func() bool {
		return h.session(id).Status == session.StatusFailed
	})
	if !waitForExit(int(*rec.AgentPID), testTimeout) {
		t.Fatal("the agent outlived its worker")
	}
	wantError(t, h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: id, Data: []byte("x")}}), "is not running")
}

// The daemon is killed while it is creating sessions, at random points of
// the create: before the row exists, while the worker starts, or just after.
// The next daemon must leave each session either running and usable or
// recorded as failed, and an agent whose session failed must not run on.
func TestCrashDuringCreate(t *testing.T) {
	h := newHarnessWithConfig(t, `
[agents.pidfile]
command = "/bin/sh"
args = ["-c", '''echo $$ > "$AGENTD_SESSION_ID.pid"; stty -echo; while IFS= read -r l; do echo "got:$l"; done''']
`)
	const rounds = 6
	for i := range rounds {
		daemon := h.startDaemonProcess()
		conn := h.dial()
		name := fmt.Sprintf("crash-%d", i)
		if err := protocol.WriteRequest(conn, &protocol.Request{CreateSession: &protocol.CreateSession{
			Cwd: h.cwd, Name: &name, Agent: "pidfile",
		}}); err != nil {
			t.Fatal(err)
		}
		// Anywhere from before the request is read to after the worker
		// is up.
		time.Sleep(time.Duration(rand.IntN(40)) * time.Millisecond)
		sigkill(t, daemon)
		conn.Close()
	}

	h.start()
	outcomes := map[string]int{}
	for i := range rounds {
		id := fmt.Sprintf("crash-%d", i)
		resp := h.request(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}})
		if resp.Session == nil {
			outcomes["never created"]++
			continue
		}
		// A worker that was still starting either finishes (and is
		// adopted) or is refused by the new daemon's reconciliation and
		// stops its agent; give it time to do either.
		var rec *session.Record
		h.eventually(id+" to settle", func() bool {
			rec = h.session(id)
			return rec.Status != session.StatusCreating
		})
		agentPID := 0
		if data, err := os.ReadFile(filepath.Join(h.cwd, id+".pid")); err == nil {
			agentPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		switch rec.Status {
		case session.StatusRunning:
			outcomes["running"]++
			c := h.attach(id)
			c.input("alive\n")
			c.expectOutput("got:alive")
			c.conn.Close()
		case session.StatusFailed:
			outcomes["failed"]++
			if agentPID > 0 && !waitForExit(agentPID, testTimeout) {
				t.Fatalf("%s failed (%s) but its agent (pid %d) runs on; worker log:\n%s", id, deref(rec.Error), agentPID, workerLog(h, id))
			}
			if h.srv.workerAnswers(id) {
				t.Fatalf("%s failed but its worker still answers", id)
			}
		default:
			t.Fatalf("%s: status %s after a crash during create", id, rec.Status)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}

// `agentd upgrade` with live sessions: the daemon is replaced, the session's
// worker is handed to the new binary with its agent still running, and an
// attached client gets back to the same agent.
func TestUpgradeWithLiveSessions(t *testing.T) {
	h := newHarness(t)
	// Upgrade starts the new daemon as `<exe> serve`, which finds the root
	// through AGENTD_DIR.
	t.Setenv("AGENTD_DIR", h.paths.Root)
	id := h.mustCreate("upgraded")
	c := h.attachRestartable(id)
	c.input("before\n")
	c.expectOutput("got:before")
	before := h.session(id)
	t.Cleanup(func() {
		// Stop the daemon Upgrade started.
		if conn, err := transport.DialUnix(h.paths.Socket, time.Second); err == nil {
			conn.SetDeadline(time.Now().Add(testTimeout))
			protocol.WriteManagementRequest(conn, &protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}})
			protocol.ReadManagementResponse(newBufReader(conn))
			conn.Close()
		}
	})

	next := nextBinary(t)
	results, err := Upgrade(h.paths, next)
	if err != nil {
		t.Fatal(err)
	}
	h.waitServeReturned()
	if len(results) != 1 || results[0].HandedOff == nil || results[0].HandedOff.WorkerPID != *before.WorkerPID {
		t.Fatalf("results = %+v", results)
	}
	st := h.management(&protocol.ManagementRequest{Status: protocol.Empty}).Status
	if st == nil || int(st.PID) == os.Getpid() {
		t.Fatalf("the daemon was not replaced: %+v", st)
	}
	// The old daemon's proxy closed the attachment; the client attaches
	// again through the new daemon, to the same agent.
	c.expectClosed()
	c2 := h.attachRestartable(id)
	if !bytes.Contains(c2.snapshot, []byte("got:before")) {
		t.Fatalf("snapshot after upgrade: %q", c2.snapshot)
	}
	c2.input("after\n")
	c2.expectOutput("got:after")
	if after := h.session(id); after.Status != session.StatusRunning || *after.AgentPID != *before.AgentPID {
		t.Fatalf("record after upgrade = %+v", after)
	}
}

func newBufReader(conn transport.Stream) *bufio.Reader { return bufio.NewReader(conn) }
