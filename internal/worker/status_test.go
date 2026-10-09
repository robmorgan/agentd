package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mitchellh.com/libghostty"

	"github.com/robmorgan/agentd/internal/session"
)

func report(id string, state libghostty.ProgramStatusState, mod ...func(*libghostty.ProgramStatus)) libghostty.ProgramStatus {
	r := libghostty.ProgramStatus{ID: id, State: state, Progress: -1}
	for _, m := range mod {
		m(&r)
	}
	return r
}

// The shadow terminal reports program status (OSC 7501) from the same
// VTWrite that keeps the screen. This also pins what the worker expects of
// libghostty: parsed reports, a detection reply through the write-pty
// effect, and primary prompt starts. A libghostty bump that changes any of
// it fails here rather than silently muting the primary activity source.
func TestTerminalReportsProgramStatus(t *testing.T) {
	ts, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.close()

	// An ST-terminated report, fully parsed and validated by libghostty.
	_, fx := ts.feed([]byte("\x1b]7501;state=working:progress=47:app=cargo\x1b\\"))
	if len(fx.statusEvents) != 1 || fx.statusEvents[0].report == nil {
		t.Fatalf("working report: %+v", fx)
	}
	if r := fx.statusEvents[0].report; r.State != libghostty.ProgramStatusStateWorking || r.Progress != 47 || r.App != "cargo" {
		t.Fatalf("working report = %+v", r)
	}

	// A BEL-terminated blocked report with base64 text.
	_, fx = ts.feed([]byte("\x1b]7501;state=blocked:kind=permission:msg=UnVuIHRlc3RzPw==\x07"))
	if len(fx.statusEvents) != 1 || fx.statusEvents[0].report == nil {
		t.Fatalf("blocked report: %+v", fx)
	}
	if r := fx.statusEvents[0].report; r.Kind != libghostty.ProgramStatusKindPermission || r.Message != "Run tests?" {
		t.Fatalf("blocked report = %+v", r)
	}

	// A report split across reads arrives once complete.
	if _, fx = ts.feed([]byte("\x1b]7501;state=do")); len(fx.statusEvents) != 0 {
		t.Fatalf("partial report: %+v", fx)
	}
	_, fx = ts.feed([]byte("ne\x1b\\"))
	if len(fx.statusEvents) != 1 || fx.statusEvents[0].report == nil || fx.statusEvents[0].report.State != libghostty.ProgramStatusStateDone {
		t.Fatalf("completed report: %+v", fx)
	}

	// The detection query is answered (the query echoed back, with its
	// own terminator) and kept apart from the other terminal replies, so
	// the worker can send it even while a client terminal is attached.
	for _, query := range []string{"\x1b]7501;?\x1b\\", "\x1b]7501;?\x07"} {
		writes, fx := ts.feed([]byte(query))
		if len(writes) != 0 {
			t.Fatalf("query %q: reply in the pending writes: %q", query, writes)
		}
		if len(fx.statusReplies) != 1 || !bytes.Equal(fx.statusReplies[0], []byte(query)) {
			t.Fatalf("query %q: replies = %q", query, fx.statusReplies)
		}
	}

	// A new primary shell prompt (OSC 133 A) is reported in stream order
	// with the reports: a report AFTER the prompt in the same read belongs
	// to the next command and must survive the prompt's record drop.
	_, fx = ts.feed([]byte("\x1b]133;A\x07\x1b]7501;state=blocked\x1b\\"))
	if len(fx.statusEvents) != 2 || fx.statusEvents[0].report != nil || fx.statusEvents[1].report == nil {
		t.Fatalf("prompt then report: %+v", fx.statusEvents)
	}
}

