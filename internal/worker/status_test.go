package worker

import (
	"bytes"
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
	if len(fx.statusReports) != 1 {
		t.Fatalf("working report: %+v", fx)
	}
	if r := fx.statusReports[0]; r.State != libghostty.ProgramStatusStateWorking || r.Progress != 47 || r.App != "cargo" {
		t.Fatalf("working report = %+v", r)
	}

	// A BEL-terminated blocked report with base64 text.
	_, fx = ts.feed([]byte("\x1b]7501;state=blocked:kind=permission:msg=UnVuIHRlc3RzPw==\x07"))
	if len(fx.statusReports) != 1 {
		t.Fatalf("blocked report: %+v", fx)
	}
	if r := fx.statusReports[0]; r.Kind != libghostty.ProgramStatusKindPermission || r.Message != "Run tests?" {
		t.Fatalf("blocked report = %+v", r)
	}

	// A report split across reads arrives once complete.
	if _, fx = ts.feed([]byte("\x1b]7501;state=do")); len(fx.statusReports) != 0 {
		t.Fatalf("partial report: %+v", fx)
	}
	_, fx = ts.feed([]byte("ne\x1b\\"))
	if len(fx.statusReports) != 1 || fx.statusReports[0].State != libghostty.ProgramStatusStateDone {
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

	// A new primary shell prompt (OSC 133 A) is reported, which drops
	// working and blocked records.
	if _, fx = ts.feed([]byte("\x1b]133;A\x07")); !fx.promptStart {
		t.Fatalf("prompt: %+v", fx)
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

func TestStatusTrackerPromptDropsWorkingAndBlocked(t *testing.T) {
	now := time.Now()
	tr := newStatusTracker()
	tr.apply(now, report("a", libghostty.ProgramStatusStateWorking))
	tr.apply(now, report("b", libghostty.ProgramStatusStateBlocked))
	tr.apply(now, report("c", libghostty.ProgramStatusStateDone))
	tr.apply(now, report("d", libghostty.ProgramStatusStateError))
	tr.apply(now, report("e", libghostty.ProgramStatusStateIdle))
	tr.promptSeen()
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true, "e": true} {
		if _, ok := tr.records[id]; ok != want {
			t.Errorf("after a prompt, record %q kept = %v, want %v", id, ok, want)
		}
	}
	if !tr.seen() {
		t.Error("seen() = false after reports")
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
