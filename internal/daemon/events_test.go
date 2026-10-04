package daemon

import (
	"bufio"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

func (h *harness) events(sessionID string) []session.Event {
	h.t.Helper()
	req := &protocol.ListEvents{Limit: 1000}
	if sessionID != "" {
		req.SessionID = &sessionID
	}
	resp := h.request(&protocol.Request{ListEvents: req})
	if resp.Events == nil {
		h.t.Fatalf("list events: %#v", resp.Error)
	}
	return *resp.Events
}

func kinds(events []session.Event) string {
	var out []string
	for _, ev := range events {
		out = append(out, string(ev.Kind))
	}
	return strings.Join(out, " ")
}

// waitEvent waits until a session has recorded an event of kind.
func (h *harness) waitEvent(id string, kind session.EventKind) session.Event {
	h.t.Helper()
	var found session.Event
	h.eventually(fmt.Sprintf("a %s event", kind), func() bool {
		for _, ev := range h.events(id) {
			if ev.Kind == kind {
				found = ev
				return true
			}
		}
		return false
	})
	return found
}

// subscriber is a test client for an events stream.
type subscriber struct {
	t    *testing.T
	conn transport.Stream
	r    *bufio.Reader
}

func (h *harness) subscribe(req *protocol.SubscribeEvents) *subscriber {
	h.t.Helper()
	conn := h.dial()
	h.t.Cleanup(func() { conn.Close() })
	if err := protocol.WriteRequest(conn, &protocol.Request{SubscribeEvents: req}); err != nil {
		h.t.Fatal(err)
	}
	return &subscriber{t: h.t, conn: conn, r: bufio.NewReader(conn)}
}

func (s *subscriber) next() session.Event {
	s.t.Helper()
	s.conn.SetReadDeadline(time.Now().Add(testTimeout))
	resp, err := protocol.ReadResponse(s.r)
	if err != nil || resp == nil || resp.Event == nil {
		s.t.Fatalf("event: %#v, %v", resp, err)
	}
	return *resp.Event
}

// until reads events until one of kind arrives, and returns those read.
func (s *subscriber) until(kind session.EventKind) []session.Event {
	s.t.Helper()
	var got []session.Event
	for {
		ev := s.next()
		got = append(got, ev)
		if ev.Kind == kind {
			return got
		}
	}
}

func TestLifecycleEventsAreRecorded(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("life")
	if got := kinds(h.events(id)); got != "created started" {
		t.Fatalf("after create: %s", got)
	}
	if resp := h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id}}); resp.KillSession == nil {
		t.Fatalf("kill: %#v", resp.Error)
	}
	events := h.events(id)
	if got := kinds(events); got != "created started killed" {
		t.Fatalf("after kill: %s", got)
	}
	for i, ev := range events {
		if ev.SessionID != id || ev.At.IsZero() || ev.Summary == "" || (i > 0 && ev.ID <= events[i-1].ID) {
			t.Fatalf("event %d = %#v", i, ev)
		}
	}
	// Killing it was the user's doing: nothing needs them.
	if rec := h.session(id); rec.Attention != session.AttentionInfo || rec.Activity != session.ActivityUnknown {
		// The base encoding has no activity; the attention is info.
		t.Fatalf("attention after kill = %s (%v)", rec.Attention, rec.AttentionSummary)
	}

	// An agent that fails needs action.
	failing := h.mustCreate("failing")
	h.sendInput(failing, "quit\n")
	ev := h.waitEvent(failing, session.EventFailed)
	if ev.Attention != session.AttentionAction || !strings.Contains(ev.Summary, "code 3") {
		t.Fatalf("failed event = %#v", ev)
	}
	if rec := h.session(failing); rec.Attention != session.AttentionAction {
		t.Fatalf("attention = %s", rec.Attention)
	}

	// Removing a session removes its events.
	h.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: true}})
	for _, ev := range h.events("") {
		if ev.SessionID == id {
			t.Fatalf("event of a removed session: %#v", ev)
		}
	}
}