// A coalesced read carrying a prompt and the next command's report applies
// them in stream order: the record reported after the prompt survives.
func TestPromptInSameFeedKeepsLaterReport(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	apply := func(events []statusEvent) {
		for _, ev := range events {
			if ev.report != nil {
				tr.apply(now, *ev.report)
			} else {
				tr.promptSeen()
			}
		}
	}
	// Report, then prompt: the old command's record is dropped.
	apply([]statusEvent{
		{report: &libghostty.ProgramStatus{State: libghostty.ProgramStatusStateBlocked, Progress: -1}},
		{},
	})
	if tr.derive() != nil {
		t.Fatalf("record survived the prompt that followed it: %+v", tr.derive())
	}
	// Prompt, then report: the new command's record survives.
	apply([]statusEvent{
		{},
		{report: &libghostty.ProgramStatus{State: libghostty.ProgramStatusStateBlocked, Progress: -1}},
	})
	if got := tr.derive(); got == nil || got.activity != session.ActivityBlocked {
		t.Fatalf("record reported after the prompt was dropped: %+v", got)
	}
}

// Typing or attaching means the user saw the session: done and error
// records drop (the spec keeps them only until seen), while a blocked
// record stays for the input-suppression rule to govern.
func TestUserSeenDropsDoneAndError(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	tr.apply(now, report("a", libghostty.ProgramStatusStateDone))
	tr.apply(now, report("b", libghostty.ProgramStatusStateError))
	tr.apply(now, report("c", libghostty.ProgramStatusStateBlocked))
	tr.noteInput(now.Add(time.Second))
	for id, want := range map[string]bool{"a": false, "b": false, "c": true} {
		if _, ok := tr.records[id]; ok != want {
			t.Errorf("after input, record %q kept = %v, want %v", id, ok, want)
		}
	}
}

