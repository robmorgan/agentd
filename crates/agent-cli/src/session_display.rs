use chrono::{DateTime, Utc};

use agentd_shared::session::{AttentionLevel, SessionRecord, SessionStatus};

const ANSI_RESET: &str = "\x1b[0m";
const ANSI_RUNNING: &str = "\x1b[32m";
const ANSI_FAILED: &str = "\x1b[31m";
const ANSI_INACTIVE: &str = "\x1b[90m";
const ANSI_EMPHASIS: &str = "\x1b[1m";
const ANSI_DIM_TEXT: &str = "\x1b[2m\x1b[90m";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RunState {
    Starting,
    Running,
    Exited,
    Failed,
    Recovered,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct SessionDisplayRow {
    pub run_state: RunState,
    pub age_text: String,
    pub name: String,
    pub cwd: String,
    pub needs_attention: bool,
}

pub(crate) fn session_elapsed_label(session: &SessionRecord) -> String {
    session_elapsed_label_at(session, Utc::now())
}

pub(crate) fn build_session_display_row(session: &SessionRecord) -> SessionDisplayRow {
    let run_state = session_run_state(session);
    SessionDisplayRow {
        run_state,
        age_text: session_elapsed_label(session),
        name: session.session_id.clone(),
        cwd: display_cwd(&session.cwd, std::env::var_os("HOME").as_deref()),
        needs_attention: run_state == RunState::Failed
            || session.attention == AttentionLevel::Action,
    }
}

/// Shortens a session cwd for list views by replacing the home directory with
/// `~`. The full path is still shown by `agent status`.
pub(crate) fn display_cwd(cwd: &str, home: Option<&std::ffi::OsStr>) -> String {
    let cwd = escape_controls(cwd);
    let Some(home) = home.and_then(|home| home.to_str()).map(|home| home.trim_end_matches('/'))
    else {
        return cwd;
    };
    if home.is_empty() {
        return cwd;
    }
    match cwd.strip_prefix(home) {
        Some("") => "~".to_string(),
        Some(rest) if rest.starts_with('/') => format!("~{rest}"),
        _ => cwd,
    }
}

/// Directory names may contain control characters, including escape
/// sequences a terminal would act on. Anything shown from a path is passed
/// through this first, so it is printed rather than interpreted.
pub(crate) fn escape_controls(text: &str) -> String {
    if !text.chars().any(char::is_control) {
        return text.to_string();
    }
    text.chars()
        .map(|c| if c.is_control() { c.escape_default().to_string() } else { c.to_string() })
        .collect()
}

pub(crate) fn session_run_state(session: &SessionRecord) -> RunState {
    match session.status {
        SessionStatus::Creating => RunState::Starting,
        SessionStatus::Running => RunState::Running,
        SessionStatus::Exited => RunState::Exited,
        SessionStatus::Failed => RunState::Failed,
        SessionStatus::UnknownRecovered => RunState::Recovered,
    }
}

pub(crate) fn render_run_icon(run_state: RunState) -> &'static str {
    match run_state {
        RunState::Starting | RunState::Running => "●",
        RunState::Exited | RunState::Recovered => "○",
        RunState::Failed => "✖",
    }
}

pub(crate) fn style_run(text: &str, run_state: RunState) -> String {
    style_text(
        text,
        match run_state {
            RunState::Starting | RunState::Running => ANSI_RUNNING,
            RunState::Exited | RunState::Recovered => ANSI_INACTIVE,
            RunState::Failed => ANSI_FAILED,
        },
    )
}

pub(crate) fn style_age(text: &str) -> String {
    style_text(text, ANSI_DIM_TEXT)
}

pub(crate) fn style_name(text: &str) -> String {
    style_text(text, ANSI_EMPHASIS)
}

pub(crate) fn style_cwd(text: &str) -> String {
    style_text(text, ANSI_DIM_TEXT)
}

fn style_text(text: &str, prefix: &str) -> String {
    format!("{prefix}{text}{ANSI_RESET}")
}

fn session_elapsed_label_at(session: &SessionRecord, now: DateTime<Utc>) -> String {
    let end = session.exited_at.unwrap_or(now);
    let elapsed_seconds = end.signed_duration_since(session.created_at).num_seconds().max(0) as u64;
    format_elapsed_seconds(elapsed_seconds)
}

