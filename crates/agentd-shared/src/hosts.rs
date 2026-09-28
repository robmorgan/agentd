//! Remote daemons this machine can reach, each pinned to its key.
//!
//! `hosts.toml` in the runtime root is the client side of agentd's SSH-style
//! trust: like `~/.ssh/known_hosts`, it records which key each named host
//! must present. The daemon side (which client keys a daemon accepts) lives
//! with the daemon, in `remote/authorized_clients`.

use std::{fs, io::Write, os::unix::fs::OpenOptionsExt};

use anyhow::{Context, Result, bail};
use base64::Engine;
use indexmap::IndexMap;
use serde::{Deserialize, Serialize};

use crate::paths::AppPaths;

const FINGERPRINT_PREFIX: &str = "SHA256:";

/// A key fingerprint: `SHA256:` and the unpadded base64 SHA-256 of a
/// certificate's SubjectPublicKeyInfo. The daemon (go/internal/transport)
/// computes the same value.
pub fn valid_fingerprint(fingerprint: &str) -> bool {
    let Some(raw) = fingerprint.strip_prefix(FINGERPRINT_PREFIX) else {
        return false;
    };
    base64::engine::general_purpose::STANDARD_NO_PAD
        .decode(raw)
        .is_ok_and(|digest| digest.len() == 32)
}

/// Formats a SHA-256 digest of a SubjectPublicKeyInfo as a fingerprint.
pub fn format_fingerprint(spki_sha256: &[u8]) -> String {
    format!(
        "{FINGERPRINT_PREFIX}{}",
        base64::engine::general_purpose::STANDARD_NO_PAD.encode(spki_sha256)
    )
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Host {
    /// UDP `host:port` of the daemon's QUIC listener.
    pub address: String,
    /// The key the daemon must present.
    pub fingerprint: String,
}

#[derive(Debug, Default, Serialize, Deserialize)]
struct HostsFile {
    #[serde(default)]
    hosts: IndexMap<String, Host>,
}

/// Host names are used in `host/session` addresses, so they follow the
/// session-name rules and never contain `/`.
pub fn validate_host_name(name: &str) -> Result<()> {
    if crate::session::validate_session_name(name).is_err() {
        bail!(
            "invalid host name `{name}`: use 1-64 lowercase letters, numbers, and single hyphens"
        );
    }
    Ok(())
}

pub fn load(paths: &AppPaths) -> Result<IndexMap<String, Host>> {
    let path = paths.hosts_path();
    match fs::read_to_string(path.as_std_path()) {
        Ok(contents) => Ok(toml::from_str::<HostsFile>(&contents)
            .with_context(|| format!("failed to parse {path}"))?
            .hosts),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(IndexMap::new()),
        Err(err) => Err(err).with_context(|| format!("failed to read {path}")),
    }
}

pub fn get(paths: &AppPaths, name: &str) -> Result<Host> {
    load(paths)?.shift_remove(name).with_context(|| {
        format!("unknown host `{name}`; add it with `agent host add {name} ADDRESS`")
    })
}

pub fn add(paths: &AppPaths, name: &str, host: Host) -> Result<()> {
    validate_host_name(name)?;
    if !valid_fingerprint(&host.fingerprint) {
        bail!("`{}` is not a key fingerprint (expected SHA256:...)", host.fingerprint);
    }
    let mut hosts = load(paths)?;
    if hosts.contains_key(name) {
        bail!("host `{name}` already exists; remove it first with `agent host rm {name}`");
    }
    hosts.insert(name.to_string(), host);
    save(paths, hosts)
}

/// Removes a host. Reports whether it existed.
pub fn remove(paths: &AppPaths, name: &str) -> Result<bool> {
    let mut hosts = load(paths)?;
    if hosts.shift_remove(name).is_none() {
        return Ok(false);
    }
    save(paths, hosts)?;
    Ok(true)
}

fn save(paths: &AppPaths, hosts: IndexMap<String, Host>) -> Result<()> {
    let path = paths.hosts_path();
    let contents = toml::to_string_pretty(&HostsFile { hosts })?;
    let tmp = path.with_extension("toml.tmp");
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(tmp.as_std_path())
        .with_context(|| format!("failed to write {tmp}"))?;
    file.write_all(contents.as_bytes())?;
    file.sync_all()?;
    fs::rename(tmp.as_std_path(), path.as_std_path())
        .with_context(|| format!("failed to write {path}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        os::unix::fs::PermissionsExt,
        sync::atomic::{AtomicU64, Ordering},
    };

    fn test_paths() -> AppPaths {
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let root = camino::Utf8PathBuf::from(format!(
            "/tmp/agentd-hosts-test-{}-{}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        let paths = AppPaths::from_root(root);
        paths.ensure_layout().unwrap();
        paths
    }

    const FP: &str = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU";

    #[test]
    fn fingerprints_are_validated() {
        assert!(valid_fingerprint(FP));
        assert!(!valid_fingerprint("SHA256:bogus"));
        assert!(!valid_fingerprint("MD5:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"));
        assert_eq!(
            format_fingerprint(&[0u8; 32]),
            "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
        );
    }

    #[test]
    fn hosts_round_trip_and_are_private() {
        let paths = test_paths();
        assert!(load(&paths).unwrap().is_empty());
        let host = Host { address: "100.64.0.5:7433".into(), fingerprint: FP.into() };
        add(&paths, "devbox", host.clone()).unwrap();
        assert!(add(&paths, "devbox", host.clone()).is_err(), "duplicate name accepted");
        assert!(add(&paths, "Dev/Box", host.clone()).is_err(), "invalid name accepted");
        assert!(
            add(&paths, "other", Host { fingerprint: "SHA256:nope".into(), ..host.clone() })
                .is_err(),
            "malformed fingerprint accepted"
        );
        assert_eq!(get(&paths, "devbox").unwrap(), host);
        let mode = fs::metadata(paths.hosts_path().as_std_path()).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
        assert!(remove(&paths, "devbox").unwrap());
        assert!(!remove(&paths, "devbox").unwrap());
        assert!(get(&paths, "devbox").unwrap_err().to_string().contains("agent host add"));
        let _ = fs::remove_dir_all(paths.root.as_std_path());
    }
}
