package worker

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/session"
)

// Activity and attention detection.
//
// The worker sees everything the agent writes, so it is where "which agent
// needs me?" is answered. The primary signal is the program's own word:
// program status reports (OSC 7501), which statusTracker keeps and reads
// (status.go). While any records exist, they decide the session's activity
// (working with progress, blocked on the user and why, done, error) and
// raise its attention.
//
// For programs that do not report, the worker falls back to heuristics,
// none of which needs the agent's cooperation beyond what terminals
// already understand:
//
//   - the bell (BEL), and desktop notifications (OSC 9, OSC 777;notify),
//     which libghostty's parser reports while it shadows the PTY. Claude
//     Code and Codex emit them when they wait for the user, if configured
//     to (see the README). Either makes the session `waiting` and raises
//     action-level attention until someone types into it. While the
//     program reports status, these are recorded as info-level context
//     instead: the reports say what the program needs.
//   - output timing: output within idleAfter means `working`, none means
//     `idle`; going idle after working records an idle event.
//
// Two more signals stay on regardless, since no report carries them:
//
//   - the PTY's foreground process group (tcgetpgrp on the master), whose
//     leader's name is the session's foreground command; sampled every
//     activityTick.
//   - the terminal title (OSC 0/2), kept as context.
//
// The "7501:" comments mark the seams between the two sources; if the
// heuristics are ever dropped, they list what goes.
//
// Cost on the PTY hot path: the effects come out of the VTWrite the worker
// already makes, and each output chunk adds a clock read. Database writes
// happen only on transitions, off the owner goroutine (recorder).
//
// Noise: a bell is recorded unless another was seen within bellQuiet with
// no input since, so a program ringing in a loop records one event; a
// repeated notification text likewise. Idle events are at most one per
// idleEventGap, and a working event is recorded only to close an idle one.

var (
	// idleAfter is how long without output makes a session idle.
	idleAfter = 10 * time.Second
	// stallAfter is how long a command other than the agent may hold the
	// foreground without output before the session counts as stalled.
	stallAfter = 30 * time.Minute
	// activityTick is how often idleness and the foreground are checked.
	activityTick = time.Second
	// bellQuiet and noticeQuiet suppress repeats of a bell or the same
	// notification without input in between.
	bellQuiet   = 10 * time.Second
	noticeQuiet = 10 * time.Second
	// idleEventGap is the least time between two idle events.
	idleEventGap = 30 * time.Second
)

// bellAfterNotice: a bell this soon after a notification is the same
// request (Claude Code's "iterm2_with_bell" sends both).
const bellAfterNotice = 2 * time.Second

// maxSummary bounds the text kept from a notification or title.
const maxSummary = 200

// activityTracker turns PTY output, input and foreground samples into the
// session's activity and events. It is owned by the owner goroutine (it
// lives in ownerState) and never blocks: what it records goes to the
// recorder.
type activityTracker struct {
	rec       *recorder
	agentPGID int

	state        session.Activity
	waiting      bool
	lastOutput   time.Time
	workingSince time.Time
	// idleRecorded is set when an idle event was recorded for the current
	// quiet period, so the working event that ends it is recorded too.
	idleRecorded  bool
	lastIdleEvent time.Time
	stalled       bool
	// lastBell is the last bell seen since the last input, recorded or not.
	lastBell     time.Time
	lastNotice   string
	lastNoticeAt time.Time

	fgPGID     int
	foreground string
	title      string

	// 7501: native is the program's own status (nil while it has no
	// records, which lets the heuristics apply). The last event recorded
	// for it dedups repeats: a program flipping between the same states,
	// or re-reporting the same block, gains nothing within noticeQuiet.
	// Action-level events additionally keep a rate floor of one per
	// noticeQuiet (lastNativeActionAt), like the bell's quiet rule: a
	// program churning blocked messages must not drive notifications at
	// report rate.
	// Non-action kinds additionally keep a per-kind gap
	// (lastNativeKindAt), like the heuristic idleEventGap: alternating
	// states or churning text record at most one event per kind per
	// noticeQuiet.
	native             *nativeStatus
	lastNativeKind     session.EventKind
	lastNativeSummary  string
	lastNativeAt       time.Time
	lastNativeActionAt time.Time
	lastNativeKindAt   map[session.EventKind]time.Time
}

func newActivityTracker(rec *recorder, agentPGID int, now time.Time) *activityTracker {
	return &activityTracker{rec: rec, agentPGID: agentPGID, state: session.ActivityWorking,
		lastOutput: now, workingSince: now,
		lastNativeKindAt: make(map[session.EventKind]time.Time)}
}

