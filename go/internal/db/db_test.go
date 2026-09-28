package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/robmorgan/agentd/go/internal/session"
)

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	var v int
	if err := openRaw(t, path).QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFreshDatabaseIsV8AndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, path); got != CurrentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, CurrentSchemaVersion)
	}
	createdAt, err := store.InsertSession(NewSession{SessionID: "demo", Agent: "sh", Mode: session.ModeExecute, Cwd: "/work/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning("demo", createdAt, 10, 11); err != nil {
		t.Fatal(err)
	}
	code := int32(0)
	if err := store.MarkExited("demo", &code); err != nil {
		t.Fatal(err)
	}
	rec, err := store.GetSession("demo")
	if err != nil || rec == nil {
		t.Fatalf("get: %v %v", rec, err)
	}
	if rec.Cwd != "/work/demo" || rec.Status != session.StatusExited || rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Fatalf("unexpected record %+v", rec)
	}
	// Reopening an already-current database must not touch it.
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateV7ToV8 opens a database laid out the way the Rust daemon writes
// it and checks the worktree path becomes the session cwd.
// writeV7Fixture creates a database in the Rust daemon's v7 layout.
func writeV7Fixture(t *testing.T, path string) {
	t.Helper()
	raw := openRaw(t, path)
	if _, err := raw.Exec(createSessionsTableV7 + `
        INSERT INTO sessions (
            session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
            status, integration_policy, worker_pid, agent_pid, exit_code, error, integration_state,
            attention, attention_summary, created_at, updated_at, exited_at
        ) VALUES
        ('wt', 'codex', NULL, 'execute', '/repo', '/repo', 'repo', 'main', 'agent/wt', '/root/worktrees/wt',
         'exited', 'manual_review', NULL, NULL, 0, NULL, 'applied',
         'notice', 'finished', '2026-09-01T10:00:00+00:00', '2026-09-01T11:00:00+00:00', '2026-09-01T11:00:00+00:00'),
        ('half', 'codex', 'm', 'execute', '/repo', '/repo', 'repo', 'main', 'agent/half', '',
         'failed', 'manual_review', NULL, NULL, NULL, 'worktree failed', 'idle',
         'action', 'worktree failed', '2026-09-02T10:00:00+00:00', '2026-09-02T10:00:00+00:00', NULL);
        PRAGMA user_version = 7;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

}

func TestMigrateV7ToV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writeV7Fixture(t, path)

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, path); got != 8 {
		t.Fatalf("user_version = %d, want 8", got)
	}
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	byID := map[string]session.Record{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	if got := byID["wt"].Cwd; got != "/root/worktrees/wt" {
		t.Fatalf("wt cwd = %q", got)
	}
	if byID["wt"].Status != session.StatusExited || byID["wt"].ExitedAt == nil {
		t.Fatalf("wt record not preserved: %+v", byID["wt"])
	}
	if got := byID["half"].Cwd; got != "/repo" {
		t.Fatalf("half cwd = %q (workspace fallback)", got)
	}
	if byID["half"].Model == nil || *byID["half"].Model != "m" {
		t.Fatalf("half model lost: %+v", byID["half"])
	}
}

func TestUnknownSchemaVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec("CREATE TABLE sessions (x); PRAGMA user_version = 99;"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported state database schema in "+path+": version 99") {
		t.Fatalf("got %v", err)
	}
	_ = os.Remove(path)
}

// The daemon, its workers and the CLI's local mode can all open an old
// database at once; exactly one migrates and the rest must wait, not fail.
func TestConcurrentOpenOfV7Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writeV7Fixture(t, path)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Open(path)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := userVersion(t, path); got != CurrentSchemaVersion {
		t.Fatalf("user_version = %d", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state.db mode = %v, %v", info.Mode(), err)
	}
}

// The conditional transitions exist so that the daemon's supervisor never
// overwrites an outcome the worker already recorded.
func TestStateTransitionGuards(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) *session.Record {
		t.Helper()
		rec, err := store.GetSession(id)
		if err != nil || rec == nil {
			t.Fatalf("get %s: %v %v", id, rec, err)
		}
		return rec
	}
	created := map[string]string{}
	for _, id := range []string{"a", "b"} {
		at, err := store.InsertSession(NewSession{SessionID: id, Agent: "sh", Mode: session.ModeExecute, Cwd: "/"})
		if err != nil {
			t.Fatal(err)
		}
		created[id] = at
	}

	if err := store.MarkRunning("a", created["a"], 10, 11); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning("a", created["a"], 12, 13); !errors.Is(err, ErrNotCreating) {
		t.Fatalf("second MarkRunning = %v", err)
	}
	if err := store.MarkRunning("missing", created["a"], 1, 2); !errors.Is(err, ErrNotCreating) {
		t.Fatalf("MarkRunning on missing row = %v", err)
	}
	zero := int32(0)
	if err := store.MarkExited("a", &zero); err != nil {
		t.Fatal(err)
	}
	for _, mark := range []func() error{
		func() error { return store.MarkFailedIfActive("a", "boom") },
		func() error { return store.MarkExitedIfActive("a") },
		func() error { return store.MarkUnknownRecovered("a") },
	} {
		if err := mark(); err != nil {
			t.Fatal(err)
		}
	}
	if rec := get("a"); rec.Status != session.StatusExited || rec.Error != nil || rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Fatalf("final state clobbered: %+v", rec)
	}

	// A session still being created can be failed, and then cannot be claimed.
	if err := store.MarkFailedIfActive("b", "timed out"); err != nil {
		t.Fatal(err)
	}
	if rec := get("b"); rec.Status != session.StatusFailed {
		t.Fatalf("b = %+v", rec)
	}
	if err := store.MarkRunning("b", created["b"], 10, 11); !errors.Is(err, ErrNotCreating) {
		t.Fatalf("MarkRunning after failure = %v", err)
	}

	// A worker for a removed session cannot claim a new session that
	// reused its name.
	if err := store.DeleteSession("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertSession(NewSession{SessionID: "b", Agent: "sh", Mode: session.ModeExecute, Cwd: "/other"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning("b", created["b"], 10, 11); !errors.Is(err, ErrNotCreating) {
		t.Fatalf("stale worker claimed a recreated session: %v", err)
	}
}

func TestSchemaVersionAndLegacySessionsDoNotMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if v, err := SchemaVersion(path); err != nil || v != 0 {
		t.Fatalf("missing db: %d, %v", v, err)
	}
	writeV7Fixture(t, path)
	raw := openRaw(t, path)
	if _, err := raw.Exec(`UPDATE sessions SET status = 'running', worker_pid = 42 WHERE session_id = 'wt'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if v, err := SchemaVersion(path); err != nil || v != 7 {
		t.Fatalf("v7 db: %d, %v", v, err)
	}
	legacy, err := LegacyRunningSessions(path)
	if err != nil || len(legacy) != 1 || legacy[0].SessionID != "wt" || legacy[0].WorkerPID == nil || *legacy[0].WorkerPID != 42 {
		t.Fatalf("legacy = %+v, %v", legacy, err)
	}
	if v := userVersion(t, path); v != 7 {
		t.Fatalf("peeking migrated the database to %d", v)
	}
}
