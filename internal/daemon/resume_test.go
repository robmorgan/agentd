package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
	"github.com/robmorgan/agentd/internal/transport/transporttest"
)

// These tests cover session resumption: session UIDs, attachment
// replacement, retried lifecycle requests, slow clients, and clients whose
// network changes, disappears for a long time, or keeps dropping. The
// network tests run the client over QUIC, through a UDP relay that stands
// in for the network.

// call sends one request on the control stream and waits for its answer.
func (c *controlClient) call(req *protocol.Request) *protocol.Response {
	c.t.Helper()
	c.nextID++
	c.conn.SetDeadline(time.Now().Add(testTimeout))
	c.send(c.nextID, req)
	id, resp := c.read()
	if id != c.nextID {
		c.t.Fatalf("answer to request %d, want %d", id, c.nextID)
	}
	return resp
}

// attachOn attaches over stream with req, filling in the session, kind and
// geometry when unset. It returns the client and the daemon's first answer.
func attachOn(t *testing.T, stream transport.Stream, id string, req protocol.AttachSession) (*client, *protocol.Response) {
	t.Helper()
	c := &client{t: t, conn: stream, r: bufio.NewReader(stream)}
	t.Cleanup(func() { stream.Close() })
	req.SessionID = id
	if req.Kind == "" {
		req.Kind = session.AttachmentAttach
	}
	if req.Geometry == (protocol.Geometry{}) {
		req.Geometry = defaultGeometry
	}
	c.send(&protocol.Request{AttachSession: &req})
	resp := c.read()
	if resp.Attached != nil {
		c.attachID = resp.Attached.AttachID
		c.snapshot = resp.Attached.Snapshot
	}
	return c, resp
}

func mustAttachOn(t *testing.T, stream transport.Stream, id string, req protocol.AttachSession) (*client, *protocol.Attached) {
	t.Helper()
	c, resp := attachOn(t, stream, id, req)
	if resp.Attached == nil {
		t.Fatalf("attach: unexpected response %#v (error %v)", resp, resp.Error)
	}
	return c, resp.Attached
}

var allAttachFeatures = protocol.AttachCapabilities()

func TestSessionUIDsIdentifyIncarnations(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	create := func() *session.CreateResult {
		t.Helper()
		resp := c.call(&protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: strp("uid-demo"), Agent: "sh"}})
		if resp.CreateSession == nil {
			t.Fatalf("create: %#v", resp.Error)
		}
		return resp.CreateSession
	}
	first := create()
	if len(first.UID) != 32 {
		t.Fatalf("uid = %q", first.UID)
	}
	if resp := c.call(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: "uid-demo"}}); resp.Session == nil || resp.Session.UID != first.UID {
		t.Fatalf("get = %#v", resp)
	}
	if resp := c.call(&protocol.Request{ListSessions: protocol.Empty}); resp.Sessions == nil || (*resp.Sessions)[0].UID != first.UID {
		t.Fatalf("list = %#v", resp)
	}
	a, attached := mustAttachOn(t, h.dial(), "uid-demo", protocol.AttachSession{Features: allAttachFeatures})
	if attached.SessionUID != first.UID {
		t.Fatalf("attached to uid %q, want %q", attached.SessionUID, first.UID)
	}
	a.conn.Close()

	// Removed and started again under the same name, it is another
	// incarnation: a reattach expecting the first must not land on it.
	if resp := c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: "uid-demo", Remove: true}}); resp.KillSession == nil {
		t.Fatalf("rm: %#v", resp.Error)
	}
	second := create()
	if second.UID == first.UID {
		t.Fatal("the recreated session kept the UID")
	}
	_, resp := attachOn(t, h.dial(), "uid-demo", protocol.AttachSession{Features: allAttachFeatures, ExpectUID: first.UID, Replaces: attached.AttachID})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "not the session you were attached to") {
		t.Fatalf("attach expecting the removed incarnation = %#v", resp)
	}
	b, attached := mustAttachOn(t, h.dial(), "uid-demo", protocol.AttachSession{Features: allAttachFeatures, ExpectUID: second.UID})
	if attached.SessionUID != second.UID || attached.AttachID != "attach-1" {
		t.Fatalf("attached = %+v", attached)
	}
	b.input("hi\n")
	b.expectOutput("got:hi")
}

