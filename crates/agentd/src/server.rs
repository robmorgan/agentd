use std::{io, path::Path, time::Duration};

use anyhow::{Context, Result, bail};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::{UnixListener, UnixStream, unix::OwnedWriteHalf},
};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{Request, Response},
    session::SessionStatus,
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

    let listener = UnixListener::bind(paths.socket.as_std_path())
        .context("failed to bind agentd socket")?;

    loop {
        let (stream, _) = listener.accept().await?;
        let state = state.clone();
        tokio::spawn(async move {
            if let Err(err) = handle_connection(state, stream).await {
                eprintln!("connection error: {err:#}");
            }
        });
    }
}

async fn handle_connection(state: AppState, stream: UnixStream) -> Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut lines = BufReader::new(reader).lines();
    let Some(line) = lines.next_line().await? else {
        return Ok(());
    };

    let request: Request = serde_json::from_str(&line).context("invalid request payload")?;
    match request {
        Request::CreateSession {
            workspace,
            task,
            agent,
        } => {
            match state.create_session(workspace, task, agent).await {
                Ok(session) => send_response(&mut writer, &Response::CreateSession { session }).await?,
                Err(err) => send_response(&mut writer, &Response::Error { message: err.to_string() }).await?,
            }
        }
        Request::CreateWorktree { session_id } => {
            match state.create_worktree(&session_id).await {
                Ok(worktree) => send_response(&mut writer, &Response::Worktree { worktree }).await?,
                Err(err) => send_response(&mut writer, &Response::Error { message: err.to_string() }).await?,
            }
        }
        Request::CleanupWorktree { session_id } => {
            match state.cleanup_worktree(&session_id).await {
                Ok(worktree) => send_response(&mut writer, &Response::Worktree { worktree }).await?,
                Err(err) => send_response(&mut writer, &Response::Error { message: err.to_string() }).await?,
            }
        }
        Request::DiffSession { session_id } => {
            match state.diff_session(&session_id).await {
                Ok(diff) => send_response(&mut writer, &Response::Diff { diff }).await?,
                Err(err) => send_response(&mut writer, &Response::Error { message: err.to_string() }).await?,
            }
        }
        Request::GetSession { session_id } => match state.get_session(&session_id).await? {
            Some(session) => send_response(&mut writer, &Response::Session { session }).await?,
            None => send_response(&mut writer, &Response::Error { message: format!("session `{session_id}` not found") }).await?,
        },
        Request::ListSessions => {
            let sessions = state.list_sessions().await?;
            send_response(&mut writer, &Response::Sessions { sessions }).await?;
        }
        Request::StreamLogs { session_id, follow } => {
            if let Err(err) = stream_logs(&state, &session_id, follow, &mut writer).await {
                send_response(&mut writer, &Response::Error { message: err.to_string() }).await?;
            } else {
                send_response(&mut writer, &Response::EndOfStream).await?;
            }
        }
    }

    Ok(())
}

async fn send_response(
    writer: &mut OwnedWriteHalf,
    response: &Response,
) -> Result<()> {
    let payload = serde_json::to_vec(response)?;
    writer.write_all(&payload).await?;
    writer.write_all(b"\n").await?;
    writer.flush().await?;
    Ok(())
}

async fn stream_logs(
    state: &AppState,
    session_id: &str,
    follow: bool,
    writer: &mut OwnedWriteHalf,
) -> Result<()> {
    let log_path = state.paths.log_path(session_id);
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
        let is_running = matches!(session.as_ref().map(|item| item.status), Some(SessionStatus::Running));
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

fn read_from_offset(log_path: &camino::Utf8PathBuf, position: u64) -> Result<(String, u64)> {
    let bytes = std::fs::read(log_path.as_std_path())
        .with_context(|| format!("failed to read {}", log_path))?;
    let start = position.min(bytes.len() as u64) as usize;
    let chunk = String::from_utf8_lossy(&bytes[start..]).to_string();
    Ok((chunk, bytes.len() as u64))
}
