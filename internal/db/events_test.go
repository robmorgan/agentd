package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/robmorgan/agentd/internal/session"
)

func newStore(t *testing.T) *Database {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (d *Database) mustGet(t *testing.T, id string) *session.Record {
	t.Helper()
	rec, err := d.GetSession(id)
	if err != nil || rec == nil {
		t.Fatalf("get %s: %v %v", id, rec, err)
	}
	return rec
}

func kindsOf(events []session.Event) string {
	s := ""
	for i, ev := range events {
		if i > 0 {
			s += " "
		}
		s += string(ev.Kind)
	}
	return s
}

func TestLifecycleTransitionsRecordEvents(t *testing.T) {
	store := newStore(t)
	createdAt, err := store.InsertSession(NewSession{SessionID: "s", Agent: "claude", Mode: session.ModeExecute, Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning("s", createdAt, 10, 11); err != nil {
		t.Fatal(err)
	}
	// A refused transition records nothing.
	if err := store.MarkRunning("s", createdAt, 10, 11); err == nil {
		t.Fatal("second MarkRunning accepted")
	}
	if err := store.MarkUnknownRecovered("missing"); err != nil {
		t.Fatal(err)
	}
	code := int32(0)
	if err := store.MarkExited("s", &code); err != nil {
		t.Fatal(err)
	}
	events, err := store.EventsAfter(0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(events); got != "created started exited" {
		t.Fatalf("events = %s", got)
	}
	if ev := events[2]; ev.Attention != session.AttentionNotice || ev.Summary != "finished (exit 0)" || ev.SessionID != "s" {
		t.Fatalf("exited = %#v", ev)
	}
	rec := store.mustGet(t, "s")
	if rec.Attention != session.AttentionNotice || *rec.AttentionSummary != "finished (exit 0)" || rec.AttentionAt == nil ||
		rec.Activity != session.ActivityExited {
		t.Fatalf("record = %#v", rec)
	}
}

// Activity signals raise attention to their level and never lower it;
// acknowledging clears it.
func TestAttentionRaisesAndAcknowledges(t *testing.T) {
	store := newStore(t)
	createdAt, _ := store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
	store.MarkRunning("s", createdAt, 10, 11)
	record := func(kind session.EventKind, level session.AttentionLevel, summary string) {
		t.Helper()
		if id, err := store.RecordEvent(NewEvent{SessionID: "s", Kind: kind, Attention: level, Summary: summary}); err != nil || id == 0 {
			t.Fatalf("record: %d %v", id, err)
		}
	}
	attention := func() (session.AttentionLevel, string) {
		rec := store.mustGet(t, "s")
		if rec.AttentionSummary == nil {
			return rec.Attention, ""
		}
		return rec.Attention, *rec.AttentionSummary
	}

	record(session.EventWorking, "", "output resumed")
	if a, s := attention(); a != session.AttentionInfo || s != "" {
		t.Fatalf("info event changed attention: %s %q", a, s)
	}
	record(session.EventIdle, session.AttentionNotice, "idle")
	record(session.EventBell, "", "bell")
	if a, s := attention(); a != session.AttentionAction || s != "bell" {
		t.Fatalf("after bell: %s %q", a, s)
	}
	record(session.EventIdle, session.AttentionNotice, "idle again")
	if a, s := attention(); a != session.AttentionAction || s != "bell" {
		t.Fatalf("notice lowered action: %s %q", a, s)
	}
	record(session.EventNotification, "", "Approve?")
	if a, s := attention(); a != session.AttentionAction || s != "Approve?" {
		t.Fatalf("same level did not replace: %s %q", a, s)
	}

	if changed, err := store.Acknowledge("s", "seen: attached"); err != nil || !changed {
		t.Fatalf("ack = %v %v", changed, err)
	}
	if a, s := attention(); a != session.AttentionInfo || s != "" || store.mustGet(t, "s").AttentionAt != nil {
		t.Fatalf("after ack: %s %q", a, s)
	}
	if changed, _ := store.Acknowledge("s", "again"); changed {
		t.Fatal("acknowledging nothing recorded an event")
	}
	events, _ := store.LatestEvents(0, "s", 2)
	if got := kindsOf(events); got != "notification acknowledged" {
		t.Fatalf("latest = %s", got)
	}

	// Events of sessions that no longer exist are not recorded.
	if id, err := store.RecordEvent(NewEvent{SessionID: "gone", Kind: session.EventBell}); err != nil || id != 0 {
		t.Fatalf("event for a missing session: %d %v", id, err)
	}
}

func TestEventsArePrunedPerSessionAndRemovedWithIt(t *testing.T) {
	store := newStore(t)
	for _, id := range []string{"a", "b"} {
		if _, err := store.InsertSession(NewSession{SessionID: id, Agent: "sh", Mode: session.ModeExecute, Cwd: "/"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range MaxEventsPerSession + 50 {
		if _, err := store.RecordEvent(NewEvent{SessionID: "a", Kind: session.EventWorking, Summary: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := store.EventsAfter(0, "a", 10_000)
	b, _ := store.EventsAfter(0, "b", 10_000)
	if len(a) != MaxEventsPerSession || a[len(a)-1].Summary != fmt.Sprint(MaxEventsPerSession+49) || len(b) != 1 {
		t.Fatalf("kept %d of a (last %q) and %d of b", len(a), a[len(a)-1].Summary, len(b))
	}
	last, err := store.LastEventID()
	if err != nil || last != a[len(a)-1].ID {
		t.Fatalf("last id = %d, %v", last, err)
	}

	if err := store.DeleteSession("a"); err != nil {
		t.Fatal(err)
	}
	if rest, _ := store.EventsAfter(0, "", 10_000); len(rest) != 1 || rest[0].SessionID != "b" {
		t.Fatalf("after delete: %d events", len(rest))
	}
	// Ids are never reused, even once the newest are gone: they are cursors.
	if id, _ := store.RecordEvent(NewEvent{SessionID: "b", Kind: session.EventBell}); id <= last {
		t.Fatalf("id %d reused (last was %d)", id, last)
	}
	if again, _ := store.LastEventID(); again <= last {
		t.Fatalf("last id went back to %d", again)
	}
}

func TestEventQueries(t *testing.T) {
	store := newStore(t)
	for _, id := range []string{"a", "b"} {
		store.InsertSession(NewSession{SessionID: id, Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
	}
	store.RecordEvent(NewEvent{SessionID: "a", Kind: session.EventBell})
	store.RecordEvent(NewEvent{SessionID: "b", Kind: session.EventBell})
	all, _ := store.EventsAfter(0, "", 100) // created a, created b, bell a, bell b
	if len(all) != 4 {
		t.Fatalf("%d events", len(all))
	}
	if got, _ := store.EventsAfter(all[1].ID, "a", 100); len(got) != 1 || got[0].ID != all[2].ID {
		t.Fatalf("after, filtered: %v", got)
	}
	if got, _ := store.EventsAfter(0, "", 2); len(got) != 2 || got[1].ID != all[1].ID {
		t.Fatalf("limit: %v", got)
	}
	if got, _ := store.LatestEvents(0, "", 2); len(got) != 2 || got[0].ID != all[2].ID || got[1].ID != all[3].ID {
		t.Fatalf("latest: %v", got)
	}
	if got, _ := store.LatestEvents(all[2].ID, "b", 5); len(got) != 1 || got[0].ID != all[1].ID {
		t.Fatalf("latest up to, filtered: %v", got)
	}
}

// SetActivity is keyed on the worker, so a stale worker cannot touch a
// newer session or one that has ended.
func TestSetActivityGuards(t *testing.T) {
	store := newStore(t)
	createdAt, _ := store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
	if rec := store.mustGet(t, "s"); rec.Activity != session.ActivityUnknown {
		t.Fatalf("activity before running = %q", rec.Activity)
	}
	store.MarkRunning("s", createdAt, 10, 11)
	if rec := store.mustGet(t, "s"); rec.Activity != session.ActivityWorking {
		t.Fatalf("activity when running = %q", rec.Activity)
	}
	if err := store.SetActivity("s", 10, Activity{Activity: session.ActivityIdle, Foreground: "claude", Title: "Fix"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActivity("s", 99, Activity{Activity: session.ActivityWaiting}); err != nil {
		t.Fatal(err)
	}
	rec := store.mustGet(t, "s")
	if rec.Activity != session.ActivityIdle || *rec.Foreground != "claude" || *rec.Title != "Fix" || rec.LastOutputAt != nil {
		t.Fatalf("record = %#v", rec)
	}
	store.MarkKilled("s", nil)
	store.SetActivity("s", 10, Activity{Activity: session.ActivityWorking})
	if rec := store.mustGet(t, "s"); rec.Activity != session.ActivityExited || rec.Attention != session.AttentionInfo {
		t.Fatalf("after kill: %s %s", rec.Activity, rec.Attention)
	}
}

// A version 2 database gains the events table and activity columns, and
// statements a version 2 worker still runs keep working against it.
func TestMigrateFromVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec(createSessionsTable + migrateToV2 + `
INSERT INTO sessions (session_id, agent, mode, cwd, status, attention, created_at, updated_at)
VALUES ('old', 'sh', 'execute', '/w', 'running', 'info', '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00');
PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, path); got != CurrentSchemaVersion {
		t.Fatalf("user_version = %d", got)
	}
	if rec := store.mustGet(t, "old"); rec.Activity != session.ActivityUnknown || rec.Foreground != nil {
		t.Fatalf("migrated = %#v", rec)
	}
	// Version 2's MarkExited.
	if _, err := raw.Exec(`UPDATE sessions SET status = 'exited', worker_pid = NULL, agent_pid = NULL, exit_code = 0,
        attention = 'notice', attention_summary = 'finished', updated_at = '2026-01-01T00:00:01+00:00',
        exited_at = '2026-01-01T00:00:01+00:00' WHERE session_id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordEvent(NewEvent{SessionID: "old", Kind: session.EventExited, Summary: "finished"}); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkLastEventID is the daemon's poll for events written by workers,
// made every 250ms while anyone follows events.
func BenchmarkLastEventID(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
	for range 1000 {
		store.RecordEvent(NewEvent{SessionID: "s", Kind: session.EventWorking})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.LastEventID(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecordEvent is one event with its attention update and pruning,
// in a session at its retention limit.
func BenchmarkRecordEvent(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
	for range MaxEventsPerSession {
		store.RecordEvent(NewEvent{SessionID: "s", Kind: session.EventWorking})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.RecordEvent(NewEvent{SessionID: "s", Kind: session.EventBell, Summary: "bell"}); err != nil {
			b.Fatal(err)
		}
	}
}