func TestDaemonRecordsWorkerLossAndRecovery(t *testing.T) {
	h := newHarness(t)
	kept := h.mustCreate("kept")
	lost := h.mustCreate("lost")
	rec := h.session(lost)
	h.stop()
	_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
	waitForExit(int(*rec.WorkerPID), testTimeout)

	h.start()
	if got := kinds(h.events(kept)); got != "created started recovered" {
		t.Fatalf("kept: %s", got)
	}
	ev := h.waitEvent(lost, session.EventWorkerLost)
	if ev.Attention != session.AttentionAction {
		t.Fatalf("worker lost = %#v", ev)
	}

	// A worker crash under a running daemon is recorded by its supervisor.
	crash := h.mustCreate("crash")
	rec = h.session(crash)
	_ = syscall.Kill(-int(*rec.AgentPID), syscall.SIGKILL)
	_ = syscall.Kill(int(*rec.WorkerPID), syscall.SIGKILL)
	if ev := h.waitEvent(crash, session.EventWorkerLost); !strings.Contains(ev.Summary, "exited unexpectedly") {
		t.Fatalf("crash = %#v", ev)
	}
}

// The worker reads attention signals from the PTY: a bell, and an OSC 9
// notification whose BEL terminator is not a bell.
func TestBellAndNotificationRaiseAttention(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("asks")
	sub := h.subscribe(&protocol.SubscribeEvents{SessionID: &id, Tail: 10})
	if got := kinds(sub.until(session.EventStarted)); got != "created started" {
		t.Fatalf("backlog: %s", got)
	}

	h.sendInput(id, "\x1b]9;Claude needs your permission\x07\n")
	ev := sub.next()
	if ev.Kind != session.EventNotification || ev.Attention != session.AttentionAction || ev.Summary != "Claude needs your permission" {
		t.Fatalf("notification = %#v", ev)
	}
	if rec := h.session(id); rec.Attention != session.AttentionAction || rec.AttentionSummary == nil || *rec.AttentionSummary != "Claude needs your permission" {
		t.Fatalf("record = %s %v", rec.Attention, rec.AttentionSummary)
	}

	h.sendInput(id, "\a\n")
	if ev := sub.next(); ev.Kind != session.EventBell || ev.Attention != session.AttentionAction {
		t.Fatalf("bell = %#v", ev)
	}

	// The control stream carries the session's activity to clients that
	// negotiated it.
	// The foreground process is sampled every second.
	c := h.control()
	var rec *session.Record
	reqID := uint32(0)
	h.eventually("the foreground process", func() bool {
		reqID++
		c.send(reqID, &protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}})
		_, resp := c.read()
		rec = resp.Session
		return rec != nil && rec.Foreground != nil
	})
	// /bin/sh is bash or dash on some systems.
	fg := *rec.Foreground
	if rec.Activity != session.ActivityWaiting || (fg != "sh" && fg != "bash" && fg != "dash") ||
		rec.AttentionAt == nil || rec.LastOutputAt == nil {
		t.Fatalf("session over control = %#v (foreground %q)", rec, fg)
	}
}

func TestAttachAcknowledgesAttention(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("seen")
	h.sendInput(id, "\a\n")
	h.waitEvent(id, session.EventBell)
	if rec := h.session(id); rec.Attention != session.AttentionAction {
		t.Fatalf("attention before attach = %s", rec.Attention)
	}

	c := h.attach(id)
	if rec := h.session(id); rec.Attention != session.AttentionInfo || rec.AttentionSummary != nil {
		t.Fatalf("attention after attach = %s %v", rec.Attention, rec.AttentionSummary)
	}
	ack := h.waitEvent(id, session.EventAcknowledged)
	if ack.Summary != "seen: attached" {
		t.Fatalf("ack = %#v", ack)
	}

	// While attached the bell still raises attention (the client may be in
	// a background tab); detaching acknowledges it.
	c.input("\a\n")
	h.eventually("a second bell", func() bool { return h.session(id).Attention == session.AttentionAction })
	c.conn.Close()
	h.eventually("detach to acknowledge", func() bool { return h.session(id).Attention == session.AttentionInfo })

	// An attachment that ends with the session leaves its end unseen.
	c = h.attach(id)
	c.input("quit\n")
	c.expectEnd()
	h.eventually("the failure", func() bool { return h.session(id).Status == session.StatusFailed })
	time.Sleep(100 * time.Millisecond)
	if rec := h.session(id); rec.Attention != session.AttentionAction {
		t.Fatalf("attention after the session failed = %s", rec.Attention)
	}
}

