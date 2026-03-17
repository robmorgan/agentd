use std::{
    io::{Read, Write},
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, bail};
use clap::{ArgAction, Parser, Subcommand};
use crossterm::terminal::{disable_raw_mode, enable_raw_mode};
use serde::Deserialize;
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::{UnixStream, unix::OwnedReadHalf},
    sync::mpsc,
};

mod local;

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{DaemonInfo, PROTOCOL_VERSION, Request, Response, read_response, write_request},
    session::{SessionDiff, SessionRecord, SessionStatus, WorktreeRecord},
};

use crate::local::{LocalStore, normalize_session, print_log_file, remove_session_artifacts};

#[derive(Debug, Parser)]
#[command(name = "agent")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    Create {
        #[arg(long)]
        workspace: PathBuf,
        #[arg(long)]
        task: String,
        #[arg(long)]
        agent: String,
    },
    Kill {
        #[arg(long)]
        rm: bool,
        session_id: String,
    },
    Attach {
        session_id: String,
    },
    SendInput {
        session_id: String,
        #[arg(long)]
        source_session_id: Option<String>,
        #[arg(
            required = true,
            trailing_var_arg = true,
            allow_hyphen_values = true,
            value_name = "DATA"
        )]
        data: Vec<String>,
    },
    Logs {
        session_id: String,
        #[arg(long, action = ArgAction::Set, num_args = 0..=1, default_missing_value = "true", default_value_t = true)]
        follow: bool,
    },
    Events {
        session_id: String,
        #[arg(long, action = ArgAction::Set, num_args = 0..=1, default_missing_value = "true", default_value_t = true)]
        follow: bool,
    },
    Sessions,
    Status {
        session_id: String,
    },
    Diff {
        session_id: String,
    },
    Worktree {
        #[command(subcommand)]
        command: WorktreeCommand,
    },
    Daemon {
        #[command(subcommand)]
        command: DaemonCommand,
    },
}

#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum LegacyResponse {
    DaemonInfo { info: LegacyDaemonInfo },
    Sessions { sessions: Vec<SessionRecord> },
    Error { message: String },
    Ok,
}

#[derive(Debug, Deserialize)]
struct LegacyDaemonInfo {
    daemon_version: String,
    protocol_version: u32,
}

#[derive(Debug, Subcommand)]
enum WorktreeCommand {
    Create { session_id: String },
    Cleanup { session_id: String },
}

#[derive(Debug, Subcommand)]
enum DaemonCommand {
    Info,
    Restart,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;
    ensure_config(&paths)?;
    let execution = resolve_execution_mode(&paths, &cli.command).await?;

    match (cli.command, execution) {
        (
            Command::Create {
                workspace,
                task,
                agent,
            },
            ExecutionMode::Daemon,
        ) => {
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: workspace.to_string_lossy().to_string(),
                    task,
                    agent,
                },
            )
            .await?;

