package worker

import (
	"strings"
	"time"

	"go.mitchellh.com/libghostty"

	"github.com/robmorgan/agentd/internal/session"
)

// Program status (OSC 7501).
//
// A program that speaks the program status protocol reports what it is
// doing directly: idle, working (with progress), blocked on the user (with
// why), done, or error. libghostty parses and validates the reports (and
// answers the protocol's detection query); the terminal does not remember
// them, so the worker keeps the records here, per the rules in the
// specification (https://www.superlogical.com/rex/docs/build/program-status)
// and in libghostty's ProgramStatusFunc documentation.
//
// These records are the primary source of a session's activity: while any
// exist, they decide it, and the heuristics in activityTracker (bells,
// notifications, output timing) are demoted to a fallback for programs
// that do not report. See the "7501:" comments in activity.go.
//
// Ownership: a statusTracker belongs to the owner goroutine, like the
// activityTracker it feeds.

// maxStatusRecords is the most records kept per session; the record
// updated least recently is evicted first. The specification asks for at
// least 64 and recommends 256.
const maxStatusRecords = 256

// statusRecord is one program status record: the last report for one id.
type statusRecord struct {
	State    libghostty.ProgramStatusState
	Kind     libghostty.ProgramStatusKind
	Progress int8 // 0-100, -1 when not reported
	App      string
	Title    string
	Message  string
	// UpdatedAt orders eviction and decides whether input since the
	// record's last report answers a blocked record.
	UpdatedAt time.Time
}

// nativeStatus is the session-level reading of the status records: what
// the program itself says the session's activity is, with the context a
// summary wants. It comes from the record whose state dominates
// (deriveStatus).
type nativeStatus struct {
	activity session.Activity
	// kind is why the program is blocked: "permission", "question",
	// "auth", or "" (only with ActivityBlocked, and not always then).
	kind string
	// app is the program's stable name ("claude-code"), from the
	// dominating record or its nearest ancestor.
	app string
	// message is the dominating record's message, falling back to its
	// title: one line of untrusted program text.
	message string
	// progress is 0-100, or -1 when not reported.
	progress int
}

// eventSummary is the one-line summary for an event about this status:
// the program's message (cleaned, it is untrusted), prefixed with why it
// is blocked, falling back to the bare state.
func (n *nativeStatus) eventSummary() string {
	s := clean(n.message)
	if n.activity == session.ActivityBlocked && n.kind != "" {
		if s == "" {
			s = n.kind
		} else {
			s = n.kind + ": " + s
		}
	}
	if s == "" {
		s = string(n.activity)
	}
	return s
}

// statusTracker keeps a session's program status records.
type statusTracker struct {
	records map[string]*statusRecord
	// lastInput is when someone last typed into the session. A blocked
	// record reported before it counts as answered (working) until the
	// program reports again; the next report replaces the record and the
	// suppression dissolves.
	lastInput time.Time
	// reported is set once any report arrives this incarnation, so the
	// session can say the program speaks the protocol even after its
	// records are cleared.
	reported bool
	// derived caches derive's last reading until the records (or the
	// input time) change, since derive is consulted on every PTY feed.
	derived *nativeStatus
	dirty   bool
}

func newStatusTracker() *statusTracker {
	return &statusTracker{records: make(map[string]*statusRecord)}
}

// apply folds one report into the records. The empty id is the program's
// own (root) record.
func (t *statusTracker) apply(now time.Time, r libghostty.ProgramStatus) {
	t.reported = true
	t.dirty = true
	if r.State == libghostty.ProgramStatusStateClear {
		if r.ID == "" {
			clear(t.records)
			return
		}
		delete(t.records, r.ID)
		for id := range t.records {
			if strings.HasPrefix(id, r.ID+"/") {
				delete(t.records, id)
			}
		}
		return
	}
	// A report replaces its record entirely: fields it leaves empty are
	// cleared, not carried over.
	if _, ok := t.records[r.ID]; !ok && len(t.records) >= maxStatusRecords {
		t.evict()
	}
	t.records[r.ID] = &statusRecord{
		State: r.State, Kind: r.Kind, Progress: r.Progress,
		App: r.App, Title: r.Title, Message: r.Message,
		UpdatedAt: now,
	}
}

// evict removes the record updated least recently.
func (t *statusTracker) evict() {
	var oldest string
	var at time.Time
	first := true
	for id, rec := range t.records {
		if first || rec.UpdatedAt.Before(at) {
			oldest, at, first = id, rec.UpdatedAt, false
		}
	}
	if !first {
		delete(t.records, oldest)
	}
}

