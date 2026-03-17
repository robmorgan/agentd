use serde_json::{Map, Value, json};

#[derive(Debug, Clone, PartialEq)]
pub struct ParsedCodexMessage {
    pub event_type: String,
    pub payload_json: Value,
    pub rendered: String,
    pub upstream_thread_id: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParseIssue {
    pub line: String,
    pub error: String,
}

#[derive(Default)]
pub struct CodexJsonStream {
    pending: String,
}

impl CodexJsonStream {
    pub fn push_bytes(&mut self, data: &[u8]) -> (Vec<ParsedCodexMessage>, Vec<ParseIssue>) {
        self.pending.push_str(&String::from_utf8_lossy(data));
        self.drain_complete_lines()
    }

    pub fn finish(&mut self) -> (Vec<ParsedCodexMessage>, Vec<ParseIssue>) {
        if !self.pending.is_empty() {
            self.pending.push('\n');
        }
        self.drain_complete_lines()
    }

    fn drain_complete_lines(&mut self) -> (Vec<ParsedCodexMessage>, Vec<ParseIssue>) {
        let mut messages = Vec::new();
        let mut issues = Vec::new();

        while let Some(index) = self.pending.find('\n') {
            let mut line = self.pending.drain(..=index).collect::<String>();
            if line.ends_with('\n') {
                line.pop();
            }
            if line.ends_with('\r') {
                line.pop();
            }
            if line.trim().is_empty() {
                continue;
            }
            match parse_line(&line) {
                Ok(message) => messages.push(message),
                Err(error) => issues.push(ParseIssue {
                    line,
                    error: error.to_string(),
                }),
            }
        }

        (messages, issues)
    }
}

fn parse_line(line: &str) -> serde_json::Result<ParsedCodexMessage> {
    let raw = serde_json::from_str::<Value>(line)?;
    let codex_type = raw
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or("message")
        .to_string();
    let rendered = render_message(&codex_type, &raw);
    let payload_json = normalize_payload(&codex_type, raw.clone(), &rendered);
    Ok(ParsedCodexMessage {
        event_type: format!("CODEX_{}", to_upper_snake(&codex_type)),
        payload_json,
        rendered,
        upstream_thread_id: discover_upstream_thread_id(&raw),
    })
}

fn normalize_payload(codex_type: &str, raw: Value, rendered: &str) -> Value {
    let mut payload = Map::new();
    payload.insert("source".to_string(), json!("codex"));
    payload.insert("codex_type".to_string(), json!(codex_type));
    if let Some(id) = raw.get("id").and_then(Value::as_str) {
        payload.insert("id".to_string(), json!(id));
    }
    if let Some(role) = raw.get("role").and_then(Value::as_str) {
        payload.insert("role".to_string(), json!(role));
    }
    if let Some(text) = extract_text(&raw) {
        payload.insert("text".to_string(), json!(text));
    }
    payload.insert("rendered".to_string(), json!(rendered.trim_end()));
    payload.insert("raw".to_string(), raw);
    Value::Object(payload)
}

fn render_message(codex_type: &str, value: &Value) -> String {
    let label = codex_type.replace('_', " ");
    if let Some(text) = extract_text(value) {
        return format!("[{label}] {text}\n");
    }

    match serde_json::to_string_pretty(value) {
        Ok(pretty) => format!("[{label}]\n{pretty}\n"),
        Err(_) => format!("[{label}] {value}\n"),
    }
}

fn extract_text(value: &Value) -> Option<String> {
    match value {
        Value::String(text) => Some(text.clone()),
        Value::Array(items) => {
            let mut parts = Vec::new();
            for item in items {
                if let Some(text) = extract_text(item) {
                    let trimmed = text.trim();
                    if !trimmed.is_empty() {
                        parts.push(trimmed.to_string());
                    }
                }
            }
            if parts.is_empty() {
                None
            } else {
                Some(parts.join(" "))
            }
        }
        Value::Object(object) => {
            for key in [
                "message",
                "text",
                "content",
                "summary",
                "description",
                "output",
                "delta",
            ] {
                if let Some(value) = object.get(key).and_then(extract_text) {
                    return Some(value);
                }
            }
            None
        }
        _ => None,
    }
}

fn discover_upstream_thread_id(value: &Value) -> Option<String> {
    match value {
        Value::Object(object) => {
            for key in ["session_id", "thread_id", "conversation_id"] {
                if let Some(id) = object.get(key).and_then(Value::as_str) {
                    return Some(id.to_string());
                }
            }
            object.values().find_map(discover_upstream_thread_id)
        }
        Value::Array(items) => items.iter().find_map(discover_upstream_thread_id),
        _ => None,
    }
}

fn to_upper_snake(value: &str) -> String {
    let mut output = String::with_capacity(value.len());
    let mut previous_was_separator = false;
    for ch in value.chars() {
        if ch.is_ascii_alphanumeric() {
            if ch.is_ascii_uppercase() && !output.is_empty() && !previous_was_separator {
                output.push('_');
            }
            output.push(ch.to_ascii_uppercase());
            previous_was_separator = false;
        } else if !output.is_empty() && !previous_was_separator {
            output.push('_');
            previous_was_separator = true;
        }
    }
    output.trim_matches('_').to_string()
}

pub fn preview_line(line: &str) -> String {
    let mut preview = line.replace('\n', "\\n").replace('\r', "\\r");
    if preview.len() > 160 {
        preview.truncate(160);
        preview.push_str("...");
    }
    preview
}

#[cfg(test)]
mod tests {
    use super::CodexJsonStream;

    #[test]
    fn stream_parses_json_lines_and_preserves_partial_lines() {
        let mut stream = CodexJsonStream::default();
        let (messages, issues) = stream.push_bytes(br#"{"type":"message","message":"hel"#);
        assert!(messages.is_empty());
        assert!(issues.is_empty());

        let (messages, issues) = stream.push_bytes(b"lo\"}\n");
        assert_eq!(messages.len(), 1);
        assert!(issues.is_empty());
        assert_eq!(messages[0].event_type, "CODEX_MESSAGE");
        assert_eq!(messages[0].payload_json["text"], "hello");
    }

    #[test]
    fn stream_reports_invalid_json_lines() {
        let mut stream = CodexJsonStream::default();
        let (_messages, issues) = stream.push_bytes(b"not-json\n");
        assert_eq!(issues.len(), 1);
        assert_eq!(issues[0].line, "not-json");
    }
}
