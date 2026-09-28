use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

pub const SESSION_NAME_RULES: &str = "use 1-64 lowercase letters, numbers, and single hyphens";

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionStatus {
    Creating,
    Running,
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
    pub agent: String,
    pub model: Option<String>,
    pub mode: SessionMode,
    /// Directory the agent process runs in. It may or may not be a git
    /// repository; agentd does not manage git state (docs/drop-worktrees.md).
    pub cwd: String,
    pub status: SessionStatus,
    pub worker_pid: Option<u32>,
    pub agent_pid: Option<u32>,
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
    pub cwd: String,
    pub status: SessionStatus,
    pub mode: SessionMode,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AttachmentRecord {
    pub attach_id: String,
    pub session_id: String,
    pub kind: AttachmentKind,
    pub connected_at: DateTime<Utc>,
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

impl AttentionLevel {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Info => "info",
            Self::Notice => "notice",
            Self::Action => "action",
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
    use super::{SESSION_NAME_RULES, validate_session_name};

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
}
