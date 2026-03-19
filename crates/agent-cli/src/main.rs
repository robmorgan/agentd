use std::{
    fs,
    io::{IsTerminal, Write},
    mem::MaybeUninit,
    os::fd::AsRawFd,
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, bail};
use clap::{ArgAction, Parser, Subcommand};
use crossterm::{
    cursor::{Hide, MoveTo, Show},
    event::{self, Event, KeyCode, KeyEventKind, KeyModifiers, MouseEventKind},
    execute,
    style::{Attribute as CrosAttribute, Color as CrosColor, Stylize},
    terminal::{
        Clear, ClearType, EnterAlternateScreen, LeaveAlternateScreen, disable_raw_mode,
        enable_raw_mode,
    },
    event::{DisableMouseCapture, EnableMouseCapture},
};
use libc::{
    _POSIX_VDISABLE, TCSAFLUSH, TCSANOW, VLNEXT, VMIN, VQUIT, VTIME, cfmakeraw, tcgetattr,
    tcsetattr, termios,
};
use ratatui::{
    Frame, Terminal,
    backend::CrosstermBackend,
    layout::{Alignment, Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear as WidgetClear, List, ListItem, Paragraph, Wrap},
};
use serde_json::Value;
use tokio::{io::BufReader, net::UnixStream, sync::mpsc};

mod local;

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{
        DaemonInfo, DaemonManagementRequest, DaemonManagementResponse, DaemonManagementStatus,
        PROTOCOL_VERSION, Request, Response, read_daemon_management_response, read_response,
        write_daemon_management_request, write_request,
    },
    session::{
        AttentionLevel, IntegrationState, SessionDiff, SessionMode, SessionRecord,
        SessionStatus, WorktreeRecord,
    },
};

use crate::local::{LocalStore, normalize_session, print_log_file, remove_session_artifacts};

const AGENTD_ATTACH_RESTORE_SEQUENCE: &[u8] =
    b"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[?1049l\x1b[<u\x1b[?25h";
const AGENTD_ATTACH_CLEAR_SEQUENCE: &[u8] = b"\x1b[2J\x1b[H";
const CODEX_COMPOSER_BG: Color = Color::Rgb(11, 15, 20);
const CODEX_MODELS: &[&str] = &[
    "gpt-5.4-codex",
    "gpt-5.3-codex",
    "gpt-5.1-codex-max",
    "gpt-5.1-codex",
    "gpt-5.1-codex-mini",
    "gpt-5-codex",
];

