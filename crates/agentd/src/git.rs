use std::{
    process::Command,
    sync::atomic::{AtomicU64, Ordering},
    time::{SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, bail};
use camino::{Utf8Path, Utf8PathBuf};

static APPLY_WORKTREE_COUNTER: AtomicU64 = AtomicU64::new(0);

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
            bail!("failed to create worktree: {}", String::from_utf8_lossy(&output.stderr).trim());
        }
        return Ok(());
    }

    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "add", "--detach", worktree.as_str(), base_branch])
        .output()
        .with_context(|| format!("failed to create worktree {}", worktree))?;

    if !output.status.success() {
        bail!("failed to create worktree: {}", String::from_utf8_lossy(&output.stderr).trim());
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
        bail!("failed to remove worktree: {}", String::from_utf8_lossy(&output.stderr).trim());
    }

    let prune = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["worktree", "prune"])
        .output()
        .with_context(|| format!("failed to prune worktrees for {}", repo_root))?;
    if !prune.status.success() {
        bail!("failed to prune worktrees: {}", String::from_utf8_lossy(&prune.stderr).trim());
    }

    Ok(())
}

pub fn diff_against_base(worktree: &Utf8Path, base_branch: &str) -> Result<String> {
    let status = run_git(worktree, &["status", "--short"])?;
    let committed = run_git(worktree, &["diff", "--stat", &format!("{base_branch}...HEAD")])?;
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
    Ok(!run_git(worktree, &["status", "--porcelain"])?.trim().is_empty())
}

pub fn has_committed_diff_against_base(worktree: &Utf8Path, base_branch: &str) -> Result<bool> {
    Ok(!run_git(worktree, &["diff", "--stat", &format!("{base_branch}...HEAD")])?.trim().is_empty())
}

pub fn branch_has_committed_diff(
    repo_root: &Utf8Path,
    base_branch: &str,
    branch: &str,
) -> Result<bool> {
    if !branch_exists(repo_root, branch)? {
        return Ok(false);
    }
    Ok(!run_git(repo_root, &["diff", "--stat", &format!("{base_branch}...{branch}")])?
        .trim()
        .is_empty())
}

pub fn commit_all(worktree: &Utf8Path, message: &str) -> Result<()> {
    let add = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["add", "--all"])
        .output()
        .with_context(|| format!("failed to stage changes in {}", worktree))?;
    if !add.status.success() {
        bail!("failed to stage changes: {}", String::from_utf8_lossy(&add.stderr).trim());
    }

    let commit = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["commit", "-m", message])
        .output()
        .with_context(|| format!("failed to commit changes in {}", worktree))?;
    if !commit.status.success() {
        bail!("failed to commit changes: {}", String::from_utf8_lossy(&commit.stderr).trim());
    }

    Ok(())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RebaseOutcome {
    RebasingClean,
    Conflicted,
}

pub fn rebase_onto_base(worktree: &Utf8Path, base_branch: &str) -> Result<RebaseOutcome> {
    let rebase = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["rebase", base_branch])
        .output()
        .with_context(|| format!("failed to rebase {} onto {}", worktree, base_branch))?;

    if rebase.status.success() {
        return Ok(RebaseOutcome::RebasingClean);
    }

    if is_merge_conflict_output(&rebase.stdout, &rebase.stderr) {
        let _ = Command::new("git")
            .arg("-C")
            .arg(worktree.as_str())
            .args(["rebase", "--abort"])
            .output();
        return Ok(RebaseOutcome::Conflicted);
    }

    bail!("failed to rebase: {}", merge_error_message(&rebase.stdout, &rebase.stderr));
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MergePreview {
    HasChanges,
    NoChanges,
    Conflicted,
}

pub fn preview_merge(
    repo_root: &Utf8Path,
    base_branch: &str,
    branch: &str,
) -> Result<MergePreview> {
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
        .args(["merge", "--no-ff", "--no-commit", branch])
        .output()
        .with_context(|| format!("failed to preview merge for {branch}"))?;
    let conflict =
        !merge.status.success() && is_merge_conflict_output(&merge.stdout, &merge.stderr);
    let has_changes = if merge.status.success() { has_worktree_changes(&temp)? } else { false };

    cleanup_preflight_worktree(repo_root, &temp)?;

    if conflict {
        return Ok(MergePreview::Conflicted);
    }
    if !merge.status.success() {
        bail!("failed to preview merge: {}", merge_error_message(&merge.stdout, &merge.stderr));
    }
    if has_changes { Ok(MergePreview::HasChanges) } else { Ok(MergePreview::NoChanges) }
}

pub fn apply_merge(repo_root: &Utf8Path, branch: &str) -> Result<()> {
    let merge = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["merge", branch])
        .output()
        .with_context(|| format!("failed to merge {branch}"))?;
    if !merge.status.success() {
        let _ = abort_merge(repo_root);
        bail!("failed to merge {}: {}", branch, merge_error_message(&merge.stdout, &merge.stderr));
    }
    Ok(())
}

pub fn apply_fast_forward(repo_root: &Utf8Path, branch: &str) -> Result<()> {
    let merge = Command::new("git")
        .arg("-C")
        .arg(repo_root.as_str())
        .args(["merge", "--ff-only", branch])
        .output()
        .with_context(|| format!("failed to fast-forward {branch}"))?;
    if !merge.status.success() {
        bail!(
            "failed to fast-forward {}: {}",
            branch,
            merge_error_message(&merge.stdout, &merge.stderr)
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
    let suffix = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_nanos()
        + u128::from(APPLY_WORKTREE_COUNTER.fetch_add(1, Ordering::Relaxed));
    let temp_dir =
        std::fs::canonicalize(std::env::temp_dir()).unwrap_or_else(|_| std::env::temp_dir());
    Utf8PathBuf::from_path_buf(temp_dir.join(format!("agentd-apply-{suffix}")))
        .expect("temporary worktree path should be valid UTF-8")
}

fn cleanup_preflight_worktree(repo_root: &Utf8Path, worktree: &Utf8Path) -> Result<()> {
    let _ = abort_merge(worktree);
    remove_worktree(repo_root, worktree)
}

fn abort_merge(worktree: &Utf8Path) -> Result<()> {
    let _ =
        Command::new("git").arg("-C").arg(worktree.as_str()).args(["merge", "--abort"]).output();
    let output = Command::new("git")
        .arg("-C")
        .arg(worktree.as_str())
        .args(["reset", "--hard", "HEAD"])
        .output()
        .with_context(|| format!("failed to reset worktree {}", worktree))?;
    if !output.status.success() {
        bail!("failed to reset worktree: {}", String::from_utf8_lossy(&output.stderr).trim());
    }
    Ok(())
}

fn is_merge_conflict_output(stdout: &[u8], stderr: &[u8]) -> bool {
    let output =
        format!("{}\n{}", String::from_utf8_lossy(stdout), String::from_utf8_lossy(stderr));
    output.contains("CONFLICT") || output.contains("Automatic merge failed")
}

fn merge_error_message(stdout: &[u8], stderr: &[u8]) -> String {
    let stderr = String::from_utf8_lossy(stderr).trim().to_string();
    if !stderr.is_empty() {
        return stderr;
    }
    String::from_utf8_lossy(stdout).trim().to_string()
}
