use std::{
    fs,
    io::{IsTerminal, Write},
    mem::MaybeUninit,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use clap::{
    CommandFactory, FromArgMatches, Parser, Subcommand,
    builder::{
        StyledStr,
        styling::{AnsiColor, Color, Effects, RgbColor, Styles},
    },
};
use crossterm::{
    style::{Attribute as CrosAttribute, Color as CrosColor, Stylize},
    terminal::{disable_raw_mode, enable_raw_mode},
};
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
mod session_display;

use agentd_shared::{
    config::Config,
    header::{AGENTD_PRIMARY_BLUE_RGB, agentd_header},
    paths::AppPaths,
    protocol::{
        DaemonInfo, DaemonManagementRequest, DaemonManagementResponse, DaemonManagementStatus,
        PROTOCOL_VERSION, Request, Response, read_daemon_management_response, read_response,
        write_daemon_management_request, write_request,
    },
    session::{
        ApplyState, AttachmentKind, AttachmentRecord, AttentionLevel, IntegrationPolicy,
        SESSION_NAME_RULES, SessionDiff, SessionRecord, SessionStatus, WorktreeRecord,
        validate_session_name,
    },
};

use crate::local::{LocalStore, normalize_degraded_session, remove_session_artifacts};

const AGENTD_ATTACH_ENTER_SEQUENCE: &[u8] = b"\x1b[>1u";
const AGENTD_ATTACH_RESTORE_SEQUENCE: &[u8] =
    b"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h";
const AGENTD_ATTACH_CLEAR_SEQUENCE: &[u8] = b"\x1b[2J\x1b[H";
const AGENTD_ATTACH_EXIT_TITLE: &str = "agentd";
const CODEX_MODELS: &[&str] = &[
    "gpt-5.4",
    "gpt-5.3-codex",
    "gpt-5.1-codex-max",
    "gpt-5.1-codex",
    "gpt-5.1-codex-mini",
    "gpt-5-codex",
];

const ROOT_HELP_TEMPLATE: &str = "\
{before-help}{name} {version}

{about-with-newline}{usage-heading} {usage}

{subcommands}
{options}
{after-help}";

const GROUP_HELP_TEMPLATE: &str = "\
{before-help}{name}

{about-with-newline}{usage-heading} {usage}

{subcommands}
{options}
{after-help}";

const ROOT_HELP_INTRO: &str = "\
A local multi-session coding workflow for daemon-backed agents.

Core flows:
  Start a session      agent new fix-flaky-tests
  Inspect sessions     agent list
  Reconnect live PTY   agent attach <name>
  Review changes       agent diff <name> | agent merge <name>";

fn help_accent_style() -> clap::builder::styling::Style {
    Color::Rgb(RgbColor::from(AGENTD_PRIMARY_BLUE_RGB)).on_default()
}

fn root_before_help() -> StyledStr {
    let mut styled = StyledStr::new();
    styled.push_str(&agentd_header());
    styled.push_str("\n");
    styled.push_str(ROOT_HELP_INTRO);
    styled
}

fn cli_command() -> clap::Command {
    Cli::command().before_help(root_before_help())
}

fn cli_styles() -> Styles {
    Styles::styled()
        .header(AnsiColor::Cyan.on_default() | Effects::BOLD)
        .usage(AnsiColor::Yellow.on_default() | Effects::BOLD)
        .literal(help_accent_style() | Effects::BOLD)
        .placeholder(AnsiColor::BrightBlack.on_default())
        .valid(help_accent_style())
        .invalid(AnsiColor::Red.on_default() | Effects::BOLD)
        .error(AnsiColor::Red.on_default() | Effects::BOLD)
}

#[derive(Debug, Parser)]
#[command(
    name = "agent",
    version,
    about = "Run, inspect, and review local coding sessions",
    before_help = "agent\nA local multi-session coding workflow for daemon-backed agents.\n\nCore flows:\n  Start a session      agent new fix-flaky-tests\n  Inspect sessions     agent list\n  Reconnect live PTY   agent attach <name>\n  Review changes       agent diff <name> | agent merge <name>",
    after_help = "Examples:\n  agent new add-health-checks\n  agent create --workspace . --agent codex refactor-retries\n  agent list\n  agent status <name>\n  agent daemon info\n\nUse `agent <command> --help` for command-specific details.",
    help_template = ROOT_HELP_TEMPLATE,
    styles = cli_styles(),
    disable_help_subcommand = true,
    propagate_version = true,
    next_display_order = 1
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Debug, Subcommand)]
enum Command {
    #[command(hide = true)]
    Runtime { session_id: Option<String> },
    #[command(about = "Start and attach to a new session", display_order = 1)]
    New {
        name: Option<String>,
        #[arg(long)]
        workspace: Option<PathBuf>,
        #[arg(long)]
        agent: Option<String>,
    },
    #[command(about = "Create a session without attaching", display_order = 2)]
    Create {
        #[arg(long)]
        workspace: PathBuf,
        #[arg(long)]
        agent: String,
        name: Option<String>,
    },
    #[command(about = "Stop a running session or remove its record", display_order = 3)]
    Kill {
        #[arg(long)]
        rm: bool,
        session_id: String,
    },
    #[command(about = "Attach to a live session PTY", display_order = 4)]
    Attach { session_id: String },
    #[command(about = "Detach one or more attached clients", display_order = 5)]
    Detach {
        session_id: Option<String>,
        #[arg(long)]
        attach: Option<String>,
        #[arg(long)]
        all: bool,
    },
    #[command(about = "Send background input to a live session", display_order = 6)]
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
    #[command(about = "Merge a session branch back to the base branch", display_order = 7)]
    Merge { session_id: Option<String> },
    #[command(about = "Compatibility alias for `agent merge`", display_order = 8, hide = true)]
    Accept { session_id: String },
    #[command(about = "Discard a session's worktree and changes", display_order = 9)]
    Discard {
        session_id: String,
        #[arg(long)]
        force: bool,
    },
    #[command(about = "Print captured session history", display_order = 10)]
    History {
        session_id: String,
        #[arg(long)]
        vt: bool,
    },
    #[command(
        visible_alias = "ls",
        alias = "sessions",
        about = "List known sessions",
        display_order = 11
    )]
    List,
    #[command(about = "Show currently attached clients for a session", display_order = 12)]
    Attachments { session_id: String },
    #[command(about = "Show detailed session status", display_order = 13)]
    Status { session_id: String },
    #[command(about = "Show the session diff against its base branch", display_order = 14)]
    Diff { session_id: String },
    #[command(about = "Create or clean up session worktrees", display_order = 15)]
    Worktree {
        #[command(subcommand)]
        command: WorktreeCommand,
    },
    #[command(about = "Inspect or control the local agent daemon", display_order = 16)]
    Daemon {
        #[command(subcommand)]
        command: DaemonCommand,
    },
}