#[derive(Debug, Parser)]
#[command(name = "agent")]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Debug, Subcommand)]
enum Command {
    Focus {
        session_id: Option<String>,
    },
    New {
        task: Option<String>,
        #[arg(long)]
        workspace: Option<PathBuf>,
        #[arg(long)]
        agent: Option<String>,
    },
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
    Detach {
        session_id: Option<String>,
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
    Reply {
        session_id: String,
        #[arg(
            required = true,
            trailing_var_arg = true,
            allow_hyphen_values = true,
            value_name = "PROMPT"
        )]
        prompt: Vec<String>,
    },
    Accept {
        session_id: String,
    },
    Discard {
        session_id: String,
        #[arg(long)]
        force: bool,
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
    #[command(visible_alias = "ls", alias = "sessions")]
    List,
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

#[derive(Debug, Subcommand)]
enum WorktreeCommand {
    Create { session_id: String },
    Cleanup { session_id: String },
}

#[derive(Debug, Subcommand)]
enum DaemonCommand {
    Info,
    Restart {
        #[arg(long)]
        force: bool,
    },
    Upgrade,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;
    ensure_config(&paths)?;
    let command = cli.command.unwrap_or(Command::Focus { session_id: None });
    let execution = resolve_execution_mode(&paths, &command).await?;

    match (command, execution) {
        (Command::Focus { session_id }, ExecutionMode::Daemon) => {
            if let Some(session_id) = session_id {
                focus_session(&paths, &session_id).await?;
            } else {
                focus_dashboard(&paths).await?;
            }
        }
        (Command::Focus { .. }, ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent focus` requires a compatible daemon");
        }
        (
            Command::New {
                task,
                workspace,
                agent,
            },
            ExecutionMode::Daemon,
        ) => {
            let options = resolve_new_session_options(workspace, task, agent)?;
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: options.workspace.to_string_lossy().to_string(),
                    task: options.task,
                    agent: options.agent,
                    model: None,
                    mode: SessionMode::Execute,
                },
            )
            .await?;

            match response {
                Response::CreateSession { session } => {
                    attach_session(&paths, &session.session_id).await?;
                }
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::New { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
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
                    model: None,
                    mode: SessionMode::Execute,
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
        (Command::Detach { session_id }, ExecutionMode::Daemon) => {
            let session_id = resolve_detach_session_id(session_id)?;
            let response = send_request(
                &paths,
                &Request::DetachSession {
                    session_id: session_id.clone(),
                },
            )
            .await?;

            match response {
                Response::Ok => println!("detached session {session_id}"),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Detach { .. }, ExecutionMode::Local(reason)) => {
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
        (Command::Reply { session_id, prompt }, ExecutionMode::Daemon) => {
            let session = reply_session(&paths, &session_id, &prompt.join(" ")).await?;
            print_session(&session);
        }
        (Command::Reply { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Command::Accept { session_id }, ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::ApplySession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Accept { .. }, ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Command::Discard { session_id, force }, ExecutionMode::Daemon) => {
            let response =
                send_request(&paths, &Request::DiscardSession { session_id, force }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Command::Discard { .. }, ExecutionMode::Local(reason)) => {
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
        (Command::List, ExecutionMode::Daemon) => {
            let sessions = daemon_list_sessions(&paths).await?;
            if !maybe_switch_attached_session(&paths, &sessions).await? {
                print_sessions(&sessions);
            }
        }
        (Command::List, ExecutionMode::Local(reason)) => {
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
                let status = daemon_management_status(&paths).await?;
                print_daemon_management_status(&status);
            }
            DaemonCommand::Restart { force } => {
                restart_daemon(&paths, force).await?;
                let status = daemon_management_status(&paths).await?;
                print_daemon_management_status(&status);
            }
            DaemonCommand::Upgrade => {
                upgrade_daemon(&paths).await?;
            }
        },
        (Command::Daemon { .. }, ExecutionMode::Local(reason)) => {
            bail!("{reason}. daemon management requires a reachable daemon");
        }
    }

    Ok(())
}

enum ExecutionMode {
    Daemon,
    Local(String),
}

struct NewSessionOptions {
    workspace: PathBuf,
    task: String,
    agent: String,
}

async fn resolve_execution_mode(paths: &AppPaths, command: &Command) -> Result<ExecutionMode> {
    if matches!(
        command,
        Command::Daemon {
            command: DaemonCommand::Upgrade
        }
    ) {
        return Ok(ExecutionMode::Daemon);
    }

    if matches!(command, Command::Daemon { .. }) {
        if try_connect(paths).await.is_err() {
            spawn_daemon(paths).await?;
        }
        return Ok(ExecutionMode::Daemon);
    }

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
            | Command::List
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

fn resolve_new_session_options(
    workspace: Option<PathBuf>,
    task: Option<String>,
    agent: Option<String>,
) -> Result<NewSessionOptions> {
    Ok(NewSessionOptions {
        workspace: match workspace {
            Some(workspace) => workspace,
            None => std::env::current_dir().context("failed to resolve current directory")?,
        },
        task: task.unwrap_or_default(),
        agent: agent.unwrap_or_else(|| "codex".to_string()),
    })
}

fn resolve_detach_session_id(session_id: Option<String>) -> Result<String> {
    match session_id {
        Some(session_id) => Ok(session_id),
        None => std::env::var("AGENTD_SESSION_ID")
            .context("`agent detach` without a session id only works inside a managed session"),
    }
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
    let daemon_exe = daemon_executable()?;

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

fn daemon_executable() -> Result<PathBuf> {
    let current_exe = std::env::current_exe().context("failed to resolve current executable")?;
    current_exe
        .parent()
        .map(|path| path.join("agentd"))
        .context("failed to resolve agentd executable path")
}

async fn ensure_compatible_daemon(paths: &AppPaths) -> Result<()> {
    match daemon_info(paths).await {
        Ok(info) if info.protocol_version == PROTOCOL_VERSION => Ok(()),
        Ok(info) => bail!(
            "agentd `{}` is out of date with agent `{}`; try upgrading the daemon",
            info.daemon_version,
            env!("CARGO_PKG_VERSION")
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
        Ok(Err(err)) => Err(err).context(incompatible_daemon_message()),
        Err(_) => bail!(incompatible_daemon_message()),
    }
}

async fn daemon_management_status(paths: &AppPaths) -> Result<DaemonManagementStatus> {
    let response = tokio::time::timeout(
        Duration::from_millis(250),
        send_daemon_management_request(paths, &DaemonManagementRequest::Status),
    )
    .await;
    match response {
        Ok(Ok(DaemonManagementResponse::Status { status })) => Ok(status),
        Ok(Ok(DaemonManagementResponse::Error { message })) => bail!(message),
        Ok(Ok(other)) => bail!("unexpected daemon management response: {:?}", other),
        Ok(Err(err)) => Err(err).context("daemon management status request failed"),
        Err(_) => bail!("timed out waiting for daemon management status"),
    }
}

async fn request_daemon_shutdown(paths: &AppPaths, force: bool) -> Result<()> {
    let shutdown_result = tokio::time::timeout(
        Duration::from_millis(250),
        send_daemon_management_request(paths, &DaemonManagementRequest::Shutdown { force }),
    )
    .await;
    match shutdown_result {
        Ok(Ok(DaemonManagementResponse::Shutdown {
            stopped,
            running_sessions: _,
            message,
        })) => {
            if !stopped {
                bail!(message);
            }
        }
        Ok(Ok(DaemonManagementResponse::Error { message })) => bail!(message),
        Ok(Ok(other)) => bail!("unexpected daemon management response: {:?}", other),
        Ok(Err(err)) => return Err(err).context("daemon management shutdown request failed"),
        Err(_) => bail!("timed out waiting for daemon management shutdown"),
    }
    wait_for_daemon_stop(paths).await
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

async fn restart_daemon(paths: &AppPaths, force: bool) -> Result<()> {
    match try_connect(paths).await {
        Ok(_) => {
            let status = daemon_management_status(paths).await?;
            if status.running_sessions && !force {
                bail!("cannot restart agentd while sessions are running");
            }
            request_daemon_shutdown(paths, force).await?;
            spawn_daemon(paths).await?;
            ensure_compatible_daemon(paths).await
        }
        Err(_) => {
            spawn_daemon(paths).await?;
            ensure_compatible_daemon(paths).await
        }
    }
}

async fn upgrade_daemon(paths: &AppPaths) -> Result<()> {
    let current_status = daemon_management_status(paths).await?;
    println!("✓ Current daemon `{}`", current_status.daemon_version);
    println!("✓ Current client `{}`", env!("CARGO_PKG_VERSION"));
    println!("✓ Restarting daemon to upgrade");

    let status = std::process::Command::new(daemon_executable()?)
        .arg("upgrade")
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .context("failed to run agentd upgrade")?;

    if !status.success() {
        match status.code() {
            Some(code) => bail!("agentd upgrade exited with status {code}"),
            None => bail!("agentd upgrade terminated by signal"),
        }
    }

    ensure_compatible_daemon(paths).await?;

    let upgraded_status = daemon_management_status(paths).await?;
    println!("✓ Upgraded daemon `{}`", upgraded_status.daemon_version);
    Ok(())
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

fn incompatible_daemon_message() -> &'static str {
    "agentd is out of date with the client; try upgrading the daemon"
}

async fn send_daemon_management_request(
    paths: &AppPaths,
    request: &DaemonManagementRequest,
) -> Result<DaemonManagementResponse> {
    let mut stream = try_connect(paths).await?;
    write_daemon_management_request(&mut stream, request).await?;

    let mut reader = BufReader::new(stream);
    let Some(response) = read_daemon_management_response(&mut reader).await? else {
        bail!("agentd closed the management connection");
    };
    Ok(response)
}

fn print_daemon_management_status(status: &DaemonManagementStatus) {
    println!("source: daemon_management");
    println!("daemon_version: {}", status.daemon_version);
    println!("daemon_protocol_version: {}", status.protocol_version);
    println!("client_version: {}", env!("CARGO_PKG_VERSION"));
    println!("expected_protocol_version: {}", PROTOCOL_VERSION);
    println!("pid: {}", status.pid);
    println!("root: {}", status.root);
    println!("socket: {}", status.socket);
    println!("running_sessions: {}", status.running_sessions);
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
        if session.integration_state == IntegrationState::PendingReview {
            bail!(
                "session `{session_id}` has unapplied changes; use `agent diff {session_id}` and `agent accept {session_id}` before removing it, or reconnect to the daemon and run `agent discard {session_id}`"
            );
        }
        remove_session_artifacts(paths, &session)?;
        store.delete_session(session_id)?;
    }

    print_kill_result(session_id, was_running, remove);
    Ok(())
}

async fn attach_session(paths: &AppPaths, session_id: &str) -> Result<()> {
    let mut next_session_id = session_id.to_string();
    loop {
        match attach_session_once(paths, &next_session_id).await? {
            AttachOutcome::Detached => return Ok(()),
            AttachOutcome::SessionEnded(summary) => {
                print_session_end_summary(&summary);
                return Ok(());
            }
            AttachOutcome::SwitchSession(session_id) => next_session_id = session_id,
        }
    }
}

async fn attach_session_once(paths: &AppPaths, session_id: &str) -> Result<AttachOutcome> {
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

    let snapshot = match response {
        Response::Attached { snapshot } => snapshot,
        Response::SessionEnded {
            session_id,
            status,
            integration_state,
            branch,
            worktree,
            exit_code,
            error,
        } => {
            return Ok(AttachOutcome::SessionEnded(SessionEndSummary {
                session_id,
                status,
                integration_state,
                branch,
                worktree,
                exit_code,
                error,
            }));
        }
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    };

    eprintln!("attached to {session_id}; detach with Ctrl-]");
    let _terminal = AttachTerminalGuard::enter()?;
    write_attach_bytes(AGENTD_ATTACH_CLEAR_SEQUENCE)?;
    write_attach_bytes(&snapshot)?;
    let (stdin_tx, mut stdin_rx) = mpsc::unbounded_channel();

    let stdin_task = tokio::task::spawn_blocking(move || read_attach_stdin(stdin_tx));

    loop {
        tokio::select! {
            event = stdin_rx.recv() => match event {
                Some(AttachInput::Data(data)) => {
                    write_request(&mut write_half, &Request::AttachInput { data }).await?;
                }
                Some(AttachInput::Detach) | None => break,
            },
            response = read_response(&mut reader) => {
                let Some(response) = response? else {
                    break;
                };
                match response {
                    Response::PtyOutput { data } => write_attach_bytes(&data)?,
                    Response::SwitchSession { session_id } => {
                        drop(write_half);
                        stdin_task.abort();
                        return Ok(AttachOutcome::SwitchSession(session_id));
                    }
                    Response::SessionEnded {
                        session_id,
                        status,
                        integration_state,
                        branch,
                        worktree,
                        exit_code,
                        error,
                    } => {
                        drop(write_half);
                        stdin_task.abort();
                        return Ok(AttachOutcome::SessionEnded(SessionEndSummary {
                            session_id,
                            status,
                            integration_state,
                            branch,
                            worktree,
                            exit_code,
                            error,
                        }));
                    }
                    Response::EndOfStream => break,
                    Response::Error { message } => bail!(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
        }
    }

    drop(write_half);
    stdin_task.abort();
    Ok(AttachOutcome::Detached)
}

async fn try_connect(paths: &AppPaths) -> Result<UnixStream> {
    UnixStream::connect(paths.socket.as_std_path())
        .await
        .with_context(|| format!("failed to connect to {}", paths.socket))
}

fn print_sessions(sessions: &[SessionRecord]) {
    for session in sessions {
        println!(
            "{}\t{}\t{}\t{}\t{}\t{}",
            session.session_id,
            session.agent,
            session.status_string(),
            session.integration_string(),
            session.attention_string(),
            session.branch
        );
        if let Some(summary) = &session.attention_summary {
            println!("\t{summary}");
        }
    }
}

fn print_session(session: &SessionRecord) {
    println!("session_id: {}", session.session_id);
    if let Some(thread_id) = &session.thread_id {
        println!("thread_id: {thread_id}");
    }
    println!("agent: {}", session.agent);
    if let Some(model) = &session.model {
        println!("model: {model}");
    }
    println!("status: {}", session.status_string());
    println!("integration_state: {}", session.integration_string());
    println!("attention: {}", session.attention_string());
    if let Some(summary) = &session.attention_summary {
        println!("attention_summary: {summary}");
    }
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
    print!("{}", render_diff_text(&diff.diff, diff_color_enabled()));
}

fn print_kill_result(session_id: &str, was_running: bool, removed: bool) {
    if was_running {
        println!("terminated session {session_id}");
    }
    if removed {
        println!("removed session {session_id}");
    }
}

fn read_attach_stdin(tx: mpsc::UnboundedSender<AttachInput>) -> Result<()> {
    let mut parser = AttachKeyBindingParser;
    loop {
        let event = event::read().context("failed to read attach input")?;
        let Some(input) = parser.parse_event(event) else {
            continue;
        };

        match input {
            AttachInput::Data(data) => {
                if tx.send(AttachInput::Data(data)).is_err() {
                    break;
                }
            }
            AttachInput::Detach => {
                let _ = tx.send(AttachInput::Detach);
                break;
            }
        };
    }
    Ok(())
}

struct AttachKeyBindingParser;

impl AttachKeyBindingParser {
    fn parse_event(&mut self, event: Event) -> Option<AttachInput> {
        match event {
            Event::Key(key) => self.parse_key_event(key),
            Event::Paste(data) => Some(AttachInput::Data(data.into_bytes())),
            Event::FocusGained | Event::FocusLost | Event::Mouse(_) | Event::Resize(_, _) => None,
        }
    }

    fn parse_key_event(&mut self, key: crossterm::event::KeyEvent) -> Option<AttachInput> {
        if key.kind != KeyEventKind::Press {
            return None;
        }

        if is_attach_detach_key(&key) {
            return Some(AttachInput::Detach);
        }

        encode_attach_key(key).map(AttachInput::Data)
    }
}

fn is_attach_detach_key(key: &crossterm::event::KeyEvent) -> bool {
    key.modifiers.contains(KeyModifiers::CONTROL) && matches!(key.code, KeyCode::Char(']'))
}

fn encode_attach_key(key: crossterm::event::KeyEvent) -> Option<Vec<u8>> {
    let mut bytes = match key.code {
        KeyCode::Backspace => vec![0x7f],
        KeyCode::Enter => vec![b'\r'],
        KeyCode::Left => b"\x1b[D".to_vec(),
        KeyCode::Right => b"\x1b[C".to_vec(),
        KeyCode::Up => b"\x1b[A".to_vec(),
        KeyCode::Down => b"\x1b[B".to_vec(),
        KeyCode::Home => b"\x1b[H".to_vec(),
        KeyCode::End => b"\x1b[F".to_vec(),
        KeyCode::PageUp => b"\x1b[5~".to_vec(),
        KeyCode::PageDown => b"\x1b[6~".to_vec(),
        KeyCode::Tab => {
            if key.modifiers.contains(KeyModifiers::SHIFT) {
                b"\x1b[Z".to_vec()
            } else {
                vec![b'\t']
            }
        }
        KeyCode::BackTab => b"\x1b[Z".to_vec(),
        KeyCode::Delete => b"\x1b[3~".to_vec(),
        KeyCode::Insert => b"\x1b[2~".to_vec(),
        KeyCode::Esc => vec![0x1b],
        KeyCode::Char(ch) => encode_attach_char(ch, key.modifiers)?,
        _ => return None,
    };

    if key.modifiers.contains(KeyModifiers::ALT) {
        bytes.insert(0, 0x1b);
    }

    Some(bytes)
}

fn encode_attach_char(ch: char, modifiers: KeyModifiers) -> Option<Vec<u8>> {
    if modifiers.contains(KeyModifiers::CONTROL) {
        return control_char_byte(ch).map(|byte| vec![byte]);
    }

    let mut bytes = [0_u8; 4];
    Some(ch.encode_utf8(&mut bytes).as_bytes().to_vec())
}

fn control_char_byte(ch: char) -> Option<u8> {
    match ch {
        '@' | ' ' => Some(0),
        'a'..='z' => Some((ch as u8) - b'a' + 1),
        'A'..='Z' => Some((ch as u8) - b'A' + 1),
        '[' => Some(27),
        '\\' => Some(28),
        ']' => Some(29),
        '^' => Some(30),
        '_' => Some(31),
        '?' => Some(127),
        _ => None,
    }
}

enum AttachInput {
    Data(Vec<u8>),
    Detach,
}

enum AttachOutcome {
    Detached,
    SessionEnded(SessionEndSummary),
    SwitchSession(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct SessionEndSummary {
    session_id: String,
    status: SessionStatus,
    integration_state: IntegrationState,
    branch: String,
    worktree: String,
    exit_code: Option<i32>,
    error: Option<String>,
}

fn print_session_end_summary(summary: &SessionEndSummary) {
    println!("{}", format_session_end_summary(summary));
}

fn format_session_end_summary(summary: &SessionEndSummary) -> String {
    match summary.status {
        SessionStatus::Failed => match &summary.error {
            Some(error) => format!("session {} failed: {error}", summary.session_id),
            None => format!("session {} failed", summary.session_id),
        },
        SessionStatus::Paused => format!("session {} paused", summary.session_id),
        SessionStatus::NeedsInput => format!("session {} needs input", summary.session_id),
        SessionStatus::Exited | SessionStatus::UnknownRecovered => {
            if summary.integration_state == IntegrationState::PendingReview {
                return format!(
                    "session {} finished with changes on {} ({})\nrun: agent diff {} | agent accept {} | agent discard {}",
                    summary.session_id,
                    summary.branch,
                    summary.worktree,
                    summary.session_id,
                    summary.session_id,
                    summary.session_id
                );
            }
            match summary.exit_code {
                Some(code) => format!("session {} finished (exit {code})", summary.session_id),
                None => format!("session {} finished", summary.session_id),
            }
        }
        SessionStatus::Creating | SessionStatus::Running => {
            format!("session {} ended", summary.session_id)
        }
    }
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

struct AttachTerminalGuard {
    stdin_fd: i32,
    orig_termios: Option<termios>,
}

impl AttachTerminalGuard {
    fn enter() -> Result<Self> {
        let stdin_fd = std::io::stdin().as_raw_fd();
        let mut orig_termios = MaybeUninit::<termios>::uninit();
        let termios = unsafe {
            if tcgetattr(stdin_fd, orig_termios.as_mut_ptr()) == 0 {
                let orig_termios = orig_termios.assume_init();
                let mut raw_termios = orig_termios;
                cfmakeraw(&mut raw_termios);
                raw_termios.c_cc[VLNEXT] = _POSIX_VDISABLE as _;
                raw_termios.c_cc[VQUIT] = _POSIX_VDISABLE as _;
                raw_termios.c_cc[VMIN] = 1;
                raw_termios.c_cc[VTIME] = 0;
                if tcsetattr(stdin_fd, TCSANOW, &raw_termios) != 0 {
                    bail!("failed to set raw terminal mode");
                }
                Some(orig_termios)
            } else {
                None
            }
        };
        Ok(Self {
            stdin_fd,
            orig_termios: termios,
        })
    }
}

impl Drop for AttachTerminalGuard {
    fn drop(&mut self) {
        if let Some(orig_termios) = &self.orig_termios {
            unsafe {
                tcsetattr(self.stdin_fd, TCSAFLUSH, orig_termios);
            }
        }
        let _ = write_attach_bytes(AGENTD_ATTACH_RESTORE_SEQUENCE);
    }
}

struct TerminalScreenGuard;

impl TerminalScreenGuard {
    fn enter() -> Result<Self> {
        execute!(
            std::io::stdout(),
            EnterAlternateScreen,
            EnableMouseCapture,
            Hide
        )
            .context("failed to enter alternate screen")?;
        Ok(Self)
    }
}

impl Drop for TerminalScreenGuard {
    fn drop(&mut self) {
        let _ = execute!(
            std::io::stdout(),
            Show,
            DisableMouseCapture,
            LeaveAlternateScreen
        );
    }
}

trait StatusString {
    fn status_string(&self) -> &'static str;
    fn integration_string(&self) -> &'static str;
    fn attention_string(&self) -> &'static str;
}

impl StatusString for SessionRecord {
    fn status_string(&self) -> &'static str {
        match self.status {
            SessionStatus::Creating => "creating",
            SessionStatus::Running => "running",
            SessionStatus::Paused => "paused",
            SessionStatus::NeedsInput => "needs_input",
            SessionStatus::Exited => "exited",
            SessionStatus::Failed => "failed",
            SessionStatus::UnknownRecovered => "unknown_recovered",
        }
    }

    fn integration_string(&self) -> &'static str {
        match self.integration_state {
            IntegrationState::Clean => "clean",
            IntegrationState::PendingReview => "pending_review",
            IntegrationState::Applied => "applied",
            IntegrationState::Discarded => "discarded",
        }
    }

    fn attention_string(&self) -> &'static str {
        match self.attention {
            AttentionLevel::Info => "info",
            AttentionLevel::Notice => "notice",
            AttentionLevel::Action => "action",
        }
    }
}

fn write_attach_bytes(data: &[u8]) -> Result<()> {
    let mut stdout = std::io::stdout();
    stdout.write_all(data)?;
    stdout.flush()?;
    Ok(())
}

async fn daemon_list_sessions(paths: &AppPaths) -> Result<Vec<SessionRecord>> {
    let response = send_request(paths, &Request::ListSessions).await?;
    match response {
        Response::Sessions { sessions } => Ok(sessions),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn daemon_get_session(paths: &AppPaths, session_id: &str) -> Result<SessionRecord> {
    let response = send_request(
        paths,
        &Request::GetSession {
            session_id: session_id.to_string(),
        },
    )
    .await?;
    match response {
        Response::Session { session } => Ok(session),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum DashboardFocus {
    Composer,
    SessionList,
}

#[derive(Debug, PartialEq, Eq)]
enum DashboardComposerAction<'a> {
    OpenModelPicker,
    CreateExecuteSession(&'a str),
    CreatePlanSession(&'a str),
    InvalidCommand(&'a str),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum FocusSessionPane {
    Transcript,
    Activity,
    Intervention,
}

#[derive(Debug, Default)]
struct FocusSessionViewState {
    active_pane: FocusSessionPane,
    previous_pane: FocusSessionPane,
    transcript_text: String,
    transcript_lines: Vec<Line<'static>>,
    transcript_line_count: usize,
    transcript_path: Option<String>,
    transcript_bytes: u64,
    transcript_scroll: usize,
    events: Vec<agentd_shared::event::SessionEvent>,
    activity_lines: Vec<Line<'static>>,
    activity_line_count: usize,
    activity_scroll: usize,
    last_event_id: Option<i64>,
    show_verbose: bool,
    needs_input_question: Option<String>,
    needs_input_options: Vec<String>,
}

fn session_has_pending_review(session: &SessionRecord) -> bool {
    session.integration_state == IntegrationState::PendingReview
}

fn session_mode_label(mode: SessionMode) -> &'static str {
    match mode {
        SessionMode::Execute => "exec",
        SessionMode::Plan => "plan",
    }
}

fn parse_dashboard_composer_action(composer: &str) -> Option<DashboardComposerAction<'_>> {
    let trimmed = composer.trim();
    if trimmed.is_empty() {
        return None;
    }
    if trimmed == "/model" {
        return Some(DashboardComposerAction::OpenModelPicker);
    }
    if let Some(task) = trimmed.strip_prefix("/plan") {
        let task = task.trim();
        return if task.is_empty() {
            Some(DashboardComposerAction::InvalidCommand(
                "usage: /plan <task>",
            ))
        } else {
            Some(DashboardComposerAction::CreatePlanSession(task))
        };
    }
    if trimmed.starts_with('/') {
        return Some(DashboardComposerAction::InvalidCommand(
            "unknown command",
        ));
    }
    Some(DashboardComposerAction::CreateExecuteSession(trimmed))
}

impl Default for FocusSessionPane {
    fn default() -> Self {
        Self::Transcript
    }
}

#[derive(Clone, Copy, Debug)]
struct FocusSessionLayout {
    transcript_rect: Rect,
    activity_rect: Rect,
    transcript_height: usize,
    activity_height: usize,
    intervention_rect: Option<Rect>,
}

async fn focus_dashboard(paths: &AppPaths) -> Result<()> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = TerminalScreenGuard::enter()?;
    let backend = CrosstermBackend::new(std::io::stdout());
    let mut terminal = Terminal::new(backend).context("failed to initialize terminal")?;
    let mut composer = String::new();
    let mut selected = 0_usize;
    let mut selected_model = 0_usize;
    let mut model_picker_open = false;
    let mut focus = DashboardFocus::Composer;
    let mut discard_confirm_session_id: Option<String> = None;
    let mut status = String::new();

    loop {
        let sessions = daemon_list_sessions(paths).await?;
        if selected >= sessions.len() && !sessions.is_empty() {
            selected = sessions.len() - 1;
        }

        terminal.draw(|frame| {
            let selected_session = sessions.get(selected);
            let discard_confirm_session = discard_confirm_session_id
                .as_deref()
                .and_then(|session_id| sessions.iter().find(|session| session.session_id == session_id));
            render_focus_dashboard(
                frame,
                &sessions,
                selected,
                selected_session,
                &composer,
                CODEX_MODELS[selected_model],
                focus,
                model_picker_open,
                selected_model,
                discard_confirm_session,
                &status,
            );
        })?;
        status.clear();

        if !event::poll(Duration::from_millis(250)).context("failed to poll terminal input")? {
            continue;
        }
        let Event::Key(key) = event::read().context("failed to read terminal input")? else {
            continue;
        };
        if key.kind != KeyEventKind::Press {
            continue;
        }

        if let Some(session_id) = discard_confirm_session_id.as_deref() {
            match key.code {
                KeyCode::Esc => {
                    discard_confirm_session_id = None;
                    status = "discard canceled".to_string();
                }
                KeyCode::Enter => {
                    let Some(session) = sessions
                        .iter()
                        .find(|session| session.session_id == session_id)
                    else {
                        discard_confirm_session_id = None;
                        status = "selected session is no longer available".to_string();
                        continue;
                    };
                    if !session_has_pending_review(session) {
                        discard_confirm_session_id = None;
                        status = format!("session {} has no pending changes", session.session_id);
                        continue;
                    }
                    discard_session(paths, &session.session_id, false).await?;
                    discard_confirm_session_id = None;
                    status = format!("discarded changes for {}", session.session_id);
                }
                _ => {}
            }
            continue;
        }

        match key.code {
            KeyCode::Tab => {
                if !model_picker_open {
                    focus = match focus {
                        DashboardFocus::Composer => DashboardFocus::SessionList,
                        DashboardFocus::SessionList => DashboardFocus::Composer,
                    };
                }
            }
            KeyCode::Esc => {
                if model_picker_open {
                    model_picker_open = false;
                    status = "model picker closed".to_string();
                } else if focus == DashboardFocus::SessionList {
                    focus = DashboardFocus::Composer;
                } else if !composer.is_empty() {
                    composer.clear();
                    status = "composer cleared".to_string();
                } else {
                    return Ok(());
                }
            }
            KeyCode::Char('q')
                if focus == DashboardFocus::SessionList && composer.is_empty() && !model_picker_open =>
            {
                return Ok(());
            }
            KeyCode::Up => {
                if model_picker_open {
                    selected_model = selected_model.saturating_sub(1);
                } else if focus == DashboardFocus::SessionList {
                    selected = selected.saturating_sub(1);
                }
            }
            KeyCode::Down => {
                if model_picker_open {
                    if selected_model + 1 < CODEX_MODELS.len() {
                        selected_model += 1;
                    }
                } else if focus == DashboardFocus::SessionList && selected + 1 < sessions.len() {
                    selected += 1;
                }
            }
            KeyCode::Backspace => {
                if model_picker_open {
                    model_picker_open = false;
                    status = "model picker closed".to_string();
                } else if focus == DashboardFocus::Composer {
                    composer.pop();
                }
            }
            KeyCode::Enter => {
                if model_picker_open {
                    model_picker_open = false;
                    status = format!("model set to {}", CODEX_MODELS[selected_model]);
                } else if focus == DashboardFocus::Composer {
                    match parse_dashboard_composer_action(&composer) {
                        Some(DashboardComposerAction::OpenModelPicker) => {
                            model_picker_open = true;
                            status = "choose a model".to_string();
                        }
                        Some(DashboardComposerAction::CreateExecuteSession(task)) => {
                            let created = create_dashboard_session(
                                paths,
                                task,
                                Some(CODEX_MODELS[selected_model]),
                                SessionMode::Execute,
                            )
                            .await?;
                            composer.clear();
                            focus_session(paths, &created.session_id).await?;
                            terminal.clear()?;
                        }
                        Some(DashboardComposerAction::CreatePlanSession(task)) => {
                            let created = create_dashboard_session(
                                paths,
                                task,
                                Some(CODEX_MODELS[selected_model]),
                                SessionMode::Plan,
                            )
                            .await?;
                            composer.clear();
                            focus_session(paths, &created.session_id).await?;
                            terminal.clear()?;
                        }
                        Some(DashboardComposerAction::InvalidCommand(message)) => {
                            status = message.to_string();
                        }
                        None => {}
                    }
                } else if focus == DashboardFocus::SessionList
                    && let Some(session) = sessions.get(selected)
                {
                    focus_session(paths, &session.session_id).await?;
                    terminal.clear()?;
                }
            }
            KeyCode::Char('k')
                if focus == DashboardFocus::SessionList
                    && !model_picker_open
                    && sessions.get(selected).is_some() =>
            {
                if let Some(session) = sessions.get(selected) {
                    kill_session(paths, &session.session_id).await?;
                    status = format!("terminated {}", session.session_id);
                }
            }
            KeyCode::Char('r')
                if focus == DashboardFocus::SessionList
                    && !model_picker_open
                    && sessions.get(selected).is_some() =>
            {
                if let Some(session) = sessions.get(selected) {
                    let retried = retry_session(paths, session).await?;
                    status = format!("created retry {}", retried.session_id);
                }
            }
            KeyCode::Char('A')
                if focus == DashboardFocus::SessionList
                    && !model_picker_open
                    && sessions.get(selected).is_some() =>
            {
                if let Some(session) = sessions.get(selected) {
                    if !session_has_pending_review(session) {
                        status = format!("session {} has no pending changes", session.session_id);
                    } else {
                        accept_session(paths, &session.session_id).await?;
                        status = format!("applied changes for {}", session.session_id);
                    }
                }
            }
            KeyCode::Char('D')
                if focus == DashboardFocus::SessionList
                    && !model_picker_open
                    && sessions.get(selected).is_some() =>
            {
                if let Some(session) = sessions.get(selected) {
                    if !session_has_pending_review(session) {
                        status = format!("session {} has no pending changes", session.session_id);
                    } else {
                        discard_confirm_session_id = Some(session.session_id.clone());
                    }
                }
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                if !model_picker_open {
                    if focus == DashboardFocus::SessionList {
                        focus = DashboardFocus::Composer;
                    }
                    composer.push(ch);
                }
            }
            _ => {}
        }
    }
}

async fn focus_session(paths: &AppPaths, session_id: &str) -> Result<()> {
    let raw_mode = RawModeGuard::new()?;
    let screen = TerminalScreenGuard::enter()?;
    let backend = CrosstermBackend::new(std::io::stdout());
    let mut terminal = Terminal::new(backend).context("failed to initialize terminal")?;
    let store = LocalStore::open(paths)?;
    let mut status = String::new();
    let mut discard_confirm_open = false;
    let mut reply = String::new();
    let mut selected_option = 0_usize;
    let mut view_state = FocusSessionViewState::default();
    let mut last_needs_input_question: Option<String> = None;

    loop {
        let session = daemon_get_session(paths, session_id).await?;
        let size = terminal.size()?;
        let intervention_active = session.status == SessionStatus::NeedsInput || !reply.is_empty();
        let layout = focus_session_layout(Rect::new(0, 0, size.width, size.height), intervention_active);
        refresh_focus_session_view(paths, &store, &session, &mut view_state, &layout)?;
        if view_state.needs_input_question != last_needs_input_question {
            last_needs_input_question = view_state.needs_input_question.clone();
            reply.clear();
            selected_option = 0;
        }
        if selected_option >= view_state.needs_input_options.len() && !view_state.needs_input_options.is_empty() {
            selected_option = view_state.needs_input_options.len() - 1;
        }
        clamp_focus_session_scroll(&mut view_state, &layout);
        terminal.draw(|frame| {
            render_focus_session(
                frame,
                &session,
                &view_state,
                &status,
                discard_confirm_open,
                &reply,
                selected_option,
            );
        })?;
        status.clear();

        if !event::poll(Duration::from_millis(250)).context("failed to poll terminal input")? {
            continue;
        }

        match event::read().context("failed to read terminal input")? {
            Event::Mouse(mouse) => match mouse.kind {
                MouseEventKind::Down(_) => {
                    if let Some(pane) = pane_at_position(&layout, mouse.column, mouse.row) {
                        view_state.active_pane = pane;
                    }
                }
                MouseEventKind::ScrollUp => {
                    if let Some(pane) = pane_at_position(&layout, mouse.column, mouse.row) {
                        view_state.active_pane = pane;
                        scroll_focus_session_pane(&mut view_state, &layout, -3);
                    }
                }
                MouseEventKind::ScrollDown => {
                    if let Some(pane) = pane_at_position(&layout, mouse.column, mouse.row) {
                        view_state.active_pane = pane;
                        scroll_focus_session_pane(&mut view_state, &layout, 3);
                    }
                }
                _ => {}
            },
            Event::Key(key) => {
                if key.kind != KeyEventKind::Press {
                    continue;
                }

                if discard_confirm_open {
                    match key.code {
                        KeyCode::Esc => {
                            discard_confirm_open = false;
                            status = "discard canceled".to_string();
                        }
                        KeyCode::Enter => {
                            if !session_has_pending_review(&session) {
                                status = format!("session {} has no pending changes", session_id);
                            } else {
                                discard_session(paths, session_id, false).await?;
                                status = format!("discarded changes for {}", session_id);
                            }
                            discard_confirm_open = false;
                        }
                        _ => {}
                    }
                    continue;
                }

                match key.code {
                    KeyCode::Esc => {
                        if view_state.active_pane == FocusSessionPane::Intervention && !reply.is_empty() {
                            reply.clear();
                            status = "reply cleared".to_string();
                        } else if view_state.active_pane == FocusSessionPane::Intervention {
                            view_state.active_pane = view_state.previous_pane;
                        } else {
                            return Ok(());
                        }
                    }
                    KeyCode::Char('q') => return Ok(()),
                    KeyCode::Tab => {
                        view_state.active_pane = match view_state.active_pane {
                            FocusSessionPane::Transcript => FocusSessionPane::Activity,
                            FocusSessionPane::Activity => {
                                if intervention_active {
                                    FocusSessionPane::Intervention
                                } else {
                                    FocusSessionPane::Transcript
                                }
                            }
                            FocusSessionPane::Intervention => FocusSessionPane::Transcript,
                        };
                    }
                    KeyCode::Up => {
                        if view_state.active_pane == FocusSessionPane::Intervention
                            && !view_state.needs_input_options.is_empty()
                        {
                            selected_option = selected_option.saturating_sub(1);
                        } else {
                            scroll_focus_session_pane(&mut view_state, &layout, -1);
                        }
                    }
                    KeyCode::Down => {
                        if view_state.active_pane == FocusSessionPane::Intervention
                            && selected_option + 1 < view_state.needs_input_options.len()
                        {
                            selected_option += 1;
                        } else {
                            scroll_focus_session_pane(&mut view_state, &layout, 1);
                        }
                    }
                    KeyCode::PageUp if view_state.active_pane != FocusSessionPane::Intervention => {
                        scroll_focus_session_page(&mut view_state, &layout, -1);
                    }
                    KeyCode::PageDown if view_state.active_pane != FocusSessionPane::Intervention => {
                        scroll_focus_session_page(&mut view_state, &layout, 1);
                    }
                    KeyCode::Home if view_state.active_pane != FocusSessionPane::Intervention => {
                        focus_session_home(&mut view_state);
                    }
                    KeyCode::End if view_state.active_pane != FocusSessionPane::Intervention => {
                        focus_session_end(&mut view_state, &layout);
                    }
                    KeyCode::Char('v') if view_state.active_pane != FocusSessionPane::Intervention => {
                        view_state.show_verbose = !view_state.show_verbose;
                        status = if view_state.show_verbose {
                            "verbose events shown".to_string()
                        } else {
                            "verbose events hidden".to_string()
                        };
                    }
                    KeyCode::Backspace if view_state.active_pane == FocusSessionPane::Intervention => {
                        reply.pop();
                    }
                    KeyCode::Enter if view_state.active_pane == FocusSessionPane::Intervention => {
                        if !reply.trim().is_empty() {
                            reply_session(paths, session_id, reply.trim()).await?;
                            reply.clear();
                            view_state.active_pane = view_state.previous_pane;
                            status = format!("replied to {}", session_id);
                        } else if let Some(option) = view_state.needs_input_options.get(selected_option) {
                            reply = option.clone();
                            status = "option copied to composer".to_string();
                        } else {
                            status = "reply is empty".to_string();
                        }
                    }
                    KeyCode::Char('a') if view_state.active_pane != FocusSessionPane::Intervention => {
                        if session.status == SessionStatus::Running {
                            drop(terminal);
                            drop(screen);
                            drop(raw_mode);
                            return attach_session(paths, session_id).await;
                        }
                        status = "session is not running".to_string();
                    }
                    KeyCode::Char('A') if view_state.active_pane != FocusSessionPane::Intervention => {
                        if !session_has_pending_review(&session) {
                            status = format!("session {} has no pending changes", session_id);
                        } else {
                            accept_session(paths, session_id).await?;
                            status = format!("applied changes for {}", session_id);
                        }
                    }
                    KeyCode::Char('D') if view_state.active_pane != FocusSessionPane::Intervention => {
                        if !session_has_pending_review(&session) {
                            status = format!("session {} has no pending changes", session_id);
                        } else {
                            discard_confirm_open = true;
                        }
                    }
                    KeyCode::Char('k') if view_state.active_pane != FocusSessionPane::Intervention => {
                        kill_session(paths, session_id).await?;
                        status = format!("terminated {}", session_id);
                    }
                    KeyCode::Char('r') if view_state.active_pane != FocusSessionPane::Intervention => {
                        let retried = retry_session(paths, &session).await?;
                        status = format!("created retry {}", retried.session_id);
                    }
                    KeyCode::Char('y') if session.status == SessionStatus::NeedsInput =>
                    {
                        view_state.previous_pane = view_state.active_pane;
                        view_state.active_pane = FocusSessionPane::Intervention;
                    }
                    KeyCode::Char(ch)
                        if view_state.active_pane == FocusSessionPane::Intervention
                            && (key.modifiers.is_empty()
                                || key.modifiers == KeyModifiers::SHIFT) =>
                    {
                        reply.push(ch);
                    }
                    _ => {}
                }
            }
            _ => {}
        }
    }
}

fn focus_session_layout(area: Rect, intervention_active: bool) -> FocusSessionLayout {
    let outer = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(5),
            Constraint::Min(10),
            if intervention_active {
                Constraint::Length(8)
            } else {
                Constraint::Length(0)
            },
            Constraint::Length(2),
        ])
        .split(area);
    let middle = Layout::default()
        .direction(Direction::Horizontal)
        .constraints([Constraint::Percentage(68), Constraint::Percentage(32)])
        .split(outer[1]);
    let intervention = if intervention_active {
        Some(
            Layout::default()
                .direction(Direction::Horizontal)
                .constraints([Constraint::Percentage(68), Constraint::Percentage(32)])
                .split(outer[2])[0],
        )
    } else {
        None
    };
    FocusSessionLayout {
        transcript_rect: middle[0],
        activity_rect: middle[1],
        transcript_height: middle[0].height.saturating_sub(2) as usize,
        activity_height: middle[1].height.saturating_sub(2) as usize,
        intervention_rect: intervention,
    }
}

fn pane_at_position(layout: &FocusSessionLayout, column: u16, row: u16) -> Option<FocusSessionPane> {
    let point_in_rect = |rect: Rect| {
        column >= rect.x
            && column < rect.x.saturating_add(rect.width)
            && row >= rect.y
            && row < rect.y.saturating_add(rect.height)
    };
    if point_in_rect(layout.transcript_rect) {
        Some(FocusSessionPane::Transcript)
    } else if point_in_rect(layout.activity_rect) {
        Some(FocusSessionPane::Activity)
    } else if layout
        .intervention_rect
        .is_some_and(point_in_rect)
    {
        Some(FocusSessionPane::Intervention)
    } else {
        None
    }
}

fn refresh_focus_session_view(
    paths: &AppPaths,
    store: &LocalStore,
    session: &SessionRecord,
    state: &mut FocusSessionViewState,
    layout: &FocusSessionLayout,
) -> Result<()> {
    let log_path = resolve_focus_log_path(paths, session);
    let log_path_string = log_path.to_string();
    let metadata = fs::metadata(log_path.as_std_path()).ok();
    let log_bytes = metadata.as_ref().map(|item| item.len()).unwrap_or(0);
    let transcript_was_at_bottom = state.transcript_scroll
        >= max_scroll(state.transcript_line_count, layout.transcript_height);
    let activity_was_at_bottom = state.activity_scroll
        >= max_scroll(state.activity_line_count, layout.activity_height);

    if state.transcript_path.as_deref() != Some(log_path_string.as_str())
        || state.transcript_bytes != log_bytes
    {
        state.transcript_text = read_focus_log_contents(&log_path)?;
        state.transcript_path = Some(log_path_string);
        state.transcript_bytes = log_bytes;
    }

    let new_events = store.list_events_since(&session.session_id, state.last_event_id)?;
    if let Some(last) = new_events.last() {
        state.last_event_id = Some(last.id);
    }
    state.events.extend(new_events);
    if session.status == SessionStatus::NeedsInput {
        state.needs_input_question = latest_needs_input_question(&state.events);
        state.needs_input_options = state
            .needs_input_question
            .as_deref()
            .map(parse_needs_input_options)
            .unwrap_or_default();
    } else {
        state.needs_input_question = None;
        state.needs_input_options.clear();
    }

    rebuild_focus_session_lines(state);

    if transcript_was_at_bottom {
        state.transcript_scroll = max_scroll(state.transcript_line_count, layout.transcript_height);
    }
    if activity_was_at_bottom {
        state.activity_scroll = max_scroll(state.activity_line_count, layout.activity_height);
    }
    Ok(())
}

fn resolve_focus_log_path(paths: &AppPaths, session: &SessionRecord) -> camino::Utf8PathBuf {
    let rendered = paths.rendered_log_path(&session.session_id);
    if session.agent == "codex" && rendered.exists() {
        rendered
    } else {
        paths.log_path(&session.session_id)
    }
}

fn read_focus_log_contents(path: &camino::Utf8Path) -> Result<String> {
    match fs::read_to_string(path.as_std_path()) {
        Ok(contents) => Ok(contents),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(String::new()),
        Err(err) => Err(err).with_context(|| format!("failed to read {}", path)),
    }
}

fn line_count(lines: &[Line<'static>]) -> usize {
    std::cmp::max(1, lines.len())
}

fn max_scroll(total_lines: usize, visible_lines: usize) -> usize {
    total_lines.saturating_sub(visible_lines.max(1))
}

fn clamp_focus_session_scroll(state: &mut FocusSessionViewState, layout: &FocusSessionLayout) {
    state.transcript_scroll = state
        .transcript_scroll
        .min(max_scroll(line_count(&state.transcript_lines), layout.transcript_height));
    state.activity_scroll = state
        .activity_scroll
        .min(max_scroll(line_count(&state.activity_lines), layout.activity_height));
}

fn scroll_focus_session_pane(
    state: &mut FocusSessionViewState,
    layout: &FocusSessionLayout,
    delta: isize,
) {
    match state.active_pane {
        FocusSessionPane::Transcript => {
            state.transcript_scroll = apply_scroll_delta(
                state.transcript_scroll,
                max_scroll(line_count(&state.transcript_lines), layout.transcript_height),
                delta,
            );
        }
        FocusSessionPane::Activity => {
            state.activity_scroll = apply_scroll_delta(
                state.activity_scroll,
                max_scroll(line_count(&state.activity_lines), layout.activity_height),
                delta,
            );
        }
        FocusSessionPane::Intervention => {}
    }
}

fn scroll_focus_session_page(
    state: &mut FocusSessionViewState,
    layout: &FocusSessionLayout,
    direction: isize,
) {
    let delta = match state.active_pane {
        FocusSessionPane::Transcript => layout.transcript_height.saturating_sub(1) as isize,
        FocusSessionPane::Activity => layout.activity_height.saturating_sub(1) as isize,
        FocusSessionPane::Intervention => 0,
    };
    scroll_focus_session_pane(state, layout, delta.saturating_mul(direction));
}

fn focus_session_home(state: &mut FocusSessionViewState) {
    match state.active_pane {
        FocusSessionPane::Transcript => state.transcript_scroll = 0,
        FocusSessionPane::Activity => state.activity_scroll = 0,
        FocusSessionPane::Intervention => {}
    }
}

fn focus_session_end(state: &mut FocusSessionViewState, layout: &FocusSessionLayout) {
    match state.active_pane {
        FocusSessionPane::Transcript => {
            state.transcript_scroll = max_scroll(line_count(&state.transcript_lines), layout.transcript_height)
        }
        FocusSessionPane::Activity => {
            state.activity_scroll = max_scroll(line_count(&state.activity_lines), layout.activity_height)
        }
        FocusSessionPane::Intervention => {}
    }
}

fn latest_needs_input_question(events: &[agentd_shared::event::SessionEvent]) -> Option<String> {
    events.iter().rev().find_map(|event| {
        (event.event_type == "SESSION_NEEDS_INPUT")
            .then(|| event.payload_json.get("question").and_then(Value::as_str))
            .flatten()
            .map(str::to_string)
    })
}

fn parse_needs_input_options(question: &str) -> Vec<String> {
    let mut options = Vec::new();
    let mut current: Option<String> = None;
    let mut seen_marker = false;

    for line in question.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }

        if let Some(option_text) = parse_option_marker(trimmed) {
            if let Some(option) = current.take() {
                options.push(option);
            }
            current = Some(option_text.to_string());
            seen_marker = true;
            continue;
        }

        if seen_marker && let Some(option) = current.as_mut() {
            option.push(' ');
            option.push_str(trimmed);
        }
    }

    if let Some(option) = current {
        options.push(option);
    }

    if options.len() >= 2 {
        options
    } else {
        Vec::new()
    }
}

fn parse_option_marker(line: &str) -> Option<&str> {
    let bullet = line
        .strip_prefix("- ")
        .or_else(|| line.strip_prefix("* "))
        .map(str::trim);
    if bullet.is_some() {
        return bullet;
    }

    let digit_count = line.chars().take_while(|ch| ch.is_ascii_digit()).count();
    if digit_count == 0 {
        return None;
    }
    let rest = &line[digit_count..];
    rest.strip_prefix(". ")
        .or_else(|| rest.strip_prefix(") "))
        .map(str::trim)
}

fn apply_scroll_delta(current: usize, max: usize, delta: isize) -> usize {
    if delta.is_negative() {
        current.saturating_sub(delta.unsigned_abs())
    } else {
        current.saturating_add(delta as usize).min(max)
    }
}

fn rebuild_focus_session_lines(state: &mut FocusSessionViewState) {
    state.transcript_lines =
        build_focus_transcript_lines(&state.events, &state.transcript_text, state.show_verbose);
    state.activity_lines = build_focus_activity_lines(&state.events, state.show_verbose);
    state.transcript_line_count = line_count(&state.transcript_lines);
    state.activity_line_count = line_count(&state.activity_lines);
}

fn build_focus_transcript_lines(
    events: &[agentd_shared::event::SessionEvent],
    fallback_text: &str,
    show_verbose: bool,
) -> Vec<Line<'static>> {
    let mut lines = Vec::new();
    for event in events {
        append_timeline_lines(&mut lines, event, show_verbose);
    }

    if lines.is_empty() {
        if fallback_text.trim().is_empty() {
            lines.push(Line::from(Span::styled(
                "No structured transcript yet.",
                muted_style(),
            )));
        } else {
            append_card_header(
                &mut lines,
                "log fallback",
                "rendered transcript unavailable",
                fallback_style(),
            );
            append_preformatted_block(&mut lines, fallback_text, code_block_style());
        }
    }

    lines
}

fn build_focus_activity_lines(
    events: &[agentd_shared::event::SessionEvent],
    show_verbose: bool,
) -> Vec<Line<'static>> {
    if events.is_empty() {
        return vec![Line::from(Span::styled("No activity yet", muted_style()))];
    }

    let mut lines = Vec::new();
    for event in events {
        if let Some((label, detail, style)) = summarize_activity_event(event, show_verbose) {
            lines.push(Line::from(vec![
                Span::styled(
                    format!("{:<8}", event.timestamp.format("%H:%M:%S")),
                    muted_style(),
                ),
                Span::raw(" "),
                Span::styled(label, style.add_modifier(Modifier::BOLD)),
            ]));
            if !detail.is_empty() {
                lines.push(Line::from(Span::styled(
                    format!("         {}", preview_text(&detail, 48)),
                    muted_style(),
                )));
            }
        }
    }

    if lines.is_empty() {
        lines.push(Line::from(Span::styled(
            "No activity in current filter.",
            muted_style(),
        )));
    }
    lines
}

fn append_timeline_lines(
    lines: &mut Vec<Line<'static>>,
    event: &agentd_shared::event::SessionEvent,
    show_verbose: bool,
) {
    match event.event_type.as_str() {
        "SESSION_REPLY_REQUESTED" | "SESSION_INPUT_INJECTED" => {
            let prompt = event
                .payload_json
                .get("prompt")
                .and_then(Value::as_str)
                .or_else(|| event.payload_json.get("prompt_preview").and_then(Value::as_str))
                .or_else(|| event.payload_json.get("data").and_then(Value::as_str))
                .unwrap_or_default();
            if !prompt.trim().is_empty() {
                append_message_card(lines, "you", event, prompt, user_style());
            }
        }
        "SESSION_NEEDS_INPUT" => {
            let question = event
                .payload_json
                .get("question")
                .and_then(Value::as_str)
                .unwrap_or("Agent is waiting for input.");
            append_system_card(lines, event, "needs input", question, attention_style(AttentionLevel::Action));
        }
        "SESSION_PLAN_READY" => {
            append_plan_card(lines, event);
        }
        "SESSION_PLAN_MISSING" => {
            let detail = event
                .payload_json
                .get("reason")
                .and_then(Value::as_str)
                .unwrap_or("planning finished without a final plan");
            append_system_card(lines, event, "plan missing", detail, attention_style(AttentionLevel::Action));
        }
        "SESSION_FAILED" => {
            let detail = event
                .payload_json
                .get("error")
                .and_then(Value::as_str)
                .unwrap_or("Session failed.");
            append_system_card(lines, event, "failed", detail, attention_style(AttentionLevel::Action));
        }
        "SESSION_FINISHED" if show_verbose => {
            let detail = event
                .payload_json
                .get("outcome")
                .and_then(Value::as_str)
                .unwrap_or("finished");
            append_system_card(lines, event, "finished", detail, notice_style());
        }
        "THREAD_UPSTREAM_BOUND" if show_verbose => {
            let detail = event
                .payload_json
                .get("upstream_thread_id")
                .and_then(Value::as_str)
                .unwrap_or_default();
            append_system_card(lines, event, "thread", detail, accent_style());
        }
        "CODEX_JSON_PARSE_ERROR" if show_verbose => {
            let detail = event
                .payload_json
                .get("line_preview")
                .and_then(Value::as_str)
                .unwrap_or("invalid codex json");
            append_system_card(lines, event, "parse error", detail, attention_style(AttentionLevel::Action));
        }
        _ => {
            if let Some(item) = codex_item(&event.payload_json) {
                match item_type(item) {
                    Some("agent_message") => {
                        if let Some(text) = item.get("text").and_then(Value::as_str) {
                            append_message_card(lines, "assistant", event, text, assistant_style());
                        }
                    }
                    Some("command_execution") => {
                        append_command_card(lines, event, item);
                    }
                    Some("file_change") => {
                        append_file_change_card(lines, event, item);
                    }
                    _ if show_verbose => append_system_card(
                        lines,
                        event,
                        "event",
                        &event_preview(event),
                        fallback_style(),
                    ),
                    _ => {}
                }
            } else if show_verbose {
                append_system_card(lines, event, &event.event_type, &event_preview(event), fallback_style());
            }
        }
    }
}

fn append_message_card(
    lines: &mut Vec<Line<'static>>,
    label: &str,
    event: &agentd_shared::event::SessionEvent,
    text: &str,
    style: Style,
) {
    append_card_header(
        lines,
        label,
        &event.timestamp.format("%H:%M:%S").to_string(),
        style,
    );
    append_rich_text(lines, text);
    lines.push(Line::from(""));
}

fn append_command_card(
    lines: &mut Vec<Line<'static>>,
    event: &agentd_shared::event::SessionEvent,
    item: &Value,
) {
    let status = item.get("status").and_then(Value::as_str).unwrap_or("unknown");
    let exit_code = item
        .get("exit_code")
        .and_then(Value::as_i64)
        .map(|value| format!("exit {value}"))
        .unwrap_or_else(|| "running".to_string());
    append_card_header(
        lines,
        "tool",
        &format!(
            "{}  {}  {}",
            event.timestamp.format("%H:%M:%S"),
            status,
            exit_code
        ),
        tool_style(),
    );

    if let Some(command) = item.get("command").and_then(Value::as_str) {
        lines.push(Line::from(vec![
            Span::styled("cmd ", muted_style()),
            Span::styled(command.to_string(), inline_code_style()),
        ]));
    }

    let output = item
        .get("aggregated_output")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if !output.trim().is_empty() {
        append_output_block(
            lines,
            output,
            item.get("command").and_then(Value::as_str).unwrap_or_default(),
        );
    }
    lines.push(Line::from(""));
}

fn append_plan_card(lines: &mut Vec<Line<'static>>, event: &agentd_shared::event::SessionEvent) {
    let version = event
        .payload_json
        .get("version")
        .and_then(Value::as_u64)
        .unwrap_or(1);
    let summary = event
        .payload_json
        .get("summary")
        .and_then(Value::as_str)
        .unwrap_or("plan ready");
    append_card_header(
        lines,
        "plan",
        &format!("{}  v{}", event.timestamp.format("%H:%M:%S"), version),
        notice_style(),
    );
    lines.push(Line::from(Span::styled(summary.to_string(), Style::default().add_modifier(Modifier::BOLD))));
    if let Some(body) = event.payload_json.get("body_markdown").and_then(Value::as_str) {
        append_preformatted_block(lines, body, code_block_style());
    }
    lines.push(Line::from(""));
}

fn append_file_change_card(
    lines: &mut Vec<Line<'static>>,
    event: &agentd_shared::event::SessionEvent,
    item: &Value,
) {
    append_card_header(
        lines,
        "files",
        &event.timestamp.format("%H:%M:%S").to_string(),
        file_change_style(),
    );
    let Some(changes) = item.get("changes").and_then(Value::as_array) else {
        lines.push(Line::from(Span::styled("No file details", muted_style())));
        lines.push(Line::from(""));
        return;
    };

    for change in changes {
        let path = change
            .get("path")
            .and_then(Value::as_str)
            .unwrap_or("<unknown>");
        let kind = change.get("kind").and_then(Value::as_str).unwrap_or("change");
        let kind_style = match kind {
            "add" | "create" => diff_add_style(),
            "delete" | "remove" => diff_delete_style(),
            _ => file_change_style(),
        };
        lines.push(Line::from(vec![
            Span::styled(format!("{:<7}", kind), kind_style.add_modifier(Modifier::BOLD)),
            Span::raw(" "),
            Span::styled(path.to_string(), code_block_style()),
        ]));
    }
    lines.push(Line::from(""));
}

fn append_system_card(
    lines: &mut Vec<Line<'static>>,
    event: &agentd_shared::event::SessionEvent,
    label: &str,
    detail: &str,
    style: Style,
) {
    append_card_header(
        lines,
        label,
        &event.timestamp.format("%H:%M:%S").to_string(),
        style,
    );
    if !detail.trim().is_empty() {
        lines.push(Line::from(Span::styled(detail.to_string(), muted_style())));
    }
    lines.push(Line::from(""));
}

fn append_card_header(
    lines: &mut Vec<Line<'static>>,
    label: &str,
    meta: &str,
    style: Style,
) {
    lines.push(Line::from(vec![
        Span::styled(
            format!(" {} ", label.to_ascii_uppercase()),
            style.add_modifier(Modifier::BOLD),
        ),
        Span::raw(" "),
        Span::styled(meta.to_string(), muted_style()),
    ]));
}

fn append_output_block(lines: &mut Vec<Line<'static>>, output: &str, command: &str) {
    if command.contains("git diff") || looks_like_diff(output) {
        append_diff_block(lines, output);
    } else {
        append_preformatted_block(lines, output, code_block_style());
    }
}

fn append_rich_text(lines: &mut Vec<Line<'static>>, text: &str) {
    for block in parse_rich_blocks(text) {
        match block {
            RichTextBlock::Paragraph(paragraph) => {
                for raw_line in paragraph.lines() {
                    lines.push(Line::from(inline_markdown_spans(raw_line)));
                }
                lines.push(Line::from(""));
            }
            RichTextBlock::Code { language, text } => {
                let title = language
                    .as_deref()
                    .filter(|value| !value.is_empty())
                    .unwrap_or("code");
                lines.push(Line::from(vec![
                    Span::styled(" code ", code_header_style().add_modifier(Modifier::BOLD)),
                    Span::raw(" "),
                    Span::styled(title.to_string(), muted_style()),
                ]));
                append_preformatted_block(lines, &text, code_block_style());
                lines.push(Line::from(""));
            }
            RichTextBlock::Diff(text) => {
                lines.push(Line::from(Span::styled(
                    " diff ",
                    diff_header_style().add_modifier(Modifier::BOLD),
                )));
                append_diff_block(lines, &text);
                lines.push(Line::from(""));
            }
        }
    }
}

fn append_preformatted_block(lines: &mut Vec<Line<'static>>, text: &str, style: Style) {
    for raw_line in text.lines() {
        lines.push(Line::from(Span::styled(
            format!("  {raw_line}"),
            style,
        )));
    }
    if text.is_empty() {
        lines.push(Line::from(Span::styled("  ", style)));
    }
}

fn append_diff_block(lines: &mut Vec<Line<'static>>, text: &str) {
    for raw_line in text.lines() {
        let style = diff_line_style(raw_line);
        lines.push(Line::from(Span::styled(
            format!("  {raw_line}"),
            style,
        )));
    }
}

#[derive(Debug)]
enum RichTextBlock {
    Paragraph(String),
    Code { language: Option<String>, text: String },
    Diff(String),
}

fn parse_rich_blocks(text: &str) -> Vec<RichTextBlock> {
    let mut blocks = Vec::new();
    let mut paragraph = Vec::new();
    let mut in_fence = false;
    let mut fence_lang: Option<String> = None;
    let mut fence_lines = Vec::new();

    for line in text.lines() {
        if let Some(rest) = line.strip_prefix("```") {
            if in_fence {
                let fenced_text = fence_lines.join("\n");
                if looks_like_diff_with_lang(&fence_lang, &fenced_text) {
                    blocks.push(RichTextBlock::Diff(fenced_text));
                } else {
                    blocks.push(RichTextBlock::Code {
                        language: fence_lang.take(),
                        text: fenced_text,
                    });
                }
                fence_lines.clear();
                in_fence = false;
                fence_lang = None;
            } else {
                flush_paragraph(&mut blocks, &mut paragraph);
                in_fence = true;
                let lang = rest.trim();
                if !lang.is_empty() {
                    fence_lang = Some(lang.to_string());
                }
            }
            continue;
        }

        if in_fence {
            fence_lines.push(line.to_string());
        } else {
            paragraph.push(line.to_string());
        }
    }

    if in_fence {
        let fenced_text = fence_lines.join("\n");
        if looks_like_diff_with_lang(&fence_lang, &fenced_text) {
            blocks.push(RichTextBlock::Diff(fenced_text));
        } else if !fenced_text.is_empty() {
            blocks.push(RichTextBlock::Code {
                language: fence_lang,
                text: fenced_text,
            });
        }
    } else {
        flush_paragraph(&mut blocks, &mut paragraph);
    }

    if blocks.is_empty() && !text.trim().is_empty() {
        blocks.push(RichTextBlock::Paragraph(text.to_string()));
    }

    blocks
}

fn flush_paragraph(blocks: &mut Vec<RichTextBlock>, paragraph: &mut Vec<String>) {
    let joined = paragraph.join("\n").trim().to_string();
    paragraph.clear();
    if joined.is_empty() {
        return;
    }
    if looks_like_diff(&joined) {
        blocks.push(RichTextBlock::Diff(joined));
    } else {
        blocks.push(RichTextBlock::Paragraph(joined));
    }
}

fn inline_markdown_spans(text: &str) -> Vec<Span<'static>> {
    let mut spans = Vec::new();
    let mut in_code = false;
    for part in text.split('`') {
        let span = if in_code {
            Span::styled(part.to_string(), inline_code_style())
        } else {
            Span::styled(part.to_string(), Style::default())
        };
        spans.push(span);
        in_code = !in_code;
    }
    spans
}