func TestStatusTrackerDerivation(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		reports []libghostty.ProgramStatus
		want    *nativeStatus
	}{
		{"no reports", nil, nil},
		{
			"working with progress",
			[]libghostty.ProgramStatus{report("", libghostty.ProgramStatusStateWorking, func(r *libghostty.ProgramStatus) {
				r.Progress = 47
				r.App = "cargo"
				r.Message = "Compiling"
			})},
			&nativeStatus{activity: session.ActivityWorking, app: "cargo", message: "Compiling", progress: 47},
		},
		{
			"blocked beats working",
			[]libghostty.ProgramStatus{
				report("a", libghostty.ProgramStatusStateWorking),
				report("b", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) {
					r.Kind = libghostty.ProgramStatusKindPermission
					r.Message = "Run tests?"
				}),
			},
			&nativeStatus{activity: session.ActivityBlocked, kind: "permission", message: "Run tests?", progress: -1},
		},
		{
			"error beats working and done",
			[]libghostty.ProgramStatus{
				report("a", libghostty.ProgramStatusStateDone),
				report("b", libghostty.ProgramStatusStateError, func(r *libghostty.ProgramStatus) { r.Message = "build failed" }),
				report("c", libghostty.ProgramStatusStateWorking),
			},
			&nativeStatus{activity: session.ActivityError, message: "build failed", progress: -1},
		},
		{
			"working beats done",
			[]libghostty.ProgramStatus{
				report("a", libghostty.ProgramStatusStateDone),
				report("b", libghostty.ProgramStatusStateWorking),
			},
			&nativeStatus{activity: session.ActivityWorking, progress: -1},
		},
		{
			"done beats idle",
			[]libghostty.ProgramStatus{
				report("a", libghostty.ProgramStatusStateIdle),
				report("b", libghostty.ProgramStatusStateDone, func(r *libghostty.ProgramStatus) { r.Message = "All tests passed" }),
			},
			&nativeStatus{activity: session.ActivityDone, message: "All tests passed", progress: -1},
		},
		{
			"title stands in for a missing message",
			[]libghostty.ProgramStatus{report("", libghostty.ProgramStatusStateWorking, func(r *libghostty.ProgramStatus) {
				r.Title = "deploy us-east"
			})},
			&nativeStatus{activity: session.ActivityWorking, message: "deploy us-east", progress: -1},
		},
		{
			"replacing a record clears what the report leaves out",
			[]libghostty.ProgramStatus{
				report("", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) {
					r.Kind = libghostty.ProgramStatusKindQuestion
					r.Message = "Which region?"
					r.Progress = 80
				}),
				report("", libghostty.ProgramStatusStateWorking),
			},
			&nativeStatus{activity: session.ActivityWorking, progress: -1},
		},
		{
			"clear removes the record and its descendants",
			[]libghostty.ProgramStatus{
				report("build", libghostty.ProgramStatusStateError),
				report("build/test", libghostty.ProgramStatusStateBlocked),
				report("lint", libghostty.ProgramStatusStateDone),
				report("build", libghostty.ProgramStatusStateClear),
			},
			&nativeStatus{activity: session.ActivityDone, progress: -1},
		},
		{
			"clear without an id removes everything",
			[]libghostty.ProgramStatus{
				report("a", libghostty.ProgramStatusStateBlocked),
				report("", libghostty.ProgramStatusStateError),
				report("", libghostty.ProgramStatusStateClear),
			},
			nil,
		},
		{
			"app inherited from the nearest ancestor",
			[]libghostty.ProgramStatus{
				report("", libghostty.ProgramStatusStateWorking, func(r *libghostty.ProgramStatus) { r.App = "deploy" }),
				report("us-east", libghostty.ProgramStatusStateWorking, func(r *libghostty.ProgramStatus) { r.App = "regional" }),
				report("us-east/db", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) {
					r.Kind = libghostty.ProgramStatusKindAuth
				}),
			},
			&nativeStatus{activity: session.ActivityBlocked, kind: "auth", app: "regional", progress: -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newStatusTracker()
			for i, r := range tc.reports {
				tr.apply(now.Add(time.Duration(i)*time.Second), r)
			}
			got := tr.derive()
			if tc.want == nil {
				if got != nil {
					t.Fatalf("derive() = %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("derive() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Clearing "build" removes its descendants but never a sibling that merely
// shares the prefix text.
func TestStatusClearKeepsPrefixSiblings(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	tr.apply(now, report("build", libghostty.ProgramStatusStateWorking))
	tr.apply(now.Add(time.Second), report("build/test", libghostty.ProgramStatusStateWorking))
	tr.apply(now.Add(2*time.Second), report("builder", libghostty.ProgramStatusStateWorking))
	tr.apply(now.Add(3*time.Second), report("build", libghostty.ProgramStatusStateClear))
	if _, ok := tr.records["builder"]; !ok {
		t.Error(`clearing "build" removed the sibling "builder"`)
	}
	if _, ok := tr.records["build/test"]; ok {
		t.Error(`descendant "build/test" survived the clear`)
	}
}

// Reports from one PTY feed share a timestamp; among equal ranks the id
// breaks the tie, so map order never decides whose message the session
// shows.
func TestStatusTrackerTieBreakIsDeterministic(t *testing.T) {
	now := time.Now()
	for range 10 {
		tr := newStatusTracker()
		tr.apply(now, report("b", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) { r.Message = "from b" }))
		tr.apply(now, report("a", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) { r.Message = "from a" }))
		if got := tr.derive(); got.message != "from a" {
			t.Fatalf("derive() picked %q, want the smallest id to win the tie", got.message)
		}
	}
}

// A report whose state this build does not know is ignored entirely, as
// the libghostty binding asks: it must not pin the session to a bogus
// idle record or suppress the heuristics.
func TestStatusTrackerIgnoresUnknownStates(t *testing.T) {
	tr := newStatusTracker()
	tr.apply(time.Now(), report("x", libghostty.ProgramStatusState(99)))
	if len(tr.records) != 0 || tr.derive() != nil {
		t.Fatalf("unknown state stored: records=%d derive=%+v", len(tr.records), tr.derive())
	}
}

func TestStatusTrackerInputSuppressesBlocked(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	tr.apply(now, report("", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) {
		r.Kind = libghostty.ProgramStatusKindPermission
		r.Progress = 30
	}))
	if got := tr.derive(); got.activity != session.ActivityBlocked {
		t.Fatalf("before input: activity = %v, want blocked", got.activity)
	}

	// Typing counts the block as answered until the program reports again.
	tr.noteInput(now.Add(time.Second))
	got := tr.derive()
	if got.activity != session.ActivityWorking {
		t.Fatalf("after input: activity = %v, want working", got.activity)
	}
	if got.kind != "" {
		t.Fatalf("after input: kind = %q, want none", got.kind)
	}
	if got.progress != 30 {
		t.Fatalf("after input: progress = %d, want 30", got.progress)
	}

	// A fresh blocked report re-raises it.
	tr.apply(now.Add(2*time.Second), report("", libghostty.ProgramStatusStateBlocked))
	if got := tr.derive(); got.activity != session.ActivityBlocked {
		t.Fatalf("after a new report: activity = %v, want blocked", got.activity)
	}
}

func TestStatusTrackerPromptDropsWorkingBlockedAndIdle(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	tr.apply(now, report("a", libghostty.ProgramStatusStateWorking))
	tr.apply(now, report("b", libghostty.ProgramStatusStateBlocked))
	tr.apply(now, report("c", libghostty.ProgramStatusStateDone))
	tr.apply(now, report("d", libghostty.ProgramStatusStateError))
	tr.apply(now, report("e", libghostty.ProgramStatusStateIdle))
	tr.promptSeen()
	// The prompt means that program is over: working, blocked and idle
	// go; done and error stay until the user sees them.
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true, "e": false} {
		if _, ok := tr.records[id]; ok != want {
			t.Errorf("after a prompt, record %q kept = %v, want %v", id, ok, want)
		}
	}
}

