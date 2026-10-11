// Package db is the SQLite state store (state.db) for the daemon and its
// session workers. Only they open it: the agent CLI goes through the daemon
// protocol for all session state.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/robmorgan/agentd/internal/session"
)

// CurrentSchemaVersion 2 added workspaces (the table and sessions.workspace),
// 3 the git base a session started from (sessions.git_base and
// git_base_branch), 4 session UIDs, create tokens and attach sequence
// numbers, 5 the events table and the sessions' live activity columns, 6
// the program status columns (OSC 7501).
// Changes must be additive, applied by migrate: session workers open the
// database once at startup and keep writing to it across daemon upgrades.
const CurrentSchemaVersion = 6

// rfc3339 matches chrono's `to_rfc3339()` for UTC values (a `+00:00` offset,
// fractional seconds only when non-zero), which is what the CLI parses.
const rfc3339 = "2006-01-02T15:04:05.999999999-07:00"

const createSessionsTable = `
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    model TEXT,
    mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
    cwd TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'exited', 'failed', 'unknown_recovered')),
    worker_pid INTEGER,
    agent_pid INTEGER,
    exit_code INTEGER,
    error TEXT,
    attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
    attention_summary TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    exited_at TEXT
);`

// Version 2. A fresh database is created at version 1 and migrated, so new
// and upgraded databases always have the same layout (sessions.workspace is
// the last column either way).
const migrateToV2 = `
ALTER TABLE sessions ADD COLUMN workspace TEXT;
CREATE TABLE workspaces (
    name TEXT PRIMARY KEY,
    path TEXT NOT NULL,
    created_at TEXT NOT NULL
);`

// Version 3: the commit and branch HEAD pointed at when a session started in
// a git repository, so what the agent produced can be measured from there.
// NULL for sessions created elsewhere or before this version.
const migrateToV3 = `
ALTER TABLE sessions ADD COLUMN git_base TEXT;
ALTER TABLE sessions ADD COLUMN git_base_branch TEXT;`

// Version 4. uid identifies a session incarnation (see session.Record.UID);
// rows from before it get a random one. create_token is the request token
// of the CreateSession that made the row, so a retried create finds it even
// across a daemon restart. attach_seq numbers the session's attachments, so
// attach ids are never reused within an incarnation, even by a new worker.
const migrateToV4 = `
ALTER TABLE sessions ADD COLUMN uid TEXT;
ALTER TABLE sessions ADD COLUMN create_token TEXT;
ALTER TABLE sessions ADD COLUMN attach_seq INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET uid = lower(hex(randomblob(16))) WHERE uid IS NULL;
CREATE UNIQUE INDEX sessions_create_token ON sessions (create_token) WHERE create_token IS NOT NULL;`

// Version 5: session events (see events.go) and the activity a session's
// worker reports. Event ids come from AUTOINCREMENT, so an id is never
// reused, even after the newest events are deleted with their session:
// clients use ids as cursors.
const migrateToV5 = `
ALTER TABLE sessions ADD COLUMN activity TEXT;
ALTER TABLE sessions ADD COLUMN foreground TEXT;
ALTER TABLE sessions ADD COLUMN title TEXT;
ALTER TABLE sessions ADD COLUMN last_output_at TEXT;
ALTER TABLE sessions ADD COLUMN attention_at TEXT;
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    at TEXT NOT NULL,
    kind TEXT NOT NULL,
    attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
    summary TEXT NOT NULL
);
CREATE INDEX events_by_session ON events (session_id, id);`

// Version 6: what the program itself last reported through the program
// status protocol (OSC 7501), recorded along with activity changes: its
// name, why it is blocked (permission, question, auth), its one-line
// message, and its progress (0-100). All NULL for a program that does not
// report.
const migrateToV6 = `
ALTER TABLE sessions ADD COLUMN status_app TEXT;
ALTER TABLE sessions ADD COLUMN status_kind TEXT;
ALTER TABLE sessions ADD COLUMN status_msg TEXT;
ALTER TABLE sessions ADD COLUMN status_progress INTEGER;`

// migrations[v] upgrades a database from version v+1 to v+2.
var migrations = []string{migrateToV2, migrateToV3, migrateToV4, migrateToV5, migrateToV6}

type Database struct {
	path string
	// pool holds this process's connections to the database, opened on
	// first use and kept for the life of the process. Opening and closing
	// one per call made every call pay for opening the file, and under
	// write-ahead logging closing a process's last connection checkpoints
	// and truncates the log, which needs the database's locks.
	poolOnce sync.Once
	pool     *sql.DB
	poolErr  error
}