fn summarize_activity_event(
    event: &agentd_shared::event::SessionEvent,
    show_verbose: bool,
) -> Option<(String, String, Style)> {
    match event.event_type.as_str() {
        "SESSION_REPLY_REQUESTED" => Some((
            "reply".to_string(),
            event_preview(event),
            user_style(),
        )),
        "SESSION_NEEDS_INPUT" => Some((
            "input".to_string(),
            event_preview(event),
            attention_style(AttentionLevel::Action),
        )),
        "SESSION_FAILED" => Some((
            "failed".to_string(),
            event_preview(event),
            attention_style(AttentionLevel::Action),
        )),
        "SESSION_PLAN_READY" => Some((
            "plan".to_string(),
            event.payload_json
                .get("summary")
                .and_then(Value::as_str)
                .unwrap_or("plan ready")
                .to_string(),
            notice_style(),
        )),
        "SESSION_PLAN_MISSING" => Some((
            "plan".to_string(),
            "planning finished without a final plan".to_string(),
            attention_style(AttentionLevel::Action),
        )),
        "SESSION_FINISHED" if show_verbose => Some((
            "finish".to_string(),
            event_preview(event),
            notice_style(),
        )),
        _ => {
            if let Some(item) = codex_item(&event.payload_json) {
                match item_type(item) {
                    Some("agent_message") => Some((
                        "agent".to_string(),
                        item.get("text")
                            .and_then(Value::as_str)
                            .unwrap_or_default()
                            .to_string(),
                        assistant_style(),
                    )),
                    Some("command_execution") => Some((
                        "tool".to_string(),
                        item.get("command")
                            .and_then(Value::as_str)
                            .unwrap_or_default()
                            .to_string(),
                        tool_style(),
                    )),
                    Some("file_change") => Some((
                        "files".to_string(),
                        event_preview(event),
                        file_change_style(),
                    )),
                    _ if show_verbose => Some((
                        event.event_type.to_ascii_lowercase(),
                        event_preview(event),
                        fallback_style(),
                    )),
                    _ => None,
                }
            } else if show_verbose {
                Some((
                    event.event_type.to_ascii_lowercase(),
                    event_preview(event),
                    fallback_style(),
                ))
            } else {
                None
            }
        }
    }
}