#[derive(Debug, Subcommand)]
#[command(
    name = "agent worktree",
    help_template = GROUP_HELP_TEMPLATE,
    before_help = "agent worktree\nManage the git worktree attached to a session.\n\nTypical flow:\n  agent worktree create <name>\n  agent worktree cleanup <name>",
    after_help = "Use `agent status <name>` first if you are unsure whether a session is still live.",
    styles = cli_styles(),
    next_display_order = 1
)]
enum WorktreeCommand {
    #[command(about = "Create the session worktree on disk", display_order = 1)]
    Create { session_id: String },
    #[command(about = "Remove the session worktree from disk", display_order = 2)]
    Cleanup { session_id: String },
}

#[derive(Debug, Subcommand)]
#[command(
    name = "agent daemon",
    help_template = GROUP_HELP_TEMPLATE,
    before_help = "agent daemon\nInspect, restart, or upgrade the local daemon process.",
    after_help = "Notes:\n  `restart` keeps metadata but does not preserve live PTY connectivity.\n  `upgrade` refuses to run while sessions are still live.",
    styles = cli_styles(),
    next_display_order = 1
)]
enum DaemonCommand {
    #[command(about = "Show daemon version, socket, pid, and compatibility", display_order = 1)]
    Info,
    #[command(about = "Restart the daemon", display_order = 2)]
    Restart {
        #[arg(long)]
        force: bool,
    },
    #[command(about = "Upgrade the daemon binary when no sessions are live", display_order = 3)]
    Upgrade,
}

