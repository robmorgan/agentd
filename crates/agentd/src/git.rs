use std::{
    process::Command,
    time::{SystemTime, UNIX_EPOCH},
};

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

pub fn current_branch(repo_root: &Utf8Path) -> Result<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["symbolic-ref", "--quiet", "--short", "HEAD"])
        .output()
        .with_context(|| format!("failed to inspect current branch for {}", repo_root))?;

    if !output.status.success() {
        bail!("repository `{repo_root}` is in detached HEAD state");
    }

    Ok(String::from_utf8(output.stdout)?.trim().to_string())
}

pub fn create_worktree(
    repo_root: &Utf8Path,
    base_branch: &str,
    branch: &str,
    worktree: &Utf8Path,
) -> Result<()> {
    if branch_exists(repo_root, branch)? {
        let output = Command::new("git")
            .arg("-C")
            .arg(repo_root.as_str())
            .args(["worktree", "add", worktree.as_str(), branch])
            .output()
            .with_context(|| format!("failed to recreate worktree {}", worktree))?;

        if !output.status.success() {
            bail!(
                "failed to create worktree: {}",
                String::from_utf8_lossy(&output.stderr).trim()
            );
        }
        return Ok(());
    }

    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args([
            "worktree",
            "add",
            "--detach",
            worktree.as_str(),
            base_branch,
        ])
        .output()
        .with_context(|| format!("failed to create worktree {}", worktree))?;

    if !output.status.success() {
        bail!(
            "failed to create worktree: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    let checkout = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["checkout", "-b", branch])
        .output()
        .with_context(|| format!("failed to create branch {branch} in {}", worktree))?;
    if !checkout.status.success() {
        bail!(
            "failed to create session branch: {}",
            String::from_utf8_lossy(&checkout.stderr).trim()
        );
    }

    Ok(())
}

pub fn remove_worktree(repo_root: &Utf8Path, worktree: &Utf8Path) -> Result<()> {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "remove", "--force", worktree.as_str()])
        .output()
        .with_context(|| format!("failed to remove worktree {}", worktree))?;

    if !output.status.success() {
        bail!(
            "failed to remove worktree: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    let prune = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "prune"])
        .output()
        .with_context(|| format!("failed to prune worktrees for {}", repo_root))?;
    if !prune.status.success() {
        bail!(
            "failed to prune worktrees: {}",
            String::from_utf8_lossy(&prune.stderr).trim()
        );
    }

    Ok(())
}

pub fn diff_against_base(worktree: &Utf8Path, base_branch: &str) -> Result<String> {
    let status = run_git(worktree, &["status", "--short"])?;
    let committed = run_git(
        worktree,
        &["diff", "--stat", &format!("{base_branch}...HEAD")],
    )?;
    let patch = run_git(worktree, &["diff", &format!("{base_branch}...HEAD")])?;
    let working_tree = run_git(worktree, &["diff"])?;

    let mut output = String::new();
    output.push_str("status --short\n");
    if status.trim().is_empty() {
        output.push_str("(clean)\n");
    } else {
        output.push_str(&status);
        if !status.ends_with('\n') {
            output.push('\n');
        }
    }

    output.push('\n');
    output.push_str(&format!("diff --stat {base_branch}...HEAD\n"));
    if committed.trim().is_empty() {
        output.push_str("(no committed diff)\n");
    } else {
        output.push_str(&committed);
        if !committed.ends_with('\n') {
            output.push('\n');
        }
    }

    output.push('\n');
    output.push_str(&format!("diff {base_branch}...HEAD\n"));
    if patch.trim().is_empty() {
        output.push_str("(no committed patch)\n");
    } else {
        output.push_str(&patch);
        if !patch.ends_with('\n') {
            output.push('\n');
        }
    }

    if !working_tree.trim().is_empty() {
        output.push('\n');
        output.push_str("diff (working tree)\n");
        output.push_str(&working_tree);
        if !working_tree.ends_with('\n') {
            output.push('\n');
        }
    }

    Ok(output)
}

pub fn has_worktree_changes(worktree: &Utf8Path) -> Result<bool> {
    Ok(!run_git(worktree, &["status", "--porcelain"])?
        .trim()
        .is_empty())
}

pub fn has_committed_diff_against_base(worktree: &Utf8Path, base_branch: &str) -> Result<bool> {
    Ok(!run_git(
        worktree,
        &["diff", "--stat", &format!("{base_branch}...HEAD")],
    )?
    .trim()
    .is_empty())
}

pub fn has_branch_diff_against_base(
    repo_root: &Utf8Path,
    base_branch: &str,
    branch: &str,
) -> Result<bool> {
    Ok(!run_git(
        repo_root,
        &["diff", "--stat", &format!("{base_branch}...{branch}")],
    )?
    .trim()
    .is_empty())
}

pub fn preflight_squash_merge(
    repo_root: &Utf8Path,
    base_branch: &str,
    branch: &str,
) -> Result<bool> {
    let temp = temporary_apply_worktree_path();
    let add = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "add", "--detach", temp.as_str(), base_branch])
        .output()
        .with_context(|| format!("failed to create preflight worktree from {base_branch}"))?;
    if !add.status.success() {
        bail!(
            "failed to create preflight worktree: {}",
            String::from_utf8_lossy(&add.stderr).trim()
        );
    }

    let merge = Command::new("git")
        .arg("-C")
        .arg(temp.as_str())
        .args(["merge", "--squash", "--no-commit", branch])
        .output()
        .with_context(|| format!("failed to preflight squash merge for {branch}"))?;
    let conflict =
        !merge.status.success() && is_merge_conflict_output(&merge.stdout, &merge.stderr);

    cleanup_preflight_worktree(repo_root, &temp)?;

    if conflict {
        return Ok(false);
    }
    if !merge.status.success() {
        bail!(
            "failed to preflight squash merge: {}",
            merge_error_message(&merge.stdout, &merge.stderr)
        );
    }
    Ok(true)
}