fn codex_item(payload: &Value) -> Option<&Value> {
    payload.get("raw").and_then(|raw| raw.get("item"))
}

fn item_type(item: &Value) -> Option<&str> {
    item.get("type").and_then(Value::as_str)
}

fn event_preview(event: &agentd_shared::event::SessionEvent) -> String {
    for key in ["question_preview", "question", "error", "prompt_preview", "prompt", "data", "rendered", "text"] {
        if let Some(value) = event.payload_json.get(key).and_then(Value::as_str) {
            return preview_text(value, 120);
        }
    }
    if let Some(item) = codex_item(&event.payload_json) {
        for key in ["text", "command", "status"] {
            if let Some(value) = item.get(key).and_then(Value::as_str) {
                return preview_text(value, 120);
            }
        }
    }
    preview_text(&event.payload_json.to_string(), 120)
}

fn preview_text(text: &str, max_chars: usize) -> String {
    let trimmed = text.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut preview = trimmed.chars().take(max_chars).collect::<String>();
    if trimmed.chars().count() > max_chars {
        preview.push_str("...");
    }
    preview
}

fn looks_like_diff_with_lang(language: &Option<String>, text: &str) -> bool {
    matches!(language.as_deref(), Some("diff" | "patch")) || looks_like_diff(text)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DiffLineKind {
    Header,
    Hunk,
    Add,
    Delete,
    Plain,
}

fn looks_like_diff(text: &str) -> bool {
    let mut diff_markers = 0;
    for line in text.lines().take(12) {
        let trimmed = line.trim_start();
        if trimmed.starts_with("diff --git")
            || trimmed.starts_with("@@")
            || trimmed.starts_with("+++ ")
            || trimmed.starts_with("--- ")
            || trimmed.starts_with('+')
            || trimmed.starts_with('-')
        {
            diff_markers += 1;
        }
    }
    diff_markers >= 2
}

fn classify_diff_line(line: &str) -> DiffLineKind {
    let trimmed = line.trim_start();
    if trimmed.starts_with("diff --git") || trimmed.starts_with("+++ ") || trimmed.starts_with("--- ")
    {
        DiffLineKind::Header
    } else if trimmed.starts_with("@@") {
        DiffLineKind::Hunk
    } else if trimmed.starts_with('+') {
        DiffLineKind::Add
    } else if trimmed.starts_with('-') {
        DiffLineKind::Delete
    } else {
        DiffLineKind::Plain
    }
}

fn diff_line_style(line: &str) -> Style {
    match classify_diff_line(line) {
        DiffLineKind::Header => diff_header_style(),
        DiffLineKind::Hunk => diff_hunk_style(),
        DiffLineKind::Add => diff_add_style(),
        DiffLineKind::Delete => diff_delete_style(),
        DiffLineKind::Plain => code_block_style(),
    }
}

fn diff_color_enabled() -> bool {
    should_colorize_diff_output(std::io::stdout().is_terminal(), std::env::var_os("NO_COLOR"))
}

fn should_colorize_diff_output(
    is_terminal: bool,
    no_color: Option<std::ffi::OsString>,
) -> bool {
    is_terminal && no_color.is_none()
}

fn render_diff_text(text: &str, color_enabled: bool) -> String {
    if !color_enabled {
        return text.to_string();
    }

    let mut rendered = String::new();
    for chunk in text.split_inclusive('\n') {
        let line = chunk.strip_suffix('\n').unwrap_or(chunk);
        let has_newline = chunk.ends_with('\n');
        rendered.push_str(&render_diff_line(line, color_enabled));
        if has_newline {
            rendered.push('\n');
        }
    }
    if text.is_empty() {
        return rendered;
    }
    if !text.ends_with('\n') && rendered.ends_with('\n') {
        rendered.pop();
    }
    rendered
}

fn render_diff_line(line: &str, color_enabled: bool) -> String {
    if !color_enabled {
        return line.to_string();
    }

    match classify_diff_line(line) {
        DiffLineKind::Header => format!(
            "{}",
            line.with(CrosColor::Rgb {
                r: 153,
                g: 214,
                b: 255
            })
            .attribute(CrosAttribute::Bold)
        ),
        DiffLineKind::Hunk => format!(
            "{}",
            line.with(CrosColor::Rgb {
                r: 242,
                g: 201,
                b: 76
            })
            .attribute(CrosAttribute::Bold)
        ),
        DiffLineKind::Add => format!(
            "{}",
            line.with(CrosColor::Rgb {
                r: 111,
                g: 207,
                b: 151
            })
        ),
        DiffLineKind::Delete => format!(
            "{}",
            line.with(CrosColor::Rgb {
                r: 255,
                g: 107,
                b: 107
            })
        ),
        DiffLineKind::Plain => line.to_string(),
    }
}

fn render_focus_dashboard(
    frame: &mut Frame,
    sessions: &[SessionRecord],
    selected: usize,
    selected_session: Option<&SessionRecord>,
    composer: &str,
    selected_model: &str,
    focus: DashboardFocus,
    model_picker_open: bool,
    model_picker_selected: usize,
    discard_confirm_session: Option<&SessionRecord>,
    status: &str,
) {
    let area = frame.area();
    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(3),
            Constraint::Min(8),
            Constraint::Length(5),
            Constraint::Length(1),
            Constraint::Length(2),
        ])
        .split(area);

    let header = Paragraph::new(vec![
        Line::from(vec![
            Span::styled("Codex", accent_style().add_modifier(Modifier::BOLD)),
            Span::raw("  "),
            Span::styled(
                "new task below, sessions above",
                muted_style(),
            ),
        ]),
    ])
    .block(Block::default().borders(Borders::BOTTOM));
    frame.render_widget(header, chunks[0]);

    let items = if sessions.is_empty() {
        vec![ListItem::new(Line::from(vec![Span::styled(
            "No sessions yet.",
            muted_style(),
        )]))]
    } else {
        sessions
            .iter()
            .take(12)
            .enumerate()
            .map(|(index, session)| {
                let selected_style = if index == selected && focus == DashboardFocus::SessionList {
                    accent_style().add_modifier(Modifier::BOLD)
                } else if index == selected {
                    muted_style().add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                let summary = session
                    .attention_summary
                    .as_deref()
                    .unwrap_or(session.task.as_str());
                ListItem::new(vec![
                    Line::from(vec![
                        Span::styled(
                            if index == selected { "› " } else { "  " },
                            selected_style,
                        ),
                        Span::styled(&session.session_id, selected_style),
                        Span::raw("  "),
                        Span::styled(session.attention_string(), attention_style(session.attention)),
                        Span::raw("  "),
                        Span::styled(session.status_string(), muted_style()),
                    ]),
                    Line::from(vec![
                        Span::raw("  "),
                        Span::styled(summary, Style::default().add_modifier(Modifier::BOLD)),
                    ]),
                    Line::from(vec![
                        Span::raw("  "),
                        Span::styled(
                            format!(
                                "{}  {}  {}{}",
                                session.branch,
                                session_mode_label(session.mode),
                                session.agent,
                                session
                                    .model
                                    .as_deref()
                                    .map(|model| format!("  {model}"))
                                    .unwrap_or_default()
                            ),
                            muted_style(),
                        ),
                    ]),
                ])
            })
            .collect::<Vec<_>>()
    };
    frame.render_widget(List::new(items).block(Block::default()), chunks[1]);

    let cursor = if focus == DashboardFocus::Composer {
        blinking_cursor_span()
    } else {
        Span::raw("")
    };
    let composer_chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(1),
            Constraint::Length(1),
            Constraint::Length(1),
            Constraint::Length(1),
            Constraint::Length(1),
        ])
        .split(chunks[2]);
    frame.render_widget(
        Block::default()
            .borders(Borders::TOP | Borders::BOTTOM)
            .style(composer_box_style()),
        chunks[2],
    );
    let composer_prompt = if composer.is_empty() {
        Line::from(vec![
            Span::styled("> ", accent_style()),
            Span::styled(
                "describe the task or type /model or /plan <task>",
                muted_style(),
            ),
            cursor,
        ])
    } else {
        Line::from(vec![
            Span::styled("> ", accent_style()),
            Span::raw(composer),
            cursor,
        ])
    };
    frame.render_widget(
        Paragraph::new(composer_prompt)
            .style(composer_box_style())
            .alignment(Alignment::Left),
        composer_chunks[2],
    );
    frame.render_widget(
        Paragraph::new(Line::from(vec![
            Span::styled("model ", muted_style()),
            Span::styled(selected_model, accent_style()),
        ]))
        .style(composer_box_style()),
        chunks[3],
    );

    let footer = Paragraph::new(dashboard_footer_text(
        focus,
        status,
        selected_session,
        discard_confirm_session.is_some(),
    ))
    .style(muted_style());
    frame.render_widget(footer, chunks[4]);

    if model_picker_open {
        let picker_area = centered_rect(50, 40, area);
        frame.render_widget(WidgetClear, picker_area);
        let model_items = CODEX_MODELS
            .iter()
            .enumerate()
            .map(|(index, model)| {
                let style = if index == model_picker_selected {
                    accent_style().add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                ListItem::new(Line::from(vec![
                    Span::styled(if index == model_picker_selected { "› " } else { "  " }, style),
                    Span::styled(*model, style),
                ]))
            })
            .collect::<Vec<_>>();
        frame.render_widget(
            List::new(model_items).block(
                Block::default()
                    .title(Span::styled("Select Model", muted_style()))
                    .borders(Borders::ALL)
                    .style(composer_box_style()),
            ),
            picker_area,
        );
    }

    if let Some(session) = discard_confirm_session {
        render_confirmation_modal(
            frame,
            area,
            "Discard Changes",
            &format!(
                "Discard pending changes for {}?\n\nEnter confirms. Esc cancels.",
                session.session_id
            ),
        );
    }
}

