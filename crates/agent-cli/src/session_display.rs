use chrono::{DateTime, Utc};

use agentd_shared::session::SessionRecord;

pub(crate) fn session_elapsed_label(session: &SessionRecord) -> String {
    session_elapsed_label_at(session, Utc::now())
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
    use super::{format_elapsed_seconds, session_elapsed_label_at};
    use agentd_shared::session::{
        ApplyState, AttentionLevel, IntegrationPolicy, MergeStatus, SessionMode, SessionRecord,
        SessionStatus,
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

    fn demo_session(
        created_at: chrono::DateTime<Utc>,
        exited_at: Option<chrono::DateTime<Utc>>,
    ) -> SessionRecord {
        SessionRecord {
            session_id: "demo".to_string(),
            thread_id: Some("thread-demo".to_string()),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/repo".to_string(),
            repo_path: "/tmp/repo".to_string(),
            repo_name: "repo".to_string(),
            title: "demo".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status: SessionStatus::Running,
            integration_policy: IntegrationPolicy::AutoApplySafe,
            apply_state: ApplyState::Idle,
            merge_status: MergeStatus::Unknown,
            merge_summary: None,
            has_conflicts: false,
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
