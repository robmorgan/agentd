package db

import (
	"database/sql"
	"os"
	"path/filepath"
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
	if err := store.InsertSession(NewSession{SessionID: "demo", Agent: "sh", Mode: session.ModeExecute, Cwd: "/work/demo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning("demo", 10, 11); err != nil {
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
func TestMigrateV7ToV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
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
	if _, err := Open(path); err == nil {
		t.Fatal("expected unsupported schema error")
	}
	_ = os.Remove(path)
}
