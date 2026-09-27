// Package db is the SQLite state store shared with the Rust daemon. It must
// read and write exactly the schema in crates/agentd-shared/src/sqlite_schema.rs
// and the row formats in crates/agentd/src/db.rs.
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

const CurrentSchemaVersion = 7

// rfc3339 mirrors chrono's `to_rfc3339()` for UTC values: a `+00:00` offset
// and fractional seconds only when non-zero.
const rfc3339 = "2006-01-02T15:04:05.999999999-07:00"

var expectedSessionsColumns = []string{
	"session_id", "agent", "model", "mode", "workspace", "repo_path", "repo_name",
	"base_branch", "branch", "worktree", "status", "integration_policy", "worker_pid",
	"agent_pid", "exit_code", "error", "integration_state", "attention",
	"attention_summary", "created_at", "updated_at", "exited_at",
}

const createSessionsTable = `
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
		if _, err := tx.Exec(createSessionsTable + "\nPRAGMA user_version = 7;"); err != nil {
			return err
		}
	} else {
		switch schemaVersion {
		case CurrentSchemaVersion:
		case 6:
			if err := migrateV6ToV7(tx); err != nil {
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
	_, err := tx.Exec(`ALTER TABLE sessions RENAME TO sessions_v6;` + createSessionsTable + `
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

// NewSession mirrors the Rust daemon's NewSession: the columns known when a
// session row is first created.
type NewSession struct {
	SessionID         string
	Agent             string
	Model             *string
	Mode              session.Mode
	Workspace         string
	RepoPath          string
	RepoName          string
	BaseBranch        string
	Branch            string
	Worktree          string
	IntegrationPolicy session.IntegrationPolicy
}

func (d *Database) InsertSession(s NewSession) error {
	var model any
	if s.Model != nil {
		model = *s.Model
	}
	return d.exec(`INSERT INTO sessions (
                session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch, worktree,
                status, integration_policy, attention, attention_summary, integration_state, created_at, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?16)`,
		s.SessionID, s.Agent, model, string(s.Mode), s.Workspace, s.RepoPath, s.RepoName,
		s.BaseBranch, s.Branch, s.Worktree, string(session.StatusCreating),
		string(s.IntegrationPolicy), string(session.AttentionInfo), s.SessionID,
		string(session.ApplyIdle), now())
}

func (d *Database) MarkRunning(sessionID string, workerPID, agentPID int) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, integration_state = ?3, worker_pid = ?4, agent_pid = ?5,
                 exit_code = NULL, error = NULL, attention = ?6, attention_summary = ?7,
                 updated_at = ?8, exited_at = NULL
             WHERE session_id = ?1`,
		sessionID, string(session.StatusRunning), string(session.ApplyIdle), workerPID, agentPID,
		string(session.AttentionInfo), "running", now())
}

func (d *Database) MarkFailed(sessionID, errMsg string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, error = ?3,
                 attention = ?4, attention_summary = ?3, updated_at = ?5
             WHERE session_id = ?1`,
		sessionID, string(session.StatusFailed), errMsg, string(session.AttentionAction), now())
}

func (d *Database) MarkExited(sessionID string, exitCode *int32, applyState session.ApplyState) error {
	summary := "finished"
	var code any
	if exitCode != nil {
		summary = fmt.Sprintf("finished (exit %d)", *exitCode)
		code = *exitCode
	}
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, exit_code = ?3,
                 integration_state = ?4, attention = ?5, attention_summary = ?6,
                 updated_at = ?7, exited_at = ?7
             WHERE session_id = ?1`,
		sessionID, string(session.StatusExited), code, string(applyState),
		string(session.AttentionNotice), summary, now())
}

func (d *Database) MarkUnknownRecovered(sessionID string) error {
	return d.exec(`UPDATE sessions
             SET status = ?2, worker_pid = NULL, agent_pid = NULL, attention = ?3,
                 attention_summary = ?4, updated_at = ?5
             WHERE session_id = ?1`,
		sessionID, string(session.StatusUnknownRecovered), string(session.AttentionAction),
		"daemon lost the live process", now())
}

func (d *Database) SetApplyState(sessionID string, state session.ApplyState, attention session.AttentionLevel, summary string) error {
	return d.exec(`UPDATE sessions
             SET integration_state = ?2, attention = ?3, attention_summary = ?4, updated_at = ?5
             WHERE session_id = ?1`,
		sessionID, string(state), string(attention), summary, now())
}

func (d *Database) DeleteSession(sessionID string) error {
	return d.exec("DELETE FROM sessions WHERE session_id = ?1", sessionID)
}

const selectSession = `SELECT session_id, agent, model, mode, workspace, repo_path, repo_name, base_branch, branch,
        worktree, status, integration_policy, integration_state, worker_pid, agent_pid, exit_code, error, attention, attention_summary,
        created_at, updated_at, exited_at
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
		rec                                         session.Record
		model, errText, summary, exitedAt           sql.NullString
		mode, status, policy, applyState, attention string
		workerPID, agentPID, exitCode               sql.NullInt64
		createdAt, updatedAt                        string
	)
	if err := row.Scan(&rec.SessionID, &rec.Agent, &model, &mode, &rec.Workspace, &rec.RepoPath,
		&rec.RepoName, &rec.BaseBranch, &rec.Branch, &rec.Worktree, &status, &policy, &applyState,
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
	if rec.IntegrationPolicy, err = session.ParseIntegrationPolicy(policy); err != nil {
		return nil, err
	}
	if rec.ApplyState, err = session.ParseApplyState(applyState); err != nil {
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
