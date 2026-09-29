use std::{
    fs,
    os::unix::fs::{DirBuilderExt, MetadataExt, PermissionsExt},
};

use anyhow::{Context, Result, bail};
use camino::{Utf8Path, Utf8PathBuf};
use nix::unistd::getuid;

pub const APP_DIR_NAME: &str = "agentd";

#[derive(Debug, Clone)]
pub struct AppPaths {
    pub root: Utf8PathBuf,
    pub socket: Utf8PathBuf,
    pub pid_file: Utf8PathBuf,
    pub database: Utf8PathBuf,
    pub config: Utf8PathBuf,
    pub logs_dir: Utf8PathBuf,
    pub sessions_dir: Utf8PathBuf,
}

impl AppPaths {
    pub fn discover() -> Result<Self> {
        let root = discover_root(
            std::env::var_os("AGENTD_DIR"),
            std::env::var_os("XDG_RUNTIME_DIR"),
            dirs::home_dir(),
            std::env::var_os("TMPDIR"),
            getuid().as_raw(),
        )?;
        Ok(Self::from_root(root))
    }

    /// Creates the runtime root, `logs/` and `sessions/` as private (0700)
    /// directories, tightening them if they already exist. The root holds the
    /// daemon socket, session sockets and logs of agent output, so a root that
    /// is a symlink or belongs to another user is refused rather than used.
    /// go/internal/paths EnsureLayout applies the same rules.
    pub fn ensure_layout(&self) -> Result<()> {
        ensure_private_dir(&self.root, true)?;
        for path in [&self.logs_dir, &self.sessions_dir] {
            ensure_private_dir(path, false)?;
        }
        Ok(())
    }

    pub fn log_path(&self, session_id: &str) -> Utf8PathBuf {
        self.logs_dir.join(format!("{session_id}.log"))
    }

    pub fn rendered_log_path(&self, session_id: &str) -> Utf8PathBuf {
        self.logs_dir.join(format!("{session_id}.rendered.log"))
    }

    /// The worker's own stdout/stderr, written by the Go daemon when it
    /// starts a session worker (go/internal/daemon workerLogPath).
    pub fn worker_log_path(&self, session_id: &str) -> Utf8PathBuf {
        self.logs_dir.join(format!("{session_id}.worker.log"))
    }

    /// This machine's key for connecting to remote daemons.
    pub fn client_key_path(&self) -> Utf8PathBuf {
        self.root.join("remote").join("client.key")
    }

    /// Remote daemons this machine knows, with their pinned keys.
    pub fn hosts_path(&self) -> Utf8PathBuf {
        self.root.join("hosts.toml")
    }

    pub fn session_socket_path(&self, session_id: &str) -> Utf8PathBuf {
        self.sessions_dir.join(format!("{session_id}.sock"))
    }

    pub fn as_utf8(path: &Utf8Path) -> &str {
        path.as_str()
    }

    pub fn from_root(root: Utf8PathBuf) -> Self {
        Self {
            socket: root.join("agentd.sock"),
            pid_file: root.join("agentd.pid"),
            database: root.join("state.db"),
            config: root.join("config.toml"),
            logs_dir: root.join("logs"),
            sessions_dir: root.join("sessions"),
            root,
        }
    }
}

const PRIVATE_DIR_MODE: u32 = 0o700;

fn ensure_private_dir(path: &Utf8Path, recursive: bool) -> Result<()> {
    match fs::DirBuilder::new().recursive(recursive).mode(PRIVATE_DIR_MODE).create(path) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(err) => return Err(err).with_context(|| format!("failed to create {path}")),
    }

    let meta = fs::symlink_metadata(path).with_context(|| format!("failed to inspect {path}"))?;
    if meta.file_type().is_symlink() {
        bail!("refusing to use {path}: it is a symlink; point AGENTD_DIR at a real directory");
    }
    if !meta.is_dir() {
        bail!("refusing to use {path}: it is not a directory");
    }
    let uid = getuid().as_raw();
    if meta.uid() != uid {
        bail!(
            "refusing to use {path}: it is owned by uid {} but agent runs as uid {uid}",
            meta.uid()
        );
    }
    if meta.permissions().mode() & 0o777 != PRIVATE_DIR_MODE {
        fs::set_permissions(path, fs::Permissions::from_mode(PRIVATE_DIR_MODE))
            .with_context(|| format!("failed to set permissions on {path}"))?;
    }
    Ok(())
}

/// Picks the root: `AGENTD_DIR`, else `~/.agentd`. The root holds state that
/// must survive reboots (state.db, logs, the remote keys), so a per-boot
/// runtime directory such as `XDG_RUNTIME_DIR` (`/run/user/UID`, a tmpfs) is
/// only a fallback for an account without a home directory, as are the temp
/// directories. go/internal/paths discoverRoot makes the same choice.
fn discover_root(
    agentd_dir: Option<std::ffi::OsString>,
    xdg_runtime_dir: Option<std::ffi::OsString>,
    home_dir: Option<std::path::PathBuf>,
    tmpdir: Option<std::ffi::OsString>,
    uid: u32,
) -> Result<Utf8PathBuf> {
    if let Some(root) = utf8_env_path("AGENTD_DIR", agentd_dir)? {
        return Ok(root);
    }

    if let Some(home_dir) = home_dir {
        return Utf8PathBuf::from_path_buf(home_dir.join(format!(".{APP_DIR_NAME}")))
            .map_err(|_| anyhow::anyhow!("HOME is not valid UTF-8"));
    }

    if let Some(runtime_dir) = utf8_env_path("XDG_RUNTIME_DIR", xdg_runtime_dir)? {
        return Ok(runtime_dir.join(APP_DIR_NAME));
    }

    if let Some(tmpdir) = utf8_env_path("TMPDIR", tmpdir)? {
        return Ok(tmpdir.join(format!("{APP_DIR_NAME}-{uid}")));
    }

    Ok(Utf8PathBuf::from(format!("/tmp/{APP_DIR_NAME}-{uid}")))
}

