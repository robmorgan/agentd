use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionEvent {
    pub id: i64,
    pub session_id: String,
    pub timestamp: DateTime<Utc>,
    #[serde(rename = "type")]
    pub event_type: String,
    pub payload_json: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NewSessionEvent {
    #[serde(rename = "type")]
    pub event_type: String,
    pub payload_json: Value,
}
