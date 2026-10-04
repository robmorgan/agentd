package worker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// These tests run the real worker (Run) in-process against a real PTY and
// /bin/sh, talking to it over its Unix socket exactly as the daemon does.

const testTimeout = 10 * time.Second

// echoAgent echoes each line back as got:<line>. "size" prints the PTY size,
// "where" prints the working directory and cwd environment, "flood" writes a
// burst of output ("bigflood" about 9 MB of it), "stall" stops reading input for a few seconds, "done"
// exits 0, "quit" exits 3.
const echoAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    quit) exit 3;;
    done) exit 0;;
    size) stty size;;
    stall) stty raw; sleep 3; stty -raw; echo unstalled;;
    bye) i=0; while [ $i -lt 2000 ]; do echo "tail $i"; i=$((i+1)); done; echo final-line; exit 0;;
    where) echo "pwd:$(pwd -P)"; echo "cwd:$AGENTD_CWD";;
    flood) i=0; while [ $i -lt 20000 ]; do echo "line $i padding padding padding padding"; i=$((i+1)); done; echo flood-done;;
    bigflood) seq -f "line %g padding padding padding padding" 1 200000; echo flood-done;;
    *) echo "got:$l";;
  esac
done`

var defaultGeometry = protocol.Geometry{Cols: 80, Rows: 24}

type harness struct {
	t         testing.TB
	createdAt string
	uid       string
	paths     *paths.AppPaths
	store     *db.Database
	sessionID string
	done      chan error
	exited    bool
}

func startWorker(t testing.TB) *harness {
	t.Helper()
	return startWorkerWith(t, echoAgent)
}

// newRoot creates a runtime root with one session row and points
// AGENTD_DIR at it.
func newRoot(t testing.TB) (string, *harness) {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "agdw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("AGENTD_DIR", dir)

	p := paths.FromRoot(dir)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(p.Database)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, paths: p, store: store, sessionID: "test-session", done: make(chan error, 1)}
	h.uid = db.NewUID()
	h.createdAt, err = store.InsertSession(db.NewSession{
		SessionID: h.sessionID, UID: h.uid, Agent: "sh", Mode: session.ModeExecute, Cwd: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, h
}

func startWorkerWith(t testing.TB, script string) *harness {
	t.Helper()
	dir, h := newRoot(t)
	p := h.paths
	go func() {
		h.done <- Run(Args{
			SessionID: h.sessionID, Cwd: dir, CreatedAt: h.createdAt, UID: h.uid,
			AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", script},
		})
	}()
	t.Cleanup(h.cleanup)

	socket := p.SessionSocketPath(h.sessionID)
	deadline := time.Now().Add(testTimeout)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-h.done:
			t.Fatalf("worker exited during startup: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker socket %s never came up", socket)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h
}

// cleanup kills a still-running agent so a failed test does not leak it.
func (h *harness) cleanup() {
	if h.exited {
		return
	}
	if rec, _ := h.store.GetSession(h.sessionID); rec != nil && rec.AgentPID != nil {
		_ = syscall.Kill(int(*rec.AgentPID), syscall.SIGKILL)
	}
	select {
	case <-h.done:
	case <-time.After(testTimeout):
		h.t.Error("worker did not exit during cleanup")
	}
}

func (h *harness) waitExit() {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.exited = true
		if err != nil {
			h.t.Fatalf("worker returned error: %v", err)
		}
	case <-time.After(testTimeout):
		h.t.Fatal("worker did not exit")
	}
}

func (h *harness) dial() net.Conn {
	h.t.Helper()
	conn, err := net.Dial("unix", h.paths.SessionSocketPath(h.sessionID))
	if err != nil {
		h.t.Fatal(err)
	}
	return conn
}

// request performs a one-shot request/response exchange.
func (h *harness) request(req *protocol.Request) *protocol.Response {
	h.t.Helper()
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTimeout))
	if err := protocol.WriteRequest(conn, req); err != nil {
		h.t.Fatal(err)
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(conn))
	if err != nil {
		h.t.Fatal(err)
	}
	if resp == nil {
		h.t.Fatal("worker closed connection without a response")
	}
	return resp
}

func (h *harness) sendInput(data string) {
	h.t.Helper()
	resp := h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: h.sessionID, Data: []byte(data)}})
	if resp.InputAccepted == nil {
		h.t.Fatalf("send input: unexpected response %#v", resp)
	}
}

func (h *harness) attachments() []session.AttachmentRecord {
	h.t.Helper()
	resp := h.request(&protocol.Request{ListAttachments: &protocol.SessionRef{SessionID: h.sessionID}})
	if resp.Attachments == nil {
		h.t.Fatalf("list attachments: unexpected response %#v", resp)
	}
	return *resp.Attachments
}

func (h *harness) history() string {
	h.t.Helper()
	resp := h.request(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: h.sessionID}})
	if resp.History == nil {
		h.t.Fatalf("history: unexpected response %#v", resp)
	}
	return resp.History.Data
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type client struct {
	t        testing.TB
	conn     net.Conn
	r        *bufio.Reader
	attachID string
	snapshot []byte
	output   bytes.Buffer
}

func (h *harness) attach(g protocol.Geometry) *client {
	h.t.Helper()
	c := &client{t: h.t, conn: h.dial()}
	c.r = bufio.NewReader(c.conn)
	h.t.Cleanup(func() { c.conn.Close() })
	c.send(&protocol.Request{AttachSession: &protocol.AttachSession{
		SessionID: h.sessionID, Kind: session.AttachmentAttach, Geometry: g,
	}})
	resp := c.read()
	if resp.Attached == nil {
		h.t.Fatalf("attach: unexpected response %#v", resp)
	}
	c.attachID = resp.Attached.AttachID
	c.snapshot = resp.Attached.Snapshot
	return c
}

func (c *client) send(req *protocol.Request) {
	c.t.Helper()
	if err := protocol.WriteRequest(c.conn, req); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) input(data string) {
	c.t.Helper()
	c.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: []byte(data)}})
}

func (c *client) read() *protocol.Response {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	resp, err := protocol.ReadResponse(c.r)
	if err != nil {
		c.t.Fatalf("read response: %v (output so far %q)", err, c.output.String())
	}
	if resp == nil {
		c.t.Fatalf("connection closed (output so far %q)", c.output.String())
	}
	return resp
}

// expectOutput reads PTY output until want has been seen.
func (c *client) expectOutput(want string) {
	c.t.Helper()
	for !strings.Contains(c.output.String(), want) {
		resp := c.read()
		if resp.PtyOutput == nil {
			c.t.Fatalf("waiting for %q: unexpected response %#v", want, resp)
		}
		c.output.Write(resp.PtyOutput.Data)
	}
}

// expectEnd skips PTY output and returns the first other response.
func (c *client) expectEnd() *protocol.Response {
	c.t.Helper()
	for {
		resp := c.read()
		if resp.PtyOutput == nil {
			return resp
		}
		c.output.Write(resp.PtyOutput.Data)
	}
}

func TestAttachDetachReattach(t *testing.T) {
	h := startWorker(t)

	c := h.attach(defaultGeometry)
	if c.attachID != "attach-1" {
		t.Fatalf("attach id = %q, want attach-1", c.attachID)
	}
	c.input("hello\n")
	c.expectOutput("got:hello")
	if got := h.attachments(); len(got) != 1 || got[0].AttachID != "attach-1" || got[0].Kind != session.AttachmentAttach {
		t.Fatalf("attachments = %#v", got)
	}

	// Drop the client without detaching, as a closed laptop would.
	c.conn.Close()
	h.eventually("attachment to be released", func() bool { return len(h.attachments()) == 0 })

	// The agent keeps running with nobody attached.
	h.sendInput("again\n")
	h.eventually("output while detached", func() bool { return strings.Contains(h.history(), "got:again") })

	// A new connection reattaches to the same session and is rehydrated
	// with the current screen, including output produced while detached.
	c2 := h.attach(defaultGeometry)
	if c2.attachID != "attach-2" {
		t.Fatalf("reattach id = %q, want attach-2", c2.attachID)
	}
	for _, want := range []string{"got:hello", "got:again"} {
		if !bytes.Contains(c2.snapshot, []byte(want)) {
			t.Fatalf("reattach snapshot missing %q: %q", want, c2.snapshot)
		}
	}
	c2.input("still-here\n")
	c2.expectOutput("got:still-here")

	c2.input("done\n")
	end := c2.expectEnd()
	if end.SessionEnded == nil {
		t.Fatalf("expected SessionEnded, got %#v", end)
	}
	if end.SessionEnded.Status != session.StatusExited || end.SessionEnded.ExitCode == nil || *end.SessionEnded.ExitCode != 0 {
		t.Fatalf("session ended = %#v", end.SessionEnded)
	}
	h.waitExit()

	rec, err := h.store.GetSession(h.sessionID)
	if err != nil || rec == nil {
		t.Fatalf("get session: %v", err)
	}
	if rec.Status != session.StatusExited || rec.WorkerPID != nil || rec.AgentPID != nil {
		t.Fatalf("final record = %#v", rec)
	}
	if _, err := os.Stat(h.paths.SessionSocketPath(h.sessionID)); !os.IsNotExist(err) {
		t.Fatalf("worker socket not removed: %v", err)
	}
	logged, err := os.ReadFile(h.paths.RenderedLogPath(h.sessionID))
	if err != nil || !strings.Contains(string(logged), "got:still-here") {
		t.Fatalf("rendered log = %q, %v", logged, err)
	}
}

func TestMultipleAttachers(t *testing.T) {
	h := startWorker(t)
	a := h.attach(defaultGeometry)
	b := h.attach(defaultGeometry)

	a.input("one\n")
	a.expectOutput("got:one")
	b.expectOutput("got:one")
	b.input("two\n")
	a.expectOutput("got:two")
	b.expectOutput("got:two")

	if got := h.attachments(); len(got) != 2 {
		t.Fatalf("attachments = %#v", got)
	}

	resp := h.request(&protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: h.sessionID, AttachID: a.attachID}})
	if resp.Ok == nil {
		t.Fatalf("detach attachment: %#v", resp)
	}
	if end := a.expectEnd(); end.EndOfStream == nil {
		t.Fatalf("detached client got %#v", end)
	}
	b.input("three\n")
	b.expectOutput("got:three")

	resp = h.request(&protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: h.sessionID, AttachID: "attach-99"}})
	if resp.Error == nil {
		t.Fatalf("detaching unknown attachment: %#v", resp)
	}

	resp = h.request(&protocol.Request{DetachSession: &protocol.DetachSession{SessionID: h.sessionID, All: true}})
	if resp.Ok == nil {
		t.Fatalf("detach all: %#v", resp)
	}
	if end := b.expectEnd(); end.EndOfStream == nil {
		t.Fatalf("detached client got %#v", end)
	}
	h.eventually("attachments to be released", func() bool { return len(h.attachments()) == 0 })

	h.sendInput("done\n")
	h.waitExit()
}

func TestResize(t *testing.T) {
	h := startWorker(t)
	c := h.attach(protocol.Geometry{Cols: 100, Rows: 30})
	c.input("size\n")
	c.expectOutput("30 100")

	c.send(&protocol.Request{AttachResize: &protocol.Geometry{Cols: 120, Rows: 40}})
	c.input("size\n")
	c.expectOutput("40 120")

	// The most recent attacher's geometry wins.
	c2 := h.attach(protocol.Geometry{Cols: 90, Rows: 20})
	c2.input("size\n")
	c2.expectOutput("20 90")

	c2.input("done\n")
	c2.expectEnd()
	h.waitExit()
}

func TestZeroGeometryKeepsCurrentSize(t *testing.T) {
	h := startWorker(t)
	c := h.attach(protocol.Geometry{Cols: 100, Rows: 30})
	c2 := h.attach(protocol.Geometry{})
	c.send(&protocol.Request{AttachResize: &protocol.Geometry{}})
	c2.input("size\n")
	c2.expectOutput("30 100")
	c2.input("done\n")
	c2.expectEnd()
	h.waitExit()
}

func TestAgentFailureEndsSession(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.input("quit\n")
	end := c.expectEnd()
	if end.SessionEnded == nil || end.SessionEnded.Status != session.StatusFailed {
		t.Fatalf("expected failed SessionEnded, got %#v", end)
	}
	if end.SessionEnded.Error == nil || *end.SessionEnded.Error != "agent exited with code 3" {
		t.Fatalf("session error = %v", end.SessionEnded.Error)
	}
	h.waitExit()
}

func TestKillSession(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.input("hi\n")
	c.expectOutput("got:hi")

	resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: h.sessionID}})
	if resp.Ok == nil {
		t.Fatalf("kill: %#v", resp)
	}
	// A requested kill is a deliberate stop, not a failure.
	end := c.expectEnd()
	if end.SessionEnded == nil || end.SessionEnded.Status != session.StatusExited {
		t.Fatalf("expected exited SessionEnded after kill, got %#v", end)
	}
	h.waitExit()
	if logged, err := os.ReadFile(h.paths.RenderedLogPath(h.sessionID)); err != nil || !strings.Contains(string(logged), "got:hi") {
		t.Fatalf("rendered log after kill = %q, %v", logged, err)
	}
}

func TestKillEscalatesToSIGKILL(t *testing.T) {
	old := agentKillGrace
	agentKillGrace = 200 * time.Millisecond
	t.Cleanup(func() { agentKillGrace = old })

	h := startWorkerWith(t, `trap '' TERM; echo stubborn; while :; do sleep 1; done`)
	h.eventually("agent to start", func() bool { return strings.Contains(h.history(), "stubborn") })
	c := h.attach(defaultGeometry)
	h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: h.sessionID}})
	if end := c.expectEnd(); end.SessionEnded == nil || end.SessionEnded.Status != session.StatusExited {
		t.Fatalf("expected exited SessionEnded, got %#v", end)
	}
	h.waitExit()
}

func TestMissingCwdMarksSessionFailed(t *testing.T) {
	dir, h := newRoot(t)
	err := Run(Args{
		SessionID: h.sessionID, Cwd: filepath.Join(dir, "nope"), UID: h.uid,
		AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", echoAgent},
	})
	h.exited = true
	if err == nil {
		t.Fatal("Run succeeded with a missing cwd")
	}
	rec, _ := h.store.GetSession(h.sessionID)
	if rec == nil || rec.Status != session.StatusFailed || rec.Error == nil || !strings.Contains(*rec.Error, "does not exist") {
		t.Fatalf("record = %#v", rec)
	}
}

func TestClientDisconnectDuringOutput(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.input("flood\n")
	c.expectOutput("line 1")
	c.conn.Close()

	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })
	h.eventually("attachment to be released", func() bool { return len(h.attachments()) == 0 })
	h.sendInput("done\n")
	h.waitExit()
}

// A client that stops reading must not stall the PTY or other clients, and
// must not wedge or crash the worker when it finally goes away after the
// session has ended. Fan-out is lossy under lag (see broadcaster), so this
// asserts liveness rather than that the fast client saw every byte.
func TestSlowAttacherDoesNotStallOthers(t *testing.T) {
	h := startWorker(t)
	slow := h.attach(defaultGeometry) // never reads
	fast := h.attach(defaultGeometry)

	fast.input("flood\n")
	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })
	fast.pingUntilEchoed()

	fast.input("done\n")
	if end := fast.expectEnd(); end.SessionEnded == nil {
		t.Fatalf("fast client got %#v", end)
	}
	// Run gives up on the stuck client after shutdownGrace.
	h.waitExit()

	// Unblocking the stuck handler now runs its cleanup against a stopped
	// owner; that used to panic with a send on a closed channel.
	slow.conn.Close()
	time.Sleep(100 * time.Millisecond)
}

// pingUntilEchoed proves the attachment is live even if some of its output
// was dropped while it lagged: the echo arrives as output, or within the
// snapshot of a resync that replaced output it fell behind on. Frames are
// only ever read whole (a read deadline that fires mid-frame would leave
// the reader out of step with the stream).
func (c *client) pingUntilEchoed() {
	c.t.Helper()
	c.input("ping\n")
	for !strings.Contains(c.output.String(), "got:ping") {
		resp := c.read()
		switch {
		case resp.PtyOutput != nil:
			c.output.Write(resp.PtyOutput.Data)
		case resp.AttachResync != nil:
			c.output.Write(resp.AttachResync.Data)
		default:
			c.t.Fatalf("ping: unexpected response %#v", resp)
		}
	}
}

func rawFrame(version, kind uint16, payload []byte) []byte {
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:4], 0x4147_4450)
	binary.LittleEndian.PutUint16(header[4:6], version)
	binary.LittleEndian.PutUint16(header[6:8], kind)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(payload)))
	return append(header[:], payload...)
}

func TestMalformedFramesDoNotKillWorker(t *testing.T) {
	h := startWorker(t)
	cases := map[string][]byte{
		"garbage":           []byte("this is not a frame at all"),
		"version mismatch":  rawFrame(protocol.ProtocolVersion-1, 12, nil),
		"unknown kind":      rawFrame(protocol.ProtocolVersion, 999, nil),
		"truncated header":  rawFrame(protocol.ProtocolVersion, 12, nil)[:9],
		"truncated payload": rawFrame(protocol.ProtocolVersion, 9, make([]byte, 64))[:40],
	}
	for name, frame := range cases {
		conn := h.dial()
		conn.SetDeadline(time.Now().Add(testTimeout))
		if _, err := conn.Write(frame); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		conn.(*net.UnixConn).CloseWrite()
		// The worker drops the connection without a well-formed reply.
		resp, err := protocol.ReadResponse(bufio.NewReader(conn))
		if resp != nil && resp.Error == nil {
			t.Fatalf("%s: unexpected response %#v", name, resp)
		}
		if err != nil && !isConnClosed(err) {
			t.Fatalf("%s: read: %v", name, err)
		}
		conn.Close()
	}

	// The session is unaffected.
	h.sendInput("still-alive\n")
	h.eventually("session to keep running", func() bool { return strings.Contains(h.history(), "got:still-alive") })
	h.sendInput("done\n")
	h.waitExit()
}

func isConnClosed(err error) bool {
	return err == io.EOF || strings.Contains(err.Error(), "connection reset")
}

// TestAgentRunsInCwd checks that the agent process starts in Args.Cwd and
// sees it through AGENTD_CWD. The directory is
// deliberately not a git repository.
func TestAgentRunsInCwd(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	// A fast agent prints "ready" before the attach, and then it is in the
	// snapshot rather than the output stream.
	if !bytes.Contains(c.snapshot, []byte("ready")) {
		c.expectOutput("ready")
	}
	c.input("where\n")
	real, err := filepath.EvalSymlinks(h.paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	c.expectOutput("pwd:" + real)
	c.expectOutput("cwd:" + h.paths.Root)
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}

// TestMissingCwdFailsBeforeSpawn checks that a bad cwd is refused up front
// rather than surfacing as an opaque spawn failure.
func TestMissingCwdFailsBeforeSpawn(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "agdw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("AGENTD_DIR", dir)

	err = Run(Args{
		SessionID: "missing", Cwd: filepath.Join(dir, "does-not-exist"), UID: "u",
		AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", "exit 0"},
	})
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("expected cwd error, got %v", err)
	}
	if _, statErr := os.Stat(paths.FromRoot(dir).SessionSocketPath("missing")); !os.IsNotExist(statErr) {
		t.Fatalf("worker socket should not exist after a refused cwd: %v", statErr)
	}
}

// An agent that stops reading its input while a client keeps sending must not
// wedge the session: the PTY keeps draining, requests keep being answered,
// and excess input is refused rather than blocking.
func TestUnreadInputDoesNotStallSession(t *testing.T) {
	h := startWorker(t)
	h.sendInput("stall\n")
	chunk := strings.Repeat("x", 4096) + "\n"
	refused := false
	for i := 0; i < 2*inputQueueDepth && !refused; i++ {
		resp := h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: h.sessionID, Data: []byte(chunk)}})
		if resp.Error != nil {
			if !strings.Contains(resp.Error.Message, "not reading its input") {
				t.Fatalf("unexpected error %q", resp.Error.Message)
			}
			refused = true
		}
	}
	if !refused {
		t.Fatal("input was never refused while the agent was not reading")
	}
	// The session still answers while the agent is stalled.
	start := time.Now()
	h.history()
	if time.Since(start) > time.Second {
		t.Fatalf("history took %v while input was backed up", time.Since(start))
	}
	h.eventually("agent to recover", func() bool { return strings.Contains(h.history(), "unstalled") })
	h.cleanup()
	h.exited = true
}

// Agents such as Claude Code and Codex query the terminal at startup and
// wait for the answer. With no terminal attached the worker answers on the
// terminal's behalf; with one attached it leaves the answer to the client.
func TestTerminalQueriesAnsweredWhileDetached(t *testing.T) {
	// The agent asks for the cursor position and reports how many reply
	// bytes arrived within a second.
	const querier = `stty -echo; read go; stty raw -echo min 0 time 10; printf 'ask\033[6n'; n=$(dd bs=1 count=6 2>/dev/null | wc -c | tr -d ' '); stty sane; echo "replybytes:$n"; read l`

	detached := startWorkerWith(t, querier)
	detached.sendInput("go\n")
	detached.eventually("query reply", func() bool { return strings.Contains(detached.history(), "replybytes:6") })
	detached.sendInput("\n")
	detached.waitExit()

	attached := startWorkerWith(t, querier)
	c := attached.attach(defaultGeometry)
	c.input("go\n")
	c.expectOutput("\x1b[6n") // the query reaches the attached terminal
	c.expectOutput("replybytes:")
	c.expectOutput("\n")
	if !strings.Contains(c.output.String(), "replybytes:0") {
		t.Fatalf("worker answered a query while a terminal was attached: %q", c.output.String())
	}
	c.input("\n")
	c.expectEnd()
	attached.waitExit()
}

func TestInputQueueIsBoundedInBytes(t *testing.T) {
	block := make(chan struct{})
	in := newPTYInput(blockingWriter{block})
	defer func() { close(block); in.close(); <-in.done }()
	if err := in.enqueue(make([]byte, maxQueuedInput+1)); err != errInputTooLarge {
		t.Fatalf("oversized input: %v", err)
	}
	chunk := make([]byte, 1<<20)
	accepted := 0
	for range 16 {
		if in.enqueue(chunk) != nil {
			break
		}
		accepted++
	}
	// The writer holds one chunk; the queue holds at most maxQueuedInput.
	if accepted*len(chunk) > maxQueuedInput+len(chunk) {
		t.Fatalf("accepted %d MiB while the agent was not reading", accepted)
	}
	if err := in.enqueue(chunk); err != errInputQueueFull {
		t.Fatalf("got %v, want queue full", err)
	}
}

type blockingWriter struct{ block chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.block
	return len(p), nil
}

// A kill stops the whole process group, including descendants that ignore
// SIGTERM and outlive the agent itself.
func TestKillStopsDescendantsThatIgnoreSIGTERM(t *testing.T) {
	h := startWorkerWith(t, `(trap '' TERM HUP; exec sleep 300) & echo "child:$!"; wait`)
	var child int
	childPID := regexp.MustCompile(`child:(\d+)`)
	h.eventually("child pid", func() bool {
		m := childPID.FindStringSubmatch(h.history())
		if m == nil {
			return false
		}
		child, _ = strconv.Atoi(m[1])
		return child > 0
	})
	h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: h.sessionID}})
	h.waitExit()
	deadline := time.Now().Add(testTimeout)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(child, syscall.SIGKILL)
			t.Fatal("a descendant that ignored SIGTERM survived the kill")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The agent's last output must reach both the attached client and the saved
// history even though the agent exits immediately after writing it.
func TestFinalOutputIsNotLost(t *testing.T) {
	for i := range 5 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h := startWorker(t)
			c := h.attach(defaultGeometry)
			c.input("bye\n")
			if end := c.expectEnd(); end.SessionEnded == nil {
				t.Fatalf("got %#v", end)
			}
			if !strings.Contains(c.output.String(), "final-line") {
				t.Fatal("attached client missed the final output")
			}
			h.waitExit()
			logged, err := os.ReadFile(h.paths.RenderedLogPath(h.sessionID))
			if err != nil || !strings.Contains(string(logged), "final-line") {
				t.Fatalf("saved history missed the final output (%v)", err)
			}
		})
	}
}

// Detaching a client that has stopped reading releases the attachment even
// though its handler is blocked writing to it.
func TestDetachReleasesAStuckAttachment(t *testing.T) {
	h := startWorker(t)
	stuck := h.attach(defaultGeometry) // never reads
	h.sendInput("flood\n")
	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })
	resp := h.request(&protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: h.sessionID, AttachID: stuck.attachID}})
	if resp.Ok == nil {
		t.Fatalf("detach: %#v", resp)
	}
	h.eventually("stuck attachment to be released", func() bool { return len(h.attachments()) == 0 })
	h.sendInput("done\n")
	h.waitExit()
}

// After an AttachSnapshot the client must not receive output the snapshot
// already contains.
func TestSnapshotIsAnExactBoundary(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.input("flood\n")
	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })
	// Output from the flood is still queued for this client; the snapshot
	// supersedes it.
	c.send(&protocol.Request{AttachSnapshot: protocol.Empty})
	var snap *protocol.Response
	for snap == nil {
		resp := c.read()
		if resp.AttachSnapshot != nil {
			snap = resp
		}
	}
	c.output.Reset()
	c.input("after\n")
	c.expectOutput("got:after")
	if strings.Contains(c.output.String(), "line ") {
		t.Fatalf("output already in the snapshot was replayed after it: %.200q", c.output.String())
	}
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}
