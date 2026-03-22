use std::{
    env, fs,
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
    let lock_path = workspace_root.join("third_party/ghostty.lock");
    let lock = read_lock_file(&lock_path);

    println!("cargo:rerun-if-changed={}", lock_path.display());

    println!("cargo:rerun-if-changed={}", ghostty_dir.join("include").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("src").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig").display());
    println!("cargo:rerun-if-changed={}", ghostty_dir.join("build.zig.zon").display());

    if !ghostty_dir.join(".git").exists() {
        panic!(
            "Ghostty checkout not found at {}.\nRun `make bootstrap-ghostty` to clone {} at {}.",
            ghostty_dir.display(),
            lock.url,
            lock.commit
        );
    }

    let actual_commit = git_stdout(&ghostty_dir, &["rev-parse", "HEAD"]);
    if actual_commit != lock.commit {
        panic!(
            "Ghostty checkout mismatch at {}.\nExpected commit: {}\nActual commit:   {}\nRun `make bootstrap-ghostty` to sync the local checkout.",
            ghostty_dir.display(),
            lock.commit,
            actual_commit
        );
    }

    let status = Command::new("zig")
        .current_dir(&ghostty_dir)
        .args(["build", "lib-vt"])
        .status()
        .expect("failed to execute `zig`; install Zig to build libghostty-vt");
    if !status.success() {
        panic!("`zig build lib-vt` failed in {}", ghostty_dir.display());
    }

    let lib_dir = ghostty_dir.join("zig-out/lib");
    println!("cargo:rustc-link-search=native={}", lib_dir.display());
    println!("cargo:rustc-link-lib=dylib=ghostty-vt");
    println!("cargo:rustc-link-arg=-Wl,-rpath,{}", lib_dir.display());
}

struct GhosttyLock {
    url: String,
    commit: String,
}

fn read_lock_file(path: &Path) -> GhosttyLock {
    let contents = fs::read_to_string(path)
        .unwrap_or_else(|err| panic!("failed to read {}: {err}", path.display()));
    let mut url = None;
    let mut commit = None;

    for line in contents.lines() {
        if let Some(value) = line.strip_prefix("url=") {
            url = Some(value.trim().to_string());
        } else if let Some(value) = line.strip_prefix("commit=") {
            commit = Some(value.trim().to_string());
        }
    }

    GhosttyLock {
        url: url.unwrap_or_else(|| panic!("missing `url` in {}", path.display())),
        commit: commit.unwrap_or_else(|| panic!("missing `commit` in {}", path.display())),
    }
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
