use std::{io, path::Path, time::Duration};

use anyhow::{Context, Result, anyhow, bail};
use tokio::{
    io::BufReader,
    net::{UnixListener, UnixStream, unix::OwnedWriteHalf},
    sync::{broadcast, watch},
};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{
        DaemonInfo, DaemonManagementRequest, DaemonManagementResponse, DaemonManagementStatus,
        IncomingRequest, PROTOCOL_VERSION, Request, Response, read_incoming_request, read_request,
        write_daemon_management_response, write_response,
    },
    session::{SessionRecord, SessionStatus},
};

use crate::{app::AppState, db::Database};

pub async fn serve() -> Result<()> {
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;

    if Path::new(paths.socket.as_str()).exists() {
        match std::fs::remove_file(paths.socket.as_std_path()) {
            Ok(_) => {}
            Err(err) if err.kind() == io::ErrorKind::NotFound => {}
            Err(err) => return Err(err).context("failed to remove stale agentd socket"),
        }
    }

    let db = Database::open(&paths)?;
    let config = Config::load(&paths)?;
    let state = AppState::new(paths.clone(), db, config);
    state.reconcile_sessions().await?;
    state.resume_paused_sessions().await?;

    let listener =
        UnixListener::bind(paths.socket.as_std_path()).context("failed to bind agentd socket")?;
    std::fs::write(paths.pid_file.as_std_path(), std::process::id().to_string())
        .with_context(|| format!("failed to write {}", paths.pid_file))?;
    let (shutdown_tx, mut shutdown_rx) = watch::channel(false);

    loop {
        tokio::select! {
            accept_result = listener.accept() => {
                let (stream, _) = accept_result?;
                let state = state.clone();
                let shutdown_tx = shutdown_tx.clone();
                tokio::spawn(async move {
                    if let Err(err) = handle_connection(state, shutdown_tx, stream).await {
                        eprintln!("connection error: {err:#}");
                    }
                });
            }
            changed = shutdown_rx.changed() => {
                changed?;
                if *shutdown_rx.borrow() {
                    break;
                }
            }
        }
    }

    let _ = std::fs::remove_file(paths.socket.as_std_path());
    let _ = std::fs::remove_file(paths.pid_file.as_std_path());
    Ok(())
}