// statusAgent reports program status on command; "Run tests?" and "All
// tests passed" in base64. -icanon so the query branch can read the
// detection reply, which no newline follows. "blocked" also rings the bell
// after the report: typing at the session would count the block as
// answered, so the two must ride one command.
const statusAgent = `stty -echo -icanon; echo ready
while IFS= read -r l; do
  case "$l" in
    blocked) printf '\033]7501;state=blocked:kind=permission:app=claude-code:msg=UnVuIHRlc3RzPw==\033\\'; sleep 0.2; printf '\a'; echo bell-rung;;
    done) printf '\033]7501;state=done:msg=QWxsIHRlc3RzIHBhc3NlZA==\033\\';;
    clear) printf '\033]7501;state=clear\033\\';;
    query) printf '\033]7501;?\033\\'; head -c 10 | cat -v; echo; echo query-done;;
    quit) exit 0;;
    *) echo "got:$l";;
  esac
done`

// A program that reports its own status is the primary source: its reports
// decide the activity and the events, bells are demoted to information,
// typing answers a block, and clearing the records hands the session back
// to the heuristics.
func TestProgramStatusFromThePTY(t *testing.T) {
	fastActivity(t)
	h := startWorkerWith(t, statusAgent)

	// The program reports it is blocked on a permission, then rings the
	// bell (as agents configured for both would).
	h.sendInput("blocked\n")
	blocked := h.waitEvent(0, session.EventBlocked)
	if blocked.Summary != "permission: Run tests?" || blocked.Attention != session.AttentionAction {
		t.Fatalf("blocked = %#v", blocked)
	}
	h.eventually("blocked activity", func() bool { return h.record().Activity == session.ActivityBlocked })
	if rec := h.record(); *rec.AttentionSummary != "permission: Run tests?" {
		t.Fatalf("attention summary = %q", *rec.AttentionSummary)
	}

	// The bell while the program reports status is context, not the
	// signal: information, and the session stays blocked.
	bell := h.waitEvent(blocked.ID, session.EventBell)
	if bell.Attention != session.AttentionInfo {
		t.Fatalf("bell while native = %#v", bell)
	}
	h.eventually("still blocked", func() bool { return h.record().Activity == session.ActivityBlocked })

	// Typing answers the block until the program reports again.
	h.sendInput("answered\n")
	h.eventually("blocked answered", func() bool { return h.record().Activity == session.ActivityWorking })

	// The program finishes.
	h.sendInput("done\n")
	done := h.waitEvent(bell.ID, session.EventDone)
	if done.Summary != "All tests passed" || done.Attention != session.AttentionNotice {
		t.Fatalf("done = %#v", done)
	}
	h.eventually("done activity", func() bool { return h.record().Activity == session.ActivityDone })

	// Clearing the records hands the activity back to the heuristics,
	// which judge the quiet session idle.
	h.sendInput("clear\n")
	h.eventually("heuristics resumed", func() bool { return h.record().Activity == session.ActivityIdle })

	h.sendInput("quit\n")
	h.waitExit()
}

