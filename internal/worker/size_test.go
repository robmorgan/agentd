package worker

import (
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// expectSize asks the agent for the PTY's size and waits for want
// ("rows cols").
func (c *client) expectSize(want string) {
	c.t.Helper()
	c.output.Reset()
	c.input("size\n")
	c.expectOutput(want)
}

// expectSizeNote reads until an AttachSizeOwner frame matching ok arrives
// (or one already has).
func (c *client) expectSizeNote(what string, ok func(protocol.SizeOwnership) bool) protocol.SizeOwnership {
	c.t.Helper()
	for {
		for _, o := range c.sizes {
			if ok(o) {
				return o
			}
		}
		resp := c.read()
		switch {
		case resp.AttachSizeOwner != nil:
			c.sizes = append(c.sizes, *resp.AttachSizeOwner)
		case resp.PtyOutput != nil:
			c.output.Write(resp.PtyOutput.Data)
		default:
			c.t.Fatalf("waiting for %s: unexpected response %#v (notes so far %+v)", what, resp, c.sizes)
		}
	}
}

// owner returns the id of the attachment listed as the size owner.
func (h *harness) owner() string {
	h.t.Helper()
	for _, a := range h.attachments() {
		if a.SizeOwner {
			return a.AttachID
		}
	}
	return ""
}

func TestSizeOwnerAttachRecordsShowSizes(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	a, attached := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	if !attached.SizeOwner.Owner || attached.SizeOwner.OwnerID != a.attachID || attached.SizeOwner.Geometry.Cols != 100 {
		t.Fatalf("first attach ownership = %+v", attached.SizeOwner)
	}
	b, attached := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	if attached.SizeOwner.Owner || attached.SizeOwner.OwnerID != a.attachID || attached.SizeOwner.Geometry.Rows != 30 {
		t.Fatalf("pending attach ownership = %+v", attached.SizeOwner)
	}
	b.send(&protocol.Request{AttachResize: &protocol.Geometry{Cols: 70, Rows: 21}})
	b.expectSize("30 100")

	got := map[string]session.AttachmentRecord{}
	for _, r := range h.attachments() {
		got[r.AttachID] = r
	}
	if r := got[a.attachID]; !r.SizeOwner || r.Cols != 100 || r.Rows != 30 {
		t.Fatalf("owner record = %+v", r)
	}
	if r := got[b.attachID]; r.SizeOwner || r.Cols != 70 || r.Rows != 21 {
		t.Fatalf("pending record = %+v", r)
	}
	b.input("done\n")
	b.expectEnd()
	h.waitExit()
}

// When the owner leaves, the pending attachment used most recently takes
// over: its last input, else its attach.
func TestOwnerLeavingPromotesMostRecentlyUsed(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	a := h.attach(protocol.Geometry{Cols: 100, Rows: 30})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	c, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 80, Rows: 24}})
	// c attached last, but b typed since.
	time.Sleep(10 * time.Millisecond)
	b.input("hello\n")
	b.expectOutput("got:hello")

	if resp := h.request(&protocol.Request{DetachAttachment: &protocol.DetachAttachment{SessionID: h.sessionID, AttachID: a.attachID}}); resp.Ok == nil {
		t.Fatalf("detach: %#v", resp)
	}
	b.expectSizeNote("b to own the size", func(o protocol.SizeOwnership) bool {
		return o.Owner && o.OwnerID == b.attachID && o.Geometry.Cols == 90
	})
	c.expectSizeNote("c to hear b owns it", func(o protocol.SizeOwnership) bool {
		return !o.Owner && o.OwnerID == b.attachID
	})
	c.expectSize("20 90")

	// A client that says it is leaving hands the size on at once.
	b.send(&protocol.Request{AttachLeave: protocol.Empty})
	b.conn.Close()
	c.expectSizeNote("c to own the size", func(o protocol.SizeOwnership) bool { return o.Owner && o.Geometry.Cols == 80 })
	c.expectSize("24 80")

	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}

func TestTakeSize(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})

	// At attach.
	b, attached := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}, TakeSize: true})
	if !attached.SizeOwner.Owner || attached.SizeOwner.Geometry.Cols != 90 {
		t.Fatalf("take-size attach ownership = %+v", attached.SizeOwner)
	}
	a.expectSizeNote("a to lose the size", func(o protocol.SizeOwnership) bool { return !o.Owner && o.OwnerID == b.attachID })
	a.expectSize("20 90")

	// On the stream, at the client's current size.
	a.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 110, Rows: 35}})
	b.expectSizeNote("b to lose the size", func(o protocol.SizeOwnership) bool { return !o.Owner && o.OwnerID == a.attachID })
	b.expectSize("35 110")
	if got := h.owner(); got != a.attachID {
		t.Fatalf("owner = %q, want %q", got, a.attachID)
	}

	// A client without the capability cannot take it.
	plain := h.attach(protocol.Geometry{Cols: 70, Rows: 20})
	plain.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 70, Rows: 20}})
	if end := plain.expectEnd(); end.Error == nil {
		t.Fatalf("take-size without the capability ended with %#v", end)
	}
	a.expectSize("35 110")

	a.input("done\n")
	a.expectEnd()
	h.waitExit()
}

