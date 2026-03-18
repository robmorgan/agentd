use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use slug::slugify;

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SessionStatus {
    Creating,
    Running,
    Paused,
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

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionRecord {
    pub session_id: String,
    pub thread_id: Option<String>,
    pub agent: String,
    pub workspace: String,
    pub repo_path: String,
    pub task: String,
    pub base_branch: String,
    pub branch: String,
    pub worktree: String,
    pub status: SessionStatus,
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

impl AttentionLevel {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Info => "info",
            Self::Notice => "notice",
            Self::Action => "action",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::branch_name_from_task;

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
}
