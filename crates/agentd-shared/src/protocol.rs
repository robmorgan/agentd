use serde::{Deserialize, Serialize};

use crate::session::{CreateSessionResult, SessionRecord};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Request {
    CreateSession {
        workspace: String,
        task: String,
        agent: String,
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
    CreateSession { session: CreateSessionResult },
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