// A client that lost its connection after sending a lifecycle request sends
// it again with the same token; the daemon answers with the original answer
// instead of acting twice or failing.
func TestRetriedLifecycleRequestsAreAnsweredOnce(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	if !c.features.Has(protocol.CapRequestTokens) {
		t.Fatal("daemon does not take request tokens")
	}
	create := &protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: strp("tok"), Agent: "sh", Token: "create-1"}}
	first, again := c.call(create), c.call(create)
	if first.CreateSession == nil || again.CreateSession == nil || *again.CreateSession != *first.CreateSession {
		t.Fatalf("create twice = %#v / %#v", first, again)
	}
	// A token is for one request only.
	other := c.call(&protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: strp("other"), Agent: "sh", Token: "create-1"}})
	wantError(t, other, "already used")

	// Duplicates arriving together (on two streams) create one session.
	c2 := h.control()
	dup := &protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Agent: "sh", Token: "create-2"}}
	c.send(100, dup)
	c2.send(100, dup)
	_, r1 := c.read()
	_, r2 := c2.read()
	if r1.CreateSession == nil || r2.CreateSession == nil || *r1.CreateSession != *r2.CreateSession {
		t.Fatalf("concurrent duplicates = %#v / %#v", r1, r2)
	}
	if resp := c.call(&protocol.Request{ListSessions: protocol.Empty}); len(*resp.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(*resp.Sessions))
	}

	kill := &protocol.Request{KillSession: &protocol.KillSession{SessionID: "tok", Token: "kill-1"}}
	first, again = c.call(kill), c.call(kill)
	if first.KillSession == nil || !first.KillSession.WasRunning || again.KillSession == nil || *again.KillSession != *first.KillSession {
		t.Fatalf("kill twice = %#v / %#v", first, again)
	}
	wantError(t, c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: r1.CreateSession.SessionID, Token: "kill-1"}}), "different request")
	rm := &protocol.Request{KillSession: &protocol.KillSession{SessionID: "tok", Remove: true, Token: "rm-1"}}
	first, again = c.call(rm), c.call(rm)
	if first.KillSession == nil || !first.KillSession.Removed || again.KillSession == nil || *again.KillSession != *first.KillSession {
		t.Fatalf("rm twice = %#v / %#v", first, again)
	}
	// Without a token a repeat is a new request.
	wantError(t, c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: "tok", Remove: true}}), "not found")

	add := &protocol.Request{AddWorkspace: &protocol.AddWorkspace{Name: "ws", Path: h.cwd, Token: "ws-1"}}
	first, again = c.call(add), c.call(add)
	if first.Workspace == nil || again.Workspace == nil || *again.Workspace != *first.Workspace {
		t.Fatalf("workspace add twice = %#v / %#v", first, again)
	}
	remove := &protocol.Request{RemoveWorkspace: &protocol.WorkspaceRef{Name: "ws", Token: "ws-2"}}
	if first, again = c.call(remove), c.call(remove); first.Ok == nil || again.Ok == nil {
		t.Fatalf("workspace rm twice = %#v / %#v", first, again)
	}
	wantError(t, c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: "tok", Token: strings.Repeat("x", 65)}}), "longer than")

	// A create token is kept with the session, so a retry still finds it
	// after the daemon restarts.
	persist := &protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: strp("persist"), Agent: "sh", Token: "create-3"}}
	first = c.call(persist)
	h.stop()
	h.start()
	c = h.control()
	if again = c.call(persist); first.CreateSession == nil || again.CreateSession == nil || *again.CreateSession != *first.CreateSession {
		t.Fatalf("create retried across a restart = %#v / %#v", first, again)
	}
}

// remoteHarness starts a daemon listening on QUIC with an authorized client
// key. idle, if set, shortens the daemon's dead-peer timeout.
func remoteHarness(t *testing.T, idle time.Duration) (*harness, *transport.Identity) {
	t.Helper()
	remoteIdleTimeout = idle
	t.Cleanup(func() { remoteIdleTimeout = 0 })
	h := newHarnessWithConfig(t, remoteConfig)
	client := newClientIdentity(t)
	if _, err := transport.Authorize(h.paths.AuthorizedClientsPath(), client.Fingerprint, "test"); err != nil {
		t.Fatal(err)
	}
	h.eventually("QUIC listener", func() bool { return h.srv.RemoteAddr() != "" })
	return h, client
}

