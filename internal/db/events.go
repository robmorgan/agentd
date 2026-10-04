package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

// Events (see session.Event) are rows of the events table, written by the
// daemon (lifecycle changes it causes or observes) and by session workers
// (lifecycle and the signals they read from the PTY). Workers write only on
// transitions and rate-limit noisy sources, never per output chunk.
//
// Retention: each session keeps its newest MaxEventsPerSession events;
// older ones are pruned in the transaction that inserts a new one, and a
// removed session's events go with it. The table is therefore bounded by
// the number of retained sessions. A client following events from an id
// whose events have since been pruned simply resumes at the oldest
// retained one; ids are not dense in any case, since they are shared by
// all sessions.
//
// Ordering: every write is an immediate transaction (connect's
// _txlock=immediate), so writers are serialised and ids are committed in
// increasing order. A reader that has seen id N will never later find a
// new event with an id below N, which is what makes an id a cursor.

// MaxEventsPerSession bounds the events kept per session.
const MaxEventsPerSession = 500

// NewEvent is an event to record. A zero At means now; an empty Attention
// means the kind's default.
type NewEvent struct {
	SessionID string
	Kind      session.EventKind
	Attention session.AttentionLevel
	Summary   string
	At        time.Time
}

func (e NewEvent) attention() session.AttentionLevel {
	if e.Attention == "" {
		return e.Kind.DefaultAttention()
	}
	return e.Attention
}

func (e NewEvent) at() string {
	if e.At.IsZero() {
		return now()
	}
	return e.At.UTC().Format(rfc3339)
}

// insertEvent inserts e and prunes its session's oldest events.
func insertEvent(tx *sql.Tx, e NewEvent) (uint64, error) {
	res, err := tx.Exec(`INSERT INTO events (session_id, at, kind, attention, summary) VALUES (?1, ?2, ?3, ?4, ?5)`,
		e.SessionID, e.at(), string(e.Kind), string(e.attention()), e.Summary)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`DELETE FROM events WHERE session_id = ?1 AND id <= (
            SELECT id FROM events WHERE session_id = ?1 ORDER BY id DESC LIMIT 1 OFFSET ?2)`,
		e.SessionID, MaxEventsPerSession)
	return uint64(id), err
}

// rankSQL ranks the attention column like session.AttentionLevel.Rank.
const rankSQL = `(CASE attention WHEN 'action' THEN 2 WHEN 'notice' THEN 1 ELSE 0 END)`