fn render_focus_session(
    frame: &mut Frame,
    session: &SessionRecord,
    view_state: &FocusSessionViewState,
    status: &str,
    discard_confirm_open: bool,
    reply: &str,
    selected_option: usize,
) {
    let area = frame.area();
    let intervention_active = session.status == SessionStatus::NeedsInput || !reply.is_empty();
    let layout = focus_session_layout(area, intervention_active);
    let outer = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(5),
            Constraint::Min(10),
            if intervention_active {
                Constraint::Length(8)
            } else {
                Constraint::Length(0)
            },
            Constraint::Length(2),
        ])
        .split(area);

    let mut header_lines = vec![Line::from(vec![
        Span::styled("Codex", accent_style().add_modifier(Modifier::BOLD)),
        Span::raw("  "),
        Span::styled(&session.session_id, Style::default().add_modifier(Modifier::BOLD)),
        Span::raw("  "),
        Span::styled(session_mode_label(session.mode), notice_style()),
        Span::raw("  "),
        Span::styled(session.attention_string(), attention_style(session.attention)),
        Span::raw("  "),
        Span::styled(session.status_string(), muted_style()),
    ])];
    header_lines.push(Line::from(vec![
        Span::styled(&session.task, Style::default().add_modifier(Modifier::BOLD)),
        Span::raw("  "),
        Span::styled(&session.branch, muted_style()),
        Span::raw("  "),
        Span::styled(
            session.model.as_deref().unwrap_or("default model"),
            muted_style(),
        ),
    ]));
    if let Some(summary) = &session.attention_summary {
        header_lines.push(Line::from(vec![
            Span::styled("summary", muted_style()),
            Span::raw("  "),
            Span::raw(summary),
        ]));
    } else {
        header_lines.push(Line::from(""));
    }
    frame.render_widget(
        Paragraph::new(header_lines).block(Block::default().borders(Borders::BOTTOM)),
        outer[0],
    );

    frame.render_widget(
        Paragraph::new(view_state.transcript_lines.clone())
            .block(
                Block::default()
                    .title(Span::styled(
                        if view_state.show_verbose {
                            "narrative [verbose]"
                        } else {
                            "narrative"
                        },
                        pane_title_style(view_state.active_pane == FocusSessionPane::Transcript),
                    ))
                    .borders(Borders::RIGHT),
            )
            .scroll((view_state.transcript_scroll as u16, 0))
            .wrap(Wrap { trim: false }),
        layout.transcript_rect,
    );

    frame.render_widget(
        Paragraph::new(view_state.activity_lines.clone())
            .scroll((view_state.activity_scroll as u16, 0))
            .wrap(Wrap { trim: false })
            .block(
            Block::default()
                .borders(Borders::LEFT)
                .title(Span::styled(
                    "activity",
                    pane_title_style(view_state.active_pane == FocusSessionPane::Activity),
                )),
        ),
        layout.activity_rect,
    );

    if let Some(intervention_rect) = layout.intervention_rect {
        render_intervention_pane(
            frame,
            intervention_rect,
            view_state,
            reply,
            selected_option,
        );
    }

    let footer = Paragraph::new(focus_session_footer_text(
        session,
        status,
        discard_confirm_open,
    ))
    .style(muted_style())
    .block(Block::default().borders(Borders::TOP));
    frame.render_widget(footer, outer[2]);

    if discard_confirm_open {
        render_confirmation_modal(
            frame,
            area,
            "Discard Changes",
            &format!(
                "Discard pending changes for {}?\n\nEnter confirms. Esc cancels.",
                session.session_id
            ),
        );
    }

}

