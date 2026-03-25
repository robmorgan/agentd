use std::{
    env,
    path::{Path, PathBuf},
    process::Command,
};

fn main() {
    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("missing manifest dir"));
    let workspace_root = manifest_dir
        .parent()
        .and_then(Path::parent)
        .expect("agentd crate should live under the workspace root");
    let ghostty_dir = workspace_root.join("vendor/ghostty");
    let expected_commit = git_gitlink_commit(&workspace_root, "vendor/ghostty");

    println!("cargo:rerun-if-changed={}", workspace_root.join(".gitmodules").display());

    println!("cargo:rerun-if-changed={}", ghostty_dir.join("include").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("src").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig.zon").display());

    if !ghostty_dir.join(".git").exists() {
        panic!(
            "Ghostty checkout not found at {}.\nRun `git submodule update --init --recursive` to populate the pinned submodule checkout.",
            ghostty_dir.display(),
        );
    }

    let actual_commit = git_stdout(&ghostty_dir, &["rev-parse", "HEAD"]);
    if actual_commit != expected_commit {
        panic!(
            "Ghostty checkout mismatch at {}.\nExpected commit: {}\nActual commit:   {}\nRun `git submodule update --init --recursive` to sync the local checkout.",
            ghostty_dir.display(),
            expected_commit,
            actual_commit
        );
    }

    let status = Command::new("zig")
        .current_dir(&ghostty_dir)
        .args(["build", "-Demit-lib-vt"])
        .status()
        .expect("failed to execute `zig`; install Zig to build libghostty-vt");
    if !status.success() {
        panic!("`zig build -Demit-lib-vt` failed in {}", ghostty_dir.display());
    }

    let lib_dir = ghostty_dir.join("zig-out/lib");
    println!("cargo:rustc-link-search=native={}", lib_dir.display());
    println!("cargo:rustc-link-lib=dylib=ghostty-vt");
    println!("cargo:rustc-link-arg=-Wl,-rpath,{}", lib_dir.display());
}

fn git_gitlink_commit(repo_dir: &Path, path: &str) -> String {
    let output = Command::new("git")
        .current_dir(repo_dir)
        .args(["ls-files", "--stage", "--", path])
        .output()
        .unwrap_or_else(|err| {
            panic!("failed to inspect git index in {}: {err}", repo_dir.display())
        });
    if !output.status.success() {
        panic!(
            "git ls-files --stage {} failed in {}: {}",
            path,
            repo_dir.display(),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    let line = String::from_utf8(output.stdout).expect("git output should be valid UTF-8");
    let mut fields = line.split_whitespace();
    let mode = fields.next().unwrap_or_default();
    let commit = fields.next().unwrap_or_default();
    if mode != "160000" || commit.is_empty() {
        panic!("expected gitlink entry for {} in {}", path, repo_dir.display());
    }

    commit.to_string()
}

fn git_stdout(repo_dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .current_dir(repo_dir)
        .args(args)
        .output()
        .unwrap_or_else(|err| panic!("failed to execute git in {}: {err}", repo_dir.display()));
    if !output.status.success() {
        panic!(
            "git {} failed in {}: {}",
            args.join(" "),
            repo_dir.display(),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }

    String::from_utf8(output.stdout).expect("git output should be valid UTF-8").trim().to_string()
}