// RecordEvent records an event for an existing session and raises the
// session's attention to the event's level, with its summary, unless
// something at least as urgent is already pending; an event at the same
// level replaces the summary, since the newer one is the more useful.
// Info events leave attention alone. It returns the event's id, or 0 when
// the session does not exist (it was removed meanwhile).
func (d *Database) RecordEvent(e NewEvent) (uint64, error) {
	var id uint64
	err := d.tx(func(tx *sql.Tx) error {
		level := e.attention()
		if level.Rank() > 0 {
			if _, err := tx.Exec(`UPDATE sessions SET attention = ?2, attention_summary = ?3, attention_at = ?4
                 WHERE session_id = ?1 AND `+rankSQL+` <= ?5`,
				e.SessionID, string(level), e.Summary, e.at(), level.Rank()); err != nil {
				return err
			}
		}
		var exists int
		err := tx.QueryRow("SELECT 1 FROM sessions WHERE session_id = ?1", e.SessionID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		id, err = insertEvent(tx, e)
		return err
	})
	return id, err
}

// Acknowledge clears a session's attention: the user has looked at it. It
// records an acknowledged event with summary, and reports true, only when
// there was something to clear.
func (d *Database) Acknowledge(sessionID, summary string) (bool, error) {
	return d.AcknowledgeBefore(sessionID, summary, time.Time{})
}

// AcknowledgeBefore is Acknowledge for what the user could have seen by
// time before: if an event recorded after it raised the session's attention
// again, that attention is newer than what they saw and stays. A zero
// before acknowledges whatever is pending.
//
// Event times are UTC RFC 3339 with trailing zeros trimmed from the
// fraction, which order correctly as text, so the comparison is done in
// SQL on the stored strings.
func (d *Database) AcknowledgeBefore(sessionID, summary string, before time.Time) (bool, error) {
	// Most attaches find nothing to acknowledge; a read answers that without
	// taking the write lock a transaction begins with.
	conn, err := d.connect()
	if err != nil {
		return false, err
	}
	var attention string
	err = conn.QueryRow(`SELECT attention FROM sessions WHERE session_id = ?1`, sessionID).Scan(&attention)
	conn.Close()
	if errors.Is(err, sql.ErrNoRows) || (err == nil && attention == string(session.AttentionInfo)) {
		return false, nil
	}
	ev := NewEvent{SessionID: sessionID, Kind: session.EventAcknowledged, Summary: summary}
	if before.IsZero() {
		return d.transition(ev, `UPDATE sessions SET attention = 'info', attention_summary = NULL, attention_at = NULL
             WHERE session_id = ?1 AND attention != 'info'`, sessionID)
	}
	return d.transition(ev, `UPDATE sessions SET attention = 'info', attention_summary = NULL, attention_at = NULL
         WHERE session_id = ?1 AND attention != 'info'
           AND NOT EXISTS (SELECT 1 FROM events WHERE session_id = ?1 AND at > ?2 AND attention != 'info')`,
		sessionID, before.UTC().Format(rfc3339))
}

// Activity is what a worker reports about its running session.
type Activity struct {
	Activity     session.Activity
	Foreground   string
	Title        string
	LastOutputAt time.Time
}

// SetActivity records a running session's activity. Keyed on the worker's
// pid, it never touches a newer session that reused the name, nor a session
// that has already ended. It does not change updated_at, so lists ordered
// by it do not reshuffle as agents go idle and back.
func (d *Database) SetActivity(sessionID string, workerPID int, a Activity) error {
	var lastOutput, fg, title any
	if !a.LastOutputAt.IsZero() {
		lastOutput = a.LastOutputAt.UTC().Format(rfc3339)
	}
	if a.Foreground != "" {
		fg = a.Foreground
	}
	if a.Title != "" {
		title = a.Title
	}
	return d.exec(`UPDATE sessions SET activity = ?3, foreground = ?4, title = ?5, last_output_at = ?6
             WHERE session_id = ?1 AND worker_pid = ?2 AND status = 'running'`,
		sessionID, workerPID, string(a.Activity), fg, title, lastOutput)
}

// LastEventID is the id of the newest event ever recorded (0 if none). It
// never decreases, even when that event has been deleted, so it is a cheap
// way to notice that something new was written.
func (d *Database) LastEventID() (uint64, error) {
	conn, err := d.connect()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var id uint64
	err = conn.QueryRow("SELECT seq FROM sqlite_sequence WHERE name = 'events'").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// EventsAfter returns up to limit events with ids above afterID, oldest
// first, of one session or (sessionID "") of all.
func (d *Database) EventsAfter(afterID uint64, sessionID string, limit int) ([]session.Event, error) {
	return d.queryEvents(`SELECT id, session_id, at, kind, attention, summary FROM events
             WHERE id > ?1 AND (?2 = '' OR session_id = ?2) ORDER BY id LIMIT ?3`, false,
		int64(afterID), sessionID, limit)
}

// LatestEvents returns the newest limit events with ids up to upToID (0:
// no bound), oldest first, of one session or (sessionID "") of all.
func (d *Database) LatestEvents(upToID uint64, sessionID string, limit int) ([]session.Event, error) {
	if upToID == 0 {
		upToID = 1<<63 - 1
	}
	return d.queryEvents(`SELECT id, session_id, at, kind, attention, summary FROM events
             WHERE id <= ?1 AND (?2 = '' OR session_id = ?2) ORDER BY id DESC LIMIT ?3`, true,
		int64(upToID), sessionID, limit)
}

func (d *Database) queryEvents(query string, reverse bool, args ...any) ([]session.Event, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []session.Event{}
	for rows.Next() {
		var (
			e                   session.Event
			at, kind, attention string
			id                  int64
		)
		if err := rows.Scan(&id, &e.SessionID, &at, &kind, &attention, &e.Summary); err != nil {
			return nil, err
		}
		e.ID = uint64(id)
		e.Kind = session.EventKind(kind)
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if e.Attention, err = session.ParseAttention(attention); err != nil {
			return nil, fmt.Errorf("event %d: %w", id, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}