// The owner's reattach (Replaces) keeps the size rather than handing it to
// a pending attachment.
func TestReattachKeepsSizeOwnership(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	old, attached := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	pending, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})

	fresh, re := h.attachWith(protocol.AttachSession{
		Features: all, Geometry: protocol.Geometry{Cols: 105, Rows: 31},
		ExpectUID: attached.SessionUID, Replaces: old.attachID,
	})
	if !re.SizeOwner.Owner {
		t.Fatalf("reattach ownership = %+v", re.SizeOwner)
	}
	old.expectEnd()
	pending.expectSize("31 105")
	if got := h.owner(); got != fresh.attachID {
		t.Fatalf("owner = %q, want %q", got, fresh.attachID)
	}
	fresh.input("done\n")
	fresh.expectEnd()
	h.waitExit()
}

// An owner whose stream is cut without leaving (a daemon restart, a lost
// connection) keeps the size for its reattach, until sizeOwnerGrace.
func TestCutOwnerKeepsTheSizeForItsReattach(t *testing.T) {
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	a, attached := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	b.input("typed last\n")
	b.expectOutput("got:typed last")

	a.conn.Close()
	h.eventually("the owner's stream to end", func() bool { return len(h.attachments()) == 1 })
	if got := h.owner(); got != "" {
		t.Fatalf("the size passed to %q while its owner was reserved", got)
	}
	b.expectSize("30 100")
	a2, re := h.attachWith(protocol.AttachSession{
		Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30},
		ExpectUID: attached.SessionUID, Replaces: a.attachID,
	})
	if !re.SizeOwner.Owner {
		t.Fatalf("reattach ownership = %+v", re.SizeOwner)
	}
	a2.input("done\n")
	a2.expectEnd()
	h.waitExit()
}

// A cut owner that never comes back is replaced after the grace.
func TestCutOwnerIsReplacedAfterTheGrace(t *testing.T) {
	// Set before the worker starts, which reads it.
	defer func(g time.Duration) { sizeOwnerGrace = g }(sizeOwnerGrace)
	sizeOwnerGrace = 50 * time.Millisecond
	h := startWorker(t)
	all := protocol.AttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	a.conn.Close()
	b.expectSizeNote("b to own the size", func(o protocol.SizeOwnership) bool { return o.Owner && o.Geometry.Cols == 90 })
	b.expectSize("20 90")
	b.input("done\n")
	b.expectEnd()
	h.waitExit()
}

// An attachment without a size never owns it; the next with one does.
func TestZeroGeometryNeverOwnsTheSize(t *testing.T) {
	h := startWorker(t)
	z := h.attach(protocol.Geometry{})
	if got := h.owner(); got != "" {
		t.Fatalf("owner after a sizeless attach = %q", got)
	}
	c := h.attach(protocol.Geometry{Cols: 100, Rows: 30})
	if got := h.owner(); got != c.attachID {
		t.Fatalf("owner = %q, want %q", got, c.attachID)
	}
	c.conn.Close()
	h.eventually("the owner to leave", func() bool { return len(h.attachments()) == 1 })
	if got := h.owner(); got != "" {
		t.Fatalf("a sizeless attachment took the size: %q", got)
	}
	z.expectSize("30 100")
	z.input("done\n")
	z.expectEnd()
	h.waitExit()
}

// During a handoff the owner's id stays reserved: the other attachments
// reattach without taking the size, the owner's reattach inherits it, and
// a reserved owner that never comes back is replaced after the grace.
func TestHandoffReservesTheSizeOwner(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	term, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	o := newOwner()
	s := newOwnerState("size", ptmx, term)
	s.owner = o
	attach := func(id string, g protocol.Geometry, replaces string) {
		t.Helper()
		if _, err := s.attach(id, session.AttachmentAttach, g, replaces, 0, false, true); err != nil {
			t.Fatal(err)
		}
	}
	big, small := protocol.Geometry{Cols: 100, Rows: 30}, protocol.Geometry{Cols: 90, Rows: 20}
	attach("attach-1", big, "")
	attach("attach-2", small, "")

	// The handoff ends every attachment; the owner leaves first.
	close(s.restart)
	s.removeAttachment("attach-1")
	s.removeAttachment("attach-2")
	if s.sizeOwner != "attach-1" || s.geometry != big {
		t.Fatalf("after the handoff: owner %q, size %+v", s.sizeOwner, s.geometry)
	}

	// The new image: the pending one reattaches first and stays pending.
	s.restart = make(chan struct{})
	attach("attach-3", small, "attach-2")
	if s.sizeOwner != "attach-1" || s.geometry != big {
		t.Fatalf("a pending reattach took the size: owner %q, size %+v", s.sizeOwner, s.geometry)
	}
	attach("attach-4", big, "attach-1")
	if s.sizeOwner != "attach-4" {
		t.Fatalf("the owner's reattach did not inherit: owner %q", s.sizeOwner)
	}

	// A reserved owner that does not come back.
	defer func(g time.Duration) { sizeOwnerGrace = g }(sizeOwnerGrace)
	sizeOwnerGrace = 10 * time.Millisecond
	close(s.restart)
	s.removeAttachment("attach-4")
	s.restart = make(chan struct{})
	go o.run(s)
	defer o.stop()
	deadline := time.Now().Add(testTimeout)
	for {
		var owner string
		var g protocol.Geometry
		_ = o.do(func(s *ownerState) error { owner, g = s.sizeOwner, s.geometry; return nil })
		if owner == "attach-3" && g == small {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the grace never promoted: owner %q, size %+v", owner, g)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