// The worker answers the program status detection query itself, whether or
// not a client is attached: it consumes the reports either way.
func TestProgramStatusDetectionReply(t *testing.T) {
	fastActivity(t)
	h := startWorkerWith(t, statusAgent)

	// Nobody attached.
	h.sendInput("query\n")
	h.eventually("the detection reply", func() bool { return strings.Contains(h.history(), "7501;?") })

	// Attached: other terminal replies are left to the client's terminal,
	// but this one the worker still answers (the client here answers no
	// queries at all).
	h.attach(defaultGeometry)
	h.eventually("the first query to finish", func() bool { return strings.Contains(h.history(), "query-done") })
	h.sendInput("query\n")
	h.eventually("the detection reply while attached", func() bool {
		return strings.Count(h.history(), "7501;?") >= 2
	})
}

// The records cross a worker handoff (as JSON, like the rest of
// handoffState): a blocked agent must stay blocked, since the program will
// not re-report on its own. States travel as the specification's words, so
// a record from a future state is dropped rather than misread.
func TestStatusHandoffRoundTrip(t *testing.T) {
	now := time.Now().Round(0)
	tr := newStatusTracker()
	// Input precedes the reports (noteInput after them would count the
	// done record as seen and drop it).
	tr.noteInput(now.Add(-time.Minute))
	tr.apply(now, report("", libghostty.ProgramStatusStateBlocked, func(r *libghostty.ProgramStatus) {
		r.Kind = libghostty.ProgramStatusKindPermission
		r.App = "claude-code"
		r.Message = "Run tests?"
		r.Progress = 60
	}))
	tr.apply(now, report("lint", libghostty.ProgramStatusStateDone))

	data, err := json.Marshal(tr.handoff())
	if err != nil {
		t.Fatal(err)
	}
	var h statusHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	h.Records["future"] = statusHandoffRecord{State: "pondering", UpdatedAt: now}

	restored := restoreStatusTracker(&h)
	if len(restored.records) != 2 {
		t.Fatalf("restored %d records, want 2 (the future state dropped)", len(restored.records))
	}
	want := tr.derive()
	if got := restored.derive(); got == nil || *got != *want {
		t.Fatalf("derive() = %+v, want %+v", got, want)
	}

	// An image older than the records hands over nothing.
	if empty := restoreStatusTracker(nil); empty.derive() != nil {
		t.Fatalf("restore from nil: %+v", empty)
	}
}