// dialQUIC connects to the harness daemon at addr (its own, or a relay's).
func (h *harness) dialQUIC(client *transport.Identity, addr string) *transport.QUICClient {
	h.t.Helper()
	daemonKey, err := transport.LoadOrCreateIdentity(h.paths.RemoteKeyPath())
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	qc, err := transport.DialQUIC(ctx, addr, client, daemonKey.Fingerprint)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { qc.Close() })
	return qc
}

func openStream(t *testing.T, qc *transport.QUICClient) transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	s, err := qc.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Network switching, soft: the client's address changes mid-attachment (NAT
// rebinding, or Wi-Fi to cellular behind the same NAT). QUIC migrates the
// connection, and the attachment carries on without reconnecting.
func TestAttachSurvivesAddressChange(t *testing.T) {
	h, client := remoteHarness(t, 0)
	id := h.mustCreate("rebind")
	relay := transporttest.NewRelay(t, h.srv.RemoteAddr())
	qc := h.dialQUIC(client, relay.Addr())
	c, attached := mustAttachOn(t, openStream(t, qc), id, protocol.AttachSession{Features: allAttachFeatures})
	c.input("one\n")
	c.expectOutput("got:one")
	before := h.srv.remoteConnections()

	if err := relay.Rebind(); err != nil {
		t.Fatal(err)
	}
	c.input("two\n")
	c.expectOutput("got:two")
	after := h.srv.remoteConnections()
	if len(before) != 1 || len(after) != 1 || before[0].Remote == after[0].Remote {
		t.Fatalf("connections before %+v, after %+v: want one connection that changed address", before, after)
	}
	if atts := h.attachments(id); len(atts) != 1 || atts[0].AttachID != attached.AttachID {
		t.Fatalf("attachments = %+v, want only %s", atts, attached.AttachID)
	}
}

// Network switching, hard: the old path dies outright and the client
// reconnects over another. Its reattach names the incarnation and the
// attachment it replaces, which the daemon drops at once although it has
// not noticed the old connection die yet.
func TestReattachAfterNetworkSwitchReplacesTheLostAttachment(t *testing.T) {
	h, client := remoteHarness(t, 0)
	id := h.mustCreate("switch")
	oldNet := transporttest.NewRelay(t, h.srv.RemoteAddr())
	c, attached := mustAttachOn(t, openStream(t, h.dialQUIC(client, oldNet.Addr())), id, protocol.AttachSession{Features: allAttachFeatures})
	c.input("before\n")
	c.expectOutput("got:before")

	oldNet.Drop(true)
	newNet := transporttest.NewRelay(t, h.srv.RemoteAddr())
	c2, again := mustAttachOn(t, openStream(t, h.dialQUIC(client, newNet.Addr())), id, protocol.AttachSession{
		Features: allAttachFeatures, ExpectUID: attached.SessionUID, Replaces: attached.AttachID,
	})
	if atts := h.attachments(id); len(atts) != 1 || atts[0].AttachID != again.AttachID {
		t.Fatalf("attachments = %+v, want only the new %s", atts, again.AttachID)
	}
	if again.AttachID == attached.AttachID || again.SessionUID != attached.SessionUID {
		t.Fatalf("reattached %+v after %+v", again, attached)
	}
	if !bytes.Contains(c2.snapshot, []byte("got:before")) {
		t.Fatalf("reattach snapshot missing earlier output")
	}
	c2.input("after\n")
	c2.expectOutput("got:after")
}

