use std::{
    fs,
    io::{IsTerminal, Write},
    mem::MaybeUninit,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, bail};
use clap::{ArgAction, Parser, Subcommand};
use crossterm::{
    cursor::{Hide, Show},
    event::{DisableMouseCapture, EnableMouseCapture},
    event::{Event, EventStream, KeyCode, KeyModifiers},
    execute,
    style::{Attribute as CrosAttribute, Color as CrosColor, Stylize},
    terminal::{EnterAlternateScreen, LeaveAlternateScreen, disable_raw_mode, enable_raw_mode},
};
use futures::StreamExt;
use libc::{
    _POSIX_VDISABLE, O_NONBLOCK, TCSAFLUSH, TCSANOW, VLNEXT, VMIN, VQUIT, VTIME, cfmakeraw, fcntl,
    tcgetattr, tcsetattr, termios,
};
use ratatui::layout::{Constraint, Direction, Layout, Rect};
use tokio::{
    io::{BufReader, unix::AsyncFd},
    net::UnixStream,
    signal::unix::{SignalKind, signal},
};

mod local;
mod runtime;
mod tui;

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{
        DaemonInfo, DaemonManagementRequest, DaemonManagementResponse, DaemonManagementStatus,
        PROTOCOL_VERSION, Request, Response, read_daemon_management_response, read_response,
        write_daemon_management_request, write_request,
    },
    session::{
        AttachmentKind, AttachmentRecord, AttentionLevel, IntegrationState, SessionDiff,
        SessionRecord, SessionStatus, WorktreeRecord,
    },
};

use crate::local::{LocalStore, normalize_session, print_log_file, remove_session_artifacts};

const AGENTD_ATTACH_RESTORE_SEQUENCE: &[u8] =
    b"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h";