// A process keeps one connection open between calls (enough to keep the
// write-ahead log from being checkpointed away after every call) and opens
// more while it is busy, up to maxConns. A write waiting for the write lock
// holds its connection meanwhile, so with too few a process's reads (an
// attach looking up the newest event, say) would queue behind its writes.
const (
	maxConns     = 32
	maxIdleConns = 1
)

// errUnsupportedSchema marks init failures caused by the database's layout
// rather than by I/O or locking, so only those are reported as a schema
// problem the user has to act on.
var errUnsupportedSchema = errors.New("unsupported state database schema")

// ErrWorkspaceExists is returned by AddWorkspace when the name is taken.
var ErrWorkspaceExists = errors.New("workspace already exists")

// ErrNotCreating is returned by MarkRunning when the session is no longer
// waiting for its worker (it was removed, or the daemon gave up on it).
var ErrNotCreating = errors.New("session is no longer being created")

func Open(path string) (*Database, error) {
	d := &Database{path: path}
	if err := d.init(); err != nil {
		if errors.Is(err, errUnsupportedSchema) {
			detail := strings.TrimPrefix(err.Error(), errUnsupportedSchema.Error()+": ")
			return nil, fmt.Errorf("%w in %s: %s", errUnsupportedSchema, path, detail)
		}
		return nil, fmt.Errorf("failed to open state database %s: %w", path, err)
	}
	// Session metadata is private to the user; SQLite creates files 0644.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("failed to restrict %s: %w", path, err)
	}
	// Write-ahead logging, set once the file is private (SQLite creates the
	// -wal and -shm files with the database's permissions), and kept by the
	// file from then on. The daemon and every session worker share this
	// database. In the default rollback journal a reader waits for a writer
	// and every writer for any other access, and SQLite's busy handler
	// waits by sleeping in steps of up to 100 ms, so with many sessions an
	// attach could stall for hundreds of milliseconds behind unrelated
	// writes. With WAL, readers never wait and writers wait only for each
	// other.
	if err := d.enableWAL(); err != nil {
		return nil, fmt.Errorf("failed to enable write-ahead logging in %s: %w", path, err)
	}
	return d, nil
}

// enableWAL switches the database to write-ahead logging unless it already
// is. Switching needs the database to itself and SQLite does not wait for
// it (busy_timeout does not apply), so processes opening a new database at
// the same moment retry briefly; once one has switched, the others find it
// done.
func (d *Database) enableWAL() error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		if err = conn.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil && mode == "wal" {
			return nil
		}
		if err == nil {
			err = conn.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
			if err == nil && mode == "wal" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("journal mode is %q", mode)
			}
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// connect returns the process's connection pool. Close on the result is a
// no-op, so callers keep the open-use-close shape of a connection. With
// _txlock=immediate every transaction takes the write lock when it begins,
// so concurrent writers (daemon, workers) queue on busy_timeout instead of
// deadlocking on a lock upgrade, which SQLite reports as SQLITE_BUSY
// without waiting.
func (d *Database) connect() (pooled, error) {
	d.poolOnce.Do(func() {
		pool, err := sql.Open("sqlite", d.path+"?_pragma=busy_timeout(5000)&_txlock=immediate")
		if err != nil {
			d.poolErr = fmt.Errorf("failed to open %s: %w", d.path, err)
			return
		}
		pool.SetMaxOpenConns(maxConns)
		pool.SetMaxIdleConns(maxIdleConns)
		d.pool = pool
	})
	return pooled{d.pool}, d.poolErr
}

// pooled is the shared pool, whose Close does nothing.
type pooled struct{ *sql.DB }

func (pooled) Close() error { return nil }