fn render_intervention_pane(
    frame: &mut Frame,
    area: Rect,
    view_state: &FocusSessionViewState,
    reply: &str,
    selected_option: usize,
) {
    frame.render_widget(
        Block::default()
            .title(Span::styled(
                "Needs Input",
                pane_title_style(view_state.active_pane == FocusSessionPane::Intervention),
            ))
            .borders(Borders::TOP | Borders::RIGHT),
        area,
    );

    let option_rows = if view_state.needs_input_options.is_empty() {
        0
    } else {
        view_state.needs_input_options.len().min(3) as u16
    };
    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .margin(1)
        .constraints([
            Constraint::Min(2),
            Constraint::Length(option_rows),
            Constraint::Length(2),
        ])
        .split(area);

    let question = view_state
        .needs_input_question
        .as_deref()
        .unwrap_or("Waiting for input.");
    frame.render_widget(
        Paragraph::new(question)
            .wrap(Wrap { trim: false })
            .style(Style::default()),
        chunks[0],
    );

    if option_rows > 0 {
        let items = view_state
            .needs_input_options
            .iter()
            .take(option_rows as usize)
            .enumerate()
            .map(|(index, option)| {
                let style = if index == selected_option {
                    accent_style().add_modifier(Modifier::BOLD)
                } else {
                    Style::default()
                };
                ListItem::new(Line::from(vec![
                    Span::styled(if index == selected_option { "› " } else { "  " }, style),
                    Span::styled(option.clone(), style),
                ]))
            })
            .collect::<Vec<_>>();
        frame.render_widget(List::new(items), chunks[1]);
    }

    let composer = if reply.is_empty() {
        Line::from(vec![
            Span::styled("> ", accent_style()),
            Span::styled("type a reply or choose an option", muted_style()),
            if view_state.active_pane == FocusSessionPane::Intervention {
                blinking_cursor_span()
            } else {
                Span::raw("")
            },
        ])
    } else {
        Line::from(vec![
            Span::styled("> ", accent_style()),
            Span::raw(reply.to_string()),
            if view_state.active_pane == FocusSessionPane::Intervention {
                blinking_cursor_span()
            } else {
                Span::raw("")
            },
        ])
    };
    frame.render_widget(
        Paragraph::new(composer).style(composer_box_style()),
        chunks[2],
    );
}

