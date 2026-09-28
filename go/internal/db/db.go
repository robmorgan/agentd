// Package db is the SQLite state store for the Go daemon and worker.
//
// Schema v8 replaces the git columns of the Rust daemon's v7 layout with a
// single cwd column (see docs/drop-worktrees.md). Opening a v6 or v7 database
// migrates it in place, so a runtime root created by the Rust daemon can be
// reused; the Rust daemon cannot open it afterwards.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/robmorgan/agentd/go/internal/session"
)

const CurrentSchemaVersion = 8

// rfc3339 mirrors chrono's `to_rfc3339()` for UTC values: a `+00:00` offset
// and fractional seconds only when non-zero.
const rfc3339 = "2006-01-02T15:04:05.999999999-07:00"

var expectedSessionsColumns = []string{
	"session_id", "agent", "model", "mode", "cwd", "status", "worker_pid",
	"agent_pid", "exit_code", "error", "attention", "attention_summary",
	"created_at", "updated_at", "exited_at",
}

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

// createSessionsTableV7 is the Rust daemon's layout, kept only so a v6
// database can be stepped through v7 on its way to v8.
const createSessionsTableV7 = `
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    model TEXT,
    mode TEXT NOT NULL CHECK (mode IN ('execute', 'plan')),
    workspace TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('creating', 'running', 'exited', 'failed', 'unknown_recovered')),
    integration_policy TEXT NOT NULL CHECK (integration_policy IN ('manual_review', 'auto_apply_safe')),
    worker_pid INTEGER,
    agent_pid INTEGER,
    exit_code INTEGER,
    error TEXT,
    integration_state TEXT NOT NULL CHECK (integration_state IN ('idle', 'auto_applying', 'applied', 'discarded')),
    attention TEXT NOT NULL CHECK (attention IN ('info', 'notice', 'action')),
    attention_summary TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    exited_at TEXT
);`

type Database struct {
	path string
}

func Open(path string) (*Database, error) {
	d := &Database{path: path}
	if err := d.init(); err != nil {
		return nil, fmt.Errorf("unsupported state database schema in %s: %w", path, err)
	}
	return d, nil
}

func (d *Database) connect() (*sql.DB, error) {
	conn, err := sql.Open("sqlite", d.path+"?_pragma=busy_timeout(5000)")
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
		if _, err := tx.Exec(createSessionsTable + "\nPRAGMA user_version = 8;"); err != nil {
			return err
		}
	} else {
		switch schemaVersion {
		case CurrentSchemaVersion:
		case 6:
			if err := migrateV6ToV7(tx); err != nil {
				return err
			}
			if err := migrateV7ToV8(tx); err != nil {
				return err
			}
		case 7:
			if err := migrateV7ToV8(tx); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported state database schema version %d; expected %d. Remove or migrate the runtime root.", schemaVersion, CurrentSchemaVersion)
		}
		if err := ensureSupportedSchema(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureSupportedSchema(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns = append(columns, name)
	}
	if len(columns) == 0 {
		return errors.New("unsupported state database schema: missing `sessions` table. Remove or migrate the runtime root.")
	}
	if strings.Join(columns, ",") != strings.Join(expectedSessionsColumns, ",") {
		return errors.New("unsupported state database schema: `sessions` does not match the current layout. Remove or migrate the runtime root.")
	}
	return nil
}

func migrateV6ToV7(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE sessions RENAME TO sessions_v6;` + createSessionsTableV7 + `
        INSERT INTO sessions (
            session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
            status, integration_policy, worker_pid, agent_pid, exit_code, error, integration_state,
            attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
            status, integration_policy, NULL, pid, exit_code, error, integration_state,
            attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v6;
        DROP TABLE sessions_v6;
        PRAGMA user_version = 7;`)
	return err
}

// migrateV7ToV8 drops the git columns. A v7 session always ran inside its
// worktree, so that path becomes its cwd; workspace is only a fallback for
// rows the Rust daemon left half-created.
func migrateV7ToV8(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE sessions RENAME TO sessions_v7;` + createSessionsTable + `
        INSERT INTO sessions (
            session_id, agent, model, mode, cwd, status, worker_pid, agent_pid, exit_code, error,
            attention, attention_summary, created_at, updated_at, exited_at
        )
        SELECT
            session_id, agent, model, mode,
            CASE WHEN worktree <> '' THEN worktree ELSE workspace END,
            status, worker_pid, agent_pid, exit_code, error,
            attention, attention_summary, created_at, updated_at, exited_at
        FROM sessions_v7;
        DROP TABLE sessions_v7;
        PRAGMA user_version = 8;`)
	return err
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
	Agent     string
	Model     *string
	Mode      session.Mode
	Cwd       string
}

func (d *Database) InsertSession(s NewSession) error {
	var model any
	if s.Model != nil {
		model = *s.Model
	}
	return d.exec(`INSERT INTO sessions (
                session_id, agent, model, mode, cwd, status, attention, attention_summary, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9)`,
		s.SessionID, s.Agent, model, string(s.Mode), s.Cwd, string(session.StatusCreating),
		string(session.AttentionInfo), s.SessionID, now())
}

func (d *Database) MarkRunning(sessionID string, workerPID, agentPID int) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = ?3, agent_pid = ?4,
                 exit_code = NULL, error = NULL, attention = ?5, attention_summary = ?6,
                 updated_at = ?7, exited_at = NULL
             WHERE session_id = ?1`,
		sessionID, string(session.StatusRunning), workerPID, agentPID,
		string(session.AttentionInfo), "running", now())
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
        attention, attention_summary, created_at, updated_at, exited_at
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
		mode, status, attention           string
		workerPID, agentPID, exitCode     sql.NullInt64
		createdAt, updatedAt              string
	)
	if err := row.Scan(&rec.SessionID, &rec.Agent, &model, &mode, &rec.Cwd, &status,
		&workerPID, &agentPID, &exitCode, &errText, &attention, &summary, &createdAt, &updatedAt,
		&exitedAt); err != nil {
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
