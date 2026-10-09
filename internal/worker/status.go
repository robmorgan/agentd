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
	// title: one line of untrusted program text, cleaned at derivation.
	message string
	// progress is 0-100, or -1 when not reported.
	progress int
	// summary is the one-line event summary for this status, computed
	// once here because recordNative consults it on every feed and tick.
	summary string
}

// eventSummary is the one-line summary for an event about this status:
// the program's message (already cleaned by deriveNow), prefixed with why
// it is blocked, falling back to the bare state, and held to maxSummary
// like every other summary (the prefix could push a cut message past it).
func eventSummary(n *nativeStatus) string {
	s := n.message
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
	if r := []rune(s); len(r) > maxSummary {
		s = string(r[:maxSummary-1]) + "…"
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
	// derived caches derive's last reading until the records (or the
	// input time) change, since derive is consulted on every PTY feed.
	derived *nativeStatus
	dirty   bool
}

func newStatusTracker() *statusTracker {
	return &statusTracker{records: make(map[string]*statusRecord)}
}

// apply folds one report into the records. The empty id is the program's
// own (root) record. A state this build does not know is ignored, as the
// libghostty binding asks: coercing it to a known one could demote an
// urgent future state to a quiet record.
func (t *statusTracker) apply(now time.Time, r libghostty.ProgramStatus) {
	if _, ok := statusStateName(r.State); !ok && r.State != libghostty.ProgramStatusStateClear {
		return
	}
	t.dirty = true
	if r.State == libghostty.ProgramStatusStateClear {
		if r.ID == "" {
			clear(t.records)
			return
		}
		delete(t.records, r.ID)
		prefix := r.ID + "/"
		for id := range t.records {
			if strings.HasPrefix(id, prefix) {
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

// promptSeen notes a new shell prompt (OSC 133 A): the program the records
// came from is over, so its working, blocked and idle records are dropped
// (keeping an idle record would pin the session to it while the next,
// possibly non-reporting, command runs). Done and error records stay until
// the user sees them (userSaw) or the program clears them.
//
// Assumed: a program marks prompts with OSC 133 A or reports status over
// OSC 7501, not both for the same prompt — one that did both would drop
// its own blocked record the moment it asks. Shells integrate 133; the
// agents that report 7501 do not mark their internal prompts with it.
func (t *statusTracker) promptSeen() {
	t.dirty = true
	for id, rec := range t.records {
		switch rec.State {
		case libghostty.ProgramStatusStateWorking, libghostty.ProgramStatusStateBlocked,
			libghostty.ProgramStatusStateIdle:
			delete(t.records, id)
		}
	}
}

// noteInput notes that someone typed into the session: whatever finished
// has been seen, and whatever was blocked counts as answered until the
// program reports again.
func (t *statusTracker) noteInput(now time.Time) {
	t.lastInput = now
	t.dirty = true
	t.userSaw()
}

// userSaw drops the done and error records: the specification keeps them
// only "until the user has seen them", which here is typing into the
// session or attaching to look at it. Without this, one finished tool's
// record would pin the session's activity (and demote its heuristics) for
// every later program that does not report.
func (t *statusTracker) userSaw() {
	t.dirty = true
	for id, rec := range t.records {
		switch rec.State {
		case libghostty.ProgramStatusStateDone, libghostty.ProgramStatusStateError:
			delete(t.records, id)
		}
	}
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
		switch {
		case top == nil,
			statusRank(state) > statusRank(topState),
			// Among equal ranks the latest report wins; reports from one
			// feed share a time, so the id breaks the remaining tie (map
			// order must not decide whose message the session shows).
			statusRank(state) == statusRank(topState) && rec.UpdatedAt.After(top.UpdatedAt),
			statusRank(state) == statusRank(topState) && rec.UpdatedAt.Equal(top.UpdatedAt) && id < topID:
			top, topID, topState = rec, id, state
		}
	}
	if top == nil {
		return nil
	}
	n := &nativeStatus{
		activity: statusActivity(topState),
		app:      t.app(topID, top),
		// The message is cleaned here once (it is untrusted program
		// text), so the summary, the session record and the change
		// comparison in setNative all see the same canonical form.
		message:  clean(top.Message),
		progress: int(top.Progress),
	}
	if n.message == "" {
		n.message = clean(top.Title)
	}
	if topState == libghostty.ProgramStatusStateBlocked {
		n.kind = statusKindName(top.Kind)
	}
	n.summary = eventSummary(n)
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

// statusHandoff is the tracker's state carried across a worker handoff.
// States and kinds travel as the specification's words rather than
// libghostty's numeric values, which are not pinned across versions; a
// record whose state the next image does not know is dropped (the program
// re-reports, as it would to a fresh terminal). An unknown kind degrades
// to none instead: the record keeps its blocked state and only loses the
// word for why, which the next report restores.
type statusHandoff struct {
	Records   map[string]statusHandoffRecord
	LastInput time.Time
}

type statusHandoffRecord struct {
	State     string
	Kind      string `json:",omitempty"`
	Progress  int8
	App       string `json:",omitempty"`
	Title     string `json:",omitempty"`
	Message   string `json:",omitempty"`
	UpdatedAt time.Time
}

func (t *statusTracker) handoff() *statusHandoff {
	if t == nil {
		return nil
	}
	h := &statusHandoff{Records: make(map[string]statusHandoffRecord, len(t.records)),
		LastInput: t.lastInput}
	for id, rec := range t.records {
		state, ok := statusStateName(rec.State)
		if !ok {
			// apply never stores an unknown state; a record from a future
			// build is dropped rather than misnamed.
			continue
		}
		h.Records[id] = statusHandoffRecord{
			State: state, Kind: statusKindName(rec.Kind),
			Progress: rec.Progress, App: rec.App, Title: rec.Title, Message: rec.Message,
			UpdatedAt: rec.UpdatedAt,
		}
	}
	return h
}

// restoreStatusTracker rebuilds the tracker from a handoff; h may be nil
// (an image older than the records, or a session that never reported).
func restoreStatusTracker(h *statusHandoff) *statusTracker {
	t := newStatusTracker()
	if h == nil {
		return t
	}
	t.lastInput, t.dirty = h.LastInput, true
	for id, rec := range h.Records {
		state, ok := statusStateFromName(rec.State)
		if !ok {
			continue
		}
		t.records[id] = &statusRecord{
			State: state, Kind: statusKindFromName(rec.Kind),
			Progress: rec.Progress, App: rec.App, Title: rec.Title, Message: rec.Message,
			UpdatedAt: rec.UpdatedAt,
		}
	}
	return t
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

func statusKindFromName(name string) libghostty.ProgramStatusKind {
	switch name {
	case "permission":
		return libghostty.ProgramStatusKindPermission
	case "question":
		return libghostty.ProgramStatusKindQuestion
	case "auth":
		return libghostty.ProgramStatusKindAuth
	}
	return libghostty.ProgramStatusKindNone
}

// statusStateName is the specification's word for a record's state, and
// whether this build knows the state at all. clear never makes a record.
func statusStateName(s libghostty.ProgramStatusState) (string, bool) {
	switch s {
	case libghostty.ProgramStatusStateIdle:
		return "idle", true
	case libghostty.ProgramStatusStateWorking:
		return "working", true
	case libghostty.ProgramStatusStateDone:
		return "done", true
	case libghostty.ProgramStatusStateBlocked:
		return "blocked", true
	case libghostty.ProgramStatusStateError:
		return "error", true
	}
	return "", false
}

func statusStateFromName(name string) (libghostty.ProgramStatusState, bool) {
	switch name {
	case "idle":
		return libghostty.ProgramStatusStateIdle, true
	case "working":
		return libghostty.ProgramStatusStateWorking, true
	case "done":
		return libghostty.ProgramStatusStateDone, true
	case "blocked":
		return libghostty.ProgramStatusStateBlocked, true
	case "error":
		return libghostty.ProgramStatusStateError, true
	}
	return 0, false
}