fn dashboard_footer_text(
    focus: DashboardFocus,
    status: &str,
    selected_session: Option<&SessionRecord>,
    discard_confirm_open: bool,
) -> String {
    if !status.is_empty() {
        return status.to_string();
    }
    if discard_confirm_open {
        return "Enter confirm discard  Esc cancel".to_string();
    }

    match focus {
        DashboardFocus::Composer => {
            "Enter create  /model picker  /plan task  Tab sessions  Esc clear/quit".to_string()
        }
        DashboardFocus::SessionList => {
            if selected_session.is_some_and(session_has_pending_review) {
                "Enter open  ↑↓ move  A accept  D discard  r retry  k kill  q quit  Tab composer"
                    .to_string()
            } else {
                "Enter open  ↑↓ move  r retry  k kill  q quit  Tab composer".to_string()
            }
        }
    }
}

fn focus_session_footer_text(
    session: &SessionRecord,
    status: &str,
    discard_confirm_open: bool,
) -> String {
    if !status.is_empty() {
        return format!("Status: {status}");
    }
    if discard_confirm_open {
        return "Enter confirm discard  Esc cancel".to_string();
    }

    let mut footer = "Tab pane  Wheel scroll".to_string();
    if session.status == SessionStatus::NeedsInput {
        footer.push_str("  y intervene  ↑↓ options  Enter prefill/send  Esc clear");
    } else {
        footer.push_str("  ↑↓ PgUp PgDn Home End  v verbose  a attach");
    }
    if session_has_pending_review(session) {
        footer.push_str("  A accept  D discard");
    }
    footer.push_str("  r retry  k kill  q back");
    footer
}

fn render_confirmation_modal(frame: &mut Frame, area: Rect, title: &str, body: &str) {
    let modal_area = centered_rect(60, 20, area);
    frame.render_widget(WidgetClear, modal_area);
    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([Constraint::Length(1), Constraint::Min(3), Constraint::Length(1)])
        .split(modal_area);
    frame.render_widget(
        Block::default()
            .title(Span::styled(title.to_string(), muted_style()))
            .borders(Borders::ALL)
            .style(composer_box_style()),
        modal_area,
    );
    frame.render_widget(
        Paragraph::new(body.to_string())
            .style(composer_box_style())
            .wrap(Wrap { trim: false }),
        chunks[1],
    );
}

fn pane_title_style(active: bool) -> Style {
    if active {
        accent_style().add_modifier(Modifier::BOLD)
    } else {
        muted_style()
    }
}

fn composer_box_style() -> Style {
    Style::default().bg(CODEX_COMPOSER_BG).fg(Color::White)
}

fn accent_style() -> Style {
    Style::default().fg(Color::Rgb(84, 173, 255))
}

fn muted_style() -> Style {
    Style::default().fg(Color::Rgb(118, 131, 143))
}

fn notice_style() -> Style {
    Style::default().fg(Color::Rgb(242, 201, 76))
}

fn assistant_style() -> Style {
    Style::default()
        .fg(Color::Rgb(223, 232, 255))
        .bg(Color::Rgb(30, 49, 87))
}

fn user_style() -> Style {
    Style::default()
        .fg(Color::Rgb(16, 28, 34))
        .bg(Color::Rgb(104, 211, 145))
}

fn tool_style() -> Style {
    Style::default()
        .fg(Color::Rgb(29, 21, 7))
        .bg(Color::Rgb(255, 191, 71))
}

fn file_change_style() -> Style {
    Style::default()
        .fg(Color::Rgb(229, 244, 255))
        .bg(Color::Rgb(23, 78, 115))
}

fn fallback_style() -> Style {
    Style::default()
        .fg(Color::Rgb(229, 229, 229))
        .bg(Color::Rgb(66, 66, 66))
}

fn code_header_style() -> Style {
    Style::default()
        .fg(Color::Rgb(212, 239, 255))
        .bg(Color::Rgb(25, 45, 65))
}

fn code_block_style() -> Style {
    Style::default().fg(Color::Rgb(206, 214, 222))
}

fn inline_code_style() -> Style {
    Style::default()
        .fg(Color::Rgb(153, 214, 255))
        .add_modifier(Modifier::BOLD)
}

fn diff_header_style() -> Style {
    Style::default()
        .fg(Color::Rgb(153, 214, 255))
        .add_modifier(Modifier::BOLD)
}

fn diff_hunk_style() -> Style {
    Style::default()
        .fg(Color::Rgb(242, 201, 76))
        .add_modifier(Modifier::BOLD)
}

fn diff_add_style() -> Style {
    Style::default().fg(Color::Rgb(111, 207, 151))
}

fn diff_delete_style() -> Style {
    Style::default().fg(Color::Rgb(255, 107, 107))
}

fn attention_style(attention: AttentionLevel) -> Style {
    match attention {
        AttentionLevel::Info => muted_style(),
        AttentionLevel::Notice => notice_style(),
        AttentionLevel::Action => Style::default()
            .fg(Color::Rgb(255, 107, 107))
            .add_modifier(Modifier::BOLD),
    }
}

fn blinking_cursor_span() -> Span<'static> {
    if cursor_visible() {
        Span::styled("|", accent_style().add_modifier(Modifier::BOLD))
    } else {
        Span::styled(" ", composer_box_style())
    }
}

fn cursor_visible() -> bool {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| (duration.as_millis() / 500) % 2 == 0)
        .unwrap_or(true)
}

