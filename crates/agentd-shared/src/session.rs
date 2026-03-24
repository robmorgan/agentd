use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::path::Path;

pub const SESSION_NAME_RULES: &str = "use 1-64 lowercase letters, numbers, and single hyphens";

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionStatus {
    Creating,
    Running,
    NeedsInput,
    Exited,
    Failed,
    UnknownRecovered,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum AttentionLevel {
    Info,
    Notice,
    Action,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ApplyState {
    Idle,
    AutoApplying,
    Applied,
    Discarded,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum IntegrationPolicy {
    ManualReview,
    AutoApplySafe,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionMode {
    Execute,
    Plan,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum AttachmentKind {
    Attach,
    Tui,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionRecord {
    pub session_id: String,
    pub thread_id: Option<String>,
    pub agent: String,
    pub model: Option<String>,
    pub mode: SessionMode,
    pub workspace: String,
    pub repo_path: String,
    pub repo_name: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
    pub status: SessionStatus,
    pub integration_policy: IntegrationPolicy,
    pub apply_state: ApplyState,
    pub has_commits: bool,
    pub pid: Option<u32>,
    pub exit_code: Option<i32>,
    pub error: Option<String>,
    pub attention: AttentionLevel,
    pub attention_summary: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub exited_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CreateSessionResult {
    pub session_id: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
    pub status: SessionStatus,
    pub mode: SessionMode,
    pub integration_policy: IntegrationPolicy,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct WorktreeRecord {
    pub session_id: String,
    pub repo_path: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionDiff {
    pub session_id: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
    pub diff: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AttachmentRecord {
    pub attach_id: String,
    pub session_id: String,
    pub kind: AttachmentKind,
    pub connected_at: DateTime<Utc>,
}

pub fn branch_name_from_session_id(session_id: &str) -> String {
    format!("agent/{session_id}")
}

pub fn validate_session_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err(SESSION_NAME_RULES.to_string());
    }
    if name.len() > 64 {
        return Err(SESSION_NAME_RULES.to_string());
    }

    let bytes = name.as_bytes();
    if bytes[0] == b'-' || bytes[bytes.len() - 1] == b'-' {
        return Err(SESSION_NAME_RULES.to_string());
    }

    let mut last_was_hyphen = false;
    for byte in bytes {
        match byte {
            b'a'..=b'z' | b'0'..=b'9' => last_was_hyphen = false,
            b'-' if !last_was_hyphen => last_was_hyphen = true,
            _ => return Err(SESSION_NAME_RULES.to_string()),
        }
    }

    Ok(())
}

pub fn repo_name_from_path(path: &str) -> String {
    Path::new(path)
        .file_name()
        .and_then(|value| value.to_str())
        .filter(|value| !value.is_empty())
        .unwrap_or(path)
        .to_string()
}

impl AttentionLevel {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Info => "info",
            Self::Notice => "notice",
            Self::Action => "action",
        }
    }
}

impl ApplyState {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::AutoApplying => "auto_applying",
            Self::Applied => "applied",
            Self::Discarded => "discarded",
        }
    }
}

impl IntegrationPolicy {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::ManualReview => "manual_review",
            Self::AutoApplySafe => "auto_apply_safe",
        }
    }
}

impl SessionMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Execute => "execute",
            Self::Plan => "plan",
        }
    }
}

impl AttachmentKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Attach => "attach",
            Self::Tui => "tui",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{
        SESSION_NAME_RULES, branch_name_from_session_id, repo_name_from_path, validate_session_name,
    };

    #[test]
    fn branch_names_follow_session_id() {
        assert_eq!(branch_name_from_session_id("fix-failing-tests"), "agent/fix-failing-tests");
    }

    #[test]
    fn valid_session_names_pass_validation() {
        assert!(validate_session_name("fix-failing-tests").is_ok());
        assert!(validate_session_name("demo2").is_ok());
    }

    #[test]
    fn invalid_session_names_fail_validation() {
        for invalid in ["", "Fix", "fix tests", "fix_tests", "-fix", "fix-", "fix--tests"] {
            assert_eq!(validate_session_name(invalid), Err(SESSION_NAME_RULES.to_string()));
        }
    }

    #[test]
    fn repo_names_are_derived_from_path() {
        assert_eq!(repo_name_from_path("/tmp/demo"), "demo");
    }
}