func (d *Database) init() error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var schemaVersion int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return err
	}
	var hasObjects bool
	if err := tx.QueryRow(`SELECT EXISTS(
            SELECT 1 FROM sqlite_master
            WHERE type IN ('table', 'index', 'trigger', 'view')
              AND name NOT LIKE 'sqlite_%')`).Scan(&hasObjects); err != nil {
		return err
	}

	if !hasObjects {
		if _, err := tx.Exec(createSessionsTable); err != nil {
			return err
		}
		schemaVersion = 1
	}
	if schemaVersion < 1 || schemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("%w: version %d; expected %d. Remove the runtime root to start fresh.", errUnsupportedSchema, schemaVersion, CurrentSchemaVersion)
	}
	if schemaVersion == CurrentSchemaVersion {
		return tx.Commit()
	}
	for v := schemaVersion; v < CurrentSchemaVersion; v++ {
		if _, err := tx.Exec(migrations[v-1]); err != nil {
			return fmt.Errorf("migrating state database to version %d: %w", v+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d;", CurrentSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(rfc3339) }

func (d *Database) exec(query string, args ...any) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Exec(query, args...)
	return err
}

// tx runs fn in one write transaction.
func (d *Database) tx(fn func(*sql.Tx) error) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// transition applies one guarded UPDATE of a session row and, if it
// changed the row, records ev in the same transaction, so a lifecycle
// change and its event are never seen apart. It reports whether the row
// changed.
func (d *Database) transition(ev NewEvent, query string, args ...any) (bool, error) {
	changed := false
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		changed = true
		_, err = insertEvent(tx, ev)
		return err
	})
	return changed, err
}

// NewSession holds the columns known when a session row is first created.
type NewSession struct {
	SessionID string
	// UID is the incarnation's id; InsertSession generates one if empty.
	UID       string
	Agent     string
	Model     *string
	Mode      session.Mode
	Cwd       string
	Workspace *string
	// GitBase and GitBaseBranch record the repository's HEAD commit and
	// branch when the session starts in one; empty otherwise.
	GitBase, GitBaseBranch string
	// CreateToken is the request token of the CreateSession, if any.
	CreateToken string
}

// NewUID returns a random session UID: 128 bits, hex-encoded.
func NewUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// ErrCreateTokenUsed is returned by InsertSession when another session was
// already created with the same token.
var ErrCreateTokenUsed = errors.New("create token already used")

// InsertSession creates a session row in `creating`. The returned creation
// timestamp identifies this incarnation of the session: a worker passes it
// back to MarkRunning, so a stale worker can never claim a newer session
// that reused the name.
// InsertSession creates a session row in `creating` and records its created
// event. The returned creation timestamp identifies this incarnation of the
// session: a worker passes it back to MarkRunning, so a stale worker can
// never claim a newer session that reused the name.
func (d *Database) InsertSession(s NewSession) (string, error) {
	var model, workspace, gitBase, gitBranch, token any
	if s.Model != nil {
		model = *s.Model
	}
	if s.Workspace != nil {
		workspace = *s.Workspace
	}
	if s.GitBase != "" {
		gitBase, gitBranch = s.GitBase, s.GitBaseBranch
	}
	if s.CreateToken != "" {
		token = s.CreateToken
	}
	if s.UID == "" {
		s.UID = NewUID()
	}
	createdAt := now()
	_, err := d.transition(NewEvent{SessionID: s.SessionID, Kind: session.EventCreated,
		Summary: fmt.Sprintf("created: %s in %s", s.Agent, s.Cwd)},
		`INSERT INTO sessions (
                session_id, agent, model, mode, cwd, status, attention, attention_summary, created_at, updated_at,
                workspace, git_base, git_base_branch, uid, create_token
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, NULL, ?8, ?8, ?9, ?10, ?11, ?12, ?13)`,
		s.SessionID, s.Agent, model, string(s.Mode), s.Cwd, string(session.StatusCreating),
		string(session.AttentionInfo), createdAt, workspace, gitBase, gitBranch, s.UID, token)
	if err != nil && token != nil && strings.Contains(err.Error(), "sessions.create_token") {
		return "", ErrCreateTokenUsed
	}
	return createdAt, err
}

