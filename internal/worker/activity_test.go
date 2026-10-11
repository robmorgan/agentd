package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/session"
)

// The shadow terminal reports the effects agents use to ask for the user,
// from the same VTWrite that keeps the screen, across read boundaries.
func TestTerminalReportsAttentionEffects(t *testing.T) {
	ts, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.close()

	_, fx := ts.feed([]byte("hello\a"))
	if fx.bells != 1 || len(fx.notifications) != 0 || fx.titleChanged {
		t.Fatalf("bell: %+v", fx)
	}
	// OSC 9 ended by BEL: a notification, not a bell.
	_, fx = ts.feed([]byte("\x1b]9;Claude needs your permission\x07"))
	if fx.bells != 0 || len(fx.notifications) != 1 || fx.notifications[0].Body != "Claude needs your permission" {
		t.Fatalf("osc 9: %+v", fx)
	}
	_, fx = ts.feed([]byte("\x1b]777;notify;Codex;Approval "))
	if len(fx.notifications) != 0 {
		t.Fatalf("partial osc 777: %+v", fx)
	}
	_, fx = ts.feed([]byte("requested\x1b\\"))
	if len(fx.notifications) != 1 || fx.notifications[0].Title != "Codex" || fx.notifications[0].Body != "Approval requested" {
		t.Fatalf("osc 777: %+v", fx)
	}
	_, fx = ts.feed([]byte("\x1b]0;✳ Fix the tests\x07"))
	if !fx.titleChanged || ts.title() != "✳ Fix the tests" || fx.bells != 0 {
		t.Fatalf("title: %+v %q", fx, ts.title())
	}
	// A flood is bounded per read.
	var flood []byte
	for range 50 {
		flood = append(flood, "\x1b]9;spam\x07"...)
	}
	if _, fx = ts.feed(flood); len(fx.notifications) != maxNotificationsPerFeed {
		t.Fatalf("flood kept %d", len(fx.notifications))
	}
}

// fastActivity shortens the activity thresholds for a test. It must be
// called before the worker starts, which then only reads them.
func fastActivity(t *testing.T) {
	saved := []time.Duration{stallAfter, activityTick}
	stallAfter, activityTick = time.Second, 50*time.Millisecond
	t.Cleanup(func() { stallAfter, activityTick = saved[0], saved[1] })
}

// signalAgent rings the bell, notifies, sets its title, writes bursts of
// output, and runs a foreground job under job control, on command.
const signalAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    bell) printf '\a';;
    bells) i=0; while [ $i -lt 20 ]; do printf '\a'; sleep 0.02; i=$((i+1)); done; echo bells-done;;
    notify) printf '\033]777;notify;Codex;Approve the command?\033\\';;
    title) printf '\033]0;Fixing the tests\007';;
    burst) i=0; while [ $i -lt 4 ]; do echo tick; sleep 0.1; i=$((i+1)); done;;
    fg) set -m; sleep 3; set +m; echo slept;;
    quit) exit 3;;
    *) echo "got:$l";;
  esac
