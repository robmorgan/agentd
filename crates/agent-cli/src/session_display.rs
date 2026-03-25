use chrono::{DateTime, Utc};

use agentd_shared::session::{ApplyState, SessionRecord, SessionStatus};

const ANSI_RESET: &str = "\x1b[0m";
const ANSI_RUNNING: &str = "\x1b[32m";
const ANSI_COMPLETED: &str = "\x1b[36m";
const ANSI_FAILED: &str = "\x1b[31m";
const ANSI_INACTIVE: &str = "\x1b[90m";
const ANSI_DIRTY: &str = "\x1b[33m";
const ANSI_AHEAD: &str = "\x1b[35m";
const ANSI_EMPHASIS: &str = "\x1b[1m";
const ANSI_DIM_TEXT: &str = "\x1b[2m\x1b[90m";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RunState {
    Starting,
    Running,
    Exited,
    Completed,
    Failed,
    Recovered,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct SessionDisplayRow {
    pub run_state: RunState,
    pub dirty_count: u32,
    pub ahead_count: u32,
    pub age_text: String,
    pub name: String,
    pub branch: String,
    pub needs_attention: bool,
}

pub(crate) fn session_elapsed_label(session: &SessionRecord) -> String {
    session_elapsed_label_at(session, Utc::now())
}

pub(crate) fn build_session_display_row(session: &SessionRecord) -> SessionDisplayRow {
    let run_state = session_run_state(session);
    SessionDisplayRow {
        run_state,
        dirty_count: session.dirty_count,
        ahead_count: session.ahead_count,
        age_text: session_elapsed_label(session),
        name: session.session_id.clone(),
        branch: session.branch.clone(),
        needs_attention: run_state == RunState::Failed
            || session.dirty_count > 0
            || session.ahead_count > 0,
    }
}

pub(crate) fn session_run_state(session: &SessionRecord) -> RunState {
    if session.status == SessionStatus::Failed {
        return RunState::Failed;
    }
    if session.apply_state == ApplyState::Applied {
        return RunState::Completed;
    }
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
        RunState::Completed => "✓",
        RunState::Failed => "✖",
    }
}

pub(crate) fn render_count_text(count: u32) -> String {
    if count == 0 { "-".to_string() } else { count.to_string() }
}

pub(crate) fn style_run(text: &str, run_state: RunState) -> String {
    style_text(
        text,
        match run_state {
            RunState::Starting | RunState::Running => ANSI_RUNNING,
            RunState::Exited | RunState::Recovered => ANSI_INACTIVE,
            RunState::Completed => ANSI_COMPLETED,
            RunState::Failed => ANSI_FAILED,
        },
    )
}

pub(crate) fn style_dirty(text: &str, dirty_count: u32) -> String {
    if dirty_count == 0 { text.to_string() } else { style_text(text, ANSI_DIRTY) }
}

pub(crate) fn style_ahead(text: &str, ahead_count: u32) -> String {
    if ahead_count == 0 { text.to_string() } else { style_text(text, ANSI_AHEAD) }
}

pub(crate) fn style_age(text: &str) -> String {
    style_text(text, ANSI_DIM_TEXT)
}

pub(crate) fn style_name(text: &str) -> String {
    style_text(text, ANSI_EMPHASIS)
}

pub(crate) fn style_branch(text: &str) -> String {
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
        RunState, build_session_display_row, format_elapsed_seconds, render_count_text,
        render_run_icon, session_elapsed_label_at, session_run_state,
    };
    use agentd_shared::session::{
        ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord, SessionStatus,
    };
    use chrono::{Duration, Utc};

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
    fn build_display_row_marks_attention_from_counts_or_failure() {
        let mut session = demo_session(Utc::now(), None);
        session.dirty_count = 2;
        let row = build_session_display_row(&session);
        assert!(row.needs_attention);

        session.dirty_count = 0;
        session.ahead_count = 1;
        let row = build_session_display_row(&session);
        assert!(row.needs_attention);

        session.ahead_count = 0;
        session.status = SessionStatus::Failed;
        let row = build_session_display_row(&session);
        assert!(row.needs_attention);
    }

    #[test]
    fn run_state_prefers_completed_for_applied_sessions() {
        let mut session = demo_session(Utc::now(), None);
        session.apply_state = ApplyState::Applied;
        assert_eq!(session_run_state(&session), RunState::Completed);
    }

    #[test]
    fn run_state_maps_recovered_sessions() {
        let mut session = demo_session(Utc::now(), None);
        session.status = SessionStatus::UnknownRecovered;
        assert_eq!(session_run_state(&session), RunState::Recovered);
        assert_eq!(render_run_icon(RunState::Recovered), "○");
    }

    #[test]
    fn count_text_uses_dash_for_zero() {
        assert_eq!(render_count_text(0), "-");
        assert_eq!(render_count_text(3), "3");
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
            workspace: "/tmp/repo".to_string(),
            repo_path: "/tmp/repo".to_string(),
            repo_name: "repo".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status: SessionStatus::Running,
            integration_policy: IntegrationPolicy::AutoApplySafe,
            apply_state: ApplyState::Idle,
            dirty_count: 0,
            ahead_count: 0,
            has_commits: false,
            has_pending_changes: false,
            pid: Some(1),
            exit_code: None,
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: None,
            created_at,
            updated_at: created_at,
            exited_at,
        }
    }
}
