package session

import (
	"fmt"
	"time"
)

// Events and attention.
//
// An event is something that happened to a session that a user may want to
// know about without watching its terminal: a lifecycle change the daemon
// or worker caused or observed, or a signal the worker read from the PTY
// stream (a bell, a desktop notification, output stopping or starting).
// Events are persisted in state.db and numbered by one counter per runtime
// root, so a client can follow them from any point (`agent events`).
//
// Every event has an attention level. A session's record carries the
// attention it currently needs: the highest level of the events raised
// since the user last looked at it (an interactive attach or detach, which
// acknowledges it), with that event's summary. Lifecycle endings (exited,
// failed, killed, worker lost) replace the attention instead of raising
// it, since they supersede whatever the agent asked for before it stopped.

// EventKind names what happened. Kinds are strings on the wire, so a client
// shows kinds newer than itself rather than failing to decode them.
type EventKind string

const (
	// EventCreated: the daemon created the session row (info).
	EventCreated EventKind = "created"
	// EventStarted: the worker has the agent running in its PTY (info).
	EventStarted EventKind = "started"
	// EventExited: the agent exited on its own with status 0 (notice).
	EventExited EventKind = "exited"
	// EventKilled: the agent was stopped on request (`agent kill`) (info).
	EventKilled EventKind = "killed"
	// EventFailed: the session failed to start, or the agent exited with a
	// non-zero status (action).
	EventFailed EventKind = "failed"
	// EventWorkerLost: the session's worker died without recording an
	// outcome, so the agent's fate is unknown (action).
	EventWorkerLost EventKind = "worker_lost"
	// EventRecovered: a starting daemon found the session's worker still
	// running and took it over again (info).
	EventRecovered EventKind = "recovered"
	// EventBell: the program rang the terminal bell (BEL), which agents do
	// when they wait for the user (action). Rate-limited.
	EventBell EventKind = "bell"
	// EventNotification: the program asked the terminal for a desktop
	// notification (OSC 9 or OSC 777); the summary is its text (action).
	EventNotification EventKind = "notification"
	// EventIdle: output stopped after a period of activity, typically an
	// agent finishing its turn (notice; info while a client is attached,
	// since someone is watching).
	EventIdle EventKind = "idle"
	// EventWorking: output resumed after an idle event (info).
	EventWorking EventKind = "working"
	// EventStalled: no output for a long time while a command other than
	// the agent holds the terminal's foreground (notice).
	EventStalled EventKind = "stalled"
	// EventAcknowledged: the user looked at the session, clearing its
	// attention (info).
	EventAcknowledged EventKind = "acknowledged"
)

// DefaultAttention is the attention level events of kind k are recorded
// with, unless the producer has a reason to differ (see EventIdle).
func (k EventKind) DefaultAttention() AttentionLevel {
	switch k {
	case EventFailed, EventWorkerLost, EventBell, EventNotification:
		return AttentionAction
	case EventExited, EventIdle, EventStalled:
		return AttentionNotice
	}
	return AttentionInfo
}

// Event is one persisted event.
type Event struct {
	// ID increases with every event recorded under a runtime root and is
	// never reused, so it is a resumable cursor.
	ID        uint64
	SessionID string
	At        time.Time
	Kind      EventKind
	Attention AttentionLevel
	// Summary is a short human-readable line.
	Summary string
}

// Rank orders attention levels: info < notice < action. Unknown levels
// rank lowest.
func (a AttentionLevel) Rank() int {
	switch a {
	case AttentionNotice:
		return 1
	case AttentionAction:
		return 2
	}
	return 0
}

// Activity is what a live session's agent appears to be doing, judged by
// its worker from the PTY stream. The worker records it only when it
// changes.
type Activity string

const (
	// ActivityUnknown: nothing reported (an older worker, or a session
	// that never ran).
	ActivityUnknown Activity = ""
	// ActivityWorking: the program wrote output recently.
	ActivityWorking Activity = "working"
	// ActivityIdle: no output for a while (10 seconds by default).
	ActivityIdle Activity = "idle"
	// ActivityWaiting: the program rang the bell or sent a notification
	// and nobody has typed into the session since.
	ActivityWaiting Activity = "waiting"
	// ActivityExited: the agent is no longer running.
	ActivityExited Activity = "exited"
)

func ParseActivity(v string) (Activity, error) {
	switch Activity(v) {
	case ActivityUnknown, ActivityWorking, ActivityIdle, ActivityWaiting, ActivityExited:
		return Activity(v), nil
	}
	return "", fmt.Errorf("unknown session activity %q", v)
}
