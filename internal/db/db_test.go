package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/robmorgan/agentd/internal/session"
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

// A version 1 database (from before workspaces) is migrated in place and
// keeps its sessions; a worker that opened it at version 1 can keep writing.
func TestMigrateFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec(createSessionsTable + `
INSERT INTO sessions (session_id, agent, mode, cwd, status, attention, created_at, updated_at)
VALUES ('old', 'sh', 'execute', '/w', 'running', 'info', '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00');
PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, path); got != CurrentSchemaVersion {
		t.Fatalf("user_version = %d", got)
	}
	rec, err := store.GetSession("old")
	if err != nil || rec == nil || rec.Cwd != "/w" || rec.Workspace != nil || len(rec.UID) != 32 {
		t.Fatalf("migrated record = %+v, %v", rec, err)
	}
	if err := store.MarkExited("old", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWorkspace("mono", "/src/mono"); err != nil {
		t.Fatal(err)
	}
	// Sessions from before version 3 recorded no git base.
	if commit, branch, ok, err := store.GitBase("old"); err != nil || !ok || commit != "" || branch != "" {
		t.Fatalf("GitBase of a migrated session = %q %q %v %v", commit, branch, ok, err)
	}
}

func TestGitBase(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("a", 40)
	if _, err := store.InsertSession(NewSession{SessionID: "repo", Agent: "sh", Mode: session.ModeExecute, Cwd: "/w", GitBase: base, GitBaseBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertSession(NewSession{SessionID: "plain", Agent: "sh", Mode: session.ModeExecute, Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if commit, branch, ok, err := store.GitBase("repo"); err != nil || !ok || commit != base || branch != "main" {
		t.Fatalf("GitBase(repo) = %q %q %v %v", commit, branch, ok, err)
	}
	if commit, _, ok, err := store.GitBase("plain"); err != nil || !ok || commit != "" {
		t.Fatalf("GitBase(plain) = %q %v %v", commit, ok, err)
	}
	if _, _, ok, err := store.GitBase("missing"); err != nil || ok {
		t.Fatalf("GitBase(missing) = %v %v", ok, err)
	}
}

func TestWorkspaces(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if ws, err := store.ListWorkspaces(); err != nil || len(ws) != 0 {
		t.Fatalf("empty list = %v, %v", ws, err)
	}
	w, err := store.AddWorkspace("mono", "/src/mono")
	if err != nil || w.Name != "mono" || w.Path != "/src/mono" || w.CreatedAt.IsZero() {
		t.Fatalf("add = %+v, %v", w, err)
	}
	if _, err := store.AddWorkspace("mono", "/elsewhere"); !errors.Is(err, ErrWorkspaceExists) {
		t.Fatalf("duplicate add: %v", err)
	}
	store.AddWorkspace("api", "/src/api")
	ws, err := store.ListWorkspaces()
	if err != nil || len(ws) != 2 || ws[0].Name != "api" || ws[1].Name != "mono" || ws[1].Path != "/src/mono" {
		t.Fatalf("list = %+v, %v", ws, err)
	}

	name := "mono"
	if _, err := store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/src/mono", Workspace: &name}); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.RemoveWorkspace("mono"); err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if removed, _ := store.RemoveWorkspace("mono"); removed {
		t.Fatal("second remove reported a removal")
	}
	if w, err := store.GetWorkspace("mono"); err != nil || w != nil {
		t.Fatalf("get removed = %+v, %v", w, err)
	}
	// The session keeps the workspace it was started in.
	if rec, _ := store.GetSession("s"); rec == nil || rec.Workspace == nil || *rec.Workspace != "mono" {
		t.Fatalf("session after remove = %+v", rec)
	}
}

// The daemon and its workers can all open a new database at once; exactly one creates the schema and the rest must wait,
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
	// A session its worker claimed is not failed by a daemon that read it
	// as still being created.
	if err := store.MarkFailedIfCreating("a", "agentd stopped"); err != nil {
		t.Fatal(err)
	}
	if rec := get("a"); rec.Status != session.StatusRunning {
		t.Fatalf("MarkFailedIfCreating failed a running session: %+v", rec)
	}
	// A worker back from a handoff resumes only the session it ran, under
	// the same pid; the pids stay as they were.
	if err := store.MarkResumed("a", created["a"], 10); err != nil {
		t.Fatal(err)
	}
	if rec := get("a"); rec.Status != session.StatusRunning || *rec.WorkerPID != 10 || *rec.AgentPID != 11 {
		t.Fatalf("after MarkResumed: %+v", rec)
	}
	for _, bad := range []struct {
		id, at string
		pid    int
	}{{"a", created["a"], 99}, {"a", "another incarnation", 10}, {"b", created["b"], 10}} {
		if err := store.MarkResumed(bad.id, bad.at, bad.pid); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("MarkResumed(%v) = %v", bad, err)
		}
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

// Every incarnation of a session gets its own UID, a create token finds
// the session it created, and attach ids keep counting per incarnation.
func TestSessionIncarnations(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	insert := func(token string) *session.Record {
		t.Helper()
		if _, err := store.InsertSession(NewSession{SessionID: "s", Agent: "sh", Mode: session.ModeExecute, Cwd: "/w", CreateToken: token}); err != nil {
			t.Fatal(err)
		}
		rec, err := store.GetSession("s")
		if err != nil || rec == nil {
			t.Fatalf("get: %v, %v", rec, err)
		}
		return rec
	}
	first := insert("tok-1")
	if len(first.UID) != 32 {
		t.Fatalf("uid = %q", first.UID)
	}
	if rec, err := store.SessionByCreateToken("tok-1"); err != nil || rec == nil || rec.UID != first.UID {
		t.Fatalf("by token = %+v, %v", rec, err)
	}
	if rec, err := store.SessionByCreateToken("other"); err != nil || rec != nil {
		t.Fatalf("unknown token = %+v, %v", rec, err)
	}
	for want := uint64(1); want <= 3; want++ {
		if got, err := store.NextAttachID("s", first.UID); err != nil || got != want {
			t.Fatalf("attach id = %d, %v; want %d", got, err, want)
		}
	}

	if err := store.DeleteSession("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NextAttachID("s", first.UID); !errors.Is(err, ErrNoSuchIncarnation) {
		t.Fatalf("attach id of a removed incarnation: %v", err)
	}
	second := insert("")
	if second.UID == first.UID {
		t.Fatal("a recreated session reused the UID")
	}
	if _, err := store.NextAttachID("s", first.UID); !errors.Is(err, ErrNoSuchIncarnation) {
		t.Fatalf("attach id of a replaced incarnation: %v", err)
	}
	if got, err := store.NextAttachID("s", second.UID); err != nil || got != 1 {
		t.Fatalf("new incarnation attach id = %d, %v", got, err)
	}
	if _, err := store.InsertSession(NewSession{SessionID: "t", Agent: "sh", Mode: session.ModeExecute, Cwd: "/w", CreateToken: "tok-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertSession(NewSession{SessionID: "u", Agent: "sh", Mode: session.ModeExecute, Cwd: "/w", CreateToken: "tok-2"}); !errors.Is(err, ErrCreateTokenUsed) {
		t.Fatalf("reused token: %v", err)
	}
}
