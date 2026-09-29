//! Writing small private files (keys, hosts.toml) safely when several `agent`
//! processes may run at once: never a torn or truncated file, never one
//! process's key silently replaced by another's.

use std::{
    fs,
    io::Write,
    os::unix::fs::OpenOptionsExt,
    time::{SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};
use nix::fcntl::{Flock, FlockArg};

/// Writes `contents` to a new private (0600) temporary file next to `path`,
/// synced to disk. The caller links or renames it into place.
fn write_temp(path: &Utf8Path, contents: &[u8]) -> Result<Utf8PathBuf> {
    let dir = path.parent().context("path has no parent directory")?;
    let name = path.file_name().unwrap_or("file");
    let nanos = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0);
    let tmp = dir.join(format!(".{name}.{}.{nanos}.tmp", std::process::id()));
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(tmp.as_std_path())
        .with_context(|| format!("failed to write {tmp}"))?;
    let written = file.write_all(contents).and_then(|()| file.sync_all());
    if let Err(err) = written {
        let _ = fs::remove_file(tmp.as_std_path());
        return Err(err).with_context(|| format!("failed to write {tmp}"));
    }
    Ok(tmp)
}

/// Creates `path` with `contents` unless it already exists. Reports whether
/// this call created it. The file appears complete or not at all, and when
/// several processes race, exactly one wins; the others should read the
/// winner's file.
pub fn create_private(path: &Utf8Path, contents: &[u8]) -> Result<bool> {
    let tmp = write_temp(path, contents)?;
    let linked = fs::hard_link(tmp.as_std_path(), path.as_std_path());
    let _ = fs::remove_file(tmp.as_std_path());
    match linked {
        Ok(()) => Ok(true),
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => Ok(false),
        Err(err) => Err(err).with_context(|| format!("failed to write {path}")),
    }
}

/// Atomically replaces `path` with `contents` (mode 0600).
pub fn replace_private(path: &Utf8Path, contents: &[u8]) -> Result<()> {
    let tmp = write_temp(path, contents)?;
    fs::rename(tmp.as_std_path(), path.as_std_path()).map_err(|err| {
        let _ = fs::remove_file(tmp.as_std_path());
        anyhow::Error::new(err).context(format!("failed to write {path}"))
    })
}

/// Holds an exclusive lock on `<path>.lock` until dropped, serialising
/// read-modify-write updates of `path` across processes.
pub struct FileLock(#[allow(dead_code)] Flock<fs::File>);

pub fn lock(path: &Utf8Path) -> Result<FileLock> {
    let lock_path = Utf8PathBuf::from(format!("{path}.lock"));
    let file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(lock_path.as_std_path())
        .with_context(|| format!("failed to open {lock_path}"))?;
    let locked = Flock::lock(file, FlockArg::LockExclusive)
        .map_err(|(_, errno)| anyhow::anyhow!("failed to lock {lock_path}: {errno}"))?;
    Ok(FileLock(locked))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_dir(name: &str) -> Utf8PathBuf {
        let nanos = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_nanos();
        let dir =
            Utf8PathBuf::from(format!("/tmp/agentd-files-{name}-{}-{nanos}", std::process::id()));
        fs::create_dir_all(dir.as_std_path()).unwrap();
        dir
    }

    #[test]
    fn create_private_never_replaces() {
        let dir = temp_dir("create");
        let path = dir.join("client.key");
        assert!(create_private(&path, b"first").unwrap());
        assert!(!create_private(&path, b"second").unwrap());
        assert_eq!(fs::read(path.as_std_path()).unwrap(), b"first");
        // No temporary files are left behind.
        assert_eq!(fs::read_dir(dir.as_std_path()).unwrap().count(), 1);
        let _ = fs::remove_dir_all(dir.as_std_path());
    }

    #[test]
    fn concurrent_creators_agree_on_one_file() {
        let dir = temp_dir("race");
        let path = dir.join("client.key");
        let winners: usize = std::thread::scope(|scope| {
            let handles: Vec<_> = (0..8)
                .map(|i| {
                    let path = path.clone();
                    scope.spawn(move || {
                        create_private(&path, format!("key {i}").as_bytes()).unwrap()
                    })
                })
                .collect();
            handles.into_iter().map(|h| h.join().unwrap() as usize).sum()
        });
        assert_eq!(winners, 1);
        let _ = fs::remove_dir_all(dir.as_std_path());
    }
}