// promptSeen notes a new shell prompt (OSC 133 A): the command the records
// came from is over, so working and blocked records are dropped. Done and
// error records stay until the user sees them or the program clears them;
// idle ones (which the spec lets either way) stay too, so an interactive
// tool sitting at its own prompt remains visible.
func (t *statusTracker) promptSeen() {
	t.dirty = true
	for id, rec := range t.records {
		switch rec.State {
		case libghostty.ProgramStatusStateWorking, libghostty.ProgramStatusStateBlocked:
			delete(t.records, id)
		}
	}
}

// noteInput notes that someone typed into the session.
func (t *statusTracker) noteInput(now time.Time) {
	t.lastInput = now
	t.dirty = true
}

// seen reports whether any report arrived this incarnation.
func (t *statusTracker) seen() bool {
	return t.reported
}

// statusRank orders states by how much they need the user: blocked first,
// then error, working, done, idle.
func statusRank(s libghostty.ProgramStatusState) int {
	switch s {
	case libghostty.ProgramStatusStateBlocked:
		return 4
	case libghostty.ProgramStatusStateError:
		return 3
	case libghostty.ProgramStatusStateWorking:
		return 2
	case libghostty.ProgramStatusStateDone:
		return 1
	}
	return 0 // idle
}

// derive is the session-level reading of the records, or nil while there
// are none (the heuristics apply then). The dominating record is the one
// whose state needs the user most (ties go to the latest report): one
// blocked shard of a parallel program blocks the session, and one still
// working keeps it working however many are done.
func (t *statusTracker) derive() *nativeStatus {
	if !t.dirty {
		return t.derived
	}
	t.dirty = false
	t.derived = t.deriveNow()
	return t.derived
}

func (t *statusTracker) deriveNow() *nativeStatus {
	var top *statusRecord
	var topID string
	var topState libghostty.ProgramStatusState
	for id, rec := range t.records {
		state := t.effectiveState(rec)
		if top == nil || statusRank(state) > statusRank(topState) ||
			(statusRank(state) == statusRank(topState) && rec.UpdatedAt.After(top.UpdatedAt)) {
			top, topID, topState = rec, id, state
		}
	}
	if top == nil {
		return nil
	}
	n := &nativeStatus{
		activity: statusActivity(topState),
		app:      t.app(topID, top),
		message:  top.Message,
		progress: int(top.Progress),
	}
	if n.message == "" {
		n.message = top.Title
	}
	if topState == libghostty.ProgramStatusStateBlocked {
		n.kind = statusKindName(top.Kind)
	}
	return n
}

// effectiveState is the record's state with input suppression applied: a
// blocked record reported before the last input counts as working, since
// whatever it waited for has likely been answered. The program's next
// report corrects either way.
func (t *statusTracker) effectiveState(rec *statusRecord) libghostty.ProgramStatusState {
	if rec.State == libghostty.ProgramStatusStateBlocked && rec.UpdatedAt.Before(t.lastInput) {
		return libghostty.ProgramStatusStateWorking
	}
	return rec.State
}

// app is rec's app name, inherited from the nearest ancestor record when
// rec does not carry one.
func (t *statusTracker) app(id string, rec *statusRecord) string {
	if rec.App != "" {
		return rec.App
	}
	for id != "" {
		if i := strings.LastIndexByte(id, '/'); i >= 0 {
			id = id[:i]
		} else {
			id = ""
		}
		if parent, ok := t.records[id]; ok && parent.App != "" {
			return parent.App
		}
	}
	return ""
}

func statusActivity(s libghostty.ProgramStatusState) session.Activity {
	switch s {
	case libghostty.ProgramStatusStateBlocked:
		return session.ActivityBlocked
	case libghostty.ProgramStatusStateError:
		return session.ActivityError
	case libghostty.ProgramStatusStateWorking:
		return session.ActivityWorking
	case libghostty.ProgramStatusStateDone:
		return session.ActivityDone
	}
	return session.ActivityIdle
}

// statusKindName is the specification's word for a blocked record's kind,
// or "" for none.
func statusKindName(k libghostty.ProgramStatusKind) string {
	switch k {
	case libghostty.ProgramStatusKindPermission:
		return "permission"
	case libghostty.ProgramStatusKindQuestion:
		return "question"
	case libghostty.ProgramStatusKindAuth:
		return "auth"
	}
	return ""
}
