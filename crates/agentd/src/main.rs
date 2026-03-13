mod app;
mod db;
mod git;
mod ids;
mod server;

use std::process::Stdio;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};

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
