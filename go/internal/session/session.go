// Package session holds the shared session model mirrored from
// crates/agentd-shared/src/session.rs. Wire and database encodings of these
// types must stay byte-compatible with the Rust implementation.
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

type ApplyState string

const (
	ApplyIdle         ApplyState = "idle"
	ApplyAutoApplying ApplyState = "auto_applying"
	ApplyApplied      ApplyState = "applied"
	ApplyDiscarded    ApplyState = "discarded"
)

type IntegrationPolicy string

const (
	PolicyManualReview  IntegrationPolicy = "manual_review"
	PolicyAutoApplySafe IntegrationPolicy = "auto_apply_safe"
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

type Record struct {
	SessionID         string
	Agent             string
	Model             *string
	Mode              Mode
	Workspace         string
	RepoPath          string
	RepoName          string
	BaseBranch        string
	Branch            string
	Worktree          string
	Status            Status
	IntegrationPolicy IntegrationPolicy
	ApplyState        ApplyState
	DirtyCount        uint32
	AheadCount        uint32
	HasCommits        bool
	HasPendingChanges bool
	WorkerPID         *uint32
	AgentPID          *uint32
	ExitCode          *int32
	Error             *string
	Attention         AttentionLevel
	AttentionSummary  *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ExitedAt          *time.Time
}

type CreateResult struct {
	SessionID         string
	BaseBranch        string
	Branch            string
	Worktree          string
	Status            Status
	Mode              Mode
	IntegrationPolicy IntegrationPolicy
}

type WorktreeRecord struct {
	SessionID  string
	RepoPath   string
	BaseBranch string
	Branch     string
	Worktree   string
}

type Diff struct {
	SessionID  string
	BaseBranch string
	Branch     string
	Worktree   string
	Diff       string
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

func ParseApplyState(v string) (ApplyState, error) {
	switch ApplyState(v) {
	case ApplyIdle, ApplyAutoApplying, ApplyApplied, ApplyDiscarded:
		return ApplyState(v), nil
	}
	return "", fmt.Errorf("unknown apply state %q", v)
}

func ParseIntegrationPolicy(v string) (IntegrationPolicy, error) {
	switch IntegrationPolicy(v) {
	case PolicyManualReview, PolicyAutoApplySafe:
		return IntegrationPolicy(v), nil
	}
	return "", fmt.Errorf("unknown integration policy %q", v)
}

func ParseMode(v string) (Mode, error) {
	switch Mode(v) {
	case ModeExecute, ModePlan:
		return Mode(v), nil
	}
	return "", fmt.Errorf("unknown session mode %q", v)
}

func BranchNameFromSessionID(id string) string { return "agent/" + id }
