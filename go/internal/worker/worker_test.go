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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// These tests run the real worker (Run) in-process against a real PTY and
// /bin/sh, talking to it over its Unix socket exactly as the daemon does.

const testTimeout = 10 * time.Second

// echoAgent echoes each line back as got:<line>. "size" prints the PTY size,
// "where" prints the working directory and cwd environment, "flood" writes a
// burst of output, "done" exits 0, "quit" exits 3.
const echoAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    quit) exit 3;;
    done) exit 0;;
    size) stty size;;
    where) echo "pwd:$(pwd -P)"; echo "cwd:$AGENTD_CWD"; echo "ws:$AGENTD_WORKSPACE";;
    flood) i=0; while [ $i -lt 20000 ]; do echo "line $i padding padding padding padding"; i=$((i+1)); done; echo flood-done;;
    *) echo "got:$l";;
  esac
done`

var defaultGeometry = protocol.Geometry{Cols: 80, Rows: 24}

type harness struct {
	t         *testing.T
	paths     *paths.AppPaths
	store     *db.Database
	sessionID string
	done      chan error
	exited    bool
}

func startWorker(t *testing.T) *harness {
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
	if err := store.InsertSession(db.NewSession{
		SessionID: h.sessionID, Agent: "sh", Mode: session.ModeExecute, Cwd: dir,
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		h.done <- Run(Args{
			SessionID: h.sessionID, Cwd: dir,
			AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", echoAgent},
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
	t        *testing.T
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
	end := c.expectEnd()
	if end.SessionEnded == nil || end.SessionEnded.Status != session.StatusFailed {
		t.Fatalf("expected failed SessionEnded after kill, got %#v", end)
	}
	h.waitExit()
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
// was dropped while it lagged: it keeps sending pings until one echoes back.
func (c *client) pingUntilEchoed() {
	c.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for i := 0; ; i++ {
		c.input(fmt.Sprintf("ping%d\n", i))
		c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			resp, err := protocol.ReadResponse(c.r)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					break
				}
				c.t.Fatalf("ping: %v", err)
			}
			if resp == nil || resp.PtyOutput == nil {
				c.t.Fatalf("ping: unexpected response %#v", resp)
			}
			c.output.Write(resp.PtyOutput.Data)
			if strings.Contains(c.output.String(), "got:ping") {
				return
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatal("attachment never echoed a ping")
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
// sees it through AGENTD_CWD and the AGENTD_WORKSPACE alias. The directory is
// deliberately not a git repository.
func TestAgentRunsInCwd(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.expectOutput("ready")
	c.input("where\n")
	real, err := filepath.EvalSymlinks(h.paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	c.expectOutput("pwd:" + real)
	c.expectOutput("cwd:" + h.paths.Root)
	c.expectOutput("ws:" + h.paths.Root)
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
		SessionID: "missing", Cwd: filepath.Join(dir, "does-not-exist"),
		AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", "exit 0"},
	})
	if err == nil || !strings.Contains(err.Error(), "session cwd") {
		t.Fatalf("expected cwd error, got %v", err)
	}
	if _, statErr := os.Stat(paths.FromRoot(dir).SessionSocketPath("missing")); !os.IsNotExist(statErr) {
		t.Fatalf("worker socket should not exist after a refused cwd: %v", statErr)
	}
}
