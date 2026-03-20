use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use slug::slugify;
use std::path::Path;

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionStatus {
    Creating,
    Running,
    Paused,
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
pub enum IntegrationState {
    Clean,
    PendingReview,
    Applied,
    Discarded,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum GitSyncStatus {
    Unknown,
    InSync,
    NeedsSync,
    Conflicted,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionMode {
    Execute,
    Plan,
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
    pub task: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
    pub status: SessionStatus,
    pub integration_state: IntegrationState,
    pub git_sync: GitSyncStatus,
    pub git_status_summary: Option<String>,
    pub has_conflicts: bool,
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
pub struct PlanRecord {
    pub session_id: String,
    pub version: u32,
    pub summary: String,
    pub body_markdown: String,
    pub source_event_id: i64,
    pub created_at: DateTime<Utc>,
}

pub fn branch_name_from_task(task: &str) -> String {
    let slug = slugify(task);
    let trimmed = slug.trim_matches('-');
    let branch = if trimmed.is_empty() {
        "task".to_string()
    } else {
        trimmed.to_string()
    };
    format!("agent/{branch}")
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

impl IntegrationState {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Clean => "clean",
            Self::PendingReview => "pending_review",
            Self::Applied => "applied",
            Self::Discarded => "discarded",
        }
    }
}

impl GitSyncStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::InSync => "in_sync",
            Self::NeedsSync => "needs_sync",
            Self::Conflicted => "conflicted",
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

#[cfg(test)]
mod tests {
    use super::{branch_name_from_task, repo_name_from_path};

    #[test]
    fn branch_names_are_slugified() {
        assert_eq!(
            branch_name_from_task("fix failing tests"),
            "agent/fix-failing-tests"
        );
    }

    #[test]
    fn empty_tasks_fall_back_to_task() {
        assert_eq!(branch_name_from_task("!!!"), "agent/task");
    }

    #[test]
    fn repo_names_are_derived_from_path() {
        assert_eq!(repo_name_from_path("/tmp/demo"), "demo");
    }
}
