use serde::{Deserialize, Serialize};

use crate::session::{CreateSessionResult, SessionDiff, SessionRecord, WorktreeRecord};

pub const PROTOCOL_VERSION: u32 = 1;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DaemonInfo {
    pub daemon_version: String,
    pub protocol_version: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Request {
    GetDaemonInfo,
    ShutdownDaemon,
    CreateSession {
        workspace: String,
        task: String,
        agent: String,
    },
    CreateWorktree {
        session_id: String,
    },
    CleanupWorktree {
        session_id: String,
    },
    DiffSession {
        session_id: String,
    },
    GetSession {
        session_id: String,
    },
    ListSessions,
    StreamLogs {
        session_id: String,
        #[serde(default = "default_follow")]
        follow: bool,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Response {
    DaemonInfo { info: DaemonInfo },
    CreateSession { session: CreateSessionResult },
    Worktree { worktree: WorktreeRecord },
    Diff { diff: SessionDiff },
    Session { session: SessionRecord },
    Sessions { sessions: Vec<SessionRecord> },
    LogChunk { data: String },
    EndOfStream,
    Error { message: String },
    Ok,
}

fn default_follow() -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::Request;

    #[test]
    fn stream_logs_defaults_to_follow() {
        let request: Request =
            serde_json::from_str(r#"{"type":"stream_logs","session_id":"demo"}"#).unwrap();
        match request {
            Request::StreamLogs { follow, .. } => assert!(follow),
            _ => panic!("unexpected request variant"),
        }
    }
}