// preOutput notes a feed's arrival before its status effects are applied:
// setNative runs first (afterFeed), and judging it against a stale output
// clock would turn "the records ended after a quiet spell" into a bogus
// idle event with a negative duration.
func (a *activityTracker) preOutput(now time.Time) {
	a.lastOutput = now
	a.stalled = false
}

// output notes PTY output and its effects. watched reports an interactive
// client attached.
func (a *activityTracker) output(now time.Time, fx terminalEffects, title string, watched bool) {
	a.lastOutput = now
	a.stalled = false
	if fx.titleChanged {
		a.title = clean(title)
	}
	for _, n := range fx.notifications {
		text := clean(n.Body)
		if t := clean(n.Title); t != "" && text != "" {
			text = t + ": " + text
		} else if text == "" {
			text = t
		}
		if text == "" {
			text = "notification"
		}
		if text != a.lastNotice || now.Sub(a.lastNoticeAt) >= noticeQuiet {
			e := db.NewEvent{Kind: session.EventNotification, Summary: text, At: now}
			// 7501: while the program reports its own status, a
			// notification is context, not the attention signal.
			if a.native != nil {
				e.Attention = session.AttentionInfo
			}
			a.rec.event(e)
		}
		a.lastNotice, a.lastNoticeAt = text, now
		if a.native == nil {
			a.waiting = true
		}
	}
	if fx.bells > 0 {
		quiet := a.lastBell.IsZero() || now.Sub(a.lastBell) >= bellQuiet
		if quiet && now.Sub(a.lastNoticeAt) >= bellAfterNotice {
			e := db.NewEvent{Kind: session.EventBell, At: now, Summary: "bell"}
			if a.title != "" {
				e.Summary = "bell: " + a.title
			}
			// 7501: as above.
			if a.native != nil {
				e.Attention = session.AttentionInfo
			}
			a.rec.event(e)
		}
		a.lastBell = now
		if a.native == nil {
			a.waiting = true
		}
	}
	a.update(now, watched, false)
}

// 7501: setNative gives the tracker the program's own reading of its
// status, from the session's status records: the primary source of the
// activity, overriding the heuristics while it is non-nil. emit also
// records the event a change implies; input-driven calls pass false, since
// typing answers a block silently (as input does for the heuristics).
func (a *activityTracker) setNative(now time.Time, n *nativeStatus, watched, emit bool) {
	prev := a.native
	a.native = n
	if n == nil {
		if prev != nil {
			// The records are gone (cleared, or dropped at a shell
			// prompt): the heuristics resume from a fresh working period.
			a.workingSince = now
			a.idleRecorded = false
			a.update(now, watched, true)
		}
		return
	}
	if prev == nil {
		// The program's reports supersede whatever heuristic wait was
		// pending: without this, a bell from before the first report
		// would resurface, hours stale, when the records are cleared.
		a.waiting = false
		a.lastBell, a.lastNotice, a.lastNoticeAt = time.Time{}, "", time.Time{}
	}
	if emit {
		a.recordNative(prev, n, now, watched)
	}
	// Force the write-through when anything in the status changed, so a
	// progress update reaches the session record without a state change.
	a.update(now, watched, prev == nil || *prev != *n)
}

// 7501: recordNative records the event a change in the program's status
// implies. A blocked program asking something new re-raises attention even
// without a state change; repeats of the same event within noticeQuiet are
// dropped, and action-level events keep a floor of one per noticeQuiet.
// An action event the floor defers is retried on later calls (it still
// differs from the last recorded one) until it records, so a new question
// alerts at most noticeQuiet late, never not at all.
func (a *activityTracker) recordNative(prev, n *nativeStatus, now time.Time, watched bool) {
	kind, ok := nativeEventKind(n.activity)
	if !ok {
		return
	}
	summary := n.summary
	action := kind.DefaultAttention() == session.AttentionAction
	changed := prev == nil || prev.activity != n.activity
	if !changed && action && (kind != a.lastNativeKind || summary != a.lastNativeSummary) {
		// Still blocked (or failing), but on something not recorded yet:
		// a new question, or an event the floor deferred.
		changed = true
	}
	if !changed {
		return
	}
	// Non-action kinds wait out a per-kind gap however their text churns;
	// the session record still carries every latest message.
	if !action && now.Sub(a.lastNativeKindAt[kind]) < noticeQuiet {
		return
	}
	if kind == a.lastNativeKind && summary == a.lastNativeSummary && now.Sub(a.lastNativeAt) < noticeQuiet {
		return
	}
	// The rate floor: the session record already carries every new
	// message (setActivity), but notifications and --exec fire at most
	// once per noticeQuiet.
	if action && now.Sub(a.lastNativeActionAt) < noticeQuiet {
		return
	}
	e := db.NewEvent{Kind: kind, Summary: summary, At: now}
	if kind == session.EventIdle && watched {
		// Someone attached is watching it; it only needs noticing when
		// nobody is (the same rule as the heuristic idle event).
		e.Attention = session.AttentionInfo
	}
	a.rec.event(e)
	a.lastNativeKind, a.lastNativeSummary, a.lastNativeAt = kind, summary, now
	a.lastNativeKindAt[kind] = now
	if action {
		a.lastNativeActionAt = now
	}
}

