use serde::{Deserialize, Serialize};

use crate::session::{CreateSessionResult, SessionDiff, SessionRecord, WorktreeRecord};

pub const PROTOCOL_VERSION: u32 = 2;

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
    KillSession {
        session_id: String,
        #[serde(default)]
        remove: bool,
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
    KillSession { removed: bool, was_running: bool },
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
    use super::{Request, Response};

    #[test]
    fn stream_logs_defaults_to_follow() {
        let request: Request =
            serde_json::from_str(r#"{"type":"stream_logs","session_id":"demo"}"#).unwrap();
        match request {
            Request::StreamLogs { follow, .. } => assert!(follow),
            _ => panic!("unexpected request variant"),
        }
    }

    #[test]
    fn kill_session_defaults_to_keep_record() {
        let request: Request =
            serde_json::from_str(r#"{"type":"kill_session","session_id":"demo"}"#).unwrap();
        match request {
            Request::KillSession { remove, .. } => assert!(!remove),
            _ => panic!("unexpected request variant"),
        }
    }

    #[test]
    fn kill_session_response_round_trips() {
        let response = Response::KillSession {
            removed: true,
            was_running: false,
        };
        let json = serde_json::to_string(&response).unwrap();
        let decoded: Response = serde_json::from_str(&json).unwrap();
        match decoded {
            Response::KillSession {
                removed,
                was_running,
            } => {
                assert!(removed);
                assert!(!was_running);
            }
            _ => panic!("unexpected response variant"),
        }
    }
}
