use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PendingUserInputRequest {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
    pub questions: Vec<UserInputQuestion>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct UserInputQuestion {
    pub id: String,
    pub header: String,
    pub question: String,
    #[serde(default)]
    pub options: Vec<UserInputOption>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct UserInputOption {
    pub label: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default)]
    pub is_other: bool,
}

impl PendingUserInputRequest {
    pub fn from_codex_raw(raw: &Value) -> Option<Self> {
        let questions = raw.get("questions")?.as_array()?;
        let questions = questions
            .iter()
            .enumerate()
            .filter_map(|(index, question)| UserInputQuestion::from_codex_value(question, index))
            .collect::<Vec<_>>();
        if questions.is_empty() {
            return None;
        }
        Some(Self {
            turn_id: raw
                .get("turn_id")
                .and_then(Value::as_str)
                .map(str::to_string),
            questions,
        })
    }

    pub fn summary(&self) -> String {
        self.questions
            .first()
            .map(|question| question.question.as_str())
            .unwrap_or("Agent is waiting for input.")
            .to_string()
    }
}

impl UserInputQuestion {
    fn from_codex_value(value: &Value, index: usize) -> Option<Self> {
        let question = value.get("question").and_then(Value::as_str)?.trim();
        if question.is_empty() {
            return None;
        }

        let id = value
            .get("id")
            .and_then(Value::as_str)
            .filter(|value| !value.trim().is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| format!("question-{}", index + 1));
        let header = value
            .get("header")
            .and_then(Value::as_str)
            .filter(|value| !value.trim().is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| format!("Question {}", index + 1));
        let options = value
            .get("options")
            .and_then(Value::as_array)
            .map(|items| {
                items.iter()
                    .filter_map(UserInputOption::from_codex_value)
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default();

        Some(Self {
            id,
            header,
            question: question.to_string(),
            options,
        })
    }
}

impl UserInputOption {
    fn from_codex_value(value: &Value) -> Option<Self> {
        let label = value.get("label").and_then(Value::as_str)?.trim();
        if label.is_empty() {
            return None;
        }

        Some(Self {
            label: label.to_string(),
            description: value
                .get("description")
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|value| !value.is_empty())
                .map(str::to_string),
            is_other: value.get("is_other").and_then(Value::as_bool).unwrap_or(false),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::PendingUserInputRequest;
    use serde_json::json;

    #[test]
    fn parses_codex_request_user_input_payload() {
        let request = PendingUserInputRequest::from_codex_raw(&json!({
            "turn_id": "turn-1",
            "questions": [
                {
                    "id": "scope",
                    "header": "Scope",
                    "question": "Which scope should I use?",
                    "options": [
                        { "label": "Plan only" },
                        { "label": "All sessions", "description": "Use it everywhere." }
                    ]
                }
            ]
        }))
        .unwrap();

        assert_eq!(request.turn_id.as_deref(), Some("turn-1"));
        assert_eq!(request.questions.len(), 1);
        assert_eq!(request.questions[0].header, "Scope");
        assert_eq!(request.questions[0].options[1].description.as_deref(), Some("Use it everywhere."));
    }
}