// Value: protects=the events a native status change records (summaries
// fall back to the kind or the bare state, a silently answered block
// re-reported unchanged is not recorded again, a new question re-raises
// only past the one-per-noticeQuiet action rate floor, notifications are
// information while the program reports, error is action-level, native
// idle while watched is information);
// fails_when=recordNative's changed/dedup logic, the action rate floor,
// eventSummary's fallbacks, or the notification demotion regress;
// why_new=the PTY tests cover only the first block, the bell demotion and
// done; seam=none
func TestActivityTrackerNativeEvents(t *testing.T) {
	_, h := newRoot(t)
	rec := newRecorder(h.store, h.sessionID, 1)
	a := newActivityTracker(rec, 0, time.Now())
	at := func(s int) time.Time { return time.Now().Add(time.Duration(s) * time.Second) }
	blocked := &nativeStatus{activity: session.ActivityBlocked, kind: "permission", progress: -1}

	// A block with no message falls back to its kind.
	a.setNative(at(0), blocked, false, true)
	// Re-reporting the same block changes nothing.
	a.setNative(at(1), blocked, false, true)
	// Input answers it silently (writeInput passes emit=false)...
	a.setNative(at(2), &nativeStatus{activity: session.ActivityWorking, kind: "permission", progress: -1}, false, false)
	// ...and the program re-reporting the same block right after is the
	// same event: deduped within noticeQuiet, not recorded again.
	a.setNative(at(3), blocked, false, true)
	// A new question within the action rate floor updates the record but
	// records no event: notifications fire at most once per noticeQuiet.
	reAsk := *blocked
	reAsk.message = "Deploy to prod?"
	a.setNative(at(4), &reAsk, false, true)
	// Past the floor it re-raises, though the state is still blocked.
	a.setNative(at(12), &reAsk, false, true)
	// A notification while the program reports status is context, not the
	// signal, and the session stays blocked.
	fx := terminalEffects{notifications: []libghostty.TerminalDesktopNotification{{Title: "Codex", Body: "Approve?"}}}
	a.output(at(13), fx, "", false)
	if a.state != session.ActivityBlocked || a.waiting {
		t.Fatalf("after a notification while native: state=%v waiting=%v", a.state, a.waiting)
	}
	// An error is action-level too, so it waits out the same floor; native
	// idle while watched is information, with the bare state as summary.
	a.setNative(at(25), &nativeStatus{activity: session.ActivityError, message: "build failed", progress: -1}, false, true)
	a.setNative(at(26), &nativeStatus{activity: session.ActivityIdle, progress: -1}, true, true)
	rec.close()

	var got []session.Event
	for _, ev := range h.eventsSince(0) {
		if ev.Kind != session.EventCreated {
			got = append(got, ev)
		}
	}
	want := []struct {
		kind      session.EventKind
		summary   string
		attention session.AttentionLevel
	}{
		{session.EventBlocked, "permission", session.AttentionAction},
		{session.EventBlocked, "permission: Deploy to prod?", session.AttentionAction},
		{session.EventNotification, "Codex: Approve?", session.AttentionInfo},
		{session.EventError, "build failed", session.AttentionAction},
		{session.EventIdle, "idle", session.AttentionInfo},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d events, want %d: %#v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Summary != w.summary || got[i].Attention != w.attention {
			t.Errorf("event %d = %v %q %v, want %v %q %v",
				i, got[i].Kind, got[i].Summary, got[i].Attention, w.kind, w.summary, w.attention)
		}
	}
}

// Value: protects=a progress-only update (working 40% to 60%) reaching the
// session record although the activity state did not change;
// fails_when=setNative stops forcing the write-through on a changed status
// (update early-returns on an unchanged state); why_new=the end-to-end test
// reads one snapshot and never updates progress mid-state; seam=none
func TestNativeProgressWriteThrough(t *testing.T) {
	_, h := newRoot(t)
	if err := h.store.MarkRunning(h.sessionID, h.createdAt, 1, 2); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(h.store, h.sessionID, 1)
	now := time.Now()
	a := newActivityTracker(rec, 0, now)
	a.setNative(now, &nativeStatus{activity: session.ActivityWorking, app: "cargo", progress: 40}, false, true)
	a.setNative(now.Add(time.Second), &nativeStatus{activity: session.ActivityWorking, app: "cargo", progress: 60}, false, true)
	rec.close()
	r := h.record()
	if r.Activity != session.ActivityWorking || r.StatusProgress == nil || *r.StatusProgress != 60 {
		t.Fatalf("record = activity %v, progress %v, want working 60", r.Activity, r.StatusProgress)
	}
}

func TestStatusTrackerEvictsLeastRecentlyUpdated(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	for i := range maxStatusRecords {
		tr.apply(now.Add(time.Duration(i)*time.Second), report(fmt.Sprintf("r%d", i), libghostty.ProgramStatusStateWorking))
	}
	// Refresh the oldest record, making r1 the stalest.
	tr.apply(now.Add(time.Duration(maxStatusRecords)*time.Second), report("r0", libghostty.ProgramStatusStateWorking))
	if len(tr.records) != maxStatusRecords {
		t.Fatalf("records = %d, want %d", len(tr.records), maxStatusRecords)
	}
	tr.apply(now.Add(time.Duration(maxStatusRecords+1)*time.Second), report("new", libghostty.ProgramStatusStateWorking))
	if len(tr.records) != maxStatusRecords {
		t.Fatalf("records after eviction = %d, want %d", len(tr.records), maxStatusRecords)
	}
	if _, ok := tr.records["r1"]; ok {
		t.Error("r1 still present, want it evicted as least recently updated")
	}
	if _, ok := tr.records["r0"]; !ok {
		t.Error("r0 evicted although it was refreshed")
	}
	if _, ok := tr.records["new"]; !ok {
		t.Error("new record missing")
	}
}
