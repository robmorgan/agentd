package daemon

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// These tests run the daemon in-process (so the race detector covers it)
// against real session workers: a freshly built agentd binary, spawned as
// separate processes exactly as in production, each owning a real PTY.

const testTimeout = 15 * time.Second

var workerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentd-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	workerBin = filepath.Join(dir, "agentd")
	build := exec.Command("go", "build", "-o", workerBin, "../../cmd/agentd")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building agentd for daemon tests:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// echoAgent echoes each line back as got:<line>. "where" prints the working
// directory, "done" exits 0, "quit" exits 3.
const echoAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    quit) exit 3;;
    done) exit 0;;
    where) echo "pwd:$(pwd -P)";;
    *) echo "got:$l";;
  esac
done`

const testConfig = `default_agent = "sh"

[agents.sh]
command = "/bin/sh"
args = ["-c", '''` + echoAgent + `''']

[agents.stubborn]
command = "/bin/sh"
args = ["-c", "trap '' TERM; echo stubborn; while :; do sleep 1; done"]

[git]
auto_commit_message = "ignored by the Go daemon"
`

var defaultGeometry = protocol.Geometry{Cols: 80, Rows: 24}

type harness struct {
	t      *testing.T
	paths  *paths.AppPaths
	cwd    string
	srv    *Server
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir().
	root, err := os.MkdirTemp("/tmp", "agdd-")
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "work")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	p := paths.FromRoot(root)
	if err := os.WriteFile(p.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, paths: p, cwd: cwd}
	t.Cleanup(func() {
		h.stop()
		h.killWorkers()
		os.RemoveAll(root)
	})
	h.start()
	return h
}

// start runs a daemon on the harness root and waits for its socket.
func (h *harness) start() {
	h.t.Helper()
	srv, err := New(h.paths, workerBin)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.srv, h.cancel, h.done = srv, cancel, make(chan error, 1)
	go func() { h.done <- srv.Serve(ctx) }()
	h.eventually("daemon socket", func() bool {
		conn, err := net.Dial("unix", h.paths.Socket)
		if err == nil {
			conn.Close()
		}
		return err == nil
	})
}

// stop shuts the daemon down (sessions keep running) and waits for Serve.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Serve returned %v", err)
		}
	case <-time.After(testTimeout):
		h.t.Error("daemon did not stop")
	}
}

func (h *harness) waitServeReturned() {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.cancel = nil
		if err != nil {
			h.t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(testTimeout):
		h.t.Fatal("daemon did not stop")
	}
}

// killWorkers makes sure no test leaks a worker or agent process.
func (h *harness) killWorkers() {
	srv, err := New(h.paths, workerBin)
	if err != nil {
		return
	}
	recs, _ := srv.db.ListSessions()
	for _, rec := range recs {
		if rec.AgentPID != nil {
			_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
		}
		if rec.WorkerPID != nil {
			_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
		}
	}
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

func (h *harness) dial() net.Conn {
	h.t.Helper()
	conn, err := net.Dial("unix", h.paths.Socket)
	if err != nil {
		h.t.Fatal(err)
	}
	return conn
}

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
		h.t.Fatal("daemon closed the connection without a response")
	}
	return resp
}

func (h *harness) management(req *protocol.ManagementRequest) *protocol.ManagementResponse {
	h.t.Helper()
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTimeout))
	if err := protocol.WriteManagementRequest(conn, req); err != nil {
		h.t.Fatal(err)
	}
	resp, err := protocol.ReadManagementResponse(bufio.NewReader(conn))
	if err != nil || resp == nil {
		h.t.Fatalf("management response: %v, %v", resp, err)
	}
	return resp
}

func (h *harness) create(name, agent, cwd string) *protocol.Response {
	h.t.Helper()
	var n *string
	if name != "" {
		n = &name
	}
	return h.request(&protocol.Request{CreateSession: &protocol.CreateSession{Cwd: cwd, Name: n, Agent: agent}})
}

func (h *harness) mustCreate(name string) string {
	h.t.Helper()
	resp := h.create(name, "sh", h.cwd)
	if resp.CreateSession == nil {
		h.t.Fatalf("create: %#v", resp.Error)
	}
	return resp.CreateSession.SessionID
}

func (h *harness) session(id string) *session.Record {
	h.t.Helper()
	resp := h.request(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}})
	if resp.Session == nil {
		h.t.Fatalf("get session %s: %#v", id, resp.Error)
	}
	return resp.Session
}

func (h *harness) history(id string) string {
	h.t.Helper()
	resp := h.request(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: id}})
	if resp.History == nil {
		h.t.Fatalf("history %s: %#v", id, resp.Error)
	}
	return resp.History.Data
}

func (h *harness) attachments(id string) []session.AttachmentRecord {
	h.t.Helper()
	resp := h.request(&protocol.Request{ListAttachments: &protocol.SessionRef{SessionID: id}})
	if resp.Attachments == nil {
		h.t.Fatalf("attachments %s: %#v", id, resp.Error)
	}
	return *resp.Attachments
}

func (h *harness) sendInput(id, data string) {
	h.t.Helper()
	resp := h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: id, Data: []byte(data)}})
	if resp.InputAccepted == nil {
		h.t.Fatalf("send input: %#v", resp.Error)
	}
}

func wantError(t *testing.T, resp *protocol.Response, substr string) {
	t.Helper()
	if resp.Error == nil || !strings.Contains(resp.Error.Message, substr) {
		t.Fatalf("expected error containing %q, got %#v (error %v)", substr, resp, resp.Error)
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

func (h *harness) attach(id string) *client {
	h.t.Helper()
	c := &client{t: h.t, conn: h.dial()}
	c.r = bufio.NewReader(c.conn)
	h.t.Cleanup(func() { c.conn.Close() })
	c.send(&protocol.Request{AttachSession: &protocol.AttachSession{
		SessionID: id, Kind: session.AttachmentAttach, Geometry: defaultGeometry,
	}})
	resp := c.read()
	if resp.Attached == nil {
		h.t.Fatalf("attach: unexpected response %#v (error %v)", resp, resp.Error)
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

// expectClosed waits for the daemon to close the connection.
func (c *client) expectClosed() {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	for {
		resp, err := protocol.ReadResponse(c.r)
		if err != nil || resp == nil {
			return
		}
	}
}

func TestDaemonInfoAndManagementStatus(t *testing.T) {
	h := newHarness(t)
	resp := h.request(&protocol.Request{GetDaemonInfo: protocol.Empty})
	if resp.DaemonInfo == nil || resp.DaemonInfo.ProtocolVersion != protocol.ProtocolVersion {
		t.Fatalf("daemon info = %#v", resp)
	}
	status := h.management(&protocol.ManagementRequest{Status: protocol.Empty}).Status
	if status == nil || status.PID != uint32(os.Getpid()) || status.Root != h.paths.Root || status.RunningSessions {
		t.Fatalf("status = %#v", status)
	}
	if data, err := os.ReadFile(h.paths.PIDFile); err != nil || strings.TrimSpace(string(data)) != fmt.Sprint(os.Getpid()) {
		t.Fatalf("pid file = %q, %v", data, err)
	}
}

func TestSecondDaemonRefusesToStart(t *testing.T) {
	h := newHarness(t)
	srv, err := New(h.paths, workerBin)
	if err != nil {
		t.Fatal(err)
	}
	srv.lockWait = 100 * time.Millisecond
	if err := srv.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Serve = %v", err)
	}
	// The first daemon is unaffected.
	h.request(&protocol.Request{ListSessions: protocol.Empty})
}

func TestShutdownCleansUpSocketAndPIDFile(t *testing.T) {
	h := newHarness(t)
	resp := h.request(&protocol.Request{ShutdownDaemon: protocol.Empty})
	if resp.Ok == nil {
		t.Fatalf("shutdown: %#v", resp)
	}
	h.waitServeReturned()
	for _, path := range []string{h.paths.Socket, h.paths.PIDFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after shutdown: %v", path, err)
		}
	}
}

// A client on another protocol version gets an Error frame in its own
// framing, not a dropped connection.
func TestProtocolVersionMismatch(t *testing.T) {
	h := newHarness(t)
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTimeout))
	var frame [16]byte
	binary.LittleEndian.PutUint32(frame[0:4], 0x4147_4450)
	binary.LittleEndian.PutUint16(frame[4:6], protocol.ProtocolVersion+1)
	binary.LittleEndian.PutUint16(frame[6:8], 1) // GetDaemonInfo
	if _, err := conn.Write(frame[:]); err != nil {
		t.Fatal(err)
	}
	var header [16]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatal(err)
	}
	if v, k := binary.LittleEndian.Uint16(header[4:6]), binary.LittleEndian.Uint16(header[6:8]); v != protocol.ProtocolVersion+1 || k != 114 {
		t.Fatalf("reply version %d kind %d, want %d/114", v, k, protocol.ProtocolVersion+1)
	}
	payload := make([]byte, binary.LittleEndian.Uint32(header[12:16]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(fmt.Sprintf("protocol version %d", protocol.ProtocolVersion))) {
		t.Fatalf("error payload %q", payload)
	}
}

func TestMalformedFramesDoNotAffectDaemon(t *testing.T) {
	h := newHarness(t)
	for _, frame := range [][]byte{
		[]byte("definitely not a frame"),
		{0x50, 0x44, 0x47, 0x41, 1, 0, 0xe7, 0x03, 0, 0, 0, 0, 0, 0, 0, 0}, // unknown kind 999
		{0x50, 0x44, 0x47, 0x41, 1, 0},                                     // truncated header
	} {
		conn := h.dial()
		conn.Write(frame)
		conn.(*net.UnixConn).CloseWrite()
		conn.SetReadDeadline(time.Now().Add(testTimeout))
		io.Copy(io.Discard, conn)
		conn.Close()
	}
	id := h.mustCreate("")
	if h.session(id).Status != session.StatusRunning {
		t.Fatal("daemon unhealthy after malformed frames")
	}
}

func TestCreateAttachDetachReattach(t *testing.T) {
	h := newHarness(t)
	resp := h.create("demo", "sh", h.cwd)
	if resp.CreateSession == nil {
		t.Fatalf("create: %#v", resp.Error)
	}
	if got := resp.CreateSession; got.SessionID != "demo" || got.Cwd != h.cwd || got.Status != session.StatusRunning {
		t.Fatalf("create result = %#v", got)
	}
	rec := h.session("demo")
	if rec.Status != session.StatusRunning || rec.Cwd != h.cwd || rec.WorkerPID == nil || rec.AgentPID == nil {
		t.Fatalf("record = %#v", rec)
	}

	c := h.attach("demo")
	c.input("where\n")
	real, _ := filepath.EvalSymlinks(h.cwd)
	c.expectOutput("pwd:" + real)
	c.input("hello\n")
	c.expectOutput("got:hello")

	// Disconnect without detaching; the session belongs to the daemon.
	c.conn.Close()
	h.eventually("attachment release", func() bool { return len(h.attachments("demo")) == 0 })
	h.sendInput("demo", "while-away\n")
	h.eventually("output while detached", func() bool { return strings.Contains(h.history("demo"), "got:while-away") })

	c2 := h.attach("demo")
	for _, want := range []string{"got:hello", "got:while-away"} {
		if !bytes.Contains(c2.snapshot, []byte(want)) {
			t.Fatalf("reattach snapshot missing %q", want)
		}
	}
	c2.input("done\n")
	end := c2.expectEnd()
	if end.SessionEnded == nil || end.SessionEnded.Status != session.StatusExited || end.SessionEnded.ExitCode == nil || *end.SessionEnded.ExitCode != 0 {
		t.Fatalf("end = %#v", end)
	}
	h.eventually("exited status", func() bool { return h.session("demo").Status == session.StatusExited })

	// Ended sessions still serve history from the logs the worker wrote.
	if !strings.Contains(h.history("demo"), "got:while-away") {
		t.Fatal("history missing after exit")
	}
	wantError(t, h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: "demo", Data: []byte("x")}}), "is not running")
	wantError(t, h.request(&protocol.Request{AttachSession: &protocol.AttachSession{SessionID: "demo", Kind: session.AttachmentAttach, Geometry: defaultGeometry}}), "is not running")
}

func TestMultipleAttachersThroughDaemon(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	a, b := h.attach(id), h.attach(id)
	a.input("one\n")
	a.expectOutput("got:one")
	b.expectOutput("got:one")

	resp := h.request(&protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: id, AttachID: a.attachID}})
	if resp.Ok == nil {
		t.Fatalf("detach attachment: %#v", resp)
	}
	if end := a.expectEnd(); end.EndOfStream == nil {
		t.Fatalf("detached client got %#v", end)
	}
	b.input("two\n")
	b.expectOutput("got:two")
	if resp := h.request(&protocol.Request{DetachSession: &protocol.DetachSession{SessionID: id, All: true}}); resp.Ok == nil {
		t.Fatalf("detach all: %#v", resp)
	}
	if end := b.expectEnd(); end.EndOfStream == nil {
		t.Fatalf("detached client got %#v", end)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)

	// A missing cwd fails before a worker is spawned; the record is kept,
	// failed, so `agent ls` shows what happened.
	wantError(t, h.create("lost", "sh", filepath.Join(h.cwd, "missing")), "does not exist")
	if rec := h.session("lost"); rec.Status != session.StatusFailed || rec.WorkerPID != nil {
		t.Fatalf("record after bad cwd = %#v", rec)
	}

	wantError(t, h.create("", "sh", "relative/dir"), "absolute path")
	wantError(t, h.create("", "nope", h.cwd), "agent `nope` is not configured")
	wantError(t, h.create("Bad_Name", "sh", h.cwd), "invalid session name")
	wantError(t, h.create("lost", "sh", h.cwd), "already exists")

	// A non-git directory is fine.
	h.mustCreate("plain")

	// Concurrent creates with one name: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan *protocol.Response, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := h.dial()
			defer conn.Close()
			name := "race"
			protocol.WriteRequest(conn, &protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: &name, Agent: "sh"}})
			resp, _ := protocol.ReadResponse(bufio.NewReader(conn))
			results <- resp
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for resp := range results {
		if resp != nil && resp.CreateSession != nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent creates succeeded, want 1", wins)
	}
}

func TestKillAndRemove(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	c := h.attach(id)
	c.input("hi\n")
	c.expectOutput("got:hi")

	resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id}})
	if resp.KillSession == nil || !resp.KillSession.WasRunning || resp.KillSession.Removed {
		t.Fatalf("kill = %#v (%v)", resp, resp.Error)
	}
	if end := c.expectEnd(); end.SessionEnded == nil || end.SessionEnded.Status != session.StatusExited {
		t.Fatalf("attached client got %#v", end)
	}
	if rec := h.session(id); rec.Status != session.StatusExited {
		t.Fatalf("status after kill = %s", rec.Status)
	}
	wantError(t, h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id}}), "is not running")

	resp = h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: true}})
	if resp.KillSession == nil || !resp.KillSession.Removed || resp.KillSession.WasRunning {
		t.Fatalf("rm = %#v (%v)", resp, resp.Error)
	}
	wantError(t, h.request(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}}), "not found")
	for _, path := range []string{h.paths.LogPath(id), h.paths.RenderedLogPath(id), h.paths.WorkerLogPath(id)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived rm: %v", path, err)
		}
	}
}

func TestRemoveRunningSession(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	agentPID := int(*h.session(id).AgentPID)
	resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: true}})
	if resp.KillSession == nil || !resp.KillSession.Removed || !resp.KillSession.WasRunning {
		t.Fatalf("rm = %#v (%v)", resp, resp.Error)
	}
	if processExists(agentPID) {
		t.Fatal("agent still running after rm")
	}
}

func TestKillStubbornAgent(t *testing.T) {
	h := newHarness(t)
	resp := h.create("", "stubborn", h.cwd)
	if resp.CreateSession == nil {
		t.Fatalf("create: %#v", resp.Error)
	}
	id := resp.CreateSession.SessionID
	h.eventually("agent output", func() bool { return strings.Contains(h.history(id), "stubborn") })
	resp = h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id}})
	if resp.KillSession == nil || !resp.KillSession.WasRunning {
		t.Fatalf("kill = %#v (%v)", resp, resp.Error)
	}
	if rec := h.session(id); rec.Status != session.StatusExited {
		t.Fatalf("status = %s", rec.Status)
	}
}

func TestAgentFailureRecorded(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	h.sendInput(id, "quit\n")
	h.eventually("failed status", func() bool { return h.session(id).Status == session.StatusFailed })
	if rec := h.session(id); rec.Error == nil || *rec.Error != "agent exited with code 3" {
		t.Fatalf("record = %#v", rec)
	}
}

func TestShutdownRefusedWhileSessionsRun(t *testing.T) {
	h := newHarness(t)
	h.mustCreate("")
	wantError(t, h.request(&protocol.Request{ShutdownDaemon: protocol.Empty}), "sessions are running")
	res := h.management(&protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{}}).Shutdown
	if res == nil || res.Stopped || !res.RunningSessions {
		t.Fatalf("non-force shutdown = %#v", res)
	}
	if st := h.management(&protocol.ManagementRequest{Status: protocol.Empty}).Status; !st.RunningSessions {
		t.Fatal("status should report running sessions")
	}
}

// The central durability property: the daemon going away (here with an
// attached client mid-session) does not take the session with it, and a new
// daemon picks the session up again.
func TestSessionSurvivesDaemonRestart(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("durable")
	c := h.attach(id)
	c.input("before\n")
	c.expectOutput("got:before")
	workerPID := int(*h.session(id).WorkerPID)

	res := h.management(&protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}}).Shutdown
	if res == nil || !res.Stopped {
		t.Fatalf("force shutdown = %#v", res)
	}
	h.waitServeReturned()
	c.expectClosed()
	if !processExists(workerPID) {
		t.Fatal("worker died with the daemon")
	}

	h.start()
	if rec := h.session(id); rec.Status != session.StatusRunning {
		t.Fatalf("status after restart = %s", rec.Status)
	}
	c2 := h.attach(id)
	if !bytes.Contains(c2.snapshot, []byte("got:before")) {
		t.Fatalf("snapshot after restart missing earlier output: %q", c2.snapshot)
	}
	c2.input("after\n")
	c2.expectOutput("got:after")
	c2.input("done\n")
	if end := c2.expectEnd(); end.SessionEnded == nil || end.SessionEnded.Status != session.StatusExited {
		t.Fatalf("end = %#v", end)
	}
}

func TestWorkerCrashIsRecorded(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	rec := h.session(id)
	_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
	h.eventually("crash to be recorded", func() bool { return h.session(id).Status == session.StatusFailed })
	if rec := h.session(id); rec.Error == nil || !strings.Contains(*rec.Error, "exited unexpectedly") {
		t.Fatalf("record = %#v", rec)
	}
}

func TestWorkerLostWhileDaemonDown(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	rec := h.session(id)
	h.stop()
	_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
	waitForExit(int(*rec.WorkerPID), testTimeout)

	h.start()
	if got := h.session(id); got.Status != session.StatusUnknownRecovered {
		t.Fatalf("status = %s, want unknown_recovered", got.Status)
	}
}

func processExists(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processExists(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// After a crash, a session's recorded pids may belong to unrelated processes
// by the time anyone looks. The daemon must treat the session as lost and
// never signal those pids.
func TestRecycledPidsAreNeverSignalled(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	rec := h.session(id)
	h.stop()
	// The worker dies without cleaning up: its socket file stays behind.
	_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
	waitForExit(int(*rec.WorkerPID), testTimeout)
	if _, err := os.Stat(h.paths.SessionSocketPath(id)); err != nil {
		t.Fatalf("expected a stale socket file: %v", err)
	}

	// An unrelated process now holds the recorded pids.
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bystander.Process.Kill(); bystander.Wait() })
	raw, err := sql.Open("sqlite", h.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`UPDATE sessions SET status = 'running', worker_pid = ?1, agent_pid = ?1 WHERE session_id = ?2`, bystander.Process.Pid, id)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}

	h.start()
	if got := h.session(id); got.Status != session.StatusUnknownRecovered {
		t.Fatalf("status = %s, want unknown_recovered", got.Status)
	}
	wantError(t, h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id}}), "is not running")
	if resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: true}}); resp.KillSession == nil || resp.KillSession.WasRunning {
		t.Fatalf("rm = %#v (%v)", resp, resp.Error)
	}
	if st := h.management(&protocol.ManagementRequest{Status: protocol.Empty}).Status; st.RunningSessions {
		t.Fatal("a recycled pid made the daemon think a session is running")
	}
	if !processExists(bystander.Process.Pid) {
		t.Fatal("the daemon signalled an unrelated process")
	}
}

func TestBadRequestsGetAnError(t *testing.T) {
	h := newHarness(t)
	// An unknown kind at the current version gets a reply, not a hang-up.
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTimeout))
	conn.Write([]byte{0x50, 0x44, 0x47, 0x41, 1, 0, 99, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	resp, err := protocol.ReadResponse(bufio.NewReader(conn))
	if err != nil || resp == nil || resp.Error == nil || !strings.Contains(resp.Error.Message, "could not decode") {
		t.Fatalf("got %#v, %v", resp, err)
	}

	// Session ids that could never have been created are refused before
	// any path is built from them.
	for _, id := range []string{"../../etc/x", "a/b", "UPPER", ""} {
		wantError(t, h.request(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: id}}), "not found")
		wantError(t, h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: id, Data: []byte("x")}}), "not found")
	}
}

func TestRuntimeFilesArePrivate(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("")
	h.sendInput(id, "done\n")
	h.eventually("session to end", func() bool { return h.session(id).Status == session.StatusExited })
	for path, want := range map[string]os.FileMode{
		h.paths.Root:                0o700,
		h.paths.LogsDir:             0o700,
		h.paths.SessionsDir:         0o700,
		h.paths.Socket:              0o600,
		h.paths.PIDFile:             0o600,
		h.paths.Database:            0o600,
		h.paths.LogPath(id):         0o600,
		h.paths.RenderedLogPath(id): 0o600,
		h.paths.WorkerLogPath(id):   0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", path, got, want)
		}
	}
}

func TestValidSessionName(t *testing.T) {
	for name, want := range map[string]bool{
		"a": true, "a-b": true, "abc-123": true, strings.Repeat("a", 64): true,
		"": false, "-a": false, "a-": false, "a--b": false, "A": false, "a_b": false,
		"é": false, "a/b": false, "..": false, strings.Repeat("a", 65): false,
	} {
		if got := validSessionName(name); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "missing.toml"))
	if err != nil || cfg.DefaultAgent != "claude" || cfg.Agents["codex"].Command != "codex" {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("default_agent = \"x\"\n[agents.y]\ncommand = \"y\"\n"), 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "default_agent `x`") {
		t.Fatalf("got %v", err)
	}
	os.WriteFile(path, []byte("[agents.y]\ncommand = \"y\"\nmodel_flag = \"\"\n[agents.z]\ncommand = \"z\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["y"].modelFlag(); got != "" {
		t.Fatalf("empty model_flag = %q", got)
	}
	if got := cfg.Agents["z"].modelFlag(); got != "--model" {
		t.Fatalf("default model_flag = %q", got)
	}

	// Without default_agent: claude if configured, else the first agent.
	os.WriteFile(path, []byte("[agents.codex]\ncommand = \"codex\"\n[agents.claude]\ncommand = \"claude\"\n"), 0o600)
	if cfg, err := LoadConfig(path); err != nil || cfg.DefaultAgent != "claude" {
		t.Fatalf("with claude configured: %+v, %v", cfg, err)
	}
	os.WriteFile(path, []byte("[agents.zed]\ncommand = \"zed\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	if cfg, err := LoadConfig(path); err != nil || cfg.DefaultAgent != "zed" {
		t.Fatalf("without claude: %+v, %v", cfg, err)
	}
}

// Removing a session and immediately creating one with the same name must
// leave the new session reachable: the old worker's cleanup may not delete
// the new worker's socket.
func TestRemoveAndRecreateSameName(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		h.mustCreate("reuse")
		resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: "reuse", Remove: true}})
		if resp.KillSession == nil {
			t.Fatalf("round %d rm: %v", i, resp.Error)
		}
	}
	h.mustCreate("reuse")
	time.Sleep(3 * time.Second) // longer than any old worker's shutdown grace
	h.sendInput("reuse", "still-reachable\n")
	h.eventually("new session output", func() bool { return strings.Contains(h.history("reuse"), "got:still-reachable") })
}

func TestGeneratedNamesDoNotRunOut(t *testing.T) {
	h := newHarness(t)
	srv, err := New(h.paths, workerBin)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range nameAdjectives {
		for _, b := range nameAnimals {
			if _, err := srv.db.InsertSession(db.NewSession{SessionID: a + "-" + b, Agent: "sh", Mode: session.ModeExecute, Cwd: h.cwd}); err != nil {
				t.Fatal(err)
			}
		}
	}
	id := h.mustCreate("")
	if !validSessionName(id) || strings.Count(id, "-") != 2 {
		t.Fatalf("generated name %q", id)
	}
}