            match response {
                Response::CreateSession { session } => {
                    println!("session_id: {}", session.session_id);
                    println!("base_branch: {}", session.base_branch);
                    println!("branch: {}", session.branch);
                    println!("worktree: {}", session.worktree);
                }
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Create { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Command::Kill { rm, session_id }, ExecutionMode::Daemon) => {
            let response = send_request(
                &paths,
                &Request::KillSession {
                    session_id: session_id.clone(),
                    remove: rm,
                },
            )
            .await?;

            match response {
                Response::KillSession {
                    removed,
                    was_running,
                } => print_kill_result(&session_id, was_running, removed),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Kill { rm, session_id }, ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_kill(&paths, &session_id, rm)?;
        }
        (Command::Attach { session_id }, ExecutionMode::Daemon) => {
            attach_session(&paths, &session_id).await?;
        }
        (Command::Attach { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (
            Command::SendInput {
                session_id,
                source_session_id,
                data,
            },
            ExecutionMode::Daemon,
        ) => {
            let response = send_request(
                &paths,
                &Request::SendInput {
                    session_id,
                    data: data.join(" ").into_bytes(),
                    source_session_id,
                },
            )
            .await?;

            match response {
                Response::InputAccepted => {}
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::SendInput { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Command::Logs { session_id, follow }, ExecutionMode::Daemon) => {
            stream_logs(&paths, &session_id, follow).await?;
        }
        (Command::Logs { session_id, follow }, ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_logs(&paths, &session_id, follow)?;
        }
        (Command::Events { session_id, follow }, ExecutionMode::Daemon) => {
            stream_events(&paths, &session_id, follow).await?;
        }
        (Command::Events { session_id, follow }, ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_events(&paths, &session_id, follow).await?;
        }
        (Command::Sessions, ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::ListSessions).await?;
            match response {
                Response::Sessions { sessions } => print_sessions(&sessions),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Sessions, ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            let store = LocalStore::open(&paths)?;
            let sessions = store
                .list_sessions()?
                .into_iter()
                .map(normalize_session)
                .collect::<Vec<_>>();
            print_sessions(&sessions);
        }
        (Command::Diff { session_id }, ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::DiffSession { session_id }).await?;
            match response {
                Response::Diff { diff } => print_diff(&diff),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Diff { .. }, ExecutionMode::Local(reason)) => {
            bail!(
                "{reason}. `agent diff` requires a compatible daemon; use `agent sessions` and `agent kill` to recover first"
            );
        }
        (Command::Status { session_id }, ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::GetSession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Status { session_id }, ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            let store = LocalStore::open(&paths)?;
            let session = store
                .get_session(&session_id)?
                .map(normalize_session)
                .ok_or_else(|| anyhow::anyhow!("session `{session_id}` not found"))?;
            print_session(&session);
        }
        (Command::Worktree { command }, ExecutionMode::Daemon) => match command {
            WorktreeCommand::Create { session_id } => {
                let response =
                    send_request(&paths, &Request::CreateWorktree { session_id }).await?;
                match response {
                    Response::Worktree { worktree } => print_worktree(&worktree),
                    Response::Error { message } => bail!(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
            WorktreeCommand::Cleanup { session_id } => {
                let response =
                    send_request(&paths, &Request::CleanupWorktree { session_id }).await?;
                match response {
                    Response::Worktree { worktree } => {
                        println!("cleaned up worktree for session {}", worktree.session_id);
                        print_worktree(&worktree);
                    }
                    Response::Error { message } => bail!(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
        },
        (Command::Worktree { .. }, ExecutionMode::Local(reason)) => {
            bail!(
                "{reason}. worktree management requires a compatible daemon or a manual cleanup flow"
            );
        }
        (Command::Daemon { command }, ExecutionMode::Daemon) => match command {
            DaemonCommand::Info => {
                let info = daemon_info(&paths).await?;
                println!("daemon_version: {}", info.daemon_version);
                println!("protocol_version: {}", info.protocol_version);
                println!("client_version: {}", env!("CARGO_PKG_VERSION"));
                println!("expected_protocol_version: {}", PROTOCOL_VERSION);
            }
            DaemonCommand::Restart => {
                restart_daemon(&paths).await?;
                let info = daemon_info(&paths).await?;
                println!("daemon_version: {}", info.daemon_version);
                println!("protocol_version: {}", info.protocol_version);
            }
        },
        (Command::Daemon { .. }, ExecutionMode::Local(reason)) => {
            bail!("{reason}. daemon management requires a compatible daemon");
        }
    }

    Ok(())
}

enum ExecutionMode {
    Daemon,
    Local(String),
}

async fn resolve_execution_mode(paths: &AppPaths, command: &Command) -> Result<ExecutionMode> {
    if command_supports_local_mode(command) {
        if let Some(reason) = degraded_mode_reason(paths).await? {
            return Ok(ExecutionMode::Local(reason));
        }
        return Ok(ExecutionMode::Daemon);
    }

    ensure_daemon(paths).await?;
    Ok(ExecutionMode::Daemon)
}

fn command_supports_local_mode(command: &Command) -> bool {
    matches!(
        command,
        Command::Kill { .. }
            | Command::Logs { .. }
            | Command::Events { .. }
            | Command::Sessions
            | Command::Status { .. }
    )
}

async fn degraded_mode_reason(paths: &AppPaths) -> Result<Option<String>> {
    match try_connect(paths).await {
        Ok(_) => match daemon_info(paths).await {
            Ok(info) if info.protocol_version == PROTOCOL_VERSION => Ok(None),
            Ok(info) => Ok(Some(format!(
                "agentd protocol version {} is incompatible with agent protocol version {}",
                info.protocol_version, PROTOCOL_VERSION
            ))),
            Err(err) => Ok(Some(format!("agentd could not be queried: {err}"))),
        },
        Err(_) => {
            if spawn_daemon(paths).await.is_ok() && ensure_compatible_daemon(paths).await.is_ok() {
                return Ok(None);
            }
            Ok(Some("agentd is unavailable".to_string()))
        }
    }
}

fn print_degraded_notice(reason: &str) {
    eprintln!(
        "agent: {reason}; using local degraded mode for metadata/log/session cleanup commands"
    );
}

fn bail_live_command(reason: &str) -> Result<()> {
    bail!(
        "{reason}. this command needs a compatible daemon with a live PTY; use `agent sessions` and `agent kill` first"
    )
}

fn ensure_config(paths: &AppPaths) -> Result<()> {
    if !paths.config.exists() {
        Config::write_default(paths)?;
    }
    Ok(())
}

async fn ensure_daemon(paths: &AppPaths) -> Result<()> {
    if try_connect(paths).await.is_ok() {
        ensure_compatible_daemon(paths).await?;
        return Ok(());
    }

    spawn_daemon(paths).await?;
    ensure_compatible_daemon(paths).await
}

async fn spawn_daemon(paths: &AppPaths) -> Result<()> {
    let current_exe = std::env::current_exe().context("failed to resolve current executable")?;
    let daemon_exe = current_exe
        .parent()
        .map(|path| path.join("agentd"))
        .context("failed to resolve agentd executable path")?;

    std::process::Command::new(daemon_exe)
        .arg("serve")
        .arg("--daemonize")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .context("failed to start agentd")?;

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if try_connect(paths).await.is_ok() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out waiting for agentd to start");
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn ensure_compatible_daemon(paths: &AppPaths) -> Result<()> {
    match daemon_info(paths).await {
        Ok(info) if info.protocol_version == PROTOCOL_VERSION => Ok(()),
        Ok(info) => restart_incompatible_daemon(paths, Some(info)).await,
        Err(_) => restart_incompatible_daemon(paths, None).await,
    }
}

async fn restart_incompatible_daemon(paths: &AppPaths, info: Option<DaemonInfo>) -> Result<()> {
    if daemon_has_running_sessions(paths).await? {
        let daemon_version = info
            .map(|value| value.daemon_version)
            .unwrap_or_else(|| "legacy".to_string());
        bail!(
            "agentd `{daemon_version}` is incompatible with agent `{}` and cannot be restarted while sessions are running",
            env!("CARGO_PKG_VERSION")
        );
    }

    shutdown_or_kill_daemon(paths).await?;
    spawn_daemon(paths).await?;
    match daemon_info(paths).await {
        Ok(info) if info.protocol_version == PROTOCOL_VERSION => Ok(()),
        Ok(info) => bail!(
            "agentd `{}` still reports incompatible protocol version {}",
            info.daemon_version,
            info.protocol_version
        ),
        Err(err) => Err(err),
    }
}

async fn daemon_info(paths: &AppPaths) -> Result<DaemonInfo> {
    let binary_result = tokio::time::timeout(
        Duration::from_millis(250),
        send_request_no_bootstrap(paths, &Request::GetDaemonInfo),
    )
    .await;
    match binary_result {
        Ok(Ok(Response::DaemonInfo { info })) => Ok(info),
        Ok(Ok(Response::Error { message })) => bail!(message),
        Ok(Ok(other)) => bail!("unexpected response: {:?}", other),
        Ok(Err(_)) | Err(_) => legacy_daemon_info(paths).await,
    }
}

async fn daemon_has_running_sessions(paths: &AppPaths) -> Result<bool> {
    let binary_result = tokio::time::timeout(
        Duration::from_millis(250),
        send_request_no_bootstrap(paths, &Request::ListSessions),
    )
    .await;
    match binary_result {
        Ok(Ok(Response::Sessions { sessions })) => Ok(sessions
            .iter()
            .any(|session| session.status_string() == "running")),
        Ok(Ok(Response::Error { message })) => bail!(message),
        Ok(Ok(other)) => bail!("unexpected response: {:?}", other),
        Ok(Err(_)) | Err(_) => legacy_daemon_has_running_sessions(paths).await,
    }
}

async fn shutdown_or_kill_daemon(paths: &AppPaths) -> Result<()> {
    let shutdown_result = tokio::time::timeout(
        Duration::from_millis(250),
        send_request_no_bootstrap(paths, &Request::ShutdownDaemon),
    )
    .await;
    match shutdown_result {
        Ok(Ok(Response::Ok)) => {}
        Ok(Ok(Response::Error { message })) => bail!(message),
        Ok(Ok(_)) => {}
        Ok(Err(_)) | Err(_) => {
            if legacy_shutdown_daemon(paths).await.is_err() {
                kill_daemon_from_pid_file(paths)?;
            }
        }
    }
    wait_for_daemon_stop(paths).await
}

fn kill_daemon_from_pid_file(paths: &AppPaths) -> Result<()> {
    let pid = std::fs::read_to_string(paths.pid_file.as_std_path())
        .with_context(|| format!("failed to read {}", paths.pid_file))?;
    let pid: i32 = pid.trim().parse().context("invalid daemon pid file")?;
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(pid),
        Some(nix::sys::signal::Signal::SIGTERM),
    )
    .context("failed to terminate agentd")?;
    Ok(())
}

async fn wait_for_daemon_stop(paths: &AppPaths) -> Result<()> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if try_connect(paths).await.is_err() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out waiting for agentd to stop");
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn restart_daemon(paths: &AppPaths) -> Result<()> {
    match try_connect(paths).await {
        Ok(_) => {
            if daemon_has_running_sessions(paths).await? {
                bail!("cannot restart agentd while sessions are running");
            }
            shutdown_or_kill_daemon(paths).await?;
            spawn_daemon(paths).await?;
            ensure_compatible_daemon(paths).await
        }
        Err(_) => {
            spawn_daemon(paths).await?;
            ensure_compatible_daemon(paths).await
        }
    }
}

async fn send_request(paths: &AppPaths, request: &Request) -> Result<Response> {
    send_request_no_bootstrap(paths, request).await
}

async fn send_request_no_bootstrap(paths: &AppPaths, request: &Request) -> Result<Response> {
    let mut stream = try_connect(paths).await?;
    write_request(&mut stream, request).await?;

    let mut reader = BufReader::new(stream);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the connection");
    };
    Ok(response)
}

async fn legacy_request(paths: &AppPaths, payload: &str) -> Result<LegacyResponse> {
    let mut stream = try_connect(paths).await?;
    stream.write_all(payload.as_bytes()).await?;
    stream.write_all(b"\n").await?;
    stream.flush().await?;

    let mut lines = BufReader::new(stream).lines();
    let Some(line) = lines.next_line().await? else {
        bail!("agentd closed the connection");
    };
    Ok(serde_json::from_str(&line)?)
}

async fn legacy_daemon_info(paths: &AppPaths) -> Result<DaemonInfo> {
    match legacy_request(paths, r#"{"type":"get_daemon_info"}"#).await? {
        LegacyResponse::DaemonInfo { info } => Ok(DaemonInfo {
            daemon_version: info.daemon_version,
            protocol_version: u16::try_from(info.protocol_version)
                .context("legacy daemon reported invalid protocol version")?,
        }),
        LegacyResponse::Error { message } => bail!(message),
        other => bail!("unexpected legacy response: {:?}", other),
    }
}

async fn legacy_daemon_has_running_sessions(paths: &AppPaths) -> Result<bool> {
    match legacy_request(paths, r#"{"type":"list_sessions"}"#).await? {
        LegacyResponse::Sessions { sessions } => Ok(sessions
            .iter()
            .any(|session| session.status_string() == "running")),
        LegacyResponse::Error { message } => bail!(message),
        other => bail!("unexpected legacy response: {:?}", other),
    }
}

async fn legacy_shutdown_daemon(paths: &AppPaths) -> Result<()> {
    match legacy_request(paths, r#"{"type":"shutdown_daemon"}"#).await? {
        LegacyResponse::Ok => Ok(()),
        LegacyResponse::Error { message } => bail!(message),
        other => bail!("unexpected legacy response: {:?}", other),
    }
}

async fn stream_logs(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    let mut stream = try_connect(paths).await?;
    write_request(
        &mut stream,
        &Request::StreamLogs {
            session_id: session_id.to_string(),
            follow,
        },
    )
    .await?;

    let mut reader = BufReader::new(stream);
    while let Some(response) = read_response(&mut reader).await? {
        match response {
            Response::LogChunk { data } => {
                print!("{data}");
            }
            Response::EndOfStream => break,
            Response::Error { message } => bail!(message),
            other => bail!("unexpected response: {:?}", other),
        }
    }
    Ok(())
}

async fn stream_events(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    let mut stream = try_connect(paths).await?;
    write_request(
        &mut stream,
        &Request::StreamEvents {
            session_id: session_id.to_string(),
            follow,
        },
    )
    .await?;

    let mut reader = BufReader::new(stream);
    while let Some(response) = read_response(&mut reader).await? {
        match response {
            Response::Event { event } => {
                println!("{}", serde_json::to_string(&event)?);
            }
            Response::EndOfStream => break,
            Response::Error { message } => bail!(message),
            other => bail!("unexpected response: {:?}", other),
        }
    }
    Ok(())
}

fn local_logs(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    print_log_file(paths, session_id, follow)
}

async fn local_events(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    let store = LocalStore::open(paths)?;
    store
        .get_session(session_id)?
        .ok_or_else(|| anyhow::anyhow!("session `{session_id}` not found"))?;
    let mut after_id = None;

    loop {
        let events = store.list_events_since(session_id, after_id)?;
        for event in &events {
            println!("{}", serde_json::to_string(event)?);
        }
        if let Some(last) = events.last() {
            after_id = Some(last.id);
        }
        if !follow {
            return Ok(());
        }
        let session_running = store
            .get_session(session_id)?
            .map(|session| local::session_is_running(&session))
            .unwrap_or(false);
        if !session_running && events.is_empty() {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
}

fn local_kill(paths: &AppPaths, session_id: &str, remove: bool) -> Result<()> {
    let store = LocalStore::open(paths)?;
    let session = store
        .get_session(session_id)?
        .ok_or_else(|| anyhow::anyhow!("session `{session_id}` not found"))?;
    let was_running = local::session_is_running(&session);

    if !was_running && !remove {
        if session.status == SessionStatus::Running {
            store.mark_unknown_recovered(session_id)?;
        }
        bail!("session `{session_id}` is not running");
    }

    if was_running {
        local::terminate_session_process(session_id, session.pid)?;
        store.mark_exited(session_id, None)?;
    } else if session.status == SessionStatus::Running {
        store.mark_unknown_recovered(session_id)?;
    }

    if remove {
        remove_session_artifacts(paths, &session)?;
        store.delete_session(session_id)?;
    }

    print_kill_result(session_id, was_running, remove);
    Ok(())
}

async fn attach_session(paths: &AppPaths, session_id: &str) -> Result<()> {
    let mut stream = try_connect(paths).await?;
    write_request(
        &mut stream,
        &Request::AttachSession {
            session_id: session_id.to_string(),
        },
    )
    .await?;

    let (read_half, mut write_half) = stream.into_split();
    let mut reader = BufReader::new(read_half);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the connection");
    };

    match response {
        Response::Attached { snapshot } => {
            write_attach_bytes(&snapshot)?;
        }
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }

    eprintln!("attached to {session_id}; detach with Ctrl-]");
    let _raw_mode = RawModeGuard::new()?;
    let (stdin_tx, mut stdin_rx) = mpsc::unbounded_channel();

    let stdin_task = tokio::task::spawn_blocking(move || read_attach_stdin(stdin_tx));
    let mut output_task = tokio::spawn(async move { stream_attach_output(reader).await });

    loop {
        tokio::select! {
            event = stdin_rx.recv() => match event {
                Some(AttachInput::Data(data)) => {
                    write_request(&mut write_half, &Request::AttachInput { data }).await?;
                }
                Some(AttachInput::Detach) | None => break,
            },
            result = &mut output_task => {
                result??;
                break;
            }
        }
    }

    drop(write_half);
    stdin_task.abort();
    Ok(())
}

async fn try_connect(paths: &AppPaths) -> Result<UnixStream> {
    UnixStream::connect(paths.socket.as_std_path())
        .await
        .with_context(|| format!("failed to connect to {}", paths.socket))
}

fn print_sessions(sessions: &[SessionRecord]) {
    for session in sessions {
        println!(
            "{}\t{}\t{}\t{}",
            session.session_id,
            session.agent,
            session.status_string(),
            session.branch
        );
    }
}

fn print_session(session: &SessionRecord) {
    println!("session_id: {}", session.session_id);
    println!("agent: {}", session.agent);
    println!("status: {}", session.status_string());
    println!("repo_path: {}", session.repo_path);
    println!("workspace: {}", session.workspace);
    println!("task: {}", session.task);
    println!("base_branch: {}", session.base_branch);
    println!("branch: {}", session.branch);
    println!("worktree: {}", session.worktree);
    if let Some(pid) = session.pid {
        println!("pid: {pid}");
    }
    if let Some(exit_code) = session.exit_code {
        println!("exit_code: {exit_code}");
    }
    if let Some(error) = &session.error {
        println!("error: {error}");
    }
}

fn print_worktree(worktree: &WorktreeRecord) {
    println!("session_id: {}", worktree.session_id);
    println!("repo_path: {}", worktree.repo_path);
    println!("base_branch: {}", worktree.base_branch);
    println!("branch: {}", worktree.branch);
    println!("worktree: {}", worktree.worktree);
}

fn print_diff(diff: &SessionDiff) {
    println!("session_id: {}", diff.session_id);
    println!("base_branch: {}", diff.base_branch);
    println!("branch: {}", diff.branch);
    println!("worktree: {}", diff.worktree);
    println!();
    print!("{}", diff.diff);
}

fn print_kill_result(session_id: &str, was_running: bool, removed: bool) {
    if was_running {
        println!("terminated session {session_id}");
    }
    if removed {
        println!("removed session {session_id}");
    }
}

async fn stream_attach_output(mut reader: BufReader<OwnedReadHalf>) -> Result<()> {
    while let Some(response) = read_response(&mut reader).await? {
        match response {
            Response::PtyOutput { data } => write_attach_bytes(&data)?,
            Response::EndOfStream => break,
            Response::Error { message } => bail!(message),
            other => bail!("unexpected response: {:?}", other),
        }
    }
    Ok(())
}

fn read_attach_stdin(tx: mpsc::UnboundedSender<AttachInput>) -> Result<()> {
    let mut stdin = std::io::stdin();
    let mut buffer = [0_u8; 1024];
    loop {
        let count = stdin.read(&mut buffer)?;
        if count == 0 {
            let _ = tx.send(AttachInput::Detach);
            break;
        }

        let bytes = &buffer[..count];
        if let Some(index) = bytes.iter().position(|byte| *byte == DETACH_BYTE) {
            if index > 0 {
                let data = bytes[..index].to_vec();
                let _ = tx.send(AttachInput::Data(data));
            }
            let _ = tx.send(AttachInput::Detach);
            break;
        }

        let data = bytes.to_vec();
        if tx.send(AttachInput::Data(data)).is_err() {
            break;
        }
    }
    Ok(())
}

const DETACH_BYTE: u8 = 29;

enum AttachInput {
    Data(Vec<u8>),
    Detach,
}

struct RawModeGuard;

impl RawModeGuard {
    fn new() -> Result<Self> {
        enable_raw_mode().context("failed to enable raw terminal mode")?;
        Ok(Self)
    }
}

impl Drop for RawModeGuard {
    fn drop(&mut self) {
        let _ = disable_raw_mode();
    }
}

trait StatusString {
    fn status_string(&self) -> &'static str;
}

impl StatusString for SessionRecord {
    fn status_string(&self) -> &'static str {
        match self.status {
            SessionStatus::Creating => "creating",
            SessionStatus::Running => "running",
            SessionStatus::Exited => "exited",
            SessionStatus::Failed => "failed",
            SessionStatus::UnknownRecovered => "unknown_recovered",
        }
    }
}

fn write_attach_bytes(data: &[u8]) -> Result<()> {
    let mut stdout = std::io::stdout();
    stdout.write_all(data)?;
    stdout.flush()?;
    Ok(())
}
