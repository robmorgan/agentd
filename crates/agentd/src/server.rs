use std::{io, path::Path};

use anyhow::{Context, Result, bail};
use tokio::{
    io::{AsyncWriteExt, BufReader},
    net::{UnixListener, UnixStream, unix::OwnedWriteHalf},
    sync::watch,
};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{
        DaemonInfo, DaemonManagementRequest, DaemonManagementResponse, DaemonManagementStatus,
        IncomingRequest, PROTOCOL_VERSION, Request, Response, read_incoming_request, read_response,
        write_daemon_management_response, write_request, write_response,
    },
    session::AttachmentKind,
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
            name,
            agent,
            model,
            integration_policy,
        }) => match state.create_session(workspace, name, agent, model, integration_policy).await {
            Ok(session) => send_response(&mut writer, &Response::CreateSession { session }).await?,
            Err(err) => {
                send_response(&mut writer, &Response::Error { message: err.to_string() }).await?
            }
        },
        IncomingRequest::Standard(Request::CreateWorktree { session_id }) => {
            match state.create_worktree(&session_id).await {
                Ok(worktree) => {
                    send_response(&mut writer, &Response::Worktree { worktree }).await?
                }
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
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
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
                        .await?
                }
            }
        }
        IncomingRequest::Standard(Request::KillSession { session_id, remove }) => {
            match state.kill_session(&session_id, remove).await {
                Ok((removed, was_running)) => {
                    send_response(&mut writer, &Response::KillSession { removed, was_running })
                        .await?
                }
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
                        .await?
                }
            }
        }
        IncomingRequest::Standard(Request::ResolveSessionRuntime { session_id }) => {
            match runtime_socket_for(&state, &session_id).await {
                Ok(socket_path) => {
                    send_response(&mut writer, &Response::RuntimeEndpoint { socket_path }).await?
                }
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
                        .await?
                }
            }
        }
        IncomingRequest::Standard(Request::AttachSession {
            session_id,
            kind,
            cols,
            rows,
            pixel_width,
            pixel_height,
        }) => {
            attach_session(
                &state.paths,
                &session_id,
                kind,
                cols,
                rows,
                pixel_width,
                pixel_height,
                &mut reader,
                &mut writer,
            )
            .await?;
        }
        IncomingRequest::Standard(Request::AttachSnapshot) => {
            send_response(
                &mut writer,
                &Response::Error {
                    message: "attach snapshot requests are only valid during an active attach"
                        .to_string(),
                },
            )
            .await?;
        }
        IncomingRequest::Standard(Request::DetachSession { session_id, all }) => {
            proxy_runtime_request(
                &state.paths,
                &session_id,
                &Request::DetachSession { session_id: session_id.clone(), all },
                &mut writer,
            )
            .await?;
        }
        IncomingRequest::Standard(Request::DetachAttachment { session_id, attach_id }) => {
            proxy_runtime_request(
                &state.paths,
                &session_id,
                &Request::DetachAttachment { session_id: session_id.clone(), attach_id },
                &mut writer,
            )
            .await?;
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
        IncomingRequest::Standard(Request::SendInput { session_id, data, source_session_id }) => {
            let request =
                Request::SendInput { session_id: session_id.clone(), data, source_session_id };
            proxy_runtime_request(&state.paths, &session_id, &request, &mut writer).await?;
        }
        IncomingRequest::Standard(Request::ApplySession { session_id }) => {
            match state.apply_session(&session_id).await {
                Ok(session) => send_response(&mut writer, &Response::Session { session }).await?,
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
                        .await?
                }
            }
        }
        IncomingRequest::Standard(Request::DiscardSession { session_id, force }) => {
            match state.discard_session(&session_id, force).await {
                Ok(session) => send_response(&mut writer, &Response::Session { session }).await?,
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
                        .await?
                }
            }
        }
        IncomingRequest::Standard(Request::SwitchAttachedSession {
            source_session_id,
            target_session_id,
        }) => {
            let _ = (source_session_id, target_session_id);
            send_response(
                &mut writer,
                &Response::Error {
                    message: "shared attach uses client-local switching; reconnect the local client instead".to_string(),
                },
            )
            .await?
        }
        IncomingRequest::Standard(Request::DiffSession { session_id }) => {
            match state.diff_session(&session_id).await {
                Ok(diff) => send_response(&mut writer, &Response::Diff { diff }).await?,
                Err(err) => {
                    send_response(&mut writer, &Response::Error { message: err.to_string() })
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
                        &Response::Error { message: format!("session `{session_id}` not found") },
                    )
                    .await?
                }
            }
        }
        IncomingRequest::Standard(Request::ListSessions) => {
            let sessions = state.list_sessions().await?;
            send_response(&mut writer, &Response::Sessions { sessions }).await?;
        }
        IncomingRequest::Standard(Request::ListAttachments { session_id }) => {
            proxy_runtime_request(
                &state.paths,
                &session_id,
                &Request::ListAttachments { session_id: session_id.clone() },
                &mut writer,
            )
            .await?;
        }
        IncomingRequest::Standard(Request::GetHistory { session_id, vt }) => {
            if connect_runtime_stream(&state.paths, &session_id).await.is_ok() {
                proxy_runtime_request(
                    &state.paths,
                    &session_id,
                    &Request::GetHistory { session_id: session_id.clone(), vt },
                    &mut writer,
                )
                .await?;
            } else {
                match state.get_history(&session_id, vt).await {
                    Ok(data) => send_response(&mut writer, &Response::History { data }).await?,
                    Err(err) => {
                        send_response(&mut writer, &Response::Error { message: err.to_string() })
                            .await?
                    }
                }
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
    paths: &agentd_shared::paths::AppPaths,
    session_id: &str,
    kind: AttachmentKind,
    cols: u16,
    rows: u16,
    pixel_width: u16,
    pixel_height: u16,
    reader: &mut BufReader<tokio::net::unix::OwnedReadHalf>,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let mut runtime = connect_runtime_stream(paths, session_id).await?;
    write_request(
        &mut runtime,
        &Request::AttachSession {
            session_id: session_id.to_string(),
            kind,
            cols,
            rows,
            pixel_width,
            pixel_height,
        },
    )
    .await?;
    let (mut runtime_reader, mut runtime_writer) = runtime.into_split();
    let client_to_runtime = tokio::io::copy(reader, &mut runtime_writer);
    let runtime_to_client = tokio::io::copy(&mut runtime_reader, writer);
    let _ = tokio::try_join!(client_to_runtime, runtime_to_client)?;
    let _ = writer.shutdown().await;
    Ok(())
}

async fn runtime_socket_for(state: &AppState, session_id: &str) -> Result<String> {
    let session = state
        .get_session(session_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("session `{session_id}` not found"))?;
    if session.status != SessionStatus::Running {
        bail!("session `{session_id}` is not running");
    }
    let socket_path = state.paths.session_socket_path(session_id);
    if !socket_path.exists() {
        bail!("session `{session_id}` does not have a live runtime socket");
    }
    Ok(socket_path.to_string())
}

async fn connect_runtime_stream(
    paths: &agentd_shared::paths::AppPaths,
    session_id: &str,
) -> Result<UnixStream> {
    UnixStream::connect(paths.session_socket_path(session_id).as_std_path())
        .await
        .with_context(|| format!("failed to connect to runtime for session `{session_id}`"))
}

async fn proxy_runtime_request(
    paths: &agentd_shared::paths::AppPaths,
    session_id: &str,
    request: &Request,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let mut stream = connect_runtime_stream(paths, session_id).await?;
    write_request(&mut stream, request).await?;
    let mut reader = BufReader::new(stream);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("session runtime closed the connection");
    };
    send_response(writer, &response).await
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
        SessionStatus::Exited | SessionStatus::Failed | SessionStatus::UnknownRecovered => {
            Some(Response::SessionEnded {
                session_id: session.session_id.clone(),
                status: session.status,
                apply_state: session.apply_state,
                has_commits: session.has_commits,
                branch: session.branch.clone(),
                worktree: session.worktree.clone(),
                exit_code: session.exit_code,
                error: session.error.clone(),
            })
        }
        SessionStatus::Creating | SessionStatus::Running => None,
    }
}

async fn send_response(writer: &mut OwnedWriteHalf, response: &Response) -> Result<()> {
    write_response(writer, response).await
}

#[cfg(test)]
mod tests {
    use super::session_ended_response;
    use agentd_shared::{
        protocol::Response,
        session::{
            ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord,
            SessionStatus,
        },
    };
    use chrono::Utc;

    fn session(status: SessionStatus) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/workspace".to_string(),
            repo_path: "/tmp/workspace".to_string(),
            repo_name: "workspace".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/task".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status,
            integration_policy: IntegrationPolicy::AutoApplySafe,
            apply_state: ApplyState::Idle,
            dirty_count: 0,
            ahead_count: 0,
            has_commits: false,
            has_pending_changes: false,
            worker_pid: Some(123),
            agent_pid: Some(456),
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
                apply_state: ApplyState::Idle,
                has_commits: false,
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
