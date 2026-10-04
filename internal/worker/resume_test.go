package worker

import (
	"bufio"
	"bytes"
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// attachWith attaches with an AttachSession of the test's choosing and
// returns the client and the worker's Attached.
func (h *harness) attachWith(req protocol.AttachSession) (*client, *protocol.Attached) {
	h.t.Helper()
	c, resp := h.tryAttach(req)
	if resp.Attached == nil {
		h.t.Fatalf("attach: unexpected response %#v (error %v)", resp, resp.Error)
	}
	return c, resp.Attached
}

func (h *harness) tryAttach(req protocol.AttachSession) (*client, *protocol.Response) {
	h.t.Helper()
	c := &client{t: h.t, conn: h.dial()}
	c.r = bufio.NewReader(c.conn)
	h.t.Cleanup(func() { c.conn.Close() })
	req.SessionID = h.sessionID
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

func TestWorkerAnswersHelloWithItsAttachFeatures(t *testing.T) {
	h := startWorker(t)
	resp := h.request(&protocol.Request{Hello: protocol.NewHello("test")})
	if resp.Welcome == nil || !reflect.DeepEqual(resp.Welcome.Capabilities, append(protocol.AttachCapabilities(), protocol.CapWorkerHandoff)) {
		t.Fatalf("hello = %#v", resp)
	}
	h.sendInput("done\n")
	h.waitExit()
}

// The attach stream's features are echoed, the UID identifies the
// incarnation, and an attach expecting another incarnation is refused.
func TestAttachFeaturesAndSessionUID(t *testing.T) {
	h := startWorker(t)
	c, attached := h.attachWith(protocol.AttachSession{Features: append(protocol.AttachCapabilities(), "future")})
	if !reflect.DeepEqual(attached.Features, protocol.AttachCapabilities()) || attached.SessionUID != h.uid {
		t.Fatalf("attached = %+v", attached)
	}
	// A client that asks for nothing gets the base Attached.
	plain := h.attach(defaultGeometry)
	if plain.attachID != "attach-2" {
		t.Fatalf("second attach id = %q", plain.attachID)
	}
	_, resp := h.tryAttach(protocol.AttachSession{Features: []string{protocol.CapSessionUID}, ExpectUID: "another-incarnation"})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "not the session you were attached to") {
		t.Fatalf("attach expecting another incarnation = %#v", resp)
	}
	if got := len(h.attachments()); got != 2 {
		t.Fatalf("attachments = %d, want 2", got)
	}
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}

// A reattach that names the attachment it replaces drops it at once,
// without waiting for its dead connection to fail.
func TestReplacingAnAttachmentDropsItAtOnce(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	old, attached := h.attachWith(protocol.AttachSession{Features: all})

	// Without the UID, a replace is ignored: ids are only unique within
	// one incarnation.
	h.attachWith(protocol.AttachSession{Features: all, Replaces: old.attachID})
	if got := len(h.attachments()); got != 2 {
		t.Fatalf("attachments after an unchecked replace = %d", got)
	}

	fresh, _ := h.attachWith(protocol.AttachSession{Features: all, ExpectUID: attached.SessionUID, Replaces: old.attachID})
	for _, a := range h.attachments() {
		if a.AttachID == old.attachID {
			t.Fatalf("replaced attachment still listed: %#v", h.attachments())
		}
	}
	if end := old.expectEnd(); end.EndOfStream == nil {
		t.Fatalf("replaced attachment ended with %#v", end)
	}
	fresh.input("still-here\n")
	fresh.expectOutput("got:still-here")
	fresh.input("done\n")
	fresh.expectEnd()
	h.waitExit()
}

// A client that stops reading loses output, and once it reads again is sent
// a fresh snapshot (AttachResync) that is an exact boundary: nothing it
// already contains follows it.
func TestLaggingClientIsResynced(t *testing.T) {
	h := startWorker(t)
	lagging, _ := h.attachWith(protocol.AttachSession{Features: []string{protocol.CapAttachResync}})
	h.sendInput("bigflood\n")
	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })

	// Read what was queued before the drop, until the resync.
	// A resync taken before the client stalled completely may come
	// first; the last one shows the final screen.
	var resync []byte
	for !bytes.Contains(resync, []byte("flood-done")) {
		resp := lagging.read()
		switch {
		case resp.AttachResync != nil:
			resync = resp.AttachResync.Data
		case resp.PtyOutput == nil:
			t.Fatalf("unexpected response while catching up: %#v", resp)
		}
	}
	lagging.output.Reset()
	lagging.input("after\n")
	lagging.expectOutput("got:after")
	if strings.Contains(lagging.output.String(), "line ") {
		t.Fatalf("output the snapshot contains was sent after it: %.200q", lagging.output.String())
	}
	lagging.input("done\n")
	lagging.expectEnd()
	h.waitExit()
}

