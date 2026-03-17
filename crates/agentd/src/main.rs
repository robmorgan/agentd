mod app;
mod db;
mod git;
mod ids;
mod server;
mod terminal_state;

use std::{
    process::Stdio,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use clap::{Parser, Subcommand};
use nix::{errno::Errno, sys::signal::kill, unistd::Pid};

use agentd_shared::{
    config::Config, event::NewSessionEvent, paths::AppPaths, session::SessionRecord,
};

use crate::{
    app::{LaunchCommand, is_resumable_command},
    db::Database,
};

#[derive(Debug, Parser)]
#[command(name = "agentd")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    Serve {
        #[arg(long)]
        daemonize: bool,
    },
    Upgrade,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Command::Serve { daemonize } => {
            if daemonize {
                daemonize_self()
            } else {
                server::serve().await
            }
        }
        Command::Upgrade => upgrade_daemon().await,
    }
}

fn daemonize_self() -> Result<()> {
    let current_exe = std::env::current_exe().context("failed to resolve agentd executable")?;
    std::process::Command::new(current_exe)
        .arg("serve")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .context("failed to daemonize agentd")?;
    Ok(())
}

async fn upgrade_daemon() -> Result<()> {
    let paths = AppPaths::discover()?;
    paths.ensure_layout()?;
    let config = Config::load(&paths)?;
    let db = Database::open(&paths)?;

    let running_sessions = db
        .list_sessions()?
        .into_iter()
        .filter(|session| session.status == agentd_shared::session::SessionStatus::Running)
        .filter(|session| process_exists(session.pid))
        .collect::<Vec<_>>();

    let mut resolved = Vec::with_capacity(running_sessions.len());
    let mut unsupported = Vec::new();
    for session in &running_sessions {
        let launch = resolve_launch_for_upgrade(&db, &config, &paths, session)?;
        if !is_resumable_command(&launch.command) {
            unsupported.push(format!("{} ({})", session.session_id, session.agent));
            continue;
        }
        resolved.push((session.clone(), launch));
    }

    if !unsupported.is_empty() {
        bail!(
            "cannot upgrade with non-resumable running sessions: {}",
            unsupported.join(", ")
        );
    }

    for (session, launch) in &resolved {
        let args_json =
            serde_json::to_string(&launch.args).context("failed to serialize agent args")?;
        db.set_launch_info(&session.session_id, &launch.command, &args_json)?;
    }

    stop_existing_daemon(&paths).await?;

    for (session, launch) in &resolved {
        db.mark_paused(&session.session_id)?;
        db.append_events(
            &session.session_id,
            &[NewSessionEvent {
                event_type: "SESSION_PAUSED_FOR_UPGRADE".to_string(),
                payload_json: serde_json::json!({
                    "source": "daemon",
                    "agent": launch.agent_name,
                }),
            }],
        )?;
        terminate_process_if_running(&session.session_id, session.pid).await?;
    }

    daemonize_self()?;
    wait_for_new_daemon(&paths).await?;

    println!("upgraded agentd and resumed {} session(s)", resolved.len());
    Ok(())
}

fn resolve_launch_for_upgrade(
    db: &Database,
    config: &Config,
    paths: &AppPaths,
    session: &SessionRecord,
) -> Result<LaunchCommand> {
    let launch = db
        .get_launch_info(&session.session_id)?
        .ok_or_else(|| anyhow!("session `{}` not found", session.session_id))?;
    let configured = config.agents.get(&session.agent);
    let command = match launch.command {
        Some(command) => command,
        None => configured
            .map(|agent| agent.command.clone())
            .ok_or_else(|| {
                anyhow!(
                    "agent `{}` is not configured in {}",
                    session.agent,
                    paths.config
                )
            })?,
    };
    let args = match launch.args {
        Some(args) => args,
        None => configured
            .map(|agent| agent.args.clone())
            .unwrap_or_default(),
    };
    Ok(LaunchCommand {
        agent_name: session.agent.clone(),
        command,
        args,
    })
}

async fn stop_existing_daemon(paths: &AppPaths) -> Result<()> {
    let Some(pid) = read_pid(paths)? else {
        return Ok(());
    };
    send_signal(pid, nix::sys::signal::Signal::SIGTERM, "agentd")?;
    if wait_for_exit(pid, Duration::from_secs(5)).await {
        return Ok(());
    }

    send_signal(pid, nix::sys::signal::Signal::SIGKILL, "agentd")?;
    if wait_for_exit(pid, Duration::from_secs(5)).await {
        return Ok(());
    }

    bail!("agentd did not exit after SIGTERM and SIGKILL")
}

fn read_pid(paths: &AppPaths) -> Result<Option<Pid>> {
    let contents = match std::fs::read_to_string(paths.pid_file.as_std_path()) {
        Ok(contents) => contents,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err).with_context(|| format!("failed to read {}", paths.pid_file)),
    };
    let raw = contents.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let pid = raw
        .parse::<i32>()
        .with_context(|| format!("failed to parse pid from {}", paths.pid_file))?;
    Ok(Some(Pid::from_raw(pid)))
}

async fn wait_for_exit(pid: Pid, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    loop {
        match kill(pid, None) {
            Ok(()) => {}
            Err(Errno::ESRCH) => return true,
            Err(_) => return false,
        }
        if Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn terminate_process_if_running(name: &str, pid: Option<u32>) -> Result<()> {
    let Some(pid) = pid else {
        return Ok(());
    };
    if pid == 0 || !process_exists(Some(pid)) {
        return Ok(());
    }

    let pid = Pid::from_raw(pid as i32);
    send_signal(pid, nix::sys::signal::Signal::SIGTERM, name)?;
    if wait_for_exit(pid, Duration::from_secs(5)).await {
        return Ok(());
    }

    send_signal(pid, nix::sys::signal::Signal::SIGKILL, name)?;
    if wait_for_exit(pid, Duration::from_secs(5)).await {
        return Ok(());
    }

    bail!("`{name}` did not exit after SIGTERM and SIGKILL");
}

async fn wait_for_new_daemon(paths: &AppPaths) -> Result<()> {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if tokio::net::UnixStream::connect(paths.socket.as_std_path())
            .await
            .is_ok()
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out waiting for upgraded agentd to start");
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

fn process_exists(pid: Option<u32>) -> bool {
    let Some(pid) = pid else {
        return false;
    };
    if pid == 0 {
        return false;
    }
    kill(Pid::from_raw(pid as i32), None).is_ok()
}

fn send_signal(pid: Pid, signal: nix::sys::signal::Signal, name: &str) -> Result<()> {
    match kill(pid, Some(signal)) {
        Ok(()) => Ok(()),
        Err(Errno::ESRCH) => Ok(()),
        Err(err) => Err(anyhow!(err)).context(format!("failed to send {signal:?} to `{name}`")),
    }
}