// Extended offline: the client is unreachable for longer than the dead-peer
// timeout. The daemon drops its attachment, the session carries on, and
// when the network returns the client reattaches on a new connection to a
// screen that includes what happened meanwhile.
func TestReattachAfterExtendedOutage(t *testing.T) {
	h, client := remoteHarness(t, time.Second)
	id := h.mustCreate("offline")
	relay := transporttest.NewRelay(t, h.srv.RemoteAddr())
	c, attached := mustAttachOn(t, openStream(t, h.dialQUIC(client, relay.Addr())), id, protocol.AttachSession{Features: allAttachFeatures})
	c.input("one\n")
	c.expectOutput("got:one")

	relay.Drop(true)
	h.eventually("the daemon to drop the silent client", func() bool { return len(h.attachments(id)) == 0 })
	h.sendInput(id, "while-offline\n")
	h.eventually("output while offline", func() bool { return strings.Contains(h.history(id), "got:while-offline") })
	time.Sleep(2 * time.Second) // well past the dead-peer timeout
	relay.Drop(false)

	// The client has given the old connection up too.
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	for {
		resp, err := protocol.ReadResponse(c.r)
		if err != nil {
			if !transport.IsConnectionLost(err) {
				t.Fatalf("old connection failed with %v, which is not a lost connection", err)
			}
			break
		}
		if resp == nil {
			t.Fatal("old attachment ended cleanly")
		}
	}
	c2, again := mustAttachOn(t, openStream(t, h.dialQUIC(client, relay.Addr())), id, protocol.AttachSession{
		Features: allAttachFeatures, ExpectUID: attached.SessionUID, Replaces: attached.AttachID,
	})
	if again.SessionUID != attached.SessionUID || !bytes.Contains(c2.snapshot, []byte("got:while-offline")) {
		t.Fatalf("reattach after the outage: %+v, snapshot has offline output: %v", again, bytes.Contains(c2.snapshot, []byte("got:while-offline")))
	}
	c2.input("back\n")
	c2.expectOutput("got:back")
}

// Repeated reconnect loops: fifty abrupt connection drops and reattaches,
// half of them replacing an attachment whose connection is still open,
// leave nothing behind in the daemon or the worker.
func TestRepeatedReconnectsLeakNothing(t *testing.T) {
	h, client := remoteHarness(t, 0)
	id := h.mustCreate("loops")
	uid := h.session(id).UID
	workerPID := int(*h.session(id).WorkerPID)

	warm := h.dialQUIC(client, h.srv.RemoteAddr())
	mustAttachOn(t, openStream(t, warm), id, protocol.AttachSession{Features: allAttachFeatures})
	warm.Close()
	idle := func() bool {
		return len(h.attachments(id)) == 0 && len(h.srv.remoteConnections()) == 0 && h.srv.openStreams() == 0
	}
	h.eventually("warm-up connection to go", idle)
	baseGoroutines, baseFDs := settle()
	baseWorkerFDs := processFDs(t, workerPID)

	var prev *transport.QUICClient
	prevID := ""
	for i := range 50 {
		qc := h.dialQUIC(client, h.srv.RemoteAddr())
		c, attached := mustAttachOn(t, openStream(t, qc), id, protocol.AttachSession{
			Features: allAttachFeatures, ExpectUID: uid, Replaces: prevID,
		})
		if i%10 == 0 {
			c.input(fmt.Sprintf("ping%d\n", i))
			c.expectOutput(fmt.Sprintf("got:ping%d", i))
		}
		if prev != nil {
			prev.Close()
			prev = nil
		}
		if i%2 == 0 {
			qc.Close() // gone without detaching
			prevID = ""
		} else {
			prev, prevID = qc, attached.AttachID
		}
	}
	if prev != nil {
		prev.Close()
	}
	h.eventually("attachments, streams and connections to go", idle)
	h.eventually("goroutines and descriptors to return to their baseline", func() bool {
		g, fds := settle()
		wfds := processFDs(t, workerPID)
		if g > baseGoroutines || fds > baseFDs || wfds > baseWorkerFDs {
			t.Logf("goroutines %d (baseline %d), fds %d (%d), worker fds %d (%d)", g, baseGoroutines, fds, baseFDs, wfds, baseWorkerFDs)
			return false
		}
		return true
	})
}