// Input is bounded too: an attached client that keeps typing at an agent
// that has stopped reading is told why its attachment ends once the
// session's input queue is full, rather than having keystrokes dropped.
func TestAttachInputOverflowEndsTheAttachmentWithTheReason(t *testing.T) {
	h := startWorker(t)
	c := h.attach(defaultGeometry)
	c.input("stall\n")
	time.Sleep(300 * time.Millisecond)
	chunk := &protocol.Request{AttachInput: &protocol.Bytes{Data: bytes.Repeat([]byte("x"), 64<<10)}}
	go func() {
		for range 2 * maxQueuedInput / (64 << 10) {
			if protocol.WriteRequest(c.conn, chunk) != nil {
				return
			}
		}
	}()
	for {
		resp := c.read()
		if resp.PtyOutput != nil {
			continue
		}
		if resp.Error == nil || !strings.Contains(resp.Error.Message, "not reading its input") {
			t.Fatalf("attachment ended with %#v", resp)
		}
		break
	}
}

// The subscriber queue is bounded in bytes as well as chunks.
func TestSubscriberQueueIsBounded(t *testing.T) {
	b := newBroadcaster()
	s := b.subscribe()
	chunk := make([]byte, 8192)
	for range 2 * subscriberBuffer {
		b.publish(chunk)
	}
	if q := s.queued.Load(); q > subscriberMaxBytes || len(s.ch) != subscriberMaxBytes/len(chunk) || !s.lagged.Load() {
		t.Fatalf("queued %d bytes in %d chunks, lagged %v", q, len(s.ch), s.lagged.Load())
	}
	s.discard()
	if s.queued.Load() != 0 || len(s.ch) != 0 || s.lagged.Load() {
		t.Fatalf("after discard: %d bytes, %d chunks, lagged %v", s.queued.Load(), len(s.ch), s.lagged.Load())
	}
	for range 2 * subscriberBuffer {
		b.publish([]byte("x"))
	}
	if len(s.ch) != subscriberBuffer || s.queued.Load() != subscriberBuffer || !s.lagged.Load() {
		t.Fatalf("small chunks: %d queued, lagged %v", len(s.ch), s.lagged.Load())
	}
	data, _ := s.next()
	s.took(nil)
	if len(data) != 1 || s.queued.Load() != subscriberBuffer-1 {
		t.Fatalf("next = %q, %d bytes queued", data, s.queued.Load())
	}
	b.unsubscribe(s)
	if b.count() != 0 {
		t.Fatal("subscriber still registered")
	}
}

// Attaching and dropping the connection over and over, sometimes replacing
// the previous attachment while its connection is still open, leaves
// nothing behind: no attachment, subscriber, goroutine or file descriptor.
func TestRepeatedReattachLeaksNothing(t *testing.T) {
	h := startWorker(t)
	warm := h.attach(defaultGeometry)
	warm.conn.Close()
	h.eventually("warm-up attachment to go", func() bool { return len(h.attachments()) == 0 })
	baseGoroutines, baseFDs := settle()

	all := protocol.AttachCapabilities()
	var prev *client
	for i := range 50 {
		req := protocol.AttachSession{Features: all, ExpectUID: h.uid}
		if prev != nil {
			req.Replaces = prev.attachID
		}
		c, _ := h.attachWith(req)
		if i%10 == 0 {
			c.input("ping\n")
			c.expectOutput("got:ping")
		}
		if prev != nil {
			prev.conn.Close()
		}
		if i%2 == 0 {
			c.conn.Close() // dropped without detaching
			prev = nil
		} else {
			prev = c // replaced by the next one while still open
		}
	}
	if prev != nil {
		prev.conn.Close()
	}
	h.eventually("attachments to go", func() bool { return len(h.attachments()) == 0 })
	h.eventually("goroutines and descriptors to return to their baseline", func() bool {
		g, fds := settle()
		if g > baseGoroutines || fds > baseFDs {
			t.Logf("goroutines %d (baseline %d), fds %d (baseline %d)", g, baseGoroutines, fds, baseFDs)
			return false
		}
		return true
	})
	h.sendInput("done\n")
	h.waitExit()
}

// settle lets finished goroutines exit and returns the goroutine and open
// file descriptor counts of this process.
func settle() (goroutines, fds int) {
	time.Sleep(50 * time.Millisecond)
	goruntime.GC()
	entries, _ := os.ReadDir("/dev/fd")
	return goruntime.NumGoroutine(), len(entries)
}
