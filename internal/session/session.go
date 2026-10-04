// Package session holds the session model shared by the daemon, its workers
// and (through the wire protocol) the agent CLI. A session runs in a working
// directory (Cwd); agentd does not manage git state there.
package session

import (
	"fmt"
	"time"
)

type Status string

const (
	StatusCreating         Status = "creating"
	StatusRunning          Status = "running"
	StatusExited           Status = "exited"
	StatusFailed           Status = "failed"
	StatusUnknownRecovered Status = "unknown_recovered"
)

type AttentionLevel string

const (
	AttentionInfo   AttentionLevel = "info"
	AttentionNotice AttentionLevel = "notice"
	AttentionAction AttentionLevel = "action"
)

type Mode string

const (
	ModeExecute Mode = "execute"
	ModePlan    Mode = "plan"
)

type AttachmentKind string

const (
	AttachmentAttach AttachmentKind = "attach"
	AttachmentTui    AttachmentKind = "tui"
)

// Record is the durable description of a session. A session belongs to the
// daemon, not to any client connection: the record outlives every attach.
type Record struct {
	// SessionID is the session's name. It is unique among the sessions
	// that exist, but may be reused once a session is removed.
	SessionID string
	// UID identifies this incarnation of the session: random, immutable,
	// and never reused, unlike the name. A future multi-host id is the
	// host's identity plus the UID.
	UID   string
	Agent string
	Model *string
	Mode  Mode
	// Cwd is the directory the agent process runs in. It may or may not be a
	// git repository; the daemon does not care.
	Cwd              string
	Status           Status
	WorkerPID        *uint32
	AgentPID         *uint32
	ExitCode         *int32
	Error            *string
	Attention        AttentionLevel
	AttentionSummary *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ExitedAt         *time.Time
	// Workspace is the workspace the session was started in, if any. Cwd was
	// resolved from it at creation; later changes to the workspace do not
	// move the session.
	Workspace *string
}

// Workspace names a directory on the daemon's machine, so clients can start
// sessions there without knowing its paths. Workspaces live in state.db and
// are managed through the protocol (`agent workspace add|ls|rm`). agentd does
// not create, clone or sync the directory.
type Workspace struct {
	Name      string
	Path      string
	CreatedAt time.Time
}

type CreateResult struct {
	SessionID string
	// UID is the new session's incarnation id (see Record.UID).
	UID    string
	Cwd    string
	Status Status
	Mode   Mode
}

type AttachmentRecord struct {
	AttachID    string
	SessionID   string
	Kind        AttachmentKind
	ConnectedAt time.Time
}

func ParseStatus(v string) (Status, error) {
	switch Status(v) {
	case StatusCreating, StatusRunning, StatusExited, StatusFailed, StatusUnknownRecovered:
		return Status(v), nil
	}
	return "", fmt.Errorf("unknown session status %q", v)
}

func ParseAttention(v string) (AttentionLevel, error) {
	switch AttentionLevel(v) {
	case AttentionInfo, AttentionNotice, AttentionAction:
		return AttentionLevel(v), nil
	}
	return "", fmt.Errorf("unknown attention level %q", v)
}

func ParseMode(v string) (Mode, error) {
	switch Mode(v) {
	case ModeExecute, ModePlan:
		return Mode(v), nil
	}
	return "", fmt.Errorf("unknown session mode %q", v)
}