// A remote client that stops reading for a long time while its session
// produces far more output than any buffer holds: the PTY and the other
// attachment keep going, the daemon's and the worker's memory stay
// bounded, and when the client reads again it is resynced to the current
// screen.
func TestSlowRemoteClientIsBoundedAndResynced(t *testing.T) {
	h, client := remoteHarness(t, 0)
	id := h.mustCreate("slow")
	workerPID := int(*h.session(id).WorkerPID)
	slow, _ := mustAttachOn(t, openStream(t, h.dialQUIC(client, h.srv.RemoteAddr())), id, protocol.AttachSession{Features: allAttachFeatures})
	fast := h.attach(id) // local, and without resync: lossy but live

	baseHeap := heapInUse()
	baseRSS := processRSS(t, workerPID)
	fastDone := make(chan int64, 1)
	stopFast := make(chan struct{})
	go func() {
		// Keep reading, as an interactive client would, until the flood
		// is over.
		var n int64
		defer func() { fastDone <- n }()
		for {
			select {
			case <-stopFast:
				return
			default:
			}
			fast.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			resp, err := protocol.ReadResponse(fast.r)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			if resp != nil && resp.PtyOutput != nil {
				n += int64(len(resp.PtyOutput.Data))
			}
		}
	}()

	start := time.Now()
	h.sendInput(id, "bigflood\n")
	var peakHeap uint64
	var peakRSS int64
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.Contains(h.history(id), "flood-done") {
		if time.Now().After(deadline) {
			t.Fatal("the PTY stalled behind a client that stopped reading")
		}
		peakHeap = max(peakHeap, heapInUse())
		peakRSS = max(peakRSS, processRSS(t, workerPID))
		time.Sleep(100 * time.Millisecond)
	}
	close(stopFast)
	read := <-fastDone
	t.Logf("flood took %s; the live client read %d MB; daemon heap %d -> peak %d MB; worker RSS %d -> peak %d MB",
		time.Since(start).Round(time.Millisecond), read>>20, baseHeap>>20, peakHeap>>20, baseRSS>>20, peakRSS>>20)
	if read < 8<<20 {
		t.Fatalf("the live client only received %d bytes during the flood", read)
	}
	// About 67 MB went through the PTY. Unbounded buffering for the stalled
	// client would hold most of it; bounded, the daemon holds at most its
	// copy buffers and QUIC's windows, and the worker one queue and one
	// snapshot.
	if peakHeap > baseHeap+32<<20 {
		t.Errorf("daemon heap grew from %d to %d MB", baseHeap>>20, peakHeap>>20)
	}
	if peakRSS > baseRSS+48<<20 {
		t.Errorf("worker RSS grew from %d to %d MB", baseRSS>>20, peakRSS>>20)
	}

	// The fast client is still interactive.
	fast.pingUntilEchoed()

	// The slow one catches up: what was in flight, then a resync to the
	// current screen, then live output.
	// It may first get a resync taken early in the flood, when it last
	// drained before stalling; each later one is fresher, and the last
	// shows the final screen.
	var resync []byte
	resyncs := 0
	for !bytes.Contains(resync, []byte("flood-done")) {
		resp := slow.read()
		switch {
		case resp.AttachResync != nil:
			resync = resp.AttachResync.Data
			resyncs++
		case resp.PtyOutput == nil:
			t.Fatalf("unexpected response while catching up: %#v", resp)
		}
	}
	t.Logf("caught up after %d resyncs", resyncs)
	slow.output.Reset()
	slow.input("after\n")
	slow.expectOutput("got:after")
	if strings.Contains(slow.output.String(), "line ") {
		t.Fatalf("output the resync contains was sent after it: %.200q", slow.output.String())
	}
}

// pingUntilEchoed proves the attachment is live even if some of its output
// was dropped while it lagged: it keeps sending pings until one echoes back.
func (c *client) pingUntilEchoed() {
	c.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for i := 0; ; i++ {
		c.input(fmt.Sprintf("ping%d\n", i))
		c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
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

// settle lets finished goroutines exit and returns this process's goroutine
// and open file descriptor counts.
func settle() (goroutines, fds int) {
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	entries, _ := os.ReadDir("/dev/fd")
	return runtime.NumGoroutine(), len(entries)
}

// heapInUse is this process's live heap after a collection.
func heapInUse() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

// processFDs counts another process's open files.
func processFDs(t *testing.T, pid int) int {
	t.Helper()
	if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		return len(entries)
	}
	out, err := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("lsof: %v", err)
	}
	return strings.Count(string(out), "\n")
}

// processRSS is another process's resident set size in bytes.
func processRSS(t *testing.T, pid int) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("ps output %q: %v", out, err)
	}
	return kb << 10
}

func strp(s string) *string { return &s }
