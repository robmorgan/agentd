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

func TestFreshDatabaseRoundTrips(t *testing.T) {
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

func TestUnknownSchemaVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec("CREATE TABLE sessions (x); PRAGMA user_version = 99;"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported state database schema in "+path+": version 99") || !strings.Contains(err.Error(), "start fresh") {
		t.Fatalf("got %v", err)
	}
	_ = os.Remove(path)
}

// The daemon, its workers and the CLI's local mode can all open a new
// database at once; exactly one creates the schema and the rest must wait,
// not fail.
func TestConcurrentOpenOfNewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
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
