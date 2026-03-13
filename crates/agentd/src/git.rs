use std::process::Command;

use anyhow::{Context, Result, bail};
use camino::{Utf8Path, Utf8PathBuf};

pub fn canonical_repo_root(workspace: &Utf8Path) -> Result<Utf8PathBuf> {
    let output = Command::new("git")
        .arg("-C")
        .arg(workspace.as_str())
        .args(["rev-parse", "--show-toplevel"])
        .output()
        .with_context(|| format!("failed to inspect git repo for {}", workspace))?;

    if !output.status.success() {
        bail!(
            "workspace `{}` is not a git repository: {}",
            workspace,
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    let root = String::from_utf8(output.stdout)?.trim().to_string();
    Utf8PathBuf::from_path_buf(std::path::PathBuf::from(root))
        .map_err(|_| anyhow::anyhow!("git repo root is not valid UTF-8"))
}

pub fn branch_exists(repo_root: &Utf8Path, branch: &str) -> Result<bool> {
    let status = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["show-ref", "--verify", "--quiet"])
        .arg(format!("refs/heads/{branch}"))
        .status()
        .with_context(|| format!("failed to test branch {branch}"))?;
    Ok(status.success())
}

pub fn create_worktree(repo_root: &Utf8Path, branch: &str, worktree: &Utf8Path) -> Result<()> {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "add", "-b", branch, worktree.as_str(), "HEAD"])
        .output()
        .with_context(|| format!("failed to create worktree {}", worktree))?;

    if !output.status.success() {
        bail!(
            "failed to create worktree: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    Ok(())
}