done`

func (h *harness) eventsSince(after uint64) []session.Event {
	h.t.Helper()
	events, err := h.store.EventsAfter(after, h.sessionID, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return events
}

// waitEvent waits for an event of kind after id, and returns it.
func (h *harness) waitEvent(after uint64, kind session.EventKind) session.Event {
	h.t.Helper()
	var found session.Event
	h.eventually(string(kind)+" event", func() bool {
		for _, ev := range h.eventsSince(after) {
			if ev.Kind == kind {
				found = ev
				return true
			}
		}
		return false
	})
	return found
}

func (h *harness) record() *session.Record {
	h.t.Helper()
	rec, err := h.store.GetSession(h.sessionID)
	if err != nil || rec == nil {
		h.t.Fatalf("session: %v %v", rec, err)
	}
	return rec
}

// Bells, notifications and titles are observed facts, recorded as info
// events: they raise no attention and change no activity (the program's
// status reports alone decide those). The foreground sample and the stall
// watch still work for programs that do not report.
func TestObservedSignalsFromThePTY(t *testing.T) {
	fastActivity(t)
	h := startWorkerWith(t, signalAgent)
	h.eventually("the foreground process", func() bool { return h.record().Foreground != nil })

	// A bell is information, recorded once per quiet window however often
	// it rings; the session stays plainly working with nothing pending.
	h.sendInput("bells\n")
	bell := h.waitEvent(0, session.EventBell)
	if bell.Attention != session.AttentionInfo {
		t.Fatalf("bell = %#v", bell)
	}
	h.eventually("the bell loop to finish", func() bool { return strings.Contains(h.history(), "bells-done") })
	for _, ev := range h.eventsSince(bell.ID) {
		if ev.Kind == session.EventBell {
			t.Fatalf("a bell loop recorded a second event: %#v", ev)
		}
	}
	rec := h.record()
	if rec.Activity != session.ActivityWorking || rec.Attention != session.AttentionInfo || rec.LastOutputAt == nil {
		t.Fatalf("after bell: %#v", rec)
	}

	// A notification records its text, as information, and the title is
	// kept as context.
	h.sendInput("title\n")
	h.sendInput("notify\n")
	note := h.waitEvent(bell.ID, session.EventNotification)
	if note.Summary != "Codex: Approve the command?" || note.Attention != session.AttentionInfo {
		t.Fatalf("notification = %#v", note)
	}
	if rec := h.record(); rec.Title == nil || *rec.Title != "Fixing the tests" {
		t.Fatalf("title = %v", rec.Title)
	}

	// A foreground job under job control is the session's foreground
	// command, and long silence while a non-reporting command runs is a
	// stall.
	h.sendInput("fg\n")
	h.eventually("sleep in the foreground", func() bool {
		rec := h.record()
		return rec.Foreground != nil && *rec.Foreground == "sleep"
	})
	stalled := h.waitEvent(note.ID, session.EventStalled)
	if !strings.Contains(stalled.Summary, "`sleep`") || stalled.Attention != session.AttentionNotice {
		t.Fatalf("stalled = %#v", stalled)
	}

	// Its end supersedes earlier attention.
	h.sendInput("quit\n")
	h.waitExit()
	rec = h.record()
	if rec.Activity != session.ActivityExited || rec.Attention != session.AttentionAction || rec.Status != session.StatusFailed {
		t.Fatalf("after exit: %#v", rec)
	}
	all := h.eventsSince(0)
	if last := all[len(all)-1]; last.Kind != session.EventFailed {
		t.Fatalf("last event = %#v", last)
	}
}

// The recorder never blocks its caller: past maxPendingEvents it drops.
func TestRecorderDropsWhenFull(t *testing.T) {
	_, h := newRoot(t)
	r := newRecorder(h.store, h.sessionID, 1)
	// Fill the queue under the lock, so the writer cannot drain it first.
	r.mu.Lock()
	for range maxPendingEvents {
		r.events = append(r.events, db.NewEvent{SessionID: h.sessionID, Kind: session.EventIdle})
	}
	r.mu.Unlock()
	for range 10 {
		r.event(db.NewEvent{Kind: session.EventBell, Summary: "x"})
	}
	r.close()
	if n := len(h.eventsSince(0)); n > maxPendingEvents+1 { // +1: created
		t.Fatalf("%d events written", n)
	}
	// Calls after close are ignored.
	r.event(db.NewEvent{Kind: session.EventBell})
	r.setActivity(db.Activity{Activity: session.ActivityIdle})
}

func TestCleanSummaries(t *testing.T) {
	if got := clean("  a\tb\x1b[31m\nc  "); got != "a b [31m c" {
		t.Fatalf("got %q", got)
	}
	if got := clean(strings.Repeat("x", 300)); len([]rune(got)) != maxSummary {
		t.Fatalf("len %d", len([]rune(got)))
	}
}