// nativeEventKind is the event kind a native activity records.
func nativeEventKind(act session.Activity) (session.EventKind, bool) {
	switch act {
	case session.ActivityBlocked:
		return session.EventBlocked, true
	case session.ActivityError:
		return session.EventError, true
	case session.ActivityDone:
		return session.EventDone, true
	case session.ActivityIdle:
		return session.EventIdle, true
	case session.ActivityWorking:
		return session.EventWorking, true
	}
	return "", false
}

// 7501: exportAlertMemory copies the native-event dedup state into a
// status handoff. Seeding the next image from its restored records would
// mark a floor-deferred question as already alerted and lose it; the real
// memory tells the two apart.
func (a *activityTracker) exportAlertMemory(h *statusHandoff) {
	if h == nil {
		return
	}
	h.LastAlertKind = string(a.lastNativeKind)
	h.LastAlertSummary = a.lastNativeSummary
	h.LastAlertAt = a.lastNativeAt
	h.LastActionAt = a.lastNativeActionAt
	h.LastKindAt = make(map[string]time.Time, len(a.lastNativeKindAt))
	for kind, at := range a.lastNativeKindAt {
		h.LastKindAt[string(kind)] = at
	}
}

// restoreAlertMemory is exportAlertMemory's inverse, for a handoff
// restore.
func (a *activityTracker) restoreAlertMemory(h *statusHandoff) {
	if h == nil {
		return
	}
	a.lastNativeKind = session.EventKind(h.LastAlertKind)
	a.lastNativeSummary = h.LastAlertSummary
	a.lastNativeAt = h.LastAlertAt
	a.lastNativeActionAt = h.LastActionAt
	for kind, at := range h.LastKindAt {
		a.lastNativeKindAt[session.EventKind(kind)] = at
	}
}

// input notes that someone typed into the session (an attached client or
// send-input): whatever it was waiting for has been answered.
func (a *activityTracker) input(now time.Time, watched bool) {
	a.waiting = false
	a.lastBell, a.lastNotice, a.lastNoticeAt = time.Time{}, "", time.Time{}
	a.update(now, watched, false)
}

// tick checks for idleness and stalls, samples the foreground process,
// and retries a floor-deferred native action event: without this, a
// program that re-asked within the floor and then went silent (waiting
// for its answer) would never alert. Deriving fresh from a.native means a
// block that input answered, or a clear removed, cancels the retry
// naturally; the changed/dedup/floor checks make it a no-op otherwise.
func (a *activityTracker) tick(now time.Time, fgPGID int, watched bool) {
	if a.native != nil {
		a.recordNative(a.native, a.native, now, watched)
	}
	fgChanged := false
	if fgPGID > 0 && fgPGID != a.fgPGID {
		// The group is remembered only once its name is known: a process
		// that has just started (or a busy machine) can fail the lookup,
		// which the next tick then tries again.
		if name := processName(fgPGID); name != "" {
			a.fgPGID = fgPGID
			if name != a.foreground {
				a.foreground = name
				fgChanged = true
			}
		}
	}
	a.update(now, watched, fgChanged)
	if a.state == session.ActivityIdle && !a.stalled && now.Sub(a.lastOutput) >= stallAfter &&
		a.fgPGID > 0 && a.fgPGID != a.agentPGID {
		a.stalled = true
		a.rec.event(db.NewEvent{Kind: session.EventStalled, At: now,
			Summary: fmt.Sprintf("no output for %s while `%s` runs", roundDuration(now.Sub(a.lastOutput)), a.foreground)})
	}
}