async fn handle_connection(
    state: AppState,
    shutdown_tx: watch::Sender<bool>,
    stream: UnixStream,
) -> Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    let Some(request) = read_incoming_request(&mut reader).await? else {
        return Ok(());
    };
    match request {
        IncomingRequest::DaemonManagement(request) => {
            handle_daemon_management_request(&state, shutdown_tx, &mut writer, request).await?;
        }
        IncomingRequest::Standard(Request::GetDaemonInfo) => {
            let info = DaemonInfo {
                daemon_version: env!("CARGO_PKG_VERSION").to_string(),
                protocol_version: PROTOCOL_VERSION,
            };
            send_response(&mut writer, &Response::DaemonInfo { info }).await?;
        }
        IncomingRequest::Standard(Request::ShutdownDaemon) => {
            if state.has_running_sessions().await? {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: "cannot shut down agentd while sessions are running".to_string(),
                    },
                )
                .await?;
            } else {
                send_response(&mut writer, &Response::Ok).await?;
                let _ = shutdown_tx.send(true);
            }
        }
        IncomingRequest::Standard(Request::CreateSession {
            workspace,
            task,
            agent,
            model,
            mode,
        }) => match state.create_session(workspace, task, agent, model, mode).await {
            Ok(session) => send_response(&mut writer, &Response::CreateSession { session }).await?,
            Err(err) => {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: err.to_string(),
                    },
                )
                .await?
            }
        },
        IncomingRequest::Standard(Request::CreateWorktree { session_id }) => {
            match state.create_worktree(&session_id).await {
                Ok(worktree) => {
                    send_response(&mut writer, &Response::Worktree { worktree }).await?
                }
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::CleanupWorktree { session_id }) => {
            match state.cleanup_worktree(&session_id).await {
                Ok(worktree) => {
                    send_response(&mut writer, &Response::Worktree { worktree }).await?
                }
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::KillSession { session_id, remove }) => {
            match state.kill_session(&session_id, remove).await {
                Ok((removed, was_running)) => {
                    send_response(
                        &mut writer,
                        &Response::KillSession {
                            removed,
                            was_running,
                        },
                    )
                    .await?
                }
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::AttachSession { session_id }) => {
            attach_session(&state, &session_id, &mut reader, &mut writer).await?;
        }
        IncomingRequest::Standard(Request::DetachSession { session_id }) => {
            match state.detach_session(&session_id).await {
                Ok(()) => send_response(&mut writer, &Response::Ok).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::AttachInput { .. }) => {
            send_response(
                &mut writer,
                &Response::Error {
                    message: "attach_input is only valid during an attached session".to_string(),
                },
            )
            .await?;
        }
        IncomingRequest::Standard(Request::AttachResize { .. }) => {
            send_response(
                &mut writer,
                &Response::Error {
                    message: "attach_resize is only valid during an attached session".to_string(),
                },
            )
            .await?;
        }
        IncomingRequest::Standard(Request::SendInput {
            session_id,
            data,
            source_session_id,
        }) => match state.send_input(&session_id, data, source_session_id).await {
            Ok(()) => send_response(&mut writer, &Response::InputAccepted).await?,
            Err(err) => {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: err.to_string(),
                    },
                )
                .await?
            }
        },
        IncomingRequest::Standard(Request::ReplyToSession { session_id, prompt }) => {
            match state.reply_to_session(&session_id, prompt).await {
                Ok(session) => send_response(&mut writer, &Response::Session { session }).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::ApplySession { session_id }) => {
            match state.apply_session(&session_id).await {
                Ok(session) => send_response(&mut writer, &Response::Session { session }).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::DiscardSession { session_id, force }) => {
            match state.discard_session(&session_id, force).await {
                Ok(session) => send_response(&mut writer, &Response::Session { session }).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::SwitchAttachedSession {
            source_session_id,
            target_session_id,
        }) => match state
            .switch_attached_session(&source_session_id, &target_session_id)
            .await
        {
            Ok(()) => send_response(&mut writer, &Response::Ok).await?,
            Err(err) => {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: err.to_string(),
                    },
                )
                .await?
            }
        },
        IncomingRequest::Standard(Request::DiffSession { session_id }) => {
            match state.diff_session(&session_id).await {
                Ok(diff) => send_response(&mut writer, &Response::Diff { diff }).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::GetSession { session_id }) => {
            match state.get_session(&session_id).await? {
                Some(session) => send_response(&mut writer, &Response::Session { session }).await?,
                None => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: format!("session `{session_id}` not found"),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::ListSessions) => {
            let sessions = state.list_sessions().await?;
            send_response(&mut writer, &Response::Sessions { sessions }).await?;
        }
        IncomingRequest::Standard(Request::AppendSessionEvents { session_id, events }) => {
            match state.append_session_events(&session_id, events).await {
                Ok(_) => send_response(&mut writer, &Response::Ok).await?,
                Err(err) => {
                    send_response(
                        &mut writer,
                        &Response::Error {
                            message: err.to_string(),
                        },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::StreamLogs { session_id, follow }) => {
            if let Err(err) = stream_logs(&state, &session_id, follow, &mut writer).await {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: err.to_string(),
                    },
                )
                .await?;
            } else {
                send_response(&mut writer, &Response::EndOfStream).await?;
            }
        }
        IncomingRequest::Standard(Request::StreamEvents { session_id, follow }) => {
            if let Err(err) = stream_events(&state, &session_id, follow, &mut writer).await {
                send_response(
                    &mut writer,
                    &Response::Error {
                        message: err.to_string(),
                    },
                )
                .await?;
            } else {
                send_response(&mut writer, &Response::EndOfStream).await?;
            }
        }
    }

    Ok(())
}

async fn handle_daemon_management_request(
    state: &AppState,
    shutdown_tx: watch::Sender<bool>,
    writer: &mut OwnedWriteHalf,
    request: DaemonManagementRequest,
) -> Result<()> {
    match request {
        DaemonManagementRequest::Status => {
            let response = DaemonManagementResponse::Status {
                status: DaemonManagementStatus {
                    daemon_version: env!("CARGO_PKG_VERSION").to_string(),
                    protocol_version: PROTOCOL_VERSION,
                    pid: std::process::id(),
                    root: state.paths.root.to_string(),
                    socket: state.paths.socket.to_string(),
                    running_sessions: state.has_running_sessions().await?,
                },
            };
            write_daemon_management_response(writer, &response).await?;
        }
        DaemonManagementRequest::Shutdown { force } => {
            let running_sessions = state.has_running_sessions().await?;
            if running_sessions && !force {
                write_daemon_management_response(
                    writer,
                    &DaemonManagementResponse::Shutdown {
                        stopped: false,
                        running_sessions: true,
                        message: "cannot shut down agentd while sessions are running".to_string(),
                    },
                )
                .await?;
            } else {
                write_daemon_management_response(
                    writer,
                    &DaemonManagementResponse::Shutdown {
                        stopped: true,
                        running_sessions,
                        message: "agentd stopping".to_string(),
                    },
                )
                .await?;
                let _ = shutdown_tx.send(true);
            }
        }
    }
    Ok(())
}

async fn attach_session(
    state: &AppState,
    session_id: &str,
    reader: &mut BufReader<tokio::net::unix::OwnedReadHalf>,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let (_lease, snapshot, mut output_rx, mut control_rx) =
        match state.attach_session(session_id).await {
            Ok(attached) => attached,
            Err(err) => {
                let response = match ended_session_response(state, session_id).await? {
                    Some(response) => response,
                    None => Response::Error {
                        message: err.to_string(),
                    },
                };
                send_response(writer, &response).await?;
                return Ok(());
            }
        };

    send_response(writer, &Response::Attached { snapshot }).await?;
    let mut final_response = Some(Response::EndOfStream);

    loop {
        tokio::select! {
            output = output_rx.recv() => match output {
                Ok(data) => {
                    send_response(
                        writer,
                        &Response::PtyOutput { data },
                    )
                    .await?
                }
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => {
                    final_response = end_of_attach_response(state, session_id).await?;
                    break;
                }
            },
            control = control_rx.recv() => match control {
                Ok(crate::app::AttachControl::SwitchSession(target_session_id)) => {
                    send_response(
                        writer,
                        &Response::SwitchSession { session_id: target_session_id },
                    )
                    .await?;
                    final_response = None;
                    break;
                }
                Ok(crate::app::AttachControl::Detach) => break,
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => {
                    final_response = end_of_attach_response(state, session_id).await?;
                    break;
                }
            },
            request = read_request(reader) => {
                let Some(request) = request? else {
                    break;
                };
                match request {
                    Request::AttachResize { cols, rows } => {
                        if let Err(err) = state.resize_attached_session(session_id, cols, rows).await {
                            final_response = Some(match ended_session_response(state, session_id).await? {
                                Some(response) => response,
                                None => Response::Error {
                                    message: err.to_string(),
                                },
                            });
                            break;
                        }
                    }
                    Request::AttachInput { data } => {
                        if let Err(err) = state.write_attached_input(session_id, data).await {
                            final_response = Some(match ended_session_response(state, session_id).await? {
                                Some(response) => response,
                                None => Response::Error {
                                    message: err.to_string(),
                                },
                            });
                            break;
                        }
                    }
                    other => {
                        final_response = Some(Response::Error {
                            message: format!("unexpected request during attach: {other:?}"),
                        });
                        break;
                    }
                }
            }
        }
    }

    if let Some(response) = final_response {
        send_response(writer, &response).await?;
    }
    Ok(())
}

async fn end_of_attach_response(state: &AppState, session_id: &str) -> Result<Option<Response>> {
    Ok(match ended_session_response(state, session_id).await? {
        Some(response) => Some(response),
        None => Some(Response::EndOfStream),
    })
}

async fn ended_session_response(state: &AppState, session_id: &str) -> Result<Option<Response>> {
    let session = state.get_session(session_id).await?;
    Ok(session.as_ref().and_then(session_ended_response))
}

fn session_ended_response(session: &SessionRecord) -> Option<Response> {
    match session.status {
        SessionStatus::NeedsInput
        | SessionStatus::Exited
        | SessionStatus::Failed
        | SessionStatus::UnknownRecovered => {
            Some(Response::SessionEnded {
                session_id: session.session_id.clone(),
                status: session.status,
                integration_state: session.integration_state,
                branch: session.branch.clone(),
                worktree: session.worktree.clone(),
                exit_code: session.exit_code,
                error: session.error.clone(),
            })
        }
        SessionStatus::Creating | SessionStatus::Running | SessionStatus::Paused => None,
    }
}

async fn send_response(writer: &mut OwnedWriteHalf, response: &Response) -> Result<()> {
    write_response(writer, response).await
}

async fn stream_logs(
    state: &AppState,
    session_id: &str,
    follow: bool,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let session = state
        .get_session(session_id)
        .await?
        .ok_or_else(|| anyhow!("session `{session_id}` not found"))?;
    let rendered_log_path = state.paths.rendered_log_path(session_id);
    let log_path = if session.agent == "codex" && rendered_log_path.exists() {
        rendered_log_path
    } else {
        state.paths.log_path(session_id)
    };
    if !log_path.exists() {
        bail!("no log file exists for session `{session_id}`");
    }

    let mut position = 0_u64;
    loop {
        let (chunk, next_position) = read_from_offset(&log_path, position)?;
        if !chunk.is_empty() {
            send_response(writer, &Response::LogChunk { data: chunk }).await?;
            position = next_position;
        }

        let session = state.get_session(session_id).await?;
        let is_running = matches!(
            session.as_ref().map(|item| item.status),
            Some(SessionStatus::Running)
        );
        if !follow || !is_running {
            let (remainder, _) = read_from_offset(&log_path, position)?;
            if !remainder.is_empty() {
                send_response(writer, &Response::LogChunk { data: remainder }).await?;
            }
            break;
        }

        tokio::time::sleep(Duration::from_millis(200)).await;
    }

    Ok(())
}

async fn stream_events(
    state: &AppState,
    session_id: &str,
    follow: bool,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let mut last_event_id = None;

    loop {
        let events = state.list_events_since(session_id, last_event_id).await?;
        for event in events {
            last_event_id = Some(event.id);
            send_response(writer, &Response::Event { event }).await?;
        }

        let session = state.get_session(session_id).await?;
        let is_running = matches!(
            session.as_ref().map(|item| item.status),
            Some(SessionStatus::Running)
        );
        if !follow || !is_running {
            let trailing = state.list_events_since(session_id, last_event_id).await?;
            for event in trailing {
                send_response(writer, &Response::Event { event }).await?;
            }
            break;
        }

        tokio::time::sleep(Duration::from_millis(200)).await;
    }

    Ok(())
}

fn read_from_offset(log_path: &camino::Utf8PathBuf, position: u64) -> Result<(String, u64)> {
    let bytes = std::fs::read(log_path.as_std_path())
        .with_context(|| format!("failed to read {}", log_path))?;
    let start = position.min(bytes.len() as u64) as usize;
    let chunk = String::from_utf8_lossy(&bytes[start..]).to_string();
    Ok((chunk, bytes.len() as u64))
}

#[cfg(test)]
mod tests {
    use super::session_ended_response;
    use agentd_shared::{
        protocol::Response,
        session::{
            AttentionLevel, GitSyncStatus, IntegrationState, SessionMode, SessionRecord,
            SessionStatus,
        },
    };
    use chrono::Utc;

    fn session(status: SessionStatus) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            thread_id: Some("thread-demo".to_string()),
            agent: "codex".to_string(),
            model: Some("gpt-5.3-codex".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/workspace".to_string(),
            repo_path: "/tmp/workspace".to_string(),
            repo_name: "workspace".to_string(),
            task: "task".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/task".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status,
            integration_state: IntegrationState::Clean,
            git_sync: GitSyncStatus::Unknown,
            git_status_summary: None,
            has_conflicts: false,
            pid: Some(123),
            exit_code: Some(0),
            error: None,
            attention: AttentionLevel::Info,
            attention_summary: Some("task".to_string()),
            created_at: now,
            updated_at: now,
            exited_at: Some(now),
        }
    }

    #[test]
    fn ended_sessions_map_to_session_ended_response() {
        let response = session_ended_response(&session(SessionStatus::Exited)).unwrap();
        assert_eq!(
            response,
            Response::SessionEnded {
                session_id: "demo".to_string(),
                status: SessionStatus::Exited,
                integration_state: IntegrationState::Clean,
                branch: "agent/task".to_string(),
                worktree: "/tmp/worktree".to_string(),
                exit_code: Some(0),
                error: None,
            }
        );
    }

    #[test]
    fn running_sessions_do_not_map_to_session_ended_response() {
        assert!(session_ended_response(&session(SessionStatus::Running)).is_none());
    }
}