fn format_elapsed_seconds(seconds: u64) -> String {
    if seconds < 60 {
        format!("{seconds}s")
    } else if seconds < 60 * 60 {
        format!("{}m", seconds / 60)
    } else if seconds < 60 * 60 * 24 {
        format!("{}h", seconds / (60 * 60))
    } else {
        format!("{}d", seconds / (60 * 60 * 24))
    }
}

#[cfg(test)]
mod tests {
    use super::{
        RunState, build_session_display_row, display_cwd, escape_controls, format_elapsed_seconds,
        render_run_icon, session_elapsed_label_at, session_run_state,
    };
    use agentd_shared::session::{AttentionLevel, SessionMode, SessionRecord, SessionStatus};
    use chrono::{Duration, Utc};
    use std::ffi::OsStr;

    #[test]
    fn format_elapsed_seconds_uses_largest_unit() {
        assert_eq!(format_elapsed_seconds(30), "30s");
        assert_eq!(format_elapsed_seconds(59), "59s");
        assert_eq!(format_elapsed_seconds(60), "1m");
        assert_eq!(format_elapsed_seconds(59 * 60 + 59), "59m");
        assert_eq!(format_elapsed_seconds(60 * 60), "1h");
        assert_eq!(format_elapsed_seconds(23 * 60 * 60 + 59 * 60), "23h");
        assert_eq!(format_elapsed_seconds(24 * 60 * 60), "1d");
    }

    #[test]
    fn elapsed_label_uses_exit_time_for_finished_sessions() {
        let created_at = Utc::now() - Duration::hours(4);
        let exited_at = created_at + Duration::minutes(90);
        let session = demo_session(created_at, Some(exited_at));
        let now = created_at + Duration::hours(5);
        assert_eq!(session_elapsed_label_at(&session, now), "1h");
    }

    #[test]
    fn elapsed_label_clamps_negative_durations() {
        let now = Utc::now();
        let session = demo_session(now + Duration::minutes(5), None);
        assert_eq!(session_elapsed_label_at(&session, now), "0s");
    }

    #[test]
    fn build_display_row_marks_attention_from_action_or_failure() {
        let mut session = demo_session(Utc::now(), None);
        assert!(!build_session_display_row(&session).needs_attention);

        session.attention = AttentionLevel::Action;
        assert!(build_session_display_row(&session).needs_attention);

        session.attention = AttentionLevel::Info;
        session.status = SessionStatus::Failed;
        assert!(build_session_display_row(&session).needs_attention);
    }

    #[test]
    fn run_state_maps_recovered_sessions() {
        let mut session = demo_session(Utc::now(), None);
        session.status = SessionStatus::UnknownRecovered;
        assert_eq!(session_run_state(&session), RunState::Recovered);
        assert_eq!(render_run_icon(RunState::Recovered), "○");
    }

    #[test]
    fn paths_with_control_characters_are_escaped() {
        let hostile = "/tmp/\u{1b}]52;c;cHduZWQ=\u{7}x";
        let shown = display_cwd(hostile, None);
        assert!(!shown.chars().any(char::is_control), "{shown:?}");
        assert_eq!(shown, "/tmp/\\u{1b}]52;c;cHduZWQ=\\u{7}x");
        assert_eq!(escape_controls("/plain/path"), "/plain/path");
    }

    #[test]
    fn display_cwd_abbreviates_home() {
        let home = Some(OsStr::new("/Users/tester"));
        assert_eq!(display_cwd("/Users/tester/code/app", home), "~/code/app");
        assert_eq!(display_cwd("/Users/tester", home), "~");
        assert_eq!(display_cwd("/Users/tester2/app", home), "/Users/tester2/app");
        assert_eq!(display_cwd("/srv/repo", home), "/srv/repo");
        assert_eq!(display_cwd("/srv/repo", None), "/srv/repo");
    }

    fn demo_session(
        created_at: chrono::DateTime<Utc>,
        exited_at: Option<chrono::DateTime<Utc>>,
    ) -> SessionRecord {
        SessionRecord {
            session_id: "demo".to_string(),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            cwd: "/tmp/repo".to_string(),
            status: SessionStatus::Running,
            worker_pid: Some(1),
            agent_pid: Some(2),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at,
            updated_at: created_at,
            exited_at,
            workspace: None,
        }
    }
}