#[tokio::main]
async fn main() -> Result<()> {
    let matches = cli_command().get_matches();
    let cli = match Cli::from_arg_matches(&matches) {
        Ok(cli) => cli,
        Err(err) => err.exit(),
    };
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;
    ensure_config(&paths)?;
    let execution = resolve_execution_mode(&paths, cli.command.as_ref()).await?;

    match (cli.command, execution) {
        (None, ExecutionMode::Daemon) => {
            run_runtime(&paths, None).await?;
        }
        (None, ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent` requires a compatible daemon");
        }
        (Some(Command::Runtime { session_id }), ExecutionMode::Daemon) => {
            run_runtime(&paths, session_id.as_deref()).await?;
        }
        (Some(Command::Runtime { .. }), ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent runtime` requires a compatible daemon");
        }
        (Some(Command::New { name, workspace, agent }), ExecutionMode::Daemon) => {
            let options = resolve_new_session_options(&paths, workspace, name, agent)?;
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: options.workspace.to_string_lossy().to_string(),
                    name: options.name,
                    agent: options.agent,
                    model: None,
                    integration_policy: IntegrationPolicy::ManualReview,
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
        (Some(Command::Create { workspace, agent, name }), ExecutionMode::Daemon) => {
            let name = normalize_requested_name(name)?;
            let response = send_request(
                &paths,
                &Request::CreateSession {
                    workspace: workspace.to_string_lossy().to_string(),
                    name,
                    agent,
                    model: None,
                    integration_policy: IntegrationPolicy::ManualReview,
                },
            )
            .await?;

            match response {
                Response::CreateSession { session } => {
                    println!("name: {}", session.session_id);
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
            if should_print_degraded_notice(DegradedNoticeCommand::Kill, &reason) {
                print_degraded_notice(&reason);
            }
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
                    "shared attach requires either `--all` or `--attach <attach_id>`; use Ctrl-\\ to detach the local client"
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
        (Some(Command::Merge { session_id }), ExecutionMode::Daemon) => {
            let session_id = resolve_merge_session_id(session_id)?;
            let response =
                send_request(&paths, &Request::ApplySession { session_id: session_id.clone() })
                    .await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        (Some(Command::Merge { .. }), ExecutionMode::Local(reason)) => {
            bail_daemon_command(&reason, "agent merge")?;
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
        (Some(Command::History { session_id, vt }), ExecutionMode::Daemon) => {
            print_history(&paths, &session_id, vt).await?;
        }
        (Some(Command::History { .. }), ExecutionMode::Local(reason)) => {
            bail!("{reason}. `agent history` requires a compatible daemon");
        }
        (Some(Command::List), ExecutionMode::Daemon) => {
            let sessions = daemon_list_sessions(&paths).await?;
            print_sessions(&sessions);
        }
        (Some(Command::List), ExecutionMode::Local(reason)) => {
            if should_print_degraded_notice(DegradedNoticeCommand::List, &reason) {
                print_degraded_notice(&reason);
            }
            let store = LocalStore::open(&paths)?;
            let sessions = store
                .list_sessions()?
                .into_iter()
                .map(normalize_degraded_session)
                .collect::<Vec<_>>();
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
            if should_print_degraded_notice(DegradedNoticeCommand::Status, &reason) {
                print_degraded_notice(&reason);
            }
            let store = LocalStore::open(&paths)?;
            let session = store
                .get_session(&session_id)?
                .map(normalize_degraded_session)
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

async fn run_runtime(paths: &AppPaths, initial_session_id: Option<&str>) -> Result<()> {
    let session_id = match initial_session_id {
        Some(session_id) => Some(session_id.to_string()),
        None => runtime::pick_session(paths).await?,
    };
    if let Some(session_id) = session_id {
        attach_session(paths, &session_id).await?;
    }
    Ok(())
}

enum ExecutionMode {
    Daemon,
    Local(String),
}

#[derive(Debug)]
struct NewSessionOptions {
    workspace: PathBuf,
    name: Option<String>,
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
    matches!(command, Some(Command::Kill { .. } | Command::List | Command::Status { .. }))
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

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DegradedNoticeCommand {
    Kill,
    List,
    Status,
}

fn should_print_degraded_notice(command: DegradedNoticeCommand, reason: &str) -> bool {
    !matches!(command, DegradedNoticeCommand::Kill)
        || !reason.starts_with("agentd could not be queried:")
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
    paths: &AppPaths,
    workspace: Option<PathBuf>,
    name: Option<String>,
    agent: Option<String>,
) -> Result<NewSessionOptions> {
    let config = Config::load(paths)?;
    Ok(NewSessionOptions {
        workspace: match workspace {
            Some(workspace) => workspace,
            None => std::env::current_dir().context("failed to resolve current directory")?,
        },
        name: normalize_requested_name(name)?,
        agent: match agent {
            Some(agent) => agent,
            None => config.default_agent_name(paths)?.to_string(),
        },
    })
}

fn normalize_requested_name(name: Option<String>) -> Result<Option<String>> {
    let trimmed = name.as_deref().map(str::trim).unwrap_or_default();
    if trimmed.is_empty() {
        return Ok(None);
    }
    validate_session_name(trimmed)
        .map_err(|_| anyhow!("invalid session name `{trimmed}`: {SESSION_NAME_RULES}"))?;
    Ok(Some(trimmed.to_string()))
}

fn resolve_detach_session_id(session_id: Option<String>) -> Result<String> {
    match session_id {
        Some(session_id) => Ok(session_id),
        None => std::env::var("AGENTD_SESSION_ID")
            .context("`agent detach` without a session id only works inside a managed session"),
    }
}

fn resolve_merge_session_id(session_id: Option<String>) -> Result<String> {
    match session_id {
        Some(session_id) => Ok(session_id),
        None => std::env::var("AGENTD_SESSION_ID")
            .context("`agent merge` without a session id only works inside a managed session"),
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
        Ok(Ok(DaemonManagementResponse::Shutdown { stopped, running_sessions: _, message })) => {
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

async fn print_history(paths: &AppPaths, session_id: &str, vt: bool) -> Result<()> {
    let mut stream = try_connect(paths).await?;
    write_request(&mut stream, &Request::GetHistory { session_id: session_id.to_string(), vt })
        .await?;

    let mut reader = BufReader::new(stream);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the history connection");
    };
    match response {
        Response::History { data } => {
            print!("{data}");
            Ok(())
        }
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
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
        if session.has_commits {
            bail!(
                "session `{session_id}` has committed changes; use `agent diff {session_id}` and `agent merge {session_id}` before removing it, or reconnect to the daemon and run `agent discard {session_id}`"
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
    let mut title_guard = AttachTitleGuard::new();
    loop {
        match attach_session_once(paths, &next_session_id, &mut title_guard).await? {
            AttachOutcome::Detached => return Ok(()),
            AttachOutcome::SessionEnded(summary) => {
                print_session_end_summary(&summary);
                return Ok(());
            }
            AttachOutcome::SwitchSession(session_id) => next_session_id = session_id,
        }
    }
}

pub(crate) struct AttachedSessionStream {
    pub(crate) attach_id: String,
    pub(crate) snapshot: Vec<u8>,
    pub(crate) reader: BufReader<tokio::net::unix::OwnedReadHalf>,
    pub(crate) write_half: tokio::net::unix::OwnedWriteHalf,
}

pub(crate) enum AttachHandshake {
    Attached(AttachedSessionStream),
    SessionEnded(SessionEndSummary),
}

pub(crate) fn current_terminal_size() -> (u16, u16) {
    crossterm::terminal::size().unwrap_or((80, 24))
}

pub(crate) async fn connect_attached_session(
    paths: &AppPaths,
    session_id: &str,
    kind: AttachmentKind,
    cols: u16,
    rows: u16,
) -> Result<AttachHandshake> {
    let mut stream = try_connect(paths).await?;
    write_request(
        &mut stream,
        &Request::AttachSession { session_id: session_id.to_string(), kind, cols, rows },
    )
    .await?;

    let (read_half, write_half) = stream.into_split();
    let mut reader = BufReader::new(read_half);
    let Some(response) = read_response(&mut reader).await? else {
        bail!("agentd closed the connection");
    };

    match response {
        Response::Attached { attach_id, snapshot } => {
            Ok(AttachHandshake::Attached(AttachedSessionStream {
                attach_id,
                snapshot,
                reader,
                write_half,
            }))
        }
        Response::SessionEnded {
            session_id,
            status,
            apply_state,
            has_commits,
            branch,
            worktree,
            exit_code,
            error,
        } => Ok(AttachHandshake::SessionEnded(SessionEndSummary {
            session_id,
            status,
            apply_state,
            has_commits,
            branch,
            worktree,
            exit_code,
            error,
        })),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected attach response: {other:?}"),
    }
}

async fn attach_session_once(
    paths: &AppPaths,
    session_id: &str,
    title_guard: &mut AttachTitleGuard,
) -> Result<AttachOutcome> {
    let (cols, rows) = current_terminal_size();
    let attached =
        match connect_attached_session(paths, session_id, AttachmentKind::Attach, cols, rows)
            .await?
        {
            AttachHandshake::Attached(attached) => attached,
            AttachHandshake::SessionEnded(summary) => {
                return Ok(AttachOutcome::SessionEnded(summary));
            }
        };
    let AttachedSessionStream { attach_id, snapshot, mut reader, mut write_half } = attached;

    title_guard.set_session(session_id)?;
    eprintln!(
        "attached to {session_id} ({attach_id}); Ctrl-\\\\ detaches, Ctrl-[/Ctrl-] switch running sessions"
    );
    let _terminal = AttachTerminalGuard::enter()?;
    let raw_input = AttachRawInput::new()?;
    let mut resize_signal =
        signal(SignalKind::window_change()).context("failed to watch terminal resize")?;
    write_session_snapshot(&snapshot)?;
    let mut input_parser = AttachInputParser::default();

    loop {
        tokio::select! {
            chunk = raw_input.read_chunk() => {
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
                        AttachInputAction::PreviousSession => {
                            if let Some(target_session_id) = adjacent_live_session_id(
                                paths,
                                session_id,
                                AttachSessionDirection::Previous,
                            )
                            .await?
                            {
                                drop(write_half);
                                return Ok(AttachOutcome::SwitchSession(target_session_id));
                            }
                        }
                        AttachInputAction::NextSession => {
                            if let Some(target_session_id) = adjacent_live_session_id(
                                paths,
                                session_id,
                                AttachSessionDirection::Next,
                            )
                            .await?
                            {
                                drop(write_half);
                                return Ok(AttachOutcome::SwitchSession(target_session_id));
                            }
                        }
                    }
                }
            }
            resize = resize_signal.recv() => {
                if resize.is_none() {
                    break;
                }
                let (cols, rows) = current_terminal_size();
                send_attach_resize(&mut write_half, cols, rows).await?;
            }
            response = read_response(&mut reader) => {
                let Some(response) = response? else {
                    break;
                };
                match response {
                    Response::PtyOutput { data } => {
                        write_attach_bytes(&data)?;
                    }
                    Response::SwitchSession { session_id } => {
                        drop(write_half);
                        return Ok(AttachOutcome::SwitchSession(session_id));
                    }
                    Response::SessionEnded {
                        session_id,
                        status,
                        apply_state,
                        has_commits,
                        branch,
                        worktree,
                        exit_code,
                        error,
                    } => {
                        drop(write_half);
                        return Ok(AttachOutcome::SessionEnded(SessionEndSummary {
                            session_id,
                            status,
                            apply_state,
                            has_commits,
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

async fn send_attach_resize(
    write_half: &mut tokio::net::unix::OwnedWriteHalf,
    cols: u16,
    rows: u16,
) -> Result<()> {
    write_request(write_half, &Request::AttachResize { cols, rows }).await
}

fn write_session_snapshot(snapshot: &[u8]) -> Result<()> {
    write_attach_bytes(&attach_startup_bytes(snapshot))
}

fn attach_startup_bytes(snapshot: &[u8]) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(
        AGENTD_ATTACH_ENTER_SEQUENCE.len() + AGENTD_ATTACH_CLEAR_SEQUENCE.len() + snapshot.len(),
    );
    bytes.extend_from_slice(AGENTD_ATTACH_ENTER_SEQUENCE);
    bytes.extend_from_slice(AGENTD_ATTACH_CLEAR_SEQUENCE);
    bytes.extend_from_slice(snapshot);
    bytes
}

fn format_attach_title(session_id: &str) -> String {
    format!("{session_id} - {AGENTD_ATTACH_EXIT_TITLE}")
}

fn terminal_title_bytes(title: &str) -> Vec<u8> {
    let mut bytes = Vec::with_capacity((title.len() * 2) + 10);
    bytes.extend_from_slice(b"\x1b]0;");
    bytes.extend_from_slice(title.as_bytes());
    bytes.push(b'\x07');
    bytes.extend_from_slice(b"\x1b]2;");
    bytes.extend_from_slice(title.as_bytes());
    bytes.push(b'\x07');
    bytes
}

fn write_terminal_title(title: &str) -> Result<()> {
    write_attach_bytes(&terminal_title_bytes(title))
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum AttachSessionDirection {
    Previous,
    Next,
}

async fn adjacent_live_session_id(
    paths: &AppPaths,
    current_session_id: &str,
    direction: AttachSessionDirection,
) -> Result<Option<String>> {
    let sessions = daemon_list_sessions(paths)
        .await?
        .into_iter()
        .filter(|session| session.status == SessionStatus::Running)
        .collect::<Vec<_>>();
    let ordered = runtime::ordered_sessions(&sessions);

    Ok(adjacent_live_session_id_in(&ordered, current_session_id, direction))
}

fn adjacent_live_session_id_in(
    sessions: &[&SessionRecord],
    current_session_id: &str,
    direction: AttachSessionDirection,
) -> Option<String> {
    if sessions.len() <= 1 {
        return None;
    }

    let Some(current_index) =
        sessions.iter().position(|session| session.session_id == current_session_id)
    else {
        return None;
    };

    let target_index = match direction {
        AttachSessionDirection::Previous => {
            if current_index == 0 {
                sessions.len() - 1
            } else {
                current_index - 1
            }
        }
        AttachSessionDirection::Next => (current_index + 1) % sessions.len(),
    };

    if target_index == current_index {
        return None;
    }

    Some(sessions[target_index].session_id.clone())
}

async fn try_connect(paths: &AppPaths) -> Result<UnixStream> {
    UnixStream::connect(paths.socket.as_std_path())
        .await
        .with_context(|| format!("failed to connect to {}", paths.socket))
}

fn print_sessions(sessions: &[SessionRecord]) {
    let width = crossterm::terminal::size()
        .map(|(cols, _)| cols as usize)
        .unwrap_or_else(|_| runtime::default_session_list_width());
    for line in runtime::render_session_list_lines(sessions, width) {
        println!("{line}");
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
    println!("name: {}", session.session_id);
    if let Some(thread_id) = &session.thread_id {
        println!("thread_id: {thread_id}");
    }
    println!("agent: {}", session.agent);
    if let Some(model) = &session.model {
        println!("model: {model}");
    }
    println!("status: {}", session.status_string());
    println!("apply_state: {}", session.apply_state_string());
    println!("has_commits: {}", session.has_commits);
    println!("has_pending_changes: {}", session.has_pending_changes);
    println!("attention: {}", session.attention_string());
    if let Some(summary) = &session.attention_summary {
        println!("attention_summary: {summary}");
    }
    println!("repo_name: {}", session.repo_name);
    println!("repo_path: {}", session.repo_path);
    println!("workspace: {}", session.workspace);
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
    println!("name: {}", worktree.session_id);
    println!("repo_path: {}", worktree.repo_path);
    println!("base_branch: {}", worktree.base_branch);
    println!("branch: {}", worktree.branch);
    println!("worktree: {}", worktree.worktree);
}

fn print_diff(diff: &SessionDiff) {
    println!("name: {}", diff.session_id);
    println!("base_branch: {}", diff.base_branch);
    println!("branch: {}", diff.branch);
    println!("worktree: {}", diff.worktree);
    println!();
    print!("{}", render_diff_text(&diff.diff, diff_color_enabled()));
}

fn diff_color_enabled() -> bool {
    should_colorize_diff_output(std::io::stdout().is_terminal(), std::env::var_os("NO_COLOR"))
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
                line.with(CrosColor::Rgb { r: 153, g: 214, b: 255 }).attribute(CrosAttribute::Bold)
            )
        } else if line.starts_with("@@") {
            format!(
                "{}",
                line.with(CrosColor::Rgb { r: 242, g: 201, b: 76 }).attribute(CrosAttribute::Bold)
            )
        } else if line.starts_with('+') && !line.starts_with("+++") {
            format!("{}", line.with(CrosColor::Rgb { r: 111, g: 207, b: 151 }))
        } else if line.starts_with('-') && !line.starts_with("---") {
            format!("{}", line.with(CrosColor::Rgb { r: 255, g: 107, b: 107 }))
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

const ATTACH_DETACH_BYTE: u8 = 0x1c;
const ATTACH_NEXT_SESSION_BYTE: u8 = 0x1d;
const ATTACH_PREVIOUS_SESSION_CODEPOINT: u32 = 91;
const ATTACH_DETACH_CODEPOINT: u32 = 92;
const ATTACH_NEXT_SESSION_CODEPOINT: u32 = 93;

#[derive(Debug, Clone, PartialEq, Eq)]
enum AttachInputAction {
    Data(Vec<u8>),
    Detach,
    PreviousSession,
    NextSession,
}

#[derive(Default)]
struct AttachInputParser {
    pending_escape: Vec<u8>,
}

impl AttachInputParser {
    fn push_bytes(&mut self, bytes: &[u8]) -> Vec<AttachInputAction> {
        let mut actions = Vec::new();
        let mut forwarded = Vec::new();
        let mut input = std::mem::take(&mut self.pending_escape);
        input.extend_from_slice(bytes);
        let mut index = 0;

        while index < input.len() {
            if input[index] == 0x1b {
                match parse_attach_hotkey_csi_u(&input[index..]) {
                    Some(AttachCsiUParse::Action(action, consumed)) => {
                        flush_attach_bytes(&mut actions, &mut forwarded);
                        actions.push(action);
                        index += consumed;
                        continue;
                    }
                    Some(AttachCsiUParse::Incomplete) => {
                        self.pending_escape.extend_from_slice(&input[index..]);
                        break;
                    }
                    None => {}
                }
            }

            match input[index] {
                ATTACH_DETACH_BYTE => {
                    flush_attach_bytes(&mut actions, &mut forwarded);
                    actions.push(AttachInputAction::Detach);
                }
                ATTACH_NEXT_SESSION_BYTE => {
                    flush_attach_bytes(&mut actions, &mut forwarded);
                    actions.push(AttachInputAction::NextSession);
                }
                byte => forwarded.push(byte),
            }
            index += 1;
        }

        flush_attach_bytes(&mut actions, &mut forwarded);
        actions
    }
}

enum AttachCsiUParse {
    Action(AttachInputAction, usize),
    Incomplete,
}

fn parse_attach_hotkey_csi_u(bytes: &[u8]) -> Option<AttachCsiUParse> {
    if bytes.len() < 2 || bytes[0] != 0x1b || bytes[1] != b'[' {
        return None;
    }

    let mut index = 2;
    let key_code = parse_csi_u_decimal(bytes, &mut index)?;
    let action = match key_code {
        ATTACH_PREVIOUS_SESSION_CODEPOINT => AttachInputAction::PreviousSession,
        ATTACH_DETACH_CODEPOINT => AttachInputAction::Detach,
        ATTACH_NEXT_SESSION_CODEPOINT => AttachInputAction::NextSession,
        _ => return None,
    };

    while index < bytes.len() && bytes[index] == b':' {
        index += 1;
        let Some(_) = parse_csi_u_decimal_optional(bytes, &mut index) else {
            return Some(AttachCsiUParse::Incomplete);
        };
    }

    if index >= bytes.len() {
        return Some(AttachCsiUParse::Incomplete);
    }
    if bytes[index] != b';' {
        return None;
    }
    index += 1;
    if index >= bytes.len() {
        return Some(AttachCsiUParse::Incomplete);
    }

    let mod_encoded = parse_csi_u_decimal(bytes, &mut index)?;
    if mod_encoded == 0 {
        return None;
    }
    let modifiers = mod_encoded - 1;
    let intentional_modifiers = modifiers & 0b00_111111;
    if intentional_modifiers != 0b100 {
        return None;
    }

    if index < bytes.len() && bytes[index] == b':' {
        index += 1;
        if index >= bytes.len() {
            return Some(AttachCsiUParse::Incomplete);
        }
        let event_type = parse_csi_u_decimal(bytes, &mut index)?;
        if event_type == 3 {
            return None;
        }
    }

    if index < bytes.len() && bytes[index] == b';' {
        index += 1;
        if index >= bytes.len() {
            return Some(AttachCsiUParse::Incomplete);
        }
        while index < bytes.len() && (bytes[index].is_ascii_digit() || bytes[index] == b':') {
            index += 1;
        }
    }

    if index >= bytes.len() {
        return Some(AttachCsiUParse::Incomplete);
    }
    if bytes[index] != b'u' {
        return None;
    }

    Some(AttachCsiUParse::Action(action, index + 1))
}

fn parse_csi_u_decimal(bytes: &[u8], index: &mut usize) -> Option<u32> {
    let start = *index;
    let mut value = 0_u32;
    while *index < bytes.len() && bytes[*index].is_ascii_digit() {
        value = value.checked_mul(10)?.checked_add((bytes[*index] - b'0') as u32)?;
        *index += 1;
    }

    if *index == start { None } else { Some(value) }
}

fn parse_csi_u_decimal_optional(bytes: &[u8], index: &mut usize) -> Option<Option<u32>> {
    let start = *index;
    let value = parse_csi_u_decimal(bytes, index);
    if *index == start { Some(None) } else { value.map(Some) }
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
        Ok(Self { fd: AsyncFd::new(fd).context("failed to register stdin for async reads")? })
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
                Ok(Ok(result)) => return Ok(result),
                Ok(Err(err)) if err.kind() == std::io::ErrorKind::WouldBlock => continue,
                Ok(Err(err)) => return Err(err).context("failed to read attach input"),
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
    apply_state: ApplyState,
    has_commits: bool,
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
        SessionStatus::Exited | SessionStatus::UnknownRecovered => {
            if summary.apply_state == ApplyState::Applied {
                return format!(
                    "session {} merged from {} ({})",
                    summary.session_id, summary.branch, summary.worktree
                );
            }
            if summary.has_commits {
                return format!(
                    "session {} finished with changes on {} ({})\nrun: agent diff {} | agent merge {} | agent discard {}",
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

struct AttachTitleGuard {
    owns_title: bool,
}

impl AttachTitleGuard {
    fn new() -> Self {
        Self { owns_title: false }
    }

    fn set_session(&mut self, session_id: &str) -> Result<()> {
        write_terminal_title(&format_attach_title(session_id))?;
        self.owns_title = true;
        Ok(())
    }
}

impl Drop for AttachTitleGuard {
    fn drop(&mut self) {
        if self.owns_title {
            let _ = write_terminal_title(AGENTD_ATTACH_EXIT_TITLE);
        }
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
        Ok(Self { stdin_fd, orig_termios: termios })
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

trait StatusString {
    fn status_string(&self) -> &'static str;
    fn apply_state_string(&self) -> &'static str;
    fn attention_string(&self) -> &'static str;
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

    fn apply_state_string(&self) -> &'static str {
        match self.apply_state {
            ApplyState::Idle => "idle",
            ApplyState::AutoApplying => "auto_applying",
            ApplyState::Applied => "applied",
            ApplyState::Discarded => "discarded",
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
    let stdout = std::io::stdout();
    let fd = stdout.as_raw_fd();
    let mut written = 0usize;
    while written < data.len() {
        let remaining = &data[written..];
        let result = unsafe { libc::write(fd, remaining.as_ptr().cast(), remaining.len()) };
        if result >= 0 {
            written += result as usize;
            continue;
        }

        let err = std::io::Error::last_os_error();
        if matches!(err.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::Interrupted) {
            std::thread::yield_now();
            continue;
        }

        return Err(err.into());
    }

    stdout.lock().flush()?;
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
    let response =
        send_request(paths, &Request::ListAttachments { session_id: session_id.to_string() })
            .await?;
    match response {
        Response::Attachments { attachments } => Ok(attachments),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn daemon_get_session(paths: &AppPaths, session_id: &str) -> Result<SessionRecord> {
    let response =
        send_request(paths, &Request::GetSession { session_id: session_id.to_string() }).await?;
    match response {
        Response::Session { session } => Ok(session),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn kill_session(paths: &AppPaths, session_id: &str) -> Result<()> {
    let response = send_request(
        paths,
        &Request::KillSession { session_id: session_id.to_string(), remove: false },
    )
    .await?;
    match response {
        Response::KillSession { .. } => Ok(()),
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
    use crate::runtime;

    use super::{
        AGENTD_ATTACH_ENTER_SEQUENCE, AGENTD_ATTACH_EXIT_TITLE, AGENTD_ATTACH_RESTORE_SEQUENCE,
        ATTACH_DETACH_BYTE, ATTACH_NEXT_SESSION_BYTE, AttachInputAction, AttachInputParser,
        AttachSessionDirection, Cli, Command, DaemonCommand, DegradedNoticeCommand,
        SessionEndSummary, adjacent_live_session_id_in, attach_startup_bytes, bail_daemon_command,
        clear_stale_daemon_state, cli_command, cli_styles, format_attach_title,
        format_session_end_summary, render_diff_text, resolve_detach_session_id,
        resolve_merge_session_id, resolve_new_session_options, should_colorize_diff_output,
        should_print_degraded_notice, terminal_title_bytes,
    };
    use agentd_shared::session::{
        ApplyState, AttentionLevel, IntegrationPolicy, SessionMode, SessionRecord, SessionStatus,
    };
    use agentd_shared::{header::AGENTD_PRIMARY_BLUE_RGB, paths::AppPaths};
    use chrono::{Duration, Utc};
    use clap::{
        Parser,
        builder::styling::{Color, Effects, RgbColor},
    };
    use std::{
        ffi::OsString,
        fs,
        path::PathBuf,
        sync::Mutex,
        sync::atomic::{AtomicU64, Ordering},
        time::{SystemTime, UNIX_EPOCH},
    };

    static TEST_PATH_COUNTER: AtomicU64 = AtomicU64::new(0);
    static ENV_LOCK: Mutex<()> = Mutex::new(());

    fn strip_ansi(input: &str) -> String {
        let mut output = String::new();
        let mut chars = input.chars().peekable();
        while let Some(ch) = chars.next() {
            if ch == '\u{1b}' {
                if matches!(chars.peek(), Some('[')) {
                    chars.next();
                    while let Some(next) = chars.next() {
                        if ('@'..='~').contains(&next) {
                            break;
                        }
                    }
                }
                continue;
            }
            output.push(ch);
        }
        output
    }

    #[test]
    fn cli_help_uses_agentd_blue_for_literals_and_valid_values() {
        let accent = Color::Rgb(RgbColor::from(AGENTD_PRIMARY_BLUE_RGB));
        let styles = cli_styles();

        assert_eq!(styles.get_literal().get_fg_color(), Some(accent));
        assert!(styles.get_literal().get_effects().contains(Effects::BOLD));
        assert_eq!(styles.get_valid().get_fg_color(), Some(accent));
    }

    #[test]
    fn root_help_uses_shared_agentd_header_without_plain_leadin() {
        let mut command = cli_command();
        let help = strip_ansi(&command.render_help().to_string());

        assert!(help.contains("agentd - agent multiplexer"));
        assert!(help.contains("agent 0.1.0"));
        assert!(
            !help
                .contains("agent\nA local multi-session coding workflow for daemon-backed agents.")
        );
    }

    #[test]
    fn daemon_help_does_not_include_root_agentd_header() {
        let mut command = cli_command();
        let daemon = command.find_subcommand_mut("daemon").expect("daemon subcommand");
        let help = strip_ansi(&daemon.render_help().to_string());

        assert!(!help.contains("agentd - agent multiplexer"));
        assert!(help.contains("agent daemon"));
    }

    #[test]
    fn kill_suppresses_query_failure_degraded_notice() {
        assert!(!should_print_degraded_notice(
            DegradedNoticeCommand::Kill,
            "agentd could not be queried: broken pipe"
        ));
    }

    #[test]
    fn kill_keeps_unavailable_degraded_notice() {
        assert!(should_print_degraded_notice(DegradedNoticeCommand::Kill, "agentd is unavailable"));
    }

    #[test]
    fn kill_keeps_protocol_mismatch_degraded_notice() {
        assert!(should_print_degraded_notice(
            DegradedNoticeCommand::Kill,
            "agentd protocol version 1 is incompatible with agent protocol version 2"
        ));
    }

    #[test]
    fn list_keeps_query_failure_degraded_notice() {
        assert!(should_print_degraded_notice(
            DegradedNoticeCommand::List,
            "agentd could not be queried: broken pipe"
        ));
    }

    #[test]
    fn status_keeps_query_failure_degraded_notice() {
        assert!(should_print_degraded_notice(
            DegradedNoticeCommand::Status,
            "agentd could not be queried: broken pipe"
        ));
    }

    #[test]
    fn new_command_parses_optional_name() {
        let cli = Cli::try_parse_from(["agent", "new", "fix-failing-tests"]).unwrap();
        match cli.command {
            Some(Command::New { name, workspace, agent }) => {
                assert_eq!(name.as_deref(), Some("fix-failing-tests"));
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
            Some(Command::New { name, workspace, agent }) => {
                assert_eq!(name.as_deref(), Some("fix"));
                assert_eq!(workspace, Some(PathBuf::from("/tmp/repo")));
                assert_eq!(agent.as_deref(), Some("claude"));
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn resolve_new_session_options_uses_defaults() {
        let paths = test_paths();
        let options = resolve_new_session_options(&paths, None, None, None).unwrap();
        assert_eq!(options.workspace, std::env::current_dir().unwrap());
        assert!(options.name.is_none());
        assert_eq!(options.agent, "codex");
    }

    #[test]
    fn resolve_new_session_options_uses_configured_default_agent() {
        let paths = test_paths();
        paths.ensure_layout().unwrap();
        std::fs::write(
            paths.config.as_std_path(),
            r#"
default_agent = "claude"

[agents.codex]
command = "codex"

[agents.claude]
command = "claude"
"#,
        )
        .unwrap();

        let options = resolve_new_session_options(&paths, None, None, None).unwrap();
        assert_eq!(options.agent, "claude");
    }

    #[test]
    fn resolve_new_session_options_preserves_explicit_values() {
        let paths = test_paths();
        let options = resolve_new_session_options(
            &paths,
            Some(PathBuf::from("/tmp/repo")),
            Some("fix-tests".to_string()),
            Some("claude".to_string()),
        )
        .unwrap();
        assert_eq!(options.workspace, PathBuf::from("/tmp/repo"));
        assert_eq!(options.name.as_deref(), Some("fix-tests"));
        assert_eq!(options.agent, "claude");
    }

    #[test]
    fn resolve_new_session_options_rejects_invalid_name() {
        let paths = test_paths();
        let err = resolve_new_session_options(
            &paths,
            Some(PathBuf::from("/tmp/repo")),
            Some("fix tests".to_string()),
            Some("claude".to_string()),
        )
        .unwrap_err();
        assert!(err.to_string().contains("invalid session name"));
    }

    #[test]
    fn detach_command_parses_optional_session_id() {
        let cli = Cli::try_parse_from(["agent", "detach", "demo"]).unwrap();
        match cli.command {
            Some(Command::Detach { session_id, attach, all }) => {
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
            Some(Command::Detach { session_id, attach, all }) => {
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
    fn history_command_parses() {
        let cli = Cli::try_parse_from(["agent", "history", "demo"]).unwrap();
        match cli.command {
            Some(Command::History { session_id, vt }) => {
                assert_eq!(session_id, "demo");
                assert!(!vt);
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn history_command_parses_vt_flag() {
        let cli = Cli::try_parse_from(["agent", "history", "demo", "--vt"]).unwrap();
        match cli.command {
            Some(Command::History { session_id, vt }) => {
                assert_eq!(session_id, "demo");
                assert!(vt);
            }
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn daemon_restart_parses_force_flag() {
        let cli = Cli::try_parse_from(["agent", "daemon", "restart", "--force"]).unwrap();
        match cli.command {
            Some(Command::Daemon { command: DaemonCommand::Restart { force } }) => assert!(force),
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn daemon_upgrade_parses() {
        let cli = Cli::try_parse_from(["agent", "daemon", "upgrade"]).unwrap();
        match cli.command {
            Some(Command::Daemon { command: DaemonCommand::Upgrade }) => {}
            other => panic!("unexpected command: {other:?}"),
        }
    }

    #[test]
    fn resolve_detach_session_id_prefers_explicit_value() {
        let _guard = ENV_LOCK.lock().unwrap();
        unsafe {
            std::env::set_var("AGENTD_SESSION_ID", "env-session");
        }
        let session_id = resolve_detach_session_id(Some("explicit-session".to_string())).unwrap();
        assert_eq!(session_id, "explicit-session");
    }

    #[test]
    fn resolve_detach_session_id_uses_environment() {
        let _guard = ENV_LOCK.lock().unwrap();
        unsafe {
            std::env::set_var("AGENTD_SESSION_ID", "env-session");
        }
        let session_id = resolve_detach_session_id(None).unwrap();
        assert_eq!(session_id, "env-session");
    }

    #[test]
    fn resolve_detach_session_id_errors_without_environment() {
        let _guard = ENV_LOCK.lock().unwrap();
        unsafe {
            std::env::remove_var("AGENTD_SESSION_ID");
        }
        let err = resolve_detach_session_id(None).unwrap_err();
        assert!(err.to_string().contains("only works inside a managed session"));
    }

    #[test]
    fn resolve_merge_session_id_uses_environment() {
        let _guard = ENV_LOCK.lock().unwrap();
        unsafe {
            std::env::set_var("AGENTD_SESSION_ID", "env-session");
        }
        let session_id = resolve_merge_session_id(None).unwrap();
        assert_eq!(session_id, "env-session");
    }

    #[test]
    fn daemon_command_error_for_accept_does_not_mention_pty() {
        let err = bail_daemon_command("agentd is unavailable", "agent merge").unwrap_err();
        assert_eq!(
            err.to_string(),
            "agentd is unavailable. `agent merge` requires a compatible daemon"
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
    fn attach_parser_forwards_regular_bytes() {
        let mut parser = AttachInputParser::default();
        assert_eq!(parser.push_bytes(b"hello"), vec![AttachInputAction::Data(b"hello".to_vec())]);
    }

    #[test]
    fn attach_parser_preserves_mouse_and_scroll_sequences() {
        let mut parser = AttachInputParser::default();
        let mouse = b"\x1b[<64;10;5M";
        assert_eq!(parser.push_bytes(mouse), vec![AttachInputAction::Data(mouse.to_vec())]);
    }

    #[test]
    fn attach_parser_switches_to_previous_session_on_kitty_ctrl_left_bracket() {
        let mut parser = AttachInputParser::default();
        assert_eq!(parser.push_bytes(b"\x1b[91;5u"), vec![AttachInputAction::PreviousSession]);
    }

    #[test]
    fn attach_parser_detaches_on_kitty_ctrl_backslash() {
        let mut parser = AttachInputParser::default();
        assert_eq!(parser.push_bytes(b"\x1b[92;5u"), vec![AttachInputAction::Detach]);
    }

    #[test]
    fn attach_parser_switches_to_next_session_on_kitty_ctrl_right_bracket() {
        let mut parser = AttachInputParser::default();
        assert_eq!(parser.push_bytes(b"\x1b[93;5u"), vec![AttachInputAction::NextSession]);
    }

    #[test]
    fn attach_parser_ignores_kitty_hotkey_release_events() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(b"\x1b[93;5:3u"),
            vec![AttachInputAction::Data(b"\x1b[93;5:3u".to_vec())]
        );
    }

    #[test]
    fn attach_parser_carries_incomplete_kitty_hotkey_sequences_between_reads() {
        let mut parser = AttachInputParser::default();
        assert!(parser.push_bytes(b"\x1b[91;").is_empty());
        assert_eq!(parser.push_bytes(b"5u"), vec![AttachInputAction::PreviousSession]);
    }

    #[test]
    fn attach_parser_flushes_bytes_around_control_sequences() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(b"ab\x1ccd\x1d"),
            vec![
                AttachInputAction::Data(b"ab".to_vec()),
                AttachInputAction::Detach,
                AttachInputAction::Data(b"cd".to_vec()),
                AttachInputAction::NextSession,
            ]
        );
    }

    #[test]
    fn attach_parser_detaches_on_ctrl_backslash_byte() {
        let mut parser = AttachInputParser::default();
        assert_eq!(parser.push_bytes(&[ATTACH_DETACH_BYTE]), vec![AttachInputAction::Detach]);
    }

    #[test]
    fn attach_parser_switches_to_next_session_on_ctrl_right_bracket_byte() {
        let mut parser = AttachInputParser::default();
        assert_eq!(
            parser.push_bytes(&[ATTACH_NEXT_SESSION_BYTE]),
            vec![AttachInputAction::NextSession]
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
    fn attach_startup_sequence_clears_then_replays_snapshot() {
        assert_eq!(
            attach_startup_bytes(b"\x1b[?25lhello"),
            b"\x1b[>1u\x1b[2J\x1b[H\x1b[?25lhello".to_vec()
        );
    }

    #[test]
    fn attach_startup_sequence_does_not_force_alternate_screen() {
        let startup = attach_startup_bytes(b"snapshot");
        assert!(startup.starts_with(AGENTD_ATTACH_ENTER_SEQUENCE));
        assert!(!startup.windows(6).any(|window| window == b"\x1b[?1049"));
    }

    #[test]
    fn format_attach_title_appends_agentd_suffix() {
        assert_eq!(format_attach_title("fix-tests"), "fix-tests - agentd");
    }

    #[test]
    fn terminal_title_bytes_set_both_window_title_sequences() {
        assert_eq!(
            terminal_title_bytes("fix-tests - agentd"),
            b"\x1b]0;fix-tests - agentd\x07\x1b]2;fix-tests - agentd\x07".to_vec()
        );
    }

    #[test]
    fn adjacent_session_selection_wraps_in_both_directions() {
        let sessions = vec![
            demo_session("needs-input", SessionStatus::Running, false, 1, AttentionLevel::Action),
            demo_session("running", SessionStatus::Running, true, 2, AttentionLevel::Info),
            demo_session("idle", SessionStatus::Running, false, 3, AttentionLevel::Info),
        ];
        let sessions = runtime::ordered_sessions(&sessions);

        assert_eq!(
            adjacent_live_session_id_in(&sessions, "running", AttachSessionDirection::Previous),
            Some("needs-input".to_string())
        );
        assert_eq!(
            adjacent_live_session_id_in(&sessions, "idle", AttachSessionDirection::Next),
            Some("needs-input".to_string())
        );
    }

    #[test]
    fn terminal_title_bytes_reset_to_agentd_on_exit() {
        assert_eq!(
            terminal_title_bytes(AGENTD_ATTACH_EXIT_TITLE),
            b"\x1b]0;agentd\x07\x1b]2;agentd\x07".to_vec()
        );
    }

    #[test]
    fn adjacent_session_selection_ignores_non_live_sessions() {
        let sessions = vec![
            demo_session("current", SessionStatus::Running, false, 1, AttentionLevel::Info),
            demo_session("finished", SessionStatus::Exited, false, 4, AttentionLevel::Info),
        ]
        .into_iter()
        .filter(runtime::session_accepts_attach)
        .collect::<Vec<_>>();
        let sessions = runtime::ordered_sessions(&sessions);

        assert_eq!(
            adjacent_live_session_id_in(&sessions, "current", AttachSessionDirection::Next),
            None
        );
    }

    #[test]
    fn adjacent_session_selection_uses_switcher_order() {
        let sessions = vec![
            demo_session("running", SessionStatus::Running, false, 1, AttentionLevel::Info),
            demo_session("pending", SessionStatus::Running, true, 2, AttentionLevel::Info),
            demo_session("needs-input", SessionStatus::Running, false, 3, AttentionLevel::Action),
        ];
        let sessions = sessions
            .into_iter()
            .filter(|session| session.status == SessionStatus::Running)
            .collect::<Vec<_>>();
        let sessions = runtime::ordered_sessions(&sessions);

        assert_eq!(
            sessions.iter().map(|session| session.session_id.as_str()).collect::<Vec<_>>(),
            vec!["needs-input", "pending", "running"]
        );
        assert_eq!(
            adjacent_live_session_id_in(&sessions, "pending", AttachSessionDirection::Next),
            Some("running".to_string())
        );
    }

    #[test]
    fn format_session_end_summary_reports_exit_code() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            apply_state: ApplyState::Idle,
            has_commits: false,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/demo".to_string(),
            exit_code: Some(7),
            error: None,
        };

        assert_eq!(format_session_end_summary(&summary), "session demo finished (exit 7)");
    }

    #[test]
    fn format_session_end_summary_reports_error() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Failed,
            apply_state: ApplyState::Idle,
            has_commits: false,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/demo".to_string(),
            exit_code: None,
            error: Some("boom".to_string()),
        };

        assert_eq!(format_session_end_summary(&summary), "session demo failed: boom");
    }

    #[test]
    fn format_session_end_summary_reports_failure_message() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Failed,
            apply_state: ApplyState::Idle,
            has_commits: false,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(1),
            error: Some("spawn failed".to_string()),
        };
        assert_eq!(format_session_end_summary(&summary), "session demo failed: spawn failed");
    }

    #[test]
    fn format_session_end_summary_reports_merge_actions() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            apply_state: ApplyState::Idle,
            has_commits: true,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(0),
            error: None,
        };
        assert!(format_session_end_summary(&summary).contains("agent merge demo"));
    }

    #[test]
    fn format_session_end_summary_reports_merged_sessions() {
        let summary = SessionEndSummary {
            session_id: "demo".to_string(),
            status: SessionStatus::Exited,
            apply_state: ApplyState::Applied,
            has_commits: false,
            branch: "agent/demo".to_string(),
            worktree: "/tmp/worktree".to_string(),
            exit_code: Some(0),
            error: None,
        };
        assert!(format_session_end_summary(&summary).contains("merged"));
    }

    #[test]
    fn diff_colorization_is_disabled_without_terminal() {
        assert!(!should_colorize_diff_output(false, None));
    }

    #[test]
    fn diff_colorization_respects_no_color() {
        assert!(!should_colorize_diff_output(true, Some(OsString::from("1"))));
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

    fn demo_session(
        session_id: &str,
        status: SessionStatus,
        has_pending_changes: bool,
        updated_minutes_ago: i64,
        attention: AttentionLevel,
    ) -> SessionRecord {
        let now = Utc::now();
        SessionRecord {
            session_id: session_id.to_string(),
            thread_id: Some(format!("thread-{session_id}")),
            agent: "codex".to_string(),
            model: Some("gpt-5.4".to_string()),
            mode: SessionMode::Execute,
            workspace: "/tmp/workspace".to_string(),
            repo_path: "/tmp/workspace".to_string(),
            repo_name: "workspace".to_string(),
            base_branch: "main".to_string(),
            branch: format!("agent/{session_id}"),
            worktree: format!("/tmp/{session_id}"),
            status,
            integration_policy: IntegrationPolicy::ManualReview,
            apply_state: ApplyState::Idle,
            has_commits: has_pending_changes,
            has_pending_changes,
            pid: Some(123),
            exit_code: None,
            error: None,
            attention,
            attention_summary: None,
            created_at: now - Duration::minutes(updated_minutes_ago + 5),
            updated_at: now - Duration::minutes(updated_minutes_ago),
            exited_at: None,
        }
    }

    fn test_paths() -> AppPaths {
        let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos()
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