// update recomputes the activity, records the events its change implies,
// and reports a change to the recorder.
func (a *activityTracker) update(now time.Time, watched, force bool) {
	next := session.ActivityWorking
	switch {
	// 7501: while the program reports its own status, it decides; the
	// waiting flag and the idle timer only judge programs that do not.
	case a.native != nil:
		next = a.native.activity
	case a.waiting:
		next = session.ActivityWaiting
	case now.Sub(a.lastOutput) >= idleAfter:
		next = session.ActivityIdle
	}
	if next == a.state && !force {
		return
	}
	prev := a.state
	a.state = next
	switch {
	// 7501: native transitions record their own events (recordNative);
	// the cases below are the heuristic idle/working events.
	case a.native != nil:
	case next == session.ActivityIdle && prev == session.ActivityWorking:
		if a.lastIdleEvent.IsZero() || now.Sub(a.lastIdleEvent) >= idleEventGap {
			// Someone attached is watching it go quiet; it only needs
			// noticing when nobody is.
			level := session.AttentionNotice
			if watched {
				level = session.AttentionInfo
			}
			summary := fmt.Sprintf("idle after %s of output", roundDuration(a.lastOutput.Sub(a.workingSince)))
			if a.title != "" {
				summary += ": " + a.title
			}
			a.rec.event(db.NewEvent{Kind: session.EventIdle, Attention: level, Summary: summary, At: now})
			a.idleRecorded = true
			a.lastIdleEvent = now
		}
	case next == session.ActivityWorking && prev != session.ActivityWorking:
		a.workingSince = now
		if a.idleRecorded {
			a.idleRecorded = false
			a.rec.event(db.NewEvent{Kind: session.EventWorking, Summary: "output resumed", At: now})
		}
	}
	act := db.Activity{Activity: a.state, Foreground: a.foreground, Title: a.title,
		LastOutputAt: a.lastOutput, StatusProgress: -1}
	if a.native != nil {
		// The message was cleaned when the status was derived.
		act.StatusApp, act.StatusKind = a.native.app, a.native.kind
		act.StatusMsg, act.StatusProgress = a.native.message, a.native.progress
	}
	a.rec.setActivity(act)
}

// clean makes program-supplied text safe and short for a one-line summary:
// control characters become spaces, runs of space collapse, and it is cut
// to maxSummary runes.
func clean(s string) string {
	// Format characters (unicode.Cf: bidirectional overrides, zero-width
	// characters) are invisible but reorder or hide what a summary shows,
	// exactly where the user decides what to approve; unicode.IsControl
	// does not cover them.
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}), " ")
	if r := []rune(s); len(r) > maxSummary {
		s = string(r[:maxSummary-1]) + "…"
	}
	return s
}

// roundDuration is d in its largest unit: 42s, 5m, 3h.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}

// foregroundPGID is the PTY's foreground process group, read from the
// master side (0 if it cannot be read). It goes through SyscallConn rather
// than Fd, which would switch the PTY to blocking mode under the pump.
func foregroundPGID(ptmx *os.File) int {
	conn, err := ptmx.SyscallConn()
	if err != nil {
		return 0
	}
	pgid := 0
	_ = conn.Control(func(fd uintptr) {
		if v, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil {
			pgid = v
		}
	})
	return pgid
}

// recorder writes a session's events and activity to state.db on its own
// goroutine, so a slow or locked database never stalls the owner goroutine
// and with it PTY output.
//
// Bounds: events wait in a queue of at most maxPendingEvents; while it is
// full further events are dropped (and counted in the worker log). Activity
// is a single slot: a newer snapshot replaces one not yet written, since
// only the latest matters. Pending events are written before the activity.
//
// Ownership: run is the only writer; close flushes what is pending, ends
// it, and makes later calls no-ops. Safe for concurrent use.
type recorder struct {
	db        *db.Database
	sessionID string
	workerPID int

	mu       sync.Mutex
	events   []db.NewEvent
	activity *db.Activity
	dropped  int
	closed   bool

	wake chan struct{} // capacity 1
	done chan struct{}
}

const maxPendingEvents = 64

func newRecorder(store *db.Database, sessionID string, workerPID int) *recorder {
	r := &recorder{db: store, sessionID: sessionID, workerPID: workerPID,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	go r.run()
	return r
}

func (r *recorder) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *recorder) event(e db.NewEvent) {
	e.SessionID = r.sessionID
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if len(r.events) >= maxPendingEvents {
		r.dropped++
		return
	}
	r.events = append(r.events, e)
	r.signal()
}

func (r *recorder) setActivity(a db.Activity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.activity = &a
	r.signal()
}

// close writes what is pending and stops the writer.
func (r *recorder) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.signal()
	<-r.done
}

func (r *recorder) run() {
	defer close(r.done)
	for range r.wake {
		r.mu.Lock()
		events, activity, dropped, closed := r.events, r.activity, r.dropped, r.closed
		r.events, r.activity, r.dropped = nil, nil, 0
		r.mu.Unlock()
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "session worker: dropped %d events while the database was busy\n", dropped)
		}
		for _, e := range events {
			if _, err := r.db.RecordEvent(e); err != nil {
				fmt.Fprintf(os.Stderr, "session worker: recording %s event: %v\n", e.Kind, err)
			}
		}
		if activity != nil {
			if err := r.db.SetActivity(r.sessionID, r.workerPID, *activity); err != nil {
				fmt.Fprintf(os.Stderr, "session worker: recording activity: %v\n", err)
			}
		}
		if closed {
			return
		}
	}
}
