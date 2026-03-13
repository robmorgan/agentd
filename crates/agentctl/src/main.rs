use std::{
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, bail};
use clap::{ArgAction, Parser, Subcommand};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::UnixStream,
};

use agentd_shared::{
    config::Config,
    paths::AppPaths,
    protocol::{DaemonInfo, PROTOCOL_VERSION, Request, Response},
    session::{SessionDiff, SessionRecord, WorktreeRecord},
};

#[derive(Debug, Parser)]
#[command(name = "agentctl")]
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
    Logs {
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

#[derive(Debug, Subcommand)]
enum WorktreeCommand {
    Create {
        session_id: String,
    },
    Cleanup {
        session_id: String,
    },
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
    ensure_daemon(&paths).await?;

    match cli.command {
        Command::Create {
            workspace,
            task,
            agent,
        } => {
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
        Command::Kill { rm, session_id } => {
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
        Command::Logs { session_id, follow } => {
            stream_logs(&paths, &session_id, follow).await?;
        }
        Command::Sessions => {
            let response = send_request(&paths, &Request::ListSessions).await?;
            match response {
                Response::Sessions { sessions } => print_sessions(&sessions),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        Command::Diff { session_id } => {
            let response = send_request(&paths, &Request::DiffSession { session_id }).await?;
            match response {
                Response::Diff { diff } => print_diff(&diff),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        Command::Status { session_id } => {
            let response = send_request(&paths, &Request::GetSession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
        Command::Worktree { command } => match command {
            WorktreeCommand::Create { session_id } => {
                let response = send_request(&paths, &Request::CreateWorktree { session_id }).await?;
                match response {
                    Response::Worktree { worktree } => print_worktree(&worktree),
                    Response::Error { message } => bail!(message),
                    other => bail!("unexpected response: {:?}", other),
                }
            }
            WorktreeCommand::Cleanup { session_id } => {
                let response = send_request(&paths, &Request::CleanupWorktree { session_id }).await?;
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
        Command::Daemon { command } => match command {
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
    }

    Ok(())
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
            "agentd `{daemon_version}` is incompatible with agentctl `{}` and cannot be restarted while sessions are running",
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
    match send_request_no_bootstrap(paths, &Request::GetDaemonInfo).await? {
        Response::DaemonInfo { info } => Ok(info),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn daemon_has_running_sessions(paths: &AppPaths) -> Result<bool> {
    match send_request_no_bootstrap(paths, &Request::ListSessions).await? {
        Response::Sessions { sessions } => Ok(sessions.iter().any(|session| session.status_string() == "running")),
        Response::Error { message } => bail!(message),
        other => bail!("unexpected response: {:?}", other),
    }
}

async fn shutdown_or_kill_daemon(paths: &AppPaths) -> Result<()> {
    let shutdown_result = send_request_no_bootstrap(paths, &Request::ShutdownDaemon).await;
    match shutdown_result {
        Ok(Response::Ok) => {}
        Ok(Response::Error { message }) => bail!(message),
        Ok(_) | Err(_) => kill_daemon_from_pid_file(paths)?,
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
    let payload = serde_json::to_vec(request)?;
    stream.write_all(&payload).await?;
    stream.write_all(b"\n").await?;
    stream.flush().await?;

    let mut lines = BufReader::new(stream).lines();
    let Some(line) = lines.next_line().await? else {
        bail!("agentd closed the connection");
    };
    Ok(serde_json::from_str(&line)?)
}

async fn stream_logs(paths: &AppPaths, session_id: &str, follow: bool) -> Result<()> {
    let mut stream = try_connect(paths).await?;
    let payload = serde_json::to_vec(&Request::StreamLogs {
        session_id: session_id.to_string(),
        follow,
    })?;
    stream.write_all(&payload).await?;
    stream.write_all(b"\n").await?;
    stream.flush().await?;

    let mut lines = BufReader::new(stream).lines();
    while let Some(line) = lines.next_line().await? {
        match serde_json::from_str::<Response>(&line)? {
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

async fn try_connect(paths: &AppPaths) -> Result<UnixStream> {
    UnixStream::connect(paths.socket.as_std_path())
        .await
        .with_context(|| format!("failed to connect to {}", paths.socket))
}

fn print_sessions(sessions: &[SessionRecord]) {
    for session in sessions {
        println!(
            "{}\t{}\t{}\t{}",
            session.session_id, session.agent, session.status_string(), session.branch
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

trait StatusString {
    fn status_string(&self) -> &'static str;
}

impl StatusString for SessionRecord {
    fn status_string(&self) -> &'static str {
        match self.status {
            agentd_shared::session::SessionStatus::Creating => "creating",
            agentd_shared::session::SessionStatus::Running => "running",
            agentd_shared::session::SessionStatus::Exited => "exited",
            agentd_shared::session::SessionStatus::Failed => "failed",
            agentd_shared::session::SessionStatus::UnknownRecovered => "unknown_recovered",
        }
    }
}
