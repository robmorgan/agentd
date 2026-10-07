package worker

import (
	"fmt"
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
