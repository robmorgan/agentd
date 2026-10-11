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
// needs me?" is answered — by the program's own word: program status
// reports (OSC 7501), which statusTracker keeps and reads (status.go).
// The records decide the session's activity (working with progress,
// blocked on the user and why, done, error, idle) and raise its attention;
// a program that does not report shows as plainly `working` while it runs,
// with no judgment attached. There is no inferred state: agentd stopped
// guessing from bells and output timing when the harnesses adopted the
// protocol.
//
// Three observed signals remain, since no report carries them:
//
//   - the PTY's foreground process group (tcgetpgrp on the master), whose
//     leader's name is the session's foreground command; sampled every
//     activityTick. A non-reporting foreground command that is silent for
//     stallAfter records a stalled event.
//   - the terminal title (OSC 0/2), kept as context.
//   - bells and desktop notifications (OSC 9, OSC 777;notify), recorded as
//     info-level events for observability only: they are facts about what
//     the terminal emitted (useful when a reporter misbehaves), never an
//     attention signal.
//
// Cost on the PTY hot path: the effects come out of the VTWrite the worker
// already makes, and each output chunk adds a clock read. Database writes
// happen only on transitions, off the owner goroutine (recorder).
//
// Noise: a bell is recorded unless another was seen within bellQuiet with
// no input since, so a program ringing in a loop records one event; a
// repeated notification text likewise.

var (
	// stallAfter is how long a non-reporting command other than the agent
	// may hold the foreground without output before the session counts as
	// stalled.
	stallAfter = 30 * time.Minute
	// activityTick is how often the foreground is sampled and a deferred
	// native alert retried.
	activityTick = time.Second
	// bellQuiet and noticeQuiet suppress repeats of a bell or the same
	// notification without input in between; noticeQuiet also paces the
	// native event dedup and rate floor.
	bellQuiet   = 10 * time.Second
	noticeQuiet = 10 * time.Second
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

	state      session.Activity
	lastOutput time.Time
	stalled    bool
	// lastBell is the last bell seen since the last input, recorded or not.
	lastBell     time.Time
	lastNotice   string
	lastNoticeAt time.Time

	fgPGID     int
	foreground string
	title      string

	// native is the program's own status (nil while it has no records).
	// The last event recorded for it dedups repeats: a program flipping
	// between the same states, or re-reporting the same block, gains
	// nothing within noticeQuiet. Action-level events additionally keep a
	// rate floor of one per noticeQuiet (lastNativeActionAt): a program
	// churning blocked messages must not drive notifications at report
	// rate. Non-action kinds keep a per-kind gap (lastNativeKindAt):
	// alternating states or churning text record at most one event per
	// kind per noticeQuiet.
	native             *nativeStatus
	lastNativeKind     session.EventKind
	lastNativeSummary  string
	lastNativeAt       time.Time
	lastNativeActionAt time.Time
	lastNativeKindAt   map[session.EventKind]time.Time
}

func newActivityTracker(rec *recorder, agentPGID int, now time.Time) *activityTracker {
	return &activityTracker{rec: rec, agentPGID: agentPGID, state: session.ActivityWorking,
		lastOutput: now,
		lastNativeKindAt: make(map[session.EventKind]time.Time)}
}

// output notes PTY output and its effects. watched reports an interactive
// client attached.
func (a *activityTracker) output(now time.Time, fx terminalEffects, title string, watched bool) {
	a.lastOutput = now
	a.stalled = false
	// A changed title forces the write-through: with no inferred state
	// transitions, nothing else would carry it to the session record.
	forced := false
	if fx.titleChanged {
		if t := clean(title); t != a.title {
			a.title = t
			forced = true
		}
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
			// Observability only: what the program asks for comes from
			// its status reports, never inferred from a notification.
			a.rec.event(db.NewEvent{Kind: session.EventNotification, Summary: text, At: now,
				Attention: session.AttentionInfo})
		}
		a.lastNotice, a.lastNoticeAt = text, now
	}
	if fx.bells > 0 {
		quiet := a.lastBell.IsZero() || now.Sub(a.lastBell) >= bellQuiet
		if quiet && now.Sub(a.lastNoticeAt) >= bellAfterNotice {
			e := db.NewEvent{Kind: session.EventBell, At: now, Summary: "bell",
				Attention: session.AttentionInfo}
			if a.title != "" {
				e.Summary = "bell: " + a.title
			}
			a.rec.event(e)
		}
		a.lastBell = now
	}
	a.update(now, watched, forced)
}

// setNative gives the tracker the program's own reading of its status,
// from the session's status records: the sole source of the activity.
// emit also records the event a change implies; input-driven calls pass
// false, since typing answers a block silently.
func (a *activityTracker) setNative(now time.Time, n *nativeStatus, watched, emit bool) {
	prev := a.native
	a.native = n
	if n == nil {
		if prev != nil {
			// The records are gone (cleared, or dropped at a shell
			// prompt): back to plain working, status columns cleared.
			a.update(now, watched, true)
		}
		return
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
// send-input): the bell and notification dedup starts over, so a program
// asked again after input records again.
func (a *activityTracker) input(now time.Time, watched bool) {
	a.lastBell, a.lastNotice, a.lastNoticeAt = time.Time{}, "", time.Time{}
}

// tick samples the foreground process, retries a floor-deferred native
// action event (without this, a program that re-asked within the floor and
// then went silent would never alert; deriving fresh from a.native means a
// block that input answered, or a clear removed, cancels the retry
// naturally), and checks for stalls: a non-reporting command other than
// the agent holding the foreground in silence for stallAfter.
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
	if a.native == nil && !a.stalled && now.Sub(a.lastOutput) >= stallAfter &&
		a.fgPGID > 0 && a.fgPGID != a.agentPGID {
		a.stalled = true
		a.rec.event(db.NewEvent{Kind: session.EventStalled, At: now,
			Summary: fmt.Sprintf("no output for %s while `%s` runs", roundDuration(now.Sub(a.lastOutput)), a.foreground)})
	}
}

// update recomputes the activity (the program's reported status, or plain
// working while it runs without reporting) and reports a change to the
// recorder. Events come from recordNative; no state is inferred here.
func (a *activityTracker) update(now time.Time, watched, force bool) {
	next := session.ActivityWorking
	if a.native != nil {
		next = a.native.activity
	}
	if next == a.state && !force {
		return
	}
	a.state = next
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
