// Package session holds the session model used by the Go daemon and worker.
//
// It started as a mirror of crates/agentd-shared/src/session.rs. As of
// protocol v33 it diverges: sessions carry a working directory (Cwd) and no
// git worktree, branch, or integration state. See docs/drop-worktrees.md.
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
	SessionID string
	Agent     string
	Model     *string
	Mode      Mode
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
}

type CreateResult struct {
	SessionID string
	Cwd       string
	Status    Status
	Mode      Mode
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