pub fn apply_squash_merge(repo_root: &Utf8Path, branch: &str, commit_message: &str) -> Result<()> {
    let merge = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["merge", "--squash", "--no-commit", branch])
        .output()
        .with_context(|| format!("failed to squash merge {branch}"))?;
    if !merge.status.success() {
        let _ = reset_worktree(repo_root);
        bail!(
            "failed to squash merge {}: {}",
            branch,
            merge_error_message(&merge.stdout, &merge.stderr)
        );
    }

    let commit = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["commit", "-m", commit_message])
        .output()
        .with_context(|| format!("failed to commit squashed merge for {branch}"))?;
    if !commit.status.success() {
        let _ = reset_worktree(repo_root);
        bail!(
            "failed to commit squashed merge: {}",
            merge_error_message(&commit.stdout, &commit.stderr)
        );
    }

    Ok(())
}

fn run_git(worktree: &Utf8Path, args: &[&str]) -> Result<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(args)
        .output()
        .with_context(|| format!("failed to run git in {}", worktree))?;
    if !output.status.success() {
        bail!("{}", String::from_utf8_lossy(&output.stderr).trim());
    }
    Ok(String::from_utf8(output.stdout)?)
}

fn temporary_apply_worktree_path() -> Utf8PathBuf {
    let suffix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    Utf8PathBuf::from_path_buf(std::env::temp_dir().join(format!("agentd-apply-{suffix}")))
        .expect("temporary worktree path should be valid UTF-8")
}

fn cleanup_preflight_worktree(repo_root: &Utf8Path, worktree: &Utf8Path) -> Result<()> {
    let _ = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["reset", "--hard", "HEAD"])
        .output();
    let _ = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["merge", "--abort"])
        .output();
    remove_worktree(repo_root, worktree)
}

fn reset_worktree(worktree: &Utf8Path) -> Result<()> {
    let output = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["reset", "--hard", "HEAD"])
        .output()
        .with_context(|| format!("failed to reset worktree {}", worktree))?;
    if !output.status.success() {
        bail!(
            "failed to reset worktree: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    Ok(())
}

fn is_merge_conflict_output(stdout: &[u8], stderr: &[u8]) -> bool {
    let output = format!(
        "{}\n{}",
        String::from_utf8_lossy(stdout),
        String::from_utf8_lossy(stderr)
    );
    output.contains("CONFLICT") || output.contains("Automatic merge failed")
}

fn merge_error_message(stdout: &[u8], stderr: &[u8]) -> String {
    let stderr = String::from_utf8_lossy(stderr).trim().to_string();
    if !stderr.is_empty() {
        return stderr;
    }
    String::from_utf8_lossy(stdout).trim().to_string()
}