async fn kill_session(paths: &AppPaths, session_id: &str) -> Result<()> {
    let response = send_request(
        paths,
        &Request::KillSession {
            session_id: session_id.to_string(),
            remove: false,
        },
    )
    .await?;
    match response {
        Response::KillSession { .. } => Ok(()),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn accept_session(paths: &AppPaths, session_id: &str) -> Result<SessionRecord> {
    let response = send_request(
        paths,
        &Request::ApplySession {
            session_id: session_id.to_string(),
        },
    )
    .await?;
    match response {
        Response::Session { session } => Ok(session),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn discard_session(
    paths: &AppPaths,
    session_id: &str,
    force: bool,
) -> Result<SessionRecord> {
    let response = send_request(
        paths,
        &Request::DiscardSession {
            session_id: session_id.to_string(),
            force,
        },
    )
    .await?;
    match response {
        Response::Session { session } => Ok(session),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn reply_session(paths: &AppPaths, session_id: &str, prompt: &str) -> Result<SessionRecord> {
    let response = send_request(
        paths,
        &Request::ReplyToSession {
            session_id: session_id.to_string(),
            prompt: prompt.to_string(),
        },
    )
    .await?;
    match response {
        Response::Session { session } => Ok(session),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn retry_session(paths: &AppPaths, session: &SessionRecord) -> Result<SessionRecord> {
    let response = send_request(
        paths,
        &Request::CreateSession {
            workspace: session.workspace.clone(),
            task: session.task.clone(),
            agent: session.agent.clone(),
            model: session.model.clone(),
            mode: session.mode,
        },
    )
    .await?;
    let created = match response {
        Response::CreateSession { session } => session,
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    };
    daemon_get_session(paths, &created.session_id).await
}

async fn create_dashboard_session(
    paths: &AppPaths,
    task: &str,
    model: Option<&str>,
    mode: SessionMode,
) -> Result<SessionRecord> {
    let workspace = std::env::current_dir().context("failed to determine current directory")?;
    let response = send_request(
        paths,
        &Request::CreateSession {
            workspace: workspace.to_string_lossy().to_string(),
            task: task.to_string(),
            agent: "codex".to_string(),
            model: model.map(str::to_string),
            mode,
        },
    )
    .await?;
    let created = match response {
        Response::CreateSession { session } => session,
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    };
    daemon_get_session(paths, &created.session_id).await
}

fn centered_rect(percent_x: u16, percent_y: u16, area: Rect) -> Rect {
    let popup = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Percentage((100 - percent_y) / 2),
            Constraint::Percentage(percent_y),
            Constraint::Percentage((100 - percent_y) / 2),
        ])
        .split(area);
    Layout::default()
        .direction(Direction::Horizontal)
        .constraints([
            Constraint::Percentage((100 - percent_x) / 2),
            Constraint::Percentage(percent_x),
            Constraint::Percentage((100 - percent_x) / 2),
        ])
        .split(popup[1])[1]
}

async fn maybe_switch_attached_session(
    paths: &AppPaths,
    sessions: &[SessionRecord],
) -> Result<bool> {
    let Some(current_session_id) = in_session_switch_context() else {
        return Ok(false);
    };

    let choices = sessions
        .iter()
        .filter(|session| {
            session.status == SessionStatus::Running && session.session_id != current_session_id
        })
        .cloned()
        .collect::<Vec<_>>();

    if choices.is_empty() {
        return Ok(true);
    }

    let Some(selected) = pick_session(&choices)? else {
        return Ok(true);
    };

    let response = send_request(
        paths,
        &Request::SwitchAttachedSession {
            source_session_id: current_session_id,
            target_session_id: selected.session_id,
        },
    )
    .await?;
    match response {
        Response::Ok => Ok(true),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

fn in_session_switch_context() -> Option<String> {
    if !std::io::stdin().is_terminal() || !std::io::stdout().is_terminal() {
        return None;
    }
    std::env::var("AGENTD_SESSION_ID").ok()
}

fn pick_session(sessions: &[SessionRecord]) -> Result<Option<SessionRecord>> {
    let _raw_mode = RawModeGuard::new()?;
    let _screen = TerminalScreenGuard::enter()?;
    let mut stdout = std::io::stdout();
    let mut query = String::new();
    let mut selected = 0_usize;

    loop {
        let matches = filtered_sessions(sessions, &query);
        if selected >= matches.len() && !matches.is_empty() {
            selected = matches.len() - 1;
        }
        render_session_picker(&mut stdout, &query, &matches, selected)?;

        let Event::Key(key) = event::read().context("failed to read terminal input")? else {
            continue;
        };
        if key.kind != KeyEventKind::Press {
            continue;
        }

        match key.code {
            KeyCode::Esc => return Ok(None),
            KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => return Ok(None),
            KeyCode::Enter => {
                if let Some((_, session)) = matches.get(selected) {
                    return Ok(Some((*session).clone()));
                }
            }
            KeyCode::Up => {
                selected = selected.saturating_sub(1);
            }
            KeyCode::Down => {
                if selected + 1 < matches.len() {
                    selected += 1;
                }
            }
            KeyCode::Backspace => {
                query.pop();
                selected = 0;
            }
            KeyCode::Char(ch)
                if key.modifiers.is_empty() || key.modifiers == KeyModifiers::SHIFT =>
            {
                query.push(ch);
                selected = 0;
            }
            _ => {}
        }
    }
}

fn render_session_picker(
    stdout: &mut std::io::Stdout,
    query: &str,
    matches: &[(i64, &SessionRecord)],
    selected: usize,
) -> Result<()> {
    execute!(stdout, MoveTo(0, 0), Clear(ClearType::All))?;
    writeln!(stdout, "Switch session")?;
    writeln!(stdout, "Query: {query}")?;
    writeln!(stdout, "Enter switches, Esc cancels")?;
    writeln!(stdout)?;

    if matches.is_empty() {
        writeln!(stdout, "No matching running sessions.")?;
    } else {
        for (index, (_, session)) in matches.iter().take(10).enumerate() {
            let marker = if index == selected { ">" } else { " " };
            writeln!(
                stdout,
                "{marker} {}  {}  {}  {}",
                session.session_id,
                session.agent,
                session.status_string(),
                session.branch
            )?;
            writeln!(stdout, "  {}", session.task)?;
        }
    }

    stdout.flush()?;
    Ok(())
}

fn filtered_sessions<'a>(
    sessions: &'a [SessionRecord],
    query: &str,
) -> Vec<(i64, &'a SessionRecord)> {
    let mut matches = sessions
        .iter()
        .filter_map(|session| {
            fuzzy_score(&session_search_text(session), query).map(|score| (score, session))
        })
        .collect::<Vec<_>>();
    matches.sort_by(|left, right| {
        right
            .0
            .cmp(&left.0)
            .then_with(|| left.1.created_at.cmp(&right.1.created_at))
    });
    matches
}

fn session_search_text(session: &SessionRecord) -> String {
    format!(
        "{} {} {} {} {}",
        session.session_id, session.agent, session.branch, session.task, session.workspace
    )
}

fn fuzzy_score(haystack: &str, needle: &str) -> Option<i64> {
    if needle.is_empty() {
        return Some(0);
    }

    let haystack = haystack.to_lowercase();
    let needle = needle.to_lowercase();
    let mut score = 0_i64;
    let mut last_match = None;
    let mut position = 0_usize;

    for needle_char in needle.chars() {
        let remainder = &haystack[position..];
        let offset = remainder.find(needle_char)?;
        let absolute = position + offset;
        score += 10;
        if let Some(previous) = last_match {
            if absolute == previous + needle_char.len_utf8() {
                score += 15;
            }
            score -= (absolute.saturating_sub(previous + 1)) as i64;
        } else {
            score -= absolute as i64;
        }
        last_match = Some(absolute);
        position = absolute + needle_char.len_utf8();
    }

    Some(score)
}

#[cfg(test)]
mod tests {
    use super::{
        AGENTD_ATTACH_RESTORE_SEQUENCE, AttachInput, AttachKeyBindingParser, Cli, Command,
        DaemonCommand, DashboardComposerAction, DashboardFocus, FocusSessionLayout,
        FocusSessionPane, FocusSessionViewState, SessionEndSummary, apply_scroll_delta,
        clamp_focus_session_scroll, dashboard_footer_text, focus_session_footer_text,
        format_session_end_summary, looks_like_diff, pane_at_position,
        parse_dashboard_composer_action, parse_needs_input_options, parse_rich_blocks, render_diff_text,
        resolve_detach_session_id, resolve_new_session_options, should_colorize_diff_output,
    };
    use agentd_shared::session::{
        AttentionLevel, IntegrationState, SessionMode, SessionRecord, SessionStatus,
    };
    use chrono::Utc;
    use clap::Parser;
    use crossterm::event::{Event, KeyCode, KeyEvent, KeyEventKind, KeyEventState, KeyModifiers};
    use ratatui::layout::Rect;
    use std::{ffi::OsString, path::PathBuf};

    #[test]
    fn new_command_parses_optional_positional_task() {
        let cli = Cli::try_parse_from(["agent", "new", "fix failing tests"]).unwrap();
        match cli.command {
            Some(Command::New {
                task,
                workspace,
                agent,
            }) => {
                assert_eq!(task.as_deref(), Some("fix failing tests"));
                assert!(workspace.is_none());
                assert!(agent.is_none());
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn new_command_parses_optional_flags() {
        let cli = Cli::try_parse_from([
            "agent",
            "new",
            "--workspace",
            "/tmp/repo",
            "--agent",
            "claude",
            "fix",
        ])
        .unwrap();
        match cli.command {
            Some(Command::New {
                task,
                workspace,
                agent,
            }) => {
                assert_eq!(task.as_deref(), Some("fix"));
                assert_eq!(workspace, Some(PathBuf::from("/tmp/repo")));
                assert_eq!(agent.as_deref(), Some("claude"));
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn resolve_new_session_options_uses_defaults() {
        let options = resolve_new_session_options(None, None, None).unwrap();
        assert_eq!(options.workspace, std::env::current_dir().unwrap());
        assert_eq!(options.task, "");
        assert_eq!(options.agent, "codex");
    }

    #[test]
    fn resolve_new_session_options_preserves_explicit_values() {
        let options = resolve_new_session_options(
            Some(PathBuf::from("/tmp/repo")),
            Some("fix tests".to_string()),
            Some("claude".to_string()),
        )
        .unwrap();
        assert_eq!(options.workspace, PathBuf::from("/tmp/repo"));
        assert_eq!(options.task, "fix tests");
        assert_eq!(options.agent, "claude");
    }

    #[test]
    fn detach_command_parses_optional_session_id() {
        let cli = Cli::try_parse_from(["agent", "detach", "demo"]).unwrap();
        match cli.command {
            Some(Command::Detach { session_id }) => {
                assert_eq!(session_id.as_deref(), Some("demo"));
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn daemon_restart_parses_force_flag() {
        let cli = Cli::try_parse_from(["agent", "daemon", "restart", "--force"]).unwrap();
        match cli.command {
            Some(Command::Daemon {
                command: DaemonCommand::Restart { force },
            }) => assert!(force),
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn daemon_upgrade_parses() {
        let cli = Cli::try_parse_from(["agent", "daemon", "upgrade"]).unwrap();
        match cli.command {
            Some(Command::Daemon {
                command: DaemonCommand::Upgrade,
            }) => {}
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn resolve_detach_session_id_prefers_explicit_value() {
        unsafe {
            std::env::set_var("AGENTD_SESSION_ID", "env-session");
        }
        let session_id = resolve_detach_session_id(Some("explicit-session".to_string())).unwrap();
        assert_eq!(session_id, "explicit-session");
    }

    #[test]
    fn resolve_detach_session_id_uses_environment() {
        unsafe {
            std::env::set_var("AGENTD_SESSION_ID", "env-session");
        }
        let session_id = resolve_detach_session_id(None).unwrap();
        assert_eq!(session_id, "env-session");
    }

    #[test]
    fn resolve_detach_session_id_errors_without_environment() {
        unsafe {
            std::env::remove_var("AGENTD_SESSION_ID");
        }
        let err = resolve_detach_session_id(None).unwrap_err();
        assert!(
            err.to_string()
                .contains("only works inside a managed session")
        );
    }

    #[test]
    fn attach_parser_detaches_on_ctrl_right_bracket() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(
                KeyCode::Char(']'),
                KeyModifiers::CONTROL
            ))),
            Some(AttachInput::Detach)
        ));
    }

    #[test]
    fn attach_parser_emits_printable_bytes() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Char('x'), KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == b"x"
        ));
    }

    #[test]
    fn attach_parser_encodes_special_keys() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Enter, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == b"\r"
        ));
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Backspace, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == [0x7f]
        ));
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Tab, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == b"\t"
        ));
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Esc, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == [0x1b]
        ));
    }

    #[test]
    fn attach_parser_encodes_navigation_keys_as_ansi() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Up, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == b"\x1b[A"
        ));
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(KeyCode::Left, KeyModifiers::NONE))),
            Some(AttachInput::Data(data)) if data == b"\x1b[D"
        ));
    }

    #[test]
    fn attach_parser_forwards_paste_bytes() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Paste("hello".to_string())),
            Some(AttachInput::Data(data)) if data == b"hello"
        ));
    }

    #[test]
    fn attach_parser_ignores_non_press_events() {
        let mut parser = AttachKeyBindingParser;
        assert!(
            parser
                .parse_event(Event::Key(KeyEvent {
                    code: KeyCode::Char('x'),
                    modifiers: KeyModifiers::NONE,
                    kind: KeyEventKind::Release,
                    state: KeyEventState::empty(),
                }))
                .is_none()
        );
    }

    fn key_event(code: KeyCode, modifiers: KeyModifiers) -> KeyEvent {
        KeyEvent {
            code,
            modifiers,
            kind: KeyEventKind::Press,
            state: KeyEventState::empty(),
        }
    }

    #[test]
    fn attach_parser_encodes_alt_modified_input() {
        let mut parser = AttachKeyBindingParser;
        assert!(matches!(
            parser.parse_event(Event::Key(key_event(
                KeyCode::Char('x'),
                KeyModifiers::ALT
            ))),
            Some(AttachInput::Data(data)) if data == b"\x1bx"
        ));
    }

    #[test]
    fn attach_restore_sequence_matches_agentd_cleanup() {
        assert_eq!(
            AGENTD_ATTACH_RESTORE_SEQUENCE,
            b"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[?1049l\x1b[<u\x1b[?25h"
        );
    }

    #[test]
    fn format_session_end_summary_reports_exit_code() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            integration_state: IntegrationState::Clean,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(0),
            error: None,
        };
        assert_eq!(
            format_session_end_summary(&summary),
            "session demo finished (exit 0)"
        );
    }

    #[test]
    fn format_session_end_summary_reports_failure_message() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Failed,
            integration_state: IntegrationState::Clean,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(1),
            error: Some("spawn failed".to_string()),
        };
        assert_eq!(
            format_session_end_summary(&summary),
            "session demo failed: spawn failed"
        );
    }

    #[test]
    fn format_session_end_summary_reports_pending_review_actions() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            integration_state: IntegrationState::PendingReview,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(0),
            error: None,
        };
        assert!(format_session_end_summary(&summary).contains("agent accept demo"));
    }

    #[test]
    fn apply_scroll_delta_clamps_to_bounds() {
        assert_eq!(apply_scroll_delta(5, 10, -10), 0);
        assert_eq!(apply_scroll_delta(5, 10, 20), 10);
        assert_eq!(apply_scroll_delta(5, 10, 2), 7);
    }

    #[test]
    fn clamp_focus_session_scroll_clamps_both_panes() {
        let mut state = FocusSessionViewState {
            transcript_lines: vec![ratatui::text::Line::from("x"); 40],
            transcript_line_count: 40,
            transcript_scroll: 99,
            events: vec![demo_event(1), demo_event(2), demo_event(3)],
            activity_lines: vec![ratatui::text::Line::from("x"); 6],
            activity_line_count: 6,
            activity_scroll: 99,
            ..Default::default()
        };
        let layout = FocusSessionLayout {
            transcript_rect: Rect::new(0, 0, 10, 10),
            activity_rect: Rect::new(10, 0, 10, 10),
            transcript_height: 6,
            activity_height: 4,
            intervention_rect: None,
        };

        clamp_focus_session_scroll(&mut state, &layout);

        assert_eq!(state.transcript_scroll, 34);
        assert_eq!(state.activity_scroll, 2);
    }

    #[test]
    fn pane_at_position_returns_matching_pane() {
        let layout = FocusSessionLayout {
            transcript_rect: Rect::new(0, 0, 20, 10),
            activity_rect: Rect::new(20, 0, 10, 10),
            transcript_height: 8,
            activity_height: 8,
            intervention_rect: None,
        };

        assert_eq!(
            pane_at_position(&layout, 5, 5),
            Some(FocusSessionPane::Transcript)
        );
        assert_eq!(
            pane_at_position(&layout, 25, 5),
            Some(FocusSessionPane::Activity)
        );
        assert_eq!(pane_at_position(&layout, 31, 5), None);
    }

    #[test]
    fn rich_text_parser_detects_fenced_diff_blocks() {
        let blocks = parse_rich_blocks("before\n```diff\n+added\n-removed\n```\nafter");
        assert_eq!(blocks.len(), 3);
        assert!(matches!(&blocks[1], super::RichTextBlock::Diff(text) if text.contains("+added")));
    }

    #[test]
    fn diff_detection_accepts_unified_diff_markers() {
        assert!(looks_like_diff(
            "diff --git a/demo b/demo\n--- a/demo\n+++ b/demo\n@@ -1 +1 @@\n-old\n+new"
        ));
        assert!(!looks_like_diff("plain output\nwith no patch markers"));
    }

    #[test]
    fn diff_colorization_is_disabled_without_terminal() {
        assert!(!should_colorize_diff_output(false, None));
    }

    #[test]
    fn diff_colorization_respects_no_color() {
        assert!(!should_colorize_diff_output(
            true,
            Some(OsString::from("1"))
        ));
    }

    #[test]
    fn render_diff_text_keeps_plain_output_without_color() {
        let diff = "@@ -1 +1 @@\n-old\n+new\n";
        assert_eq!(render_diff_text(diff, false), diff);
    }

    #[test]
    fn render_diff_text_adds_ansi_when_enabled() {
        let rendered = render_diff_text("@@ -1 +1 @@\n-old\n+new\n", true);
        assert!(rendered.contains("\u{1b}["));
    }

    #[test]
    fn dashboard_retry_shortcut_does_not_trigger_in_composer() {
        let focus = DashboardFocus::Composer;
        let model_picker_open = false;
        let should_retry = matches!(
            KeyCode::Char('r'),
            KeyCode::Char('r')
                if focus == DashboardFocus::SessionList
                    && !model_picker_open
                    && Some(()).is_some()
        );
        assert!(!should_retry);
    }

    #[test]
    fn dashboard_footer_advertises_accept_and_discard_for_pending_review() {
        let session = demo_session(SessionStatus::Exited, IntegrationState::PendingReview);
        let footer = dashboard_footer_text(
            DashboardFocus::SessionList,
            "",
            Some(&session),
            false,
        );
        assert!(footer.contains("A accept"));
        assert!(footer.contains("D discard"));
    }

    #[test]
    fn dashboard_footer_shows_discard_confirmation_controls() {
        let footer = dashboard_footer_text(DashboardFocus::SessionList, "", None, true);
        assert_eq!(footer, "Enter confirm discard  Esc cancel");
    }

    #[test]
    fn dashboard_composer_parses_plan_command() {
        assert_eq!(
            parse_dashboard_composer_action("/plan add a plan mode"),
            Some(DashboardComposerAction::CreatePlanSession(
                "add a plan mode"
            ))
        );
    }

    #[test]
    fn dashboard_composer_rejects_blank_plan_command() {
        assert_eq!(
            parse_dashboard_composer_action("/plan"),
            Some(DashboardComposerAction::InvalidCommand(
                "usage: /plan <task>"
            ))
        );
    }

    #[test]
    fn parse_needs_input_options_reads_numbered_choices() {
        let options = parse_needs_input_options(
            "Which mode?\n1. Fast path\n2. Safe path\n3. Hybrid path",
        );
        assert_eq!(options, vec!["Fast path", "Safe path", "Hybrid path"]);
    }

    #[test]
    fn parse_needs_input_options_reads_bulleted_choices() {
        let options = parse_needs_input_options(
            "Choose one:\n- Keep current behavior\n- Redesign the flow",
        );
        assert_eq!(options, vec!["Keep current behavior", "Redesign the flow"]);
    }

    #[test]
    fn focus_footer_advertises_accept_and_discard_for_pending_review() {
        let session = demo_session(SessionStatus::Exited, IntegrationState::PendingReview);
        let footer = focus_session_footer_text(&session, "", false);
        assert!(footer.contains("A accept"));
        assert!(footer.contains("D discard"));
    }

    #[test]
    fn focus_footer_shows_discard_confirmation_controls() {
        let session = demo_session(SessionStatus::Exited, IntegrationState::PendingReview);
        let footer = focus_session_footer_text(&session, "", true);
        assert_eq!(footer, "Enter confirm discard  Esc cancel");
    }

    fn demo_event(id: i64) -> agentd_shared::event::SessionEvent {
        agentd_shared::event::SessionEvent {
            id,
            session_id: "demo".to_string(),
            timestamp: Utc::now(),
            event_type: format!("EVENT_{id}"),
            payload_json: serde_json::json!({}),
        }
    }

    fn demo_session(status: SessionStatus, integration_state: IntegrationState) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: "demo".to_string(),
            thread_id: Some("thread-demo".to_string()),
            agent: "codex".to_string(),
            model: Some("gpt-5.4-codex".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/workspace".to_string(),
            repo_path: "/tmp/workspace".to_string(),
            task: "task".to_string(),
            base_branch: "main".to_string(),
            branch: "agent/task".to_string(),
            worktree: "/tmp/worktree".to_string(),
            status,
            integration_state,
            pid: Some(123),
            exit_code: Some(0),
            error: None,
            attention: AttentionLevel::Notice,
            attention_summary: Some("changes ready to apply".to_string()),
            created_at: now,
            updated_at: now,
            exited_at: Some(now),
        }
    }
}
