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
    protocol::{Request, Response},
    session::SessionRecord,
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
    Logs {
        session_id: String,
        #[arg(long, action = ArgAction::Set, num_args = 0..=1, default_missing_value = "true", default_value_t = true)]
        follow: bool,
    },
    Sessions,
    Status {
        session_id: String,
    },
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
                    println!("branch: {}", session.branch);
                    println!("worktree: {}", session.worktree);
                }
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
        Command::Status { session_id } => {
            let response = send_request(&paths, &Request::GetSession { session_id }).await?;
            match response {
                Response::Session { session } => print_session(&session),
                Response::Error { message } => bail!(message),
                other => bail!("unexpected response: {:?}", other),
            }
        }
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
        return Ok(());
    }

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

async fn send_request(paths: &AppPaths, request: &Request) -> Result<Response> {
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
    println!("workspace: {}", session.workspace);
    println!("task: {}", session.task);
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