func TestEventStreamResumesAndFollows(t *testing.T) {
	h := newHarness(t)
	a := h.mustCreate("alpha")
	b := h.mustCreate("beta")
	all := h.events("")
	if got := kinds(all); got != "created started created started" {
		t.Fatalf("events: %s", got)
	}

	// Resuming after an id replays exactly what followed it.
	sub := h.subscribe(&protocol.SubscribeEvents{AfterID: &all[1].ID})
	if ev := sub.next(); ev.ID != all[2].ID || ev.SessionID != b {
		t.Fatalf("resumed at %#v", ev)
	}
	sub.next()
	// A session filter keeps only its own.
	onlyA := h.subscribe(&protocol.SubscribeEvents{SessionID: &a, AfterID: &all[3].ID})
	h.sendInput(b, "\a\n")
	h.sendInput(a, "\a\n")
	// Each session's worker records its own bell, so they may land in
	// either order.
	first := sub.until(session.EventBell)
	second := sub.until(session.EventBell)
	if got := []string{first[len(first)-1].SessionID, second[len(second)-1].SessionID}; !(got[0] == a && got[1] == b || got[0] == b && got[1] == a) {
		t.Fatalf("live events %#v then %#v", first, second)
	}
	if ev := onlyA.next(); ev.Kind != session.EventBell || ev.SessionID != a {
		t.Fatalf("filtered %#v", ev)
	}

	// ListEvents pages forward from an id too.
	after := all[0].ID
	resp := h.request(&protocol.Request{ListEvents: &protocol.ListEvents{AfterID: &after, Limit: 2}})
	if resp.Events == nil || len(*resp.Events) != 2 || (*resp.Events)[0].ID != all[1].ID {
		t.Fatalf("page = %#v", resp)
	}
	// Unknown sessions are refused; subscriptions need their own stream.
	missing := "nope"
	wantError(t, h.request(&protocol.Request{ListEvents: &protocol.ListEvents{SessionID: &missing}}), "not found")
	c := h.control()
	c.send(1, &protocol.Request{SubscribeEvents: &protocol.SubscribeEvents{}})
	if _, resp := c.read(); resp.Error == nil || !strings.Contains(resp.Error.Message, "stream of its own") {
		t.Fatalf("subscribe on control = %#v", resp)
	}
}

// A subscriber that stops reading holds only its own stream: the daemon
// keeps serving others, and other subscribers keep receiving events.
func TestSlowSubscriberDoesNotAffectOthers(t *testing.T) {
	h := newHarness(t)
	// Enough events to fill any socket buffer, spread over sessions
	// (each keeps at most db.MaxEventsPerSession).
	const sessions, perSession = 10, 400
	for i := range sessions {
		id := fmt.Sprintf("bulk-%d", i)
		if _, err := h.srv.db.InsertSession(db.NewSession{SessionID: id, Agent: "sh", Mode: session.ModeExecute, Cwd: h.cwd}); err != nil {
			t.Fatal(err)
		}
		for j := range perSession {
			if _, err := h.srv.db.RecordEvent(db.NewEvent{SessionID: id, Kind: session.EventIdle, Attention: session.AttentionInfo,
				Summary: fmt.Sprintf("event %d with a summary long enough to take some room on the wire", j)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	zero := uint64(0)
	stuck := h.subscribe(&protocol.SubscribeEvents{AfterID: &zero})
	_ = stuck // never read

	fast := h.subscribe(&protocol.SubscribeEvents{AfterID: &zero})
	for range sessions * (perSession + 1) {
		fast.next()
	}
	id := h.mustCreate("live")
	h.sendInput(id, "\a\n")
	got := fast.until(session.EventBell)
	if got[len(got)-1].SessionID != id {
		t.Fatalf("live event = %#v", got[len(got)-1])
	}
	if h.srv.events.active() != true {
		t.Fatal("feed not active with subscribers")
	}
}
