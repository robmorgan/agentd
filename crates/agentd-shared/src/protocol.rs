use serde::{Deserialize, Serialize};

use crate::{
    event::{NewSessionEvent, SessionEvent},
    session::{CreateSessionResult, SessionDiff, SessionRecord, WorktreeRecord},
};

pub const PROTOCOL_VERSION: u32 = 4;

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
    AttachSession {
        session_id: String,
    },
    AttachInput {
        data: String,
    },
    SendInput {
        session_id: String,
        data: String,
        #[serde(default)]
        source_session_id: Option<String>,
    },
    DiffSession {
        session_id: String,
    },
    GetSession {
        session_id: String,
    },
    ListSessions,
    AppendSessionEvents {
        session_id: String,
        events: Vec<NewSessionEvent>,
    },
    StreamLogs {
        session_id: String,
        #[serde(default = "default_follow")]
        follow: bool,
    },
    StreamEvents {
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
    Attached,
    InputAccepted,
    Worktree { worktree: WorktreeRecord },
    Diff { diff: SessionDiff },
    Session { session: SessionRecord },
    Sessions { sessions: Vec<SessionRecord> },
    Event { event: SessionEvent },
    LogChunk { data: String },
    PtyOutput { data: String },
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
    use crate::event::SessionEvent;
    use chrono::Utc;
    use serde_json::json;

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
    fn stream_events_defaults_to_follow() {
        let request: Request =
            serde_json::from_str(r#"{"type":"stream_events","session_id":"demo"}"#).unwrap();
        match request {
            Request::StreamEvents { follow, .. } => assert!(follow),
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
    fn event_response_round_trips() {
        let response = Response::Event {
            event: SessionEvent {
                id: 7,
                session_id: "demo".to_string(),
                timestamp: Utc::now(),
                event_type: "COMMAND_EXECUTED".to_string(),
                payload_json: json!({"command":"cargo test","exit_code":1}),
            },
        };
        let json = serde_json::to_string(&response).unwrap();
        let decoded: Response = serde_json::from_str(&json).unwrap();
        match decoded {
            Response::Event { event } => {
                assert_eq!(event.id, 7);
                assert_eq!(event.event_type, "COMMAND_EXECUTED");
            }
            _ => panic!("unexpected response variant"),
        }
    }

    #[test]
    fn send_input_defaults_to_no_source_session() {
        let request: Request =
            serde_json::from_str(r#"{"type":"send_input","session_id":"demo","data":"hello"}"#)
                .unwrap();
        match request {
            Request::SendInput {
                source_session_id, ..
            } => assert!(source_session_id.is_none()),
            _ => panic!("unexpected request variant"),
        }
    }

    #[test]
    fn attach_output_response_round_trips() {
        let response = Response::PtyOutput {
            data: "hello".to_string(),
        };
        let json = serde_json::to_string(&response).unwrap();
        let decoded: Response = serde_json::from_str(&json).unwrap();
        match decoded {
            Response::PtyOutput { data } => assert_eq!(data, "hello"),
            _ => panic!("unexpected response variant"),
        }
    }
}