const AGENTD_ATTACH_CLEAR_SEQUENCE: &[u8] = b"\x1b[2J\x1b[H";
const CODEX_MODELS: &[&str] = &[
    "gpt-5.4",
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
    #[command(hide = true)]
    Runtime {
        session_id: Option<String>,
    },
    New {
        title: Option<String>,
        #[arg(long)]
        workspace: Option<PathBuf>,
        #[arg(long)]
        agent: Option<String>,
    },
    Create {
        #[arg(long)]
        workspace: PathBuf,
        #[arg(long)]
        title: Option<String>,
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
        #[arg(long)]
        attach: Option<String>,
        #[arg(long)]
        all: bool,
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
    Attachments {
        session_id: String,
    },
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
    let execution = resolve_execution_mode(&paths, cli.command.as_ref()).await?;

    match (cli.command, execution) {
        (None, ExecutionMode::Daemon) => {
            if let Some(session_id) = runtime::pick_session(&paths).await? {
                attach_session(&paths, &session_id).await?;
            }
        }
        (None, ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent` requires a compatible daemon");
        }
        (Some(Command::Runtime { session_id }), ExecutionMode::Daemon) => {
            tui::run_runtime_ui(&paths, session_id.as_deref()).await?;
        }
        (Some(Command::Runtime { .. }), ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent runtime` requires a compatible daemon");
        }
        (Some(Command::New { title, workspace, agent }), ExecutionMode::Daemon) => {
            let options = resolve_new_session_options(workspace, title, agent)?;
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: options.workspace.to_string_lossy().to_string(),
                    title: options.title,
                    agent: options.agent,
                    model: None,
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
        (Some(Command::New { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Create { workspace, title, agent }), ExecutionMode::Daemon) => {
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: workspace.to_string_lossy().to_string(),
                    title,
                    agent,
                    model: None,
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
        (Some(Command::Create { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Kill { rm, session_id }), ExecutionMode::Daemon) => {
            let response = send_request(
                &paths,
                &Request::KillSession { session_id: session_id.clone(), remove: rm },
            )
            .await?;

            match response {
                Response::KillSession { removed, was_running } => {
                    print_kill_result(&session_id, was_running, removed)
                }
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Kill { rm, session_id }), ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_kill(&paths, &session_id, rm)?;
        }
        (Some(Command::Attach { session_id }), ExecutionMode::Daemon) => {
            attach_session(&paths, &session_id).await?;
        }
        (Some(Command::Attach { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Detach { session_id, attach, all }), ExecutionMode::Daemon) => {
            let session_id = resolve_detach_session_id(session_id)?;
            if all && attach.is_some() {
                bail!("use either `--all` or `--attach <attach_id>`, not both");
            }
            let request = match (all, attach) {
                (true, None) => {
                    Request::DetachSession { session_id: session_id.clone(), all: true }
                }
                (false, Some(attach_id)) => {
                    Request::DetachAttachment { session_id: session_id.clone(), attach_id }
                }
                (false, None) => bail!(
                    "shared attach requires either `--all` or `--attach <attach_id>`; use Ctrl-] to detach the local client"
                ),
                (true, Some(_)) => unreachable!(),
            };
            let response = send_request(&paths, &request).await?;

            match response {
                Response::Ok => println!("detached from {session_id}"),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Detach { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (
            Some(Command::SendInput { session_id, source_session_id, data }),
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
        (Some(Command::SendInput { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Reply { session_id, prompt }), ExecutionMode::Daemon) => {
            let session = reply_session(&paths, &session_id, &prompt.join(" ")).await?;
            print_session(&session);
        }
        (Some(Command::Reply { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Accept { session_id }), ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::ApplySession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Accept { .. }), ExecutionMode::Local(reason)) => {
            bail_daemon_command(&reason, "agent accept")?;
        }
        (Some(Command::Discard { session_id, force }), ExecutionMode::Daemon) => {
            let response =
                send_request(&paths, &Request::DiscardSession { session_id, force }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Discard { .. }), ExecutionMode::Local(reason)) => {
            bail_live_command(&reason)?;
        }
        (Some(Command::Logs { session_id, follow }), ExecutionMode::Daemon) => {
            stream_logs(&paths, &session_id, follow).await?;
        }
        (Some(Command::Logs { session_id, follow }), ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_logs(&paths, &session_id, follow)?;
        }
        (Some(Command::Events { session_id, follow }), ExecutionMode::Daemon) => {
            stream_events(&paths, &session_id, follow).await?;
        }
        (Some(Command::Events { session_id, follow }), ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            local_events(&paths, &session_id, follow).await?;
        }
        (Some(Command::List), ExecutionMode::Daemon) => {
            let sessions = daemon_list_sessions(&paths).await?;
            print_sessions(&sessions);
        }
        (Some(Command::List), ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            let store = LocalStore::open(&paths)?;
            let sessions =
                store.list_sessions()?.into_iter().map(normalize_session).collect::<Vec<_>>();
            print_sessions(&sessions);
        }
        (Some(Command::Attachments { session_id }), ExecutionMode::Daemon) => {
            let attachments = daemon_list_attachments(&paths, &session_id).await?;
            print_attachments(&attachments);
        }
        (Some(Command::Attachments { .. }), ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent attachments` requires a compatible daemon");
        }
        (Some(Command::Diff { session_id }), ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::DiffSession { session_id }).await?;
            match response {
                Response::Diff { diff } => print_diff(&diff),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Diff { .. }), ExecutionMode::Local(reason)) => {
            bail!(
                "{reason}. `agent diff` requires a compatible daemon; use `agent sessions` and `agent kill` to recover first"
            );
        }
        (Some(Command::Status { session_id }), ExecutionMode::Daemon) => {
            let response = send_request(&paths, &Request::GetSession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Status { session_id }), ExecutionMode::Local(reason)) => {
            print_degraded_notice(&reason);
            let store = LocalStore::open(&paths)?;
            let session = store
                .get_session(&session_id)?
                .map(normalize_session)
                .ok_or_else(|| anyhow::anyhow!("session `{session_id}` not found"))?;
            print_session(&session);
        }
        (Some(Command::Worktree { command }), ExecutionMode::Daemon) => match command {
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
        (Some(Command::Worktree { .. }), ExecutionMode::Local(reason)) => {
            bail!(
                "{reason}. worktree management requires a compatible daemon or a manual cleanup flow"
            );
        }
        (Some(Command::Daemon { command }), ExecutionMode::Daemon) => match command {
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
        (Some(Command::Daemon { .. }), ExecutionMode::Local(reason)) => {
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
    title: Option<String>,
    agent: String,
}

async fn resolve_execution_mode(
    paths: &AppPaths,
    command: Option<&Command>,
) -> Result<ExecutionMode> {
    if matches!(command, Some(Command::Daemon { command: DaemonCommand::Upgrade })) {
        return Ok(ExecutionMode::Daemon);
    }

    if matches!(command, Some(Command::Daemon { .. })) {
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

fn command_supports_local_mode(command: Option<&Command>) -> bool {
    matches!(
        command,
        Some(
            Command::Kill { .. }
                | Command::Logs { .. }
                | Command::Events { .. }
                | Command::List
                | Command::Status { .. }
        )
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

fn bail_daemon_command(reason: &str, command: &str) -> Result<()> {
    bail!("{reason}. `{command}` requires a compatible daemon")
}

fn resolve_new_session_options(
    workspace: Option<PathBuf>,
    title: Option<String>,
    agent: Option<String>,
) -> Result<NewSessionOptions> {
    Ok(NewSessionOptions {
        workspace: match workspace {
            Some(workspace) => workspace,
            None => std::env::current_dir().context("failed to resolve current directory")?,
        },
        title: title.filter(|value| !value.trim().is_empty()),
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
    clear_stale_daemon_state(paths)?;

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

fn clear_stale_daemon_state(paths: &AppPaths) -> Result<()> {
    let pid = read_daemon_pid(paths)?;
    if let Some(pid) = pid
        && local::process_exists(Some(pid))
    {
        if paths.socket.exists() {
            bail!("agentd is running (pid {pid}) but not responding; restart the daemon");
        }
        bail!(
            "agentd is running (pid {pid}) but socket {} is missing; restart the daemon",
            paths.socket
        );
    }

    remove_file_if_exists(&paths.socket)
        .with_context(|| format!("failed to remove stale socket {}", paths.socket))?;
    remove_file_if_exists(&paths.pid_file)
        .with_context(|| format!("failed to remove stale pid file {}", paths.pid_file))?;
    Ok(())
}

fn read_daemon_pid(paths: &AppPaths) -> Result<Option<u32>> {
    let contents = match fs::read_to_string(paths.pid_file.as_std_path()) {
        Ok(contents) => contents,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err).with_context(|| format!("failed to read {}", paths.pid_file)),
    };
    let raw = contents.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let pid = raw
        .parse::<u32>()
        .with_context(|| format!("failed to parse pid from {}", paths.pid_file))?;
    Ok(Some(pid))
}

fn remove_file_if_exists(path: &camino::Utf8Path) -> Result<()> {
    match fs::remove_file(path.as_std_path()) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err.into()),
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
            kind: AttachmentKind::Attach,
        },
    )
    .await?;

    let (read_half, mut write_half) = stream.into_split();
    let mut reader = BufReader::new(read_half);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the connection");
    };

    let (attach_id, initial_snapshot) = match response {
        Response::Attached {
            attach_id,
            snapshot,
        } => (attach_id, snapshot),
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

    eprintln!("attached to {session_id} ({attach_id}); detach with Ctrl-]");
    let _screen = AttachScreenGuard::enter()?;
    let _terminal = AttachTerminalGuard::enter()?;
    let raw_input = AttachRawInput::new()?;
    let mut resize_signal =
        signal(SignalKind::window_change()).context("failed to watch terminal resize")?;
    if let Ok((cols, rows)) = crossterm::terminal::size() {
        write_request(&mut write_half, &Request::AttachResize { cols, rows }).await?;
    }
    if let Ok(snapshot) = fetch_session_snapshot(paths, session_id).await {
        write_session_snapshot(&snapshot)?;
    } else {
        write_session_snapshot(&initial_snapshot)?;
    }
    let mut input_parser = AttachInputParser::default();
    let mut overlay = None::<runtime::AttachOverlay>;
    let mut overlay_events = None::<EventStream>;

    loop {
        tokio::select! {
            chunk = raw_input.read_chunk(), if overlay.is_none() => {
                let Some(chunk) = chunk? else {
                    break;
                };
                for action in input_parser.push_bytes(&chunk) {
                    match action {
                        AttachInputAction::Data(data) => {
                            write_request(&mut write_half, &Request::AttachInput { data }).await?;
                        }
                        AttachInputAction::Detach => {
                            drop(write_half);
                            return Ok(AttachOutcome::Detached);
                        }
                        AttachInputAction::Leader(action) => {
                            let mut active_overlay =
                                runtime::AttachOverlay::new(paths.clone(), session_id.to_string());
                            if let Some(outcome) = active_overlay.open_leader_action(action).await? {
                                match outcome {
                                    runtime::OverlayOutcome::Close => {
                                        write_attach_bytes(AGENTD_ATTACH_CLEAR_SEQUENCE)?;
                                        if let Ok(snapshot) = fetch_session_snapshot(paths, session_id).await {
                                            write_attach_bytes(&snapshot)?;
                                        }
                                    }
                                    runtime::OverlayOutcome::SwitchSession(next_session_id) => {
                                        drop(write_half);
                                        return Ok(AttachOutcome::SwitchSession(next_session_id));
                                    }
                                }
                            } else {
                                active_overlay.draw()?;
                                overlay_events = Some(EventStream::new());
                                overlay = Some(active_overlay);
                            }
                        }
                    }
                }
            }
            resize = resize_signal.recv() => {
                if resize.is_none() {
                    break;
                }
                if let Ok((cols, rows)) = crossterm::terminal::size() {
                    write_request(&mut write_half, &Request::AttachResize { cols, rows }).await?;
                    if let Some(overlay) = overlay.as_ref() {
                        overlay.draw()?;
                    }
                }
            }
            event = next_overlay_event(overlay_events.as_mut()), if overlay.is_some() => {
                let Some(event) = event? else {
                    overlay_events = None;
                    overlay = None;
                    write_attach_bytes(AGENTD_ATTACH_CLEAR_SEQUENCE)?;
                    if let Ok(snapshot) = fetch_session_snapshot(paths, session_id).await {
                        write_attach_bytes(&snapshot)?;
                    }
                    continue;
                };
                if let Some(active_overlay) = overlay.as_mut()
                    && let Some(outcome) = active_overlay.handle_event(event).await?
                {
                    match outcome {
                        runtime::OverlayOutcome::Close => {
                            overlay_events = None;
                            overlay = None;
                            write_attach_bytes(AGENTD_ATTACH_CLEAR_SEQUENCE)?;
                            if let Ok(snapshot) = fetch_session_snapshot(paths, session_id).await {
                                write_attach_bytes(&snapshot)?;
                            }
                            continue;
                        }
                        runtime::OverlayOutcome::SwitchSession(next_session_id) => {
                            drop(write_half);
                            return Ok(AttachOutcome::SwitchSession(next_session_id));
                        }
                    }
                }
                if let Some(active_overlay) = overlay.as_ref() {
                    active_overlay.draw()?;
                }
            }
            response = read_response(&mut reader) => {
                let Some(response) = response? else {
                    break;
                };
                match response {
                    Response::PtyOutput { data } => {
                        if overlay.is_none() {
                            write_attach_bytes(&data)?;
                        }
                    }
                    Response::SwitchSession { session_id } => {
                        drop(write_half);
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
    Ok(AttachOutcome::Detached)
}

async fn next_overlay_event(stream: Option<&mut EventStream>) -> Result<Option<Event>> {
    let Some(stream) = stream else {
        return Ok(None);
    };
    loop {
        match stream.next().await {
            Some(Ok(event @ Event::Key(_))) | Some(Ok(event @ Event::Paste(_))) => {
                return Ok(Some(event));
            }
            Some(Ok(_)) => continue,
            Some(Err(err)) => return Err(err).context("failed to read attach overlay input"),
            None => return Ok(None),
        }
    }
}

async fn fetch_session_snapshot(paths: &AppPaths, session_id: &str) -> Result<Vec<u8>> {
    let mut stream = try_connect(paths).await?;
    write_request(
        &mut stream,
        &Request::AttachSession {
            session_id: session_id.to_string(),
            kind: AttachmentKind::Tui,
        },
    )
    .await?;
    let (read_half, _write_half) = stream.into_split();
    let mut reader = BufReader::new(read_half);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the snapshot connection");
    };
    match response {
        Response::Attached { snapshot, .. } => Ok(snapshot),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected snapshot response: {:?}", other),
    }
}

fn write_session_snapshot(snapshot: &[u8]) -> Result<()> {
    write_attach_bytes(AGENTD_ATTACH_CLEAR_SEQUENCE)?;
    write_attach_bytes(snapshot)
}

async fn try_connect(paths: &AppPaths) -> Result<UnixStream> {
    UnixStream::connect(paths.socket.as_std_path())
        .await
        .with_context(|| format!("failed to connect to {}", paths.socket))
}

fn print_sessions(sessions: &[SessionRecord]) {
    for session in sessions {
        println!(
            "{}\t{}\t{}\t{}\t{}\t{}\t{}",
            session.session_id,
            session.agent,
            session.status_string(),
            session.integration_string(),
            session.attention_string(),
            session.repo_name,
            session.branch
        );
        if let Some(summary) = &session.attention_summary {
            println!("\t{summary}");
        }
    }
}

fn print_attachments(attachments: &[AttachmentRecord]) {
    for attachment in attachments {
        println!(
            "{}\t{}\t{}\t{}",
            attachment.attach_id,
            attachment.session_id,
            attachment.kind.as_str(),
            attachment.connected_at.to_rfc3339(),
        );
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
    println!("repo_name: {}", session.repo_name);
    println!("repo_path: {}", session.repo_path);
    println!("workspace: {}", session.workspace);
    println!("title: {}", session.title);
    println!("base_branch: {}", session.base_branch);
    println!("branch: {}", session.branch);
    println!("worktree: {}", session.worktree);
    println!("git_sync: {}", session.git_sync.as_str());
    println!("has_conflicts: {}", session.has_conflicts);
    if let Some(summary) = &session.git_status_summary {
        println!("git_status_summary: {summary}");
    }
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

fn resolve_focus_log_path(paths: &AppPaths, session: &SessionRecord) -> camino::Utf8PathBuf {
    let rendered = paths.rendered_log_path(&session.session_id);
    if rendered.exists() {
        rendered
    } else {
        paths.log_path(&session.session_id)
    }
}

fn read_focus_log_contents(path: &camino::Utf8Path) -> Result<String> {
    Ok(fs::read_to_string(path.as_std_path())
        .with_context(|| format!("failed to read {}", path))?)
}

fn diff_color_enabled() -> bool {
    should_colorize_diff_output(
        std::io::stdout().is_terminal(),
        std::env::var_os("NO_COLOR"),
    )
}

fn should_colorize_diff_output(is_terminal: bool, no_color: Option<std::ffi::OsString>) -> bool {
    is_terminal && no_color.is_none()
}

fn render_diff_text(diff: &str, color: bool) -> String {
    if !color {
        return diff.to_string();
    }

    let mut rendered = String::with_capacity(diff.len() + 32);
    for line in diff.split_inclusive('\n') {
        let styled = if line.starts_with("diff --git")
            || line.starts_with("--- ")
            || line.starts_with("+++ ")
        {
            format!(
                "{}",
                line.with(CrosColor::Rgb {
                    r: 153,
                    g: 214,
                    b: 255,
                })
                .attribute(CrosAttribute::Bold)
            )
        } else if line.starts_with("@@") {
            format!(
                "{}",
                line.with(CrosColor::Rgb {
                    r: 242,
                    g: 201,
                    b: 76,
                })
                .attribute(CrosAttribute::Bold)
            )
        } else if line.starts_with('+') && !line.starts_with("+++") {
            format!(
                "{}",
                line.with(CrosColor::Rgb {
                    r: 111,
                    g: 207,
                    b: 151,
                })
            )
        } else if line.starts_with('-') && !line.starts_with("---") {
            format!(
                "{}",
                line.with(CrosColor::Rgb {
                    r: 255,
                    g: 107,
                    b: 107,
                })
            )
        } else {
            line.to_string()
        };
        rendered.push_str(&styled);
    }
    rendered
}

fn print_kill_result(session_id: &str, was_running: bool, removed: bool) {
    if was_running {
        println!("terminated session {session_id}");
    }
    if removed {
        println!("removed session {session_id}");
    }
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

const ATTACH_DETACH_BYTE: u8 = 0x1d;
const ATTACH_LEADER_BYTE: u8 = 0x02;

#[derive(Debug, Clone, PartialEq, Eq)]
enum AttachInputAction {
    Data(Vec<u8>),
    Detach,
    Leader(runtime::AttachLeaderAction),
}

#[derive(Default)]
struct AttachInputParser {
    leader_pending: bool,
}

impl AttachInputParser {
    fn push_bytes(&mut self, bytes: &[u8]) -> Vec<AttachInputAction> {
        let mut actions = Vec::new();
        let mut forwarded = Vec::new();

        for &byte in bytes {
            if self.leader_pending {
                self.leader_pending = false;
                if let Some(action) = attach_leader_action(byte) {
                    flush_attach_bytes(&mut actions, &mut forwarded);
                    actions.push(action);
                }
                continue;
            }

            match byte {
                ATTACH_LEADER_BYTE => {
                    flush_attach_bytes(&mut actions, &mut forwarded);
                    self.leader_pending = true;
                }
                ATTACH_DETACH_BYTE => {
                    flush_attach_bytes(&mut actions, &mut forwarded);
                    actions.push(AttachInputAction::Detach);
                }
                _ => forwarded.push(byte),
            }
        }

        flush_attach_bytes(&mut actions, &mut forwarded);
        actions
    }
}

fn attach_leader_action(byte: u8) -> Option<AttachInputAction> {
    Some(match byte {
        ATTACH_LEADER_BYTE => AttachInputAction::Data(vec![ATTACH_LEADER_BYTE]),
        b' ' => AttachInputAction::Leader(runtime::AttachLeaderAction::OpenPalette),
        b's' => AttachInputAction::Leader(runtime::AttachLeaderAction::SessionSwitcher),
        b't' => AttachInputAction::Leader(runtime::AttachLeaderAction::NewSession),
        b'g' => AttachInputAction::Leader(runtime::AttachLeaderAction::SessionDetails),
        b'd' => AttachInputAction::Leader(runtime::AttachLeaderAction::Diff),
        b'x' => AttachInputAction::Leader(runtime::AttachLeaderAction::StopSession),
        0x1b => return None,
        _ => return None,
    })
}

fn flush_attach_bytes(actions: &mut Vec<AttachInputAction>, forwarded: &mut Vec<u8>) {
    if !forwarded.is_empty() {
        actions.push(AttachInputAction::Data(std::mem::take(forwarded)));
    }
}

struct AttachRawInput {
    fd: AsyncFd<OwnedFd>,
}

impl AttachRawInput {
    fn new() -> Result<Self> {
        let duplicated = unsafe { libc::dup(std::io::stdin().as_raw_fd()) };
        if duplicated < 0 {
            return Err(std::io::Error::last_os_error()).context("failed to duplicate stdin");
        }

        let flags = unsafe { fcntl(duplicated, libc::F_GETFL) };
        if flags < 0 {
            let err = std::io::Error::last_os_error();
            unsafe {
                libc::close(duplicated);
            }
            return Err(err).context("failed to read stdin flags");
        }

        if unsafe { fcntl(duplicated, libc::F_SETFL, flags | O_NONBLOCK) } < 0 {
            let err = std::io::Error::last_os_error();
            unsafe {
                libc::close(duplicated);
            }
            return Err(err).context("failed to set stdin nonblocking");
        }

        let fd = unsafe { OwnedFd::from_raw_fd(duplicated) };
        Ok(Self {
            fd: AsyncFd::new(fd).context("failed to register stdin for async reads")?,
        })
    }

    async fn read_chunk(&self) -> Result<Option<Vec<u8>>> {
        let mut buf = [0_u8; 4096];
        loop {
            let mut guard = self.fd.readable().await?;
            match guard.try_io(|inner| match nix::unistd::read(inner.get_ref(), &mut buf) {
                Ok(0) => Ok(None),
                Ok(count) => Ok(Some(buf[..count].to_vec())),
                Err(err) => {
                    if err == nix::errno::Errno::EAGAIN
                        || err == nix::errno::Errno::EWOULDBLOCK
                        || err == nix::errno::Errno::EINTR
                    {
                        Err(std::io::Error::from(std::io::ErrorKind::WouldBlock))
                    } else {
                        Err(std::io::Error::from_raw_os_error(err as i32))
                    }
                }
            }) {
                Ok(result) => return result.context("failed to read attach input"),
                Err(_) => continue,
            }
        }
    }
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

struct AttachScreenGuard;

impl AttachScreenGuard {
    fn enter() -> Result<Self> {
        execute!(std::io::stdout(), EnterAlternateScreen, Hide)
            .context("failed to enter attach screen")?;
        Ok(Self)
    }
}

impl Drop for AttachScreenGuard {
    fn drop(&mut self) {
        let _ = execute!(std::io::stdout(), Show, LeaveAlternateScreen);
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

async fn daemon_list_attachments(
    paths: &AppPaths,
    session_id: &str,
) -> Result<Vec<AttachmentRecord>> {
    let response = send_request(
        paths,
        &Request::ListAttachments {
            session_id: session_id.to_string(),
        },
    )
    .await?;
    match response {
        Response::Attachments { attachments } => Ok(attachments),
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

#[cfg(test)]
mod tests {
    use super::{
        AGENTD_ATTACH_RESTORE_SEQUENCE, ATTACH_DETACH_BYTE, ATTACH_LEADER_BYTE, AttachInputAction,
        AttachInputParser, Cli, Command, DaemonCommand, SessionEndSummary, bail_daemon_command,
        clear_stale_daemon_state, format_session_end_summary, render_diff_text,
        resolve_detach_session_id, resolve_new_session_options, should_colorize_diff_output,
    };
    use crate::runtime::AttachLeaderAction;
    use agentd_shared::paths::AppPaths;
    use agentd_shared::session::{IntegrationState, SessionStatus};
    use clap::Parser;
    use std::{
        ffi::OsString,
        fs,
        path::PathBuf,
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

    static TEST_PATH_COUNTER: AtomicU64 = AtomicU64::new(0);

    #[test]
    fn new_command_parses_optional_title() {
        let cli = Cli::try_parse_from(["agent", "new", "fix failing tests"]).unwrap();
        match cli.command {
            Some(Command::New {
                title,
                workspace,
                agent,
            }) => {
                assert_eq!(title.as_deref(), Some("fix failing tests"));
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
                title,
                workspace,
                agent,
            }) => {
                assert_eq!(title.as_deref(), Some("fix"));
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
        assert!(options.title.is_none());
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
        assert_eq!(options.title.as_deref(), Some("fix tests"));
        assert_eq!(options.agent, "claude");
    }

    #[test]
    fn detach_command_parses_optional_session_id() {
        let cli = Cli::try_parse_from(["agent", "detach", "demo"]).unwrap();
        match cli.command {
            Some(Command::Detach {
                session_id,
                attach,
                all,
            }) => {
                assert_eq!(session_id.as_deref(), Some("demo"));
                assert!(attach.is_none());
                assert!(!all);
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn detach_command_parses_attachment_flag() {
        let cli = Cli::try_parse_from(["agent", "detach", "demo", "--attach", "attach-1"]).unwrap();
        match cli.command {
            Some(Command::Detach {
                session_id,
                attach,
                all,
            }) => {
                assert_eq!(session_id.as_deref(), Some("demo"));
                assert_eq!(attach.as_deref(), Some("attach-1"));
                assert!(!all);
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn attachments_command_parses() {
        let cli = Cli::try_parse_from(["agent", "attachments", "demo"]).unwrap();
        match cli.command {
            Some(Command::Attachments { session_id }) => assert_eq!(session_id, "demo"),
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
    fn daemon_command_error_for_accept_does_not_mention_pty() {
        let err = bail_daemon_command("agentd is unavailable", "agent accept").unwrap_err();
        assert_eq!(
            err.to_string(),
            "agentd is unavailable. `agent accept` requires a compatible daemon"
        );
    }

    #[test]
    fn clear_stale_daemon_state_removes_dead_pid_and_socket() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        fs::write(paths.pid_file.as_str(), "999999\n").unwrap();
        fs::write(paths.socket.as_str(), "").unwrap();

        clear_stale_daemon_state(&paths).unwrap();

        assert!(!paths.pid_file.exists());
        assert!(!paths.socket.exists());
    }

    #[test]
    fn attach_parser_detaches_on_ctrl_right_bracket_byte() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(&[ATTACH_DETACH_BYTE]),
            vec![AttachInputAction::Detach]
        );
    }

    #[test]
    fn attach_parser_forwards_regular_bytes() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(b"hello"),
            vec![AttachInputAction::Data(b"hello".to_vec())]
        );
    }

    #[test]
    fn attach_parser_preserves_mouse_and_scroll_sequences() {
        let mut parser = AttachInputParser::default();
        let mouse = b"\x1b[<64;10;5M";
        assert_eq!(
            parser.push_bytes(mouse),
            vec![AttachInputAction::Data(mouse.to_vec())]
        );
    }

    #[test]
    fn attach_parser_treats_ctrl_b_as_leader_prefix() {
        let mut parser = AttachInputParser::default();
        assert!(parser.push_bytes(&[ATTACH_LEADER_BYTE]).is_empty());
        assert_eq!(
            parser.push_bytes(b" "),
            vec![AttachInputAction::Leader(AttachLeaderAction::OpenPalette)]
        );
    }

    #[test]
    fn attach_parser_sends_literal_ctrl_b_on_double_prefix() {
        let mut parser = AttachInputParser::default();
        assert!(parser.push_bytes(&[ATTACH_LEADER_BYTE]).is_empty());
        assert_eq!(
            parser.push_bytes(&[ATTACH_LEADER_BYTE]),
            vec![AttachInputAction::Data(vec![ATTACH_LEADER_BYTE])]
        );
    }

    #[test]
    fn attach_parser_maps_leader_commands() {
        let mut parser = AttachInputParser::default();
        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert_eq!(
            parser.push_bytes(b"s"),
            vec![AttachInputAction::Leader(
                AttachLeaderAction::SessionSwitcher
            )]
        );

        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert_eq!(
            parser.push_bytes(b"t"),
            vec![AttachInputAction::Leader(AttachLeaderAction::NewSession)]
        );

        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert_eq!(
            parser.push_bytes(b"g"),
            vec![AttachInputAction::Leader(
                AttachLeaderAction::SessionDetails
            )]
        );

        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert_eq!(
            parser.push_bytes(b"d"),
            vec![AttachInputAction::Leader(AttachLeaderAction::Diff)]
        );

        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert_eq!(
            parser.push_bytes(b"x"),
            vec![AttachInputAction::Leader(AttachLeaderAction::StopSession)]
        );
    }

    #[test]
    fn attach_parser_drops_unknown_leader_keys() {
        let mut parser = AttachInputParser::default();
        parser.push_bytes(&[ATTACH_LEADER_BYTE]);
        assert!(parser.push_bytes(b"q").is_empty());
    }

    #[test]
    fn attach_parser_flushes_bytes_around_control_sequences() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(b"ab\x02\x02cd\x1d"),
            vec![
                AttachInputAction::Data(b"ab".to_vec()),
                AttachInputAction::Data(vec![ATTACH_LEADER_BYTE]),
                AttachInputAction::Data(b"cd".to_vec()),
                AttachInputAction::Detach,
            ]
        );
    }

    #[test]
    fn attach_restore_sequence_matches_agentd_cleanup() {
        assert_eq!(
            AGENTD_ATTACH_RESTORE_SEQUENCE,
            b"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h"
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

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos()
            + u128::from(TEST_PATH_COUNTER.fetch_add(1, Ordering::Relaxed));
        let root = camino::Utf8PathBuf::from(format!("/tmp/agent-cli-test-{suffix}"));
        AppPaths {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            worktrees_dir: root.join("worktrees"),
            root,
        }
    }
}