// GitBase returns the commit and branch a session recorded when it started
// in a git repository: empty strings if it recorded none, and ok false if
// the session does not exist.
func (d *Database) GitBase(sessionID string) (commit, branch string, ok bool, err error) {
	conn, err := d.connect()
	if err != nil {
		return "", "", false, err
	}
	defer conn.Close()
	var c, b sql.NullString
	err = conn.QueryRow("SELECT git_base, git_base_branch FROM sessions WHERE session_id = ?1", sessionID).Scan(&c, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return c.String, b.String, true, nil
}

// SessionByCreateToken returns the session created by the CreateSession
// with this request token, or nil, nil.
func (d *Database) SessionByCreateToken(token string) (*session.Record, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rec, err := scanSession(conn.QueryRow(selectSession+" WHERE create_token = ?1", token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// ReserveAttachIDs reserves n attachment numbers for the incarnation uid of
// a session and returns the first; the worker hands them out without
// touching the database again until they run out. Numbers are never reused
// within an incarnation, even by a later worker (after a handoff), though
// one that stops before using its block leaves a gap. It returns
// ErrNoSuchIncarnation once that incarnation's row is gone.
func (d *Database) ReserveAttachIDs(sessionID, uid string, n uint64) (uint64, error) {
	conn, err := d.connect()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var last uint64
	err = conn.QueryRow(`UPDATE sessions SET attach_seq = attach_seq + ?3
             WHERE session_id = ?1 AND uid = ?2 RETURNING attach_seq`, sessionID, uid, n).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSuchIncarnation
	}
	return last - n + 1, err
}

// ErrNoSuchIncarnation means a session row with the given UID no longer
// exists.
var ErrNoSuchIncarnation = errors.New("session incarnation no longer exists")

// MarkRunning records that a worker has its agent running. It only applies
// to the incarnation of the session created at createdAt, and only while it
// is still in `creating`; otherwise it returns ErrNotCreating, so a worker the
// daemon gave up on, or whose session was removed (and perhaps recreated
// under the same name) meanwhile, cannot claim it.
func (d *Database) MarkRunning(sessionID, createdAt string, workerPID, agentPID int) error {
	changed, err := d.transition(NewEvent{SessionID: sessionID, Kind: session.EventStarted,
		Summary: fmt.Sprintf("agent running (pid %d)", agentPID)},
		`UPDATE sessions
             SET status = ?2, worker_pid = ?3, agent_pid = ?4,
                 exit_code = NULL, error = NULL, attention = ?5, attention_summary = NULL, attention_at = NULL,
                 updated_at = ?6, exited_at = NULL, activity = ?7, last_output_at = NULL
             WHERE session_id = ?1 AND status = 'creating' AND created_at = ?8`,
		sessionID, string(session.StatusRunning), workerPID, agentPID,
		string(session.AttentionInfo), now(), string(session.ActivityWorking), createdAt)
	if err == nil && !changed {
		return ErrNotCreating
	}
	return err
}

// The transitions below end a session (or record that its fate is
// unknown). Each replaces the session's attention with its own event's,
// since the end supersedes whatever the agent asked for while it ran.

// ending is a transition that ends a session. Its query sets the status
// (?2), the attention (?3) and its summary (?4) and time (?5); set adds
// columns and where adds conditions, with args numbered from ?6.
type ending struct {
	kind       session.EventKind
	status     session.Status
	summary    string
	set, where string
	args       []any
}

func (d *Database) end(sessionID string, e ending) error {
	at := now()
	level := e.kind.DefaultAttention()
	// The program status columns go with the agent: an ended session
	// carries no live program status, however it ended.
	query := `UPDATE sessions SET status = ?2, worker_pid = NULL, agent_pid = NULL,
                 attention = ?3, attention_summary = ?4, attention_at = ?5, updated_at = ?5,
                 activity = 'exited',
                 status_app = NULL, status_kind = NULL, status_msg = NULL, status_progress = NULL` + e.set + " WHERE session_id = ?1" + e.where
	_, err := d.transition(NewEvent{SessionID: sessionID, Kind: e.kind, Attention: level, Summary: e.summary},
		query, append([]any{sessionID, string(e.status), string(level), e.summary, at}, e.args...)...)
	return err
}

// ErrNotResumable is returned by MarkResumed when the session is no longer
// running under the worker that handed off.
var ErrNotResumable = errors.New("session is no longer running under this worker")

// MarkResumed records that a running session's worker came back from a
// live handoff to a new executable. The worker and agent pids are unchanged
// (the worker re-executed in place), so only the update time changes. It
// applies only to the incarnation created at createdAt, still running under
// workerPID; otherwise it returns ErrNotResumable and changes nothing.
func (d *Database) MarkResumed(sessionID, createdAt string, workerPID int) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	res, err := conn.Exec(`UPDATE sessions SET updated_at = ?4
             WHERE session_id = ?1 AND created_at = ?2 AND status = 'running' AND worker_pid = ?3`,
		sessionID, createdAt, workerPID, now())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("session %s: %w", sessionID, ErrNotResumable)
	}
	return nil
}

func (d *Database) MarkFailed(sessionID, errMsg string) error {
	return d.end(sessionID, ending{kind: session.EventFailed, status: session.StatusFailed, summary: errMsg, set: ", error = ?4"})
}

// MarkExited records that the agent exited on its own.
func (d *Database) MarkExited(sessionID string, exitCode *int32) error {
	return d.markExited(session.EventExited, sessionID, exitCode)
}

// MarkKilled records that the agent exited after a stop request.
func (d *Database) MarkKilled(sessionID string, exitCode *int32) error {
	return d.markExited(session.EventKilled, sessionID, exitCode)
}

func (d *Database) markExited(kind session.EventKind, sessionID string, exitCode *int32) error {
	summary := "finished"
	if kind == session.EventKilled {
		summary = "stopped"
	}
	var code any
	if exitCode != nil {
		summary = fmt.Sprintf("%s (exit %d)", summary, *exitCode)
		code = *exitCode
	}
	return d.end(sessionID, ending{kind: kind, status: session.StatusExited, summary: summary,
		set: ", exit_code = ?6, exited_at = ?5", args: []any{code}})
}

// MarkUnknownRecovered records that a running session's worker is gone. It
// only applies to rows still marked running, so it never clobbers the final
// state a worker wrote on its way out.
func (d *Database) MarkUnknownRecovered(sessionID string) error {
	return d.end(sessionID, ending{kind: session.EventWorkerLost, status: session.StatusUnknownRecovered,
		summary: "daemon lost the live process", where: " AND status = 'running'"})
}

// MarkFailedIfActive marks a creating or running session failed. The daemon
// uses it when a worker dies without recording its own outcome.
func (d *Database) MarkFailedIfActive(sessionID, errMsg string) error {
	return d.end(sessionID, ending{kind: session.EventFailed, status: session.StatusFailed, summary: errMsg,
		set: ", error = ?4", where: " AND status IN ('creating', 'running')"})
}

// MarkFailedIfCreating marks a session failed only while it is still being
// created. A starting daemon uses it for sessions a previous daemon was
// creating: their worker may claim the session (MarkRunning) at any moment,
// and one that got there first keeps it.
func (d *Database) MarkFailedIfCreating(sessionID, errMsg string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, error = ?3,
                 attention = ?4, attention_summary = ?3, updated_at = ?5
             WHERE session_id = ?1 AND status = 'creating'`,
		sessionID, string(session.StatusFailed), errMsg, string(session.AttentionAction), now())
}

// MarkWorkerLost marks a running session failed when the given worker died
// without recording an outcome. Keyed on the worker's pid, it never touches a
// newer session that reused the name.
func (d *Database) MarkWorkerLost(sessionID string, workerPID int, errMsg string) error {
	return d.end(sessionID, ending{kind: session.EventWorkerLost, status: session.StatusFailed, summary: errMsg,
		set: ", error = ?4", where: " AND worker_pid = ?6 AND status = 'running'", args: []any{workerPID}})
}

// MarkExitedIfActive records the end of a session the daemon stopped
// itself (its worker did not record the outcome), unless one is recorded.
func (d *Database) MarkExitedIfActive(sessionID string) error {
	return d.end(sessionID, ending{kind: session.EventKilled, status: session.StatusExited, summary: "stopped",
		set: ", exited_at = ?5", where: " AND status IN ('creating', 'running')"})
}

// MarkRecovered records that a starting daemon found the session's worker
// still running.
func (d *Database) MarkRecovered(sessionID string) error {
	_, err := d.RecordEvent(NewEvent{SessionID: sessionID, Kind: session.EventRecovered,
		Summary: "agentd restarted; the session kept running"})
	return err
}

// DeleteSession removes a session and its events.
func (d *Database) DeleteSession(sessionID string) error {
	return d.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM events WHERE session_id = ?1", sessionID); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM sessions WHERE session_id = ?1", sessionID)
		return err
	})
}

const selectSession = `SELECT session_id, agent, model, mode, cwd, status, worker_pid, agent_pid, exit_code, error,
        attention, attention_summary, created_at, updated_at, exited_at, workspace, uid,
        activity, foreground, title, last_output_at, attention_at,
        status_app, status_kind, status_msg, status_progress
 FROM sessions`

// GetSession returns nil, nil when the session does not exist.
func (d *Database) GetSession(sessionID string) (*session.Record, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rec, err := scanSession(conn.QueryRow(selectSession+" WHERE session_id = ?1", sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (d *Database) ListSessions() ([]session.Record, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(selectSession + " ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Record
	for rows.Next() {
		rec, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanSession(row scanner) (*session.Record, error) {
	var (
		rec                               session.Record
		model, errText, summary, exitedAt sql.NullString
		workspace, uid                    sql.NullString
		mode, status, attention           string
		workerPID, agentPID, exitCode     sql.NullInt64
		createdAt, updatedAt              string
		activity, foreground, title       sql.NullString
		lastOutputAt, attentionAt         sql.NullString
		statusApp, statusKind, statusMsg  sql.NullString
		statusProgress                    sql.NullInt64
	)
	if err := row.Scan(&rec.SessionID, &rec.Agent, &model, &mode, &rec.Cwd, &status,
		&workerPID, &agentPID, &exitCode, &errText, &attention, &summary, &createdAt, &updatedAt,
		&exitedAt, &workspace, &uid, &activity, &foreground, &title, &lastOutputAt, &attentionAt,
		&statusApp, &statusKind, &statusMsg, &statusProgress); err != nil {
		return nil, err
	}
	var err error
	// Any activity string is accepted: a newer worker may record values
	// this build does not know.
	rec.Activity = session.Activity(activity.String)
	rec.StatusApp = nullStr(statusApp)
	rec.StatusKind = nullStr(statusKind)
	rec.StatusMsg = nullStr(statusMsg)
	if statusProgress.Valid {
		v := int(statusProgress.Int64)
		rec.StatusProgress = &v
	}
	rec.Foreground = nullStr(foreground)
	rec.Title = nullStr(title)
	if rec.LastOutputAt, err = nullTime(lastOutputAt); err != nil {
		return nil, err
	}
	if rec.AttentionAt, err = nullTime(attentionAt); err != nil {
		return nil, err
	}
	if rec.Mode, err = session.ParseMode(mode); err != nil {
		return nil, err
	}
	if rec.Status, err = session.ParseStatus(status); err != nil {
		return nil, err
	}
	if rec.Attention, err = session.ParseAttention(attention); err != nil {
		return nil, err
	}
	rec.Model = nullStr(model)
	rec.Error = nullStr(errText)
	rec.AttentionSummary = nullStr(summary)
	rec.Workspace = nullStr(workspace)
	rec.UID = uid.String
	if workerPID.Valid {
		v := uint32(workerPID.Int64)
		rec.WorkerPID = &v
	}
	if agentPID.Valid {
		v := uint32(agentPID.Int64)
		rec.AgentPID = &v
	}
	if exitCode.Valid {
		v := int32(exitCode.Int64)
		rec.ExitCode = &v
	}
	if rec.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if rec.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if exitedAt.Valid {
		t, err := parseTime(exitedAt.String)
		if err != nil {
			return nil, err
		}
		rec.ExitedAt = &t
	}
	return &rec, nil
}

func nullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	return &t, err
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: %w", v, err)
	}
	return t.UTC(), nil
}

// AddWorkspace registers a workspace. It returns ErrWorkspaceExists if the
// name is taken; re-pointing a workspace means removing and re-adding it.
func (d *Database) AddWorkspace(name, path string) (session.Workspace, error) {
	conn, err := d.connect()
	if err != nil {
		return session.Workspace{}, err
	}
	defer conn.Close()
	createdAt := now()
	res, err := conn.Exec(`INSERT INTO workspaces (name, path, created_at) VALUES (?1, ?2, ?3)
             ON CONFLICT (name) DO NOTHING`, name, path, createdAt)
	if err != nil {
		return session.Workspace{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return session.Workspace{}, err
	} else if n == 0 {
		return session.Workspace{}, ErrWorkspaceExists
	}
	created, err := parseTime(createdAt)
	return session.Workspace{Name: name, Path: path, CreatedAt: created}, err
}

// RemoveWorkspace deletes a workspace and reports whether it existed.
// Sessions started in it keep their resolved cwd and workspace name.
func (d *Database) RemoveWorkspace(name string) (bool, error) {
	conn, err := d.connect()
	if err != nil {
		return false, err
	}
	defer conn.Close()
	res, err := conn.Exec("DELETE FROM workspaces WHERE name = ?1", name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetWorkspace returns nil, nil when the workspace does not exist.
func (d *Database) GetWorkspace(name string) (*session.Workspace, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	w, err := scanWorkspace(conn.QueryRow("SELECT name, path, created_at FROM workspaces WHERE name = ?1", name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return w, err
}

func (d *Database) ListWorkspaces() ([]session.Workspace, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query("SELECT name, path, created_at FROM workspaces ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []session.Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func scanWorkspace(row scanner) (*session.Workspace, error) {
	var (
		w         session.Workspace
		createdAt string
	)
	if err := row.Scan(&w.Name, &w.Path, &createdAt); err != nil {
		return nil, err
	}
	var err error
	w.CreatedAt, err = parseTime(createdAt)
	return &w, err
}
