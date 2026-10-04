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
	"time"

	_ "modernc.org/sqlite"

	"github.com/robmorgan/agentd/internal/session"
)

// CurrentSchemaVersion 2 added workspaces (the table and sessions.workspace),
// 3 the git base a session started from (sessions.git_base and
// git_base_branch), 4 session UIDs, create tokens and attach sequence
// numbers. Changes must be additive, applied by migrate: session workers
// open the database once at startup and keep writing to it across daemon
// upgrades.
const CurrentSchemaVersion = 4

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

// migrations[v] upgrades a database from version v+1 to v+2.
var migrations = []string{migrateToV2, migrateToV3, migrateToV4}

type Database struct {
	path string
}

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
	return d, nil
}

// connect opens a connection. _txlock=immediate makes every transaction take
// the write lock when it begins, so concurrent openers (daemon, workers, the
// CLI's local mode) queue on busy_timeout instead of deadlocking on a lock
// upgrade, which SQLite reports as SQLITE_BUSY without waiting.
func (d *Database) connect() (*sql.DB, error) {
	conn, err := sql.Open("sqlite", d.path+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", d.path, err)
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

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
	err := d.exec(`INSERT INTO sessions (
                session_id, agent, model, mode, cwd, status, attention, attention_summary, created_at, updated_at,
                workspace, git_base, git_base_branch, uid, create_token
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9, ?10, ?11, ?12, ?13, ?14)`,
		s.SessionID, s.Agent, model, string(s.Mode), s.Cwd, string(session.StatusCreating),
		string(session.AttentionInfo), s.SessionID, createdAt, workspace, gitBase, gitBranch, s.UID, token)
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

// NextAttachID numbers a new attachment of the incarnation uid of a session.
// The counter lives in the row, so ids are never reused within an
// incarnation, not even by a later worker for the same session. It returns
// ErrNoSuchIncarnation if the session is gone or was recreated.
func (d *Database) NextAttachID(sessionID, uid string) (uint64, error) {
	conn, err := d.connect()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var seq uint64
	err = conn.QueryRow(`UPDATE sessions SET attach_seq = attach_seq + 1
             WHERE session_id = ?1 AND uid = ?2 RETURNING attach_seq`, sessionID, uid).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSuchIncarnation
	}
	return seq, err
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
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	res, err := conn.Exec(`UPDATE sessions
             SET status = ?2, worker_pid = ?3, agent_pid = ?4,
                 exit_code = NULL, error = NULL, attention = ?5, attention_summary = ?6,
                 updated_at = ?7, exited_at = NULL
             WHERE session_id = ?1 AND status = 'creating' AND created_at = ?8`,
		sessionID, string(session.StatusRunning), workerPID, agentPID,
		string(session.AttentionInfo), "running", now(), createdAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotCreating
	}
	return nil
}

func (d *Database) MarkFailed(sessionID, errMsg string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, error = ?3,
                 attention = ?4, attention_summary = ?3, updated_at = ?5
             WHERE session_id = ?1`,
		sessionID, string(session.StatusFailed), errMsg, string(session.AttentionAction), now())
}

func (d *Database) MarkExited(sessionID string, exitCode *int32) error {
	summary := "finished"
	var code any
	if exitCode != nil {
		summary = fmt.Sprintf("finished (exit %d)", *exitCode)
		code = *exitCode
	}
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, exit_code = ?3,
                 attention = ?4, attention_summary = ?5,
                 updated_at = ?6, exited_at = ?6
             WHERE session_id = ?1`,
		sessionID, string(session.StatusExited), code,
		string(session.AttentionNotice), summary, now())
}

// MarkUnknownRecovered records that a running session's worker is gone. It
// only applies to rows still marked running, so it never clobbers the final
// state a worker wrote on its way out.
func (d *Database) MarkUnknownRecovered(sessionID string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, attention = ?3,
                 attention_summary = ?4, updated_at = ?5
             WHERE session_id = ?1 AND status = 'running'`,
		sessionID, string(session.StatusUnknownRecovered), string(session.AttentionAction),
		"daemon lost the live process", now())
}

// MarkFailedIfActive marks a creating or running session failed. The daemon
// uses it when a worker dies without recording its own outcome.
func (d *Database) MarkFailedIfActive(sessionID, errMsg string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, error = ?3,
                 attention = ?4, attention_summary = ?3, updated_at = ?5
             WHERE session_id = ?1 AND status IN ('creating', 'running')`,
		sessionID, string(session.StatusFailed), errMsg, string(session.AttentionAction), now())
}

// MarkWorkerLost marks a running session failed when the given worker died
// without recording an outcome. Keyed on the worker's pid, it never touches a
// newer session that reused the name.
func (d *Database) MarkWorkerLost(sessionID string, workerPID int, errMsg string) error {
	return d.exec(`UPDATE sessions
             SET status = ?3, worker_pid = NULL, agent_pid = NULL, error = ?4,
                 attention = ?5, attention_summary = ?4, updated_at = ?6
             WHERE session_id = ?1 AND worker_pid = ?2 AND status = 'running'`,
		sessionID, workerPID, string(session.StatusFailed), errMsg, string(session.AttentionAction), now())
}

// MarkExitedIfActive is MarkExited restricted to sessions that have not
// already recorded an outcome.
func (d *Database) MarkExitedIfActive(sessionID string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL,
                 attention = ?3, attention_summary = ?4, updated_at = ?5, exited_at = ?5
             WHERE session_id = ?1 AND status IN ('creating', 'running')`,
		sessionID, string(session.StatusExited), string(session.AttentionNotice), "finished", now())
}

func (d *Database) DeleteSession(sessionID string) error {
	return d.exec("DELETE FROM sessions WHERE session_id = ?1", sessionID)
}

const selectSession = `SELECT session_id, agent, model, mode, cwd, status, worker_pid, agent_pid, exit_code, error,
        attention, attention_summary, created_at, updated_at, exited_at, workspace, uid
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
	)
	if err := row.Scan(&rec.SessionID, &rec.Agent, &model, &mode, &rec.Cwd, &status,
		&workerPID, &agentPID, &exitCode, &errText, &attention, &summary, &createdAt, &updatedAt,
		&exitedAt, &workspace, &uid); err != nil {
		return nil, err
	}
	var err error
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