fn utf8_env_path(name: &str, value: Option<std::ffi::OsString>) -> Result<Option<Utf8PathBuf>> {
    let Some(value) = value else {
        return Ok(None);
    };

    let path = std::path::PathBuf::from(value);
    Utf8PathBuf::from_path_buf(path)
        .map(Some)
        .map_err(|_| anyhow::anyhow!("{name} is not valid UTF-8"))
}

#[cfg(test)]
mod tests {
    use super::{APP_DIR_NAME, AppPaths, discover_root};
    use camino::Utf8PathBuf;
    use std::{fs, os::unix::fs::PermissionsExt};

    fn temp_root(name: &str) -> Utf8PathBuf {
        let suffix =
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos();
        Utf8PathBuf::from(format!("/tmp/agentd-paths-{name}-{}-{suffix}", std::process::id()))
    }

    fn mode(path: &Utf8PathBuf) -> u32 {
        fs::metadata(path.as_std_path()).unwrap().permissions().mode() & 0o777
    }

    #[test]
    fn ensure_layout_creates_private_directories() {
        let paths = AppPaths::from_root(temp_root("fresh"));
        paths.ensure_layout().unwrap();
        for dir in [&paths.root, &paths.logs_dir, &paths.sessions_dir] {
            assert_eq!(mode(dir), 0o700, "{dir}");
        }
        let _ = fs::remove_dir_all(paths.root.as_std_path());
    }

    #[test]
    fn ensure_layout_tightens_existing_directories() {
        let paths = AppPaths::from_root(temp_root("loose"));
        for dir in [&paths.root, &paths.logs_dir, &paths.sessions_dir] {
            fs::create_dir_all(dir.as_std_path()).unwrap();
            fs::set_permissions(dir.as_std_path(), fs::Permissions::from_mode(0o755)).unwrap();
        }
        paths.ensure_layout().unwrap();
        for dir in [&paths.root, &paths.logs_dir, &paths.sessions_dir] {
            assert_eq!(mode(dir), 0o700, "{dir}");
        }
        let _ = fs::remove_dir_all(paths.root.as_std_path());
    }

    #[test]
    fn ensure_layout_refuses_symlinked_root() {
        let target = temp_root("target");
        fs::create_dir_all(target.as_std_path()).unwrap();
        let link = temp_root("link");
        std::os::unix::fs::symlink(target.as_std_path(), link.as_std_path()).unwrap();

        let err = AppPaths::from_root(link.clone()).ensure_layout().unwrap_err().to_string();
        assert!(err.contains("symlink") && err.contains(link.as_str()), "{err}");
        assert!(!target.join("logs").exists());

        let _ = fs::remove_file(link.as_std_path());
        let _ = fs::remove_dir_all(target.as_std_path());
    }

    fn home(path: &str) -> Option<std::path::PathBuf> {
        Some(std::path::PathBuf::from(path))
    }

    #[test]
    fn agentd_dir_is_used_as_exact_root() {
        let root = discover_root(
            Some("/custom/agentd-root".into()),
            Some("/run/user/501".into()),
            home("/home/tester"),
            Some("/var/tmp".into()),
            501,
        )
        .unwrap();
        assert_eq!(root, Utf8PathBuf::from("/custom/agentd-root"));
    }

    #[test]
    fn home_root_wins_over_xdg_runtime_dir() {
        // /run/user is a tmpfs wiped on reboot; the root holds state and keys.
        let root = discover_root(
            None,
            Some("/run/user/501".into()),
            home("/home/tester"),
            Some("/var/tmp".into()),
            501,
        )
        .unwrap();
        assert_eq!(root, Utf8PathBuf::from("/home/tester/.agentd"));
    }

    #[test]
    fn xdg_runtime_dir_is_used_without_a_home() {
        let root =
            discover_root(None, Some("/run/user/501".into()), None, Some("/var/tmp".into()), 501)
                .unwrap();
        assert_eq!(root, Utf8PathBuf::from("/run/user/501").join(APP_DIR_NAME));
    }

    #[test]
    fn tmpdir_uses_uid_suffix_without_a_home() {
        let root = discover_root(None, None, None, Some("/var/tmp".into()), 501).unwrap();
        assert_eq!(root, Utf8PathBuf::from("/var/tmp").join(format!("{APP_DIR_NAME}-501")));
    }

    #[test]
    fn tmp_fallback_uses_uid_suffix() {
        let root = discover_root(None, None, None, None, 501).unwrap();
        assert_eq!(root, Utf8PathBuf::from(format!("/tmp/{APP_DIR_NAME}-501")));
    }

    #[test]
    fn derived_paths_follow_selected_root() {
        let paths = AppPaths::from_root(Utf8PathBuf::from("/Users/tester/.agentd"));
        assert_eq!(paths.socket, Utf8PathBuf::from("/Users/tester/.agentd/agentd.sock"));
        assert_eq!(paths.pid_file, Utf8PathBuf::from("/Users/tester/.agentd/agentd.pid"));
        assert_eq!(paths.database, Utf8PathBuf::from("/Users/tester/.agentd/state.db"));
        assert_eq!(paths.config, Utf8PathBuf::from("/Users/tester/.agentd/config.toml"));
        assert_eq!(paths.logs_dir, Utf8PathBuf::from("/Users/tester/.agentd/logs"));
        assert_eq!(paths.sessions_dir, Utf8PathBuf::from("/Users/tester/.agentd/sessions"));
    }
}
