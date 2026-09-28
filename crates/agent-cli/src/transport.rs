//! How the CLI reaches a daemon: the local Unix socket, or a remote daemon
//! over QUIC (`agent --host NAME`).
//!
//! The protocol's unit is one bidirectional stream per request or attach
//! session, ended by half-closing. Locally that is one Unix connection;
//! remotely it is one QUIC stream on a connection the CLI process opens once
//! and reuses. Callers only see [`DaemonStream`].
//!
//! Remote connections authenticate both ways with pinned keys inside TLS 1.3,
//! like SSH: this machine presents its own key (`remote/client.key`), which
//! the daemon must have authorized, and accepts only the daemon key pinned in
//! `hosts.toml`. Fingerprints match go/internal/transport byte for byte.

use std::{
    fs,
    io::Write,
    net::SocketAddr,
    os::unix::fs::OpenOptionsExt,
    sync::{Arc, Mutex, OnceLock},
    time::Duration,
};

use agentd_shared::{
    hosts::{self, Host, format_fingerprint},
    paths::AppPaths,
};
use anyhow::{Context, Result, anyhow, bail};
use rustls::{
    DigitallySignedStruct, SignatureScheme,
    client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier},
    crypto::{WebPkiSupportedAlgorithms, verify_tls13_signature},
    pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName, UnixTime},
};
use sha2::{Digest, Sha256};
use tokio::{
    io::{AsyncRead, AsyncWrite},
    net::UnixStream,
    sync::OnceCell,
};

/// ALPN of the agent protocol inside QUIC's TLS handshake.
const ALPN: &[u8] = b"agentd";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// Which daemon this CLI process talks to.
#[derive(Debug, Clone)]
pub enum Target {
    Local,
    Remote { name: String, host: Host },
}

static TARGET: OnceLock<Target> = OnceLock::new();
static CONNECTION: OnceCell<(quinn::Endpoint, quinn::Connection)> = OnceCell::const_new();

/// Chooses the daemon for the rest of this process. Called once, early in
/// `main`; without it the local daemon is used.
pub fn set_target(target: Target) {
    let _ = TARGET.set(target);
}

pub fn target() -> &'static Target {
    TARGET.get_or_init(|| Target::Local)
}

/// The remote host's name, when talking to one.
pub fn remote_name() -> Option<&'static str> {
    match target() {
        Target::Remote { name, .. } => Some(name),
        Target::Local => None,
    }
}

/// One request or attach session. Dropping the writer half-closes the
/// stream (a Unix write half shuts down; a QUIC send stream finishes), which
/// is how the daemon learns a client is done or has detached.
pub struct DaemonStream {
    pub reader: StreamReader,
    pub writer: StreamWriter,
}

pub type StreamReader = Box<dyn AsyncRead + Send + Unpin>;
pub type StreamWriter = Box<dyn AsyncWrite + Send + Unpin>;

/// Opens a stream to the current target's daemon.
pub async fn connect(paths: &AppPaths) -> Result<DaemonStream> {
    match target() {
        Target::Local => {
            let stream = UnixStream::connect(paths.socket.as_std_path())
                .await
                .with_context(|| format!("failed to connect to {}", paths.socket))?;
            let (reader, writer) = stream.into_split();
            Ok(DaemonStream { reader: Box::new(reader), writer: Box::new(writer) })
        }
        Target::Remote { name, host } => {
            let conn = remote_connection(paths, name, host).await?;
            open_stream(&conn).await
        }
    }
}

async fn remote_connection(paths: &AppPaths, name: &str, host: &Host) -> Result<quinn::Connection> {
    let (_, conn) = CONNECTION
        .get_or_try_init(|| async {
            let identity = ClientIdentity::load_or_create(paths)?;
            let (endpoint, conn) = dial(&host.address, &identity, &host.fingerprint)
                .await
                .map_err(|err| match err {
                    DialError::KeyChanged { seen } => {
                        anyhow!(key_changed_message(name, host, &seen))
                    }
                    DialError::Other(err) => {
                        err.context(format!("could not reach agentd on `{name}` ({})", host.address))
                    }
                })?;
            Ok::<_, anyhow::Error>((endpoint, conn))
        })
        .await?;
    Ok(conn.clone())
}

/// Closes the remote connection, if any, so the daemon sees this client go
/// away at once rather than after an idle timeout.
pub async fn close() {
    if let Some((endpoint, conn)) = CONNECTION.get() {
        conn.close(0u32.into(), b"done");
        let _ = tokio::time::timeout(Duration::from_secs(1), endpoint.wait_idle()).await;
    }
}

/// Adds what to do to an error from a remote daemon that refused this
/// machine's key. With TLS 1.3 the client finishes its side of the handshake
/// before the daemon judges its certificate, so the refusal can surface on
/// the first stream rather than at connect; this runs on the command's final
/// error so every path gets the hint.
pub fn explain(err: anyhow::Error, paths: &AppPaths) -> anyhow::Error {
    let Target::Remote { name, host } = target() else {
        return err;
    };
    let refused = err.chain().any(|cause| {
        matches!(
            cause.downcast_ref::<quinn::ConnectionError>(),
            Some(quinn::ConnectionError::ConnectionClosed(close))
                if (0x100..0x200).contains(&u64::from(close.error_code))
        )
    });
    if !refused {
        return err;
    }
    let fingerprint = ClientIdentity::load_or_create(paths)
        .map(|id| id.fingerprint)
        .unwrap_or_else(|_| "<this machine's key>".to_string());
    err.context(format!(
        "agentd on `{name}` ({}) refused this machine's key. If it is not authorized yet, run on `{name}`:\n  agentd remote authorize {fingerprint} {}",
        host.address,
        local_hostname()
    ))
}

fn key_changed_message(name: &str, host: &Host, seen: &str) -> String {
    format!(
        "the key of host `{name}` ({}) has changed!\n  pinned:    {}\n  presented: {seen}\nThis could mean someone is intercepting the connection, or the daemon's key was recreated.\nIf you trust the new key, run `agent host rm {name}` and add the host again.",
        host.address, host.fingerprint
    )
}

pub fn local_hostname() -> String {
    nix::unistd::gethostname()
        .ok()
        .and_then(|name| name.into_string().ok())
        .unwrap_or_else(|| "this-machine".to_string())
}

/// This machine's key for remote connections. The certificate is
/// regenerated from the key on every run; only the key is persisted.
pub struct ClientIdentity {
    cert: CertificateDer<'static>,
    key: PrivatePkcs8KeyDer<'static>,
    pub fingerprint: String,
}

impl ClientIdentity {
    /// Loads `remote/client.key`, creating it (0600, Ed25519 PKCS#8 PEM) on
    /// first use.
    pub fn load_or_create(paths: &AppPaths) -> Result<Self> {
        let path = paths.client_key_path();
        let key_pair = match fs::read_to_string(path.as_std_path()) {
            Ok(pem) => {
                rcgen::KeyPair::from_pem(&pem).with_context(|| format!("failed to parse {path}"))?
            }
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                let key_pair = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519)?;
                write_private(&path, key_pair.serialize_pem().as_bytes())?;
                key_pair
            }
            Err(err) => return Err(err).with_context(|| format!("failed to read {path}")),
        };
        Self::from_key_pair(&key_pair)
    }

    pub(crate) fn from_key_pair(key_pair: &rcgen::KeyPair) -> Result<Self> {
        let mut params = rcgen::CertificateParams::new(vec!["agentd".to_string()])?;
        params.distinguished_name.push(rcgen::DnType::CommonName, "agentd");
        let cert = params.self_signed(key_pair)?.der().clone();
        let fingerprint = fingerprint(&cert)?;
        Ok(Self { cert, key: PrivatePkcs8KeyDer::from(key_pair.serialize_der()), fingerprint })
    }
}

fn write_private(path: &camino::Utf8Path, contents: &[u8]) -> Result<()> {
    let dir = path.parent().context("key path has no parent directory")?;
    fs::create_dir_all(dir.as_std_path()).with_context(|| format!("failed to create {dir}"))?;
    fs::set_permissions(dir.as_std_path(), std::os::unix::fs::PermissionsExt::from_mode(0o700))?;
    let tmp = path.with_extension("tmp");
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(tmp.as_std_path())
        .with_context(|| format!("failed to write {tmp}"))?;
    file.write_all(contents)?;
    file.sync_all()?;
    fs::rename(tmp.as_std_path(), path.as_std_path())
        .with_context(|| format!("failed to write {path}"))
}

/// `SHA256:` and the unpadded base64 SHA-256 of the certificate's
/// SubjectPublicKeyInfo, as the daemon computes it.
pub fn fingerprint(cert: &CertificateDer<'_>) -> Result<String> {
    let (_, parsed) = x509_parser::parse_x509_certificate(cert.as_ref())
        .map_err(|err| anyhow!("unreadable certificate: {err}"))?;
    Ok(format_fingerprint(&Sha256::digest(parsed.tbs_certificate.subject_pki.raw)))
}

/// Accepts the daemon's self-signed certificate only if its key matches the
/// pinned fingerprint (or, with no pin, records whatever key it presents).
/// The TLS 1.3 handshake signature is still verified, so the daemon has to
/// hold the key; only CA chain validation is skipped.
#[derive(Debug)]
struct PinnedDaemonKey {
    pinned: Option<String>,
    seen: Mutex<Option<String>>,
    algorithms: WebPkiSupportedAlgorithms,
}

impl ServerCertVerifier for PinnedDaemonKey {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let presented =
            fingerprint(end_entity).map_err(|err| rustls::Error::General(err.to_string()))?;
        *self.seen.lock().expect("fingerprint lock") = Some(presented.clone());
        match &self.pinned {
            Some(pinned) if *pinned != presented => Err(rustls::Error::General(format!(
                "daemon key {presented} is not the pinned {pinned}"
            ))),
            _ => Ok(ServerCertVerified::assertion()),
        }
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        // Only TLS 1.3 is offered.
        Err(rustls::Error::PeerIncompatible(rustls::PeerIncompatible::Tls12NotOffered))
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        verify_tls13_signature(message, cert, dss, &self.algorithms)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.algorithms.supported_schemes()
    }
}

impl PinnedDaemonKey {
    /// The fingerprint the daemon presented, once the handshake got that far.
    fn seen(&self) -> Option<String> {
        self.seen.lock().expect("fingerprint lock").clone()
    }
}

#[derive(Debug)]
pub enum DialError {
    /// The daemon presented a different key than the pinned one.
    KeyChanged {
        seen: String,
    },
    Other(anyhow::Error),
}

/// Connects to a daemon at `address`, accepting only the daemon key
/// `pinned`. Returns the endpoint (which must outlive the connection) and
/// the connection.
pub async fn dial(
    address: &str,
    identity: &ClientIdentity,
    pinned: &str,
) -> Result<(quinn::Endpoint, quinn::Connection), DialError> {
    let (endpoint, verifier, addr) =
        client_endpoint(address, identity, Some(pinned)).await.map_err(DialError::Other)?;
    let result = handshake(&endpoint, addr, address).await;
    if let Some(seen) = verifier.seen()
        && seen != pinned
    {
        return Err(DialError::KeyChanged { seen });
    }
    result.map(|conn| (endpoint, conn)).map_err(DialError::Other)
}

/// Learns the key a daemon presents, without trusting it. Works even before
/// this machine is authorized there: the daemon's certificate arrives before
/// the daemon judges ours.
pub async fn probe_fingerprint(address: &str, identity: &ClientIdentity) -> Result<String> {
    let (endpoint, verifier, addr) = client_endpoint(address, identity, None).await?;
    let result = handshake(&endpoint, addr, address).await;
    if let Ok(conn) = &result {
        conn.close(0u32.into(), b"probe");
    }
    endpoint.close(0u32.into(), b"probe");
    match (verifier.seen(), result) {
        (Some(seen), _) => Ok(seen),
        (None, Err(err)) => Err(err),
        (None, Ok(_)) => bail!("{address} presented no key"),
    }
}

async fn client_endpoint(
    address: &str,
    identity: &ClientIdentity,
    pinned: Option<&str>,
) -> Result<(quinn::Endpoint, Arc<PinnedDaemonKey>, SocketAddr)> {
    let addr = resolve(address).await?;
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = Arc::new(PinnedDaemonKey {
        pinned: pinned.map(str::to_string),
        seen: Mutex::new(None),
        algorithms: provider.signature_verification_algorithms,
    });
    let config = client_config(provider, verifier.clone(), identity)?;
    let bind: SocketAddr = if addr.is_ipv6() {
        (std::net::Ipv6Addr::UNSPECIFIED, 0).into()
    } else {
        (std::net::Ipv4Addr::UNSPECIFIED, 0).into()
    };
    let mut endpoint = quinn::Endpoint::client(bind).context("failed to open a UDP socket")?;
    endpoint.set_default_client_config(config);
    Ok((endpoint, verifier, addr))
}

async fn handshake(
    endpoint: &quinn::Endpoint,
    addr: SocketAddr,
    address: &str,
) -> Result<quinn::Connection> {
    let connecting = endpoint.connect(addr, "agentd")?;
    match tokio::time::timeout(CONNECT_TIMEOUT, connecting).await {
        Ok(Ok(conn)) => Ok(conn),
        Ok(Err(err)) => Err(anyhow!(err).context(format!("could not connect to {address}"))),
        Err(_) => bail!("timed out connecting to {address}"),
    }
}

/// Opens one stream (one request or attach session) on a connection.
async fn open_stream(conn: &quinn::Connection) -> Result<DaemonStream> {
    let (send, recv) = conn.open_bi().await?;
    Ok(DaemonStream { reader: Box::new(recv), writer: Box::new(send) })
}

fn client_config(
    provider: Arc<rustls::crypto::CryptoProvider>,
    verifier: Arc<PinnedDaemonKey>,
    identity: &ClientIdentity,
) -> Result<quinn::ClientConfig> {
    let mut tls = rustls::ClientConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_auth_cert(
            vec![identity.cert.clone()],
            PrivateKeyDer::Pkcs8(identity.key.clone_key()),
        )?;
    tls.alpn_protocols = vec![ALPN.to_vec()];
    let quic = quinn::crypto::rustls::QuicClientConfig::try_from(tls)?;
    let mut config = quinn::ClientConfig::new(Arc::new(quic));
    let mut transport = quinn::TransportConfig::default();
    // Match the daemon: keep idle attachments alive, drop dead peers.
    transport.keep_alive_interval(Some(Duration::from_secs(15)));
    transport.max_idle_timeout(Some(Duration::from_secs(60).try_into()?));
    transport.max_concurrent_uni_streams(0u32.into());
    config.transport_config(Arc::new(transport));
    Ok(config)
}

async fn resolve(address: &str) -> Result<SocketAddr> {
    tokio::net::lookup_host(address)
        .await
        .with_context(|| format!("failed to resolve `{address}` (expected HOST:PORT)"))?
        .next()
        .with_context(|| format!("`{address}` did not resolve to an address"))
}

/// Splits `host/session` into its parts; a plain session name has no host.
pub fn split_host(session_id: &str) -> (Option<&str>, &str) {
    match session_id.split_once('/') {
        Some((host, session)) => (Some(host), session),
        None => (None, session_id),
    }
}

/// Resolves the `--host` flag and any `host/` prefix into the target.
pub fn resolve_target(
    paths: &AppPaths,
    flag: Option<&str>,
    prefixed: Option<&str>,
) -> Result<Target> {
    let name = match (flag, prefixed) {
        (Some(flag), Some(prefixed)) if flag != prefixed => {
            bail!("--host {flag} conflicts with the session address's host `{prefixed}`")
        }
        (Some(name), _) | (None, Some(name)) => name,
        (None, None) => return Ok(Target::Local),
    };
    Ok(Target::Remote { name: name.to_string(), host: hosts::get(paths, name)? })
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use rustls::{
        DistinguishedName,
        server::danger::{ClientCertVerified, ClientCertVerifier},
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    use super::*;
    use crate::tests::test_paths;

    // The same key and fingerprint are pinned in go/internal/transport's
    // tests, so both sides agree on what a fingerprint is.
    const PARITY_KEY_PEM: &str = "-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIBjksdA/xBFa67gw4s1UxuZHtUs8lCcbF6PTgueUIoCc
-----END PRIVATE KEY-----
";
    const PARITY_FINGERPRINT: &str = "SHA256:AOUfC64ic5/SwRe6zJIVSbIBIuiehNsWrqsT/Af5gtY";

    #[test]
    fn fingerprint_matches_the_daemon() {
        let key_pair = rcgen::KeyPair::from_pem(PARITY_KEY_PEM).unwrap();
        assert_eq!(
            ClientIdentity::from_key_pair(&key_pair).unwrap().fingerprint,
            PARITY_FINGERPRINT
        );
    }

    #[test]
    fn client_key_is_created_private_and_stays_stable() {
        let paths = test_paths();
        let first = ClientIdentity::load_or_create(&paths).unwrap();
        let path = paths.client_key_path();
        let mode = fs::metadata(path.as_std_path()).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
        let dir_mode =
            fs::metadata(path.parent().unwrap().as_std_path()).unwrap().permissions().mode();
        assert_eq!(dir_mode & 0o777, 0o700);
        let second = ClientIdentity::load_or_create(&paths).unwrap();
        assert_eq!(first.fingerprint, second.fingerprint);
        assert!(hosts::valid_fingerprint(&first.fingerprint));
        let _ = fs::remove_dir_all(paths.root.as_std_path());
    }

    #[test]
    fn session_addresses_split_on_the_first_slash() {
        assert_eq!(split_host("devbox/auth"), (Some("devbox"), "auth"));
        assert_eq!(split_host("auth"), (None, "auth"));
    }

    #[test]
    fn resolve_target_rejects_conflicting_hosts() {
        let paths = test_paths();
        assert!(matches!(resolve_target(&paths, None, None).unwrap(), Target::Local));
        let err = resolve_target(&paths, Some("a"), Some("b")).unwrap_err();
        assert!(err.to_string().contains("conflicts"), "{err}");
        let err = resolve_target(&paths, Some("a"), None).unwrap_err();
        assert!(err.to_string().contains("agent host add"), "{err:#}");
    }

    /// Accepts client keys whose fingerprints are listed, like the daemon's
    /// authorized_clients.
    #[derive(Debug)]
    struct AllowList {
        allowed: Vec<String>,
        algorithms: WebPkiSupportedAlgorithms,
    }

    impl ClientCertVerifier for AllowList {
        fn root_hint_subjects(&self) -> &[DistinguishedName] {
            &[]
        }

        fn verify_client_cert(
            &self,
            end_entity: &CertificateDer<'_>,
            _intermediates: &[CertificateDer<'_>],
            _now: UnixTime,
        ) -> Result<ClientCertVerified, rustls::Error> {
            let fp =
                fingerprint(end_entity).map_err(|err| rustls::Error::General(err.to_string()))?;
            if self.allowed.contains(&fp) {
                Ok(ClientCertVerified::assertion())
            } else {
                Err(rustls::Error::General(format!("client key {fp} is not authorized")))
            }
        }

        fn verify_tls12_signature(
            &self,
            _message: &[u8],
            _cert: &CertificateDer<'_>,
            _dss: &DigitallySignedStruct,
        ) -> Result<HandshakeSignatureValid, rustls::Error> {
            Err(rustls::Error::PeerIncompatible(rustls::PeerIncompatible::Tls12NotOffered))
        }

        fn verify_tls13_signature(
            &self,
            message: &[u8],
            cert: &CertificateDer<'_>,
            dss: &DigitallySignedStruct,
        ) -> Result<HandshakeSignatureValid, rustls::Error> {
            verify_tls13_signature(message, cert, dss, &self.algorithms)
        }

        fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
            self.algorithms.supported_schemes()
        }
    }

    fn identity() -> ClientIdentity {
        ClientIdentity::from_key_pair(&rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap())
            .unwrap()
    }

    /// A stand-in daemon: every stream echoes its input upper-cased once
    /// the client half-closes.
    fn start_echo(server: &ClientIdentity, allowed: &[&str]) -> (quinn::Endpoint, String) {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let verifier = Arc::new(AllowList {
            allowed: allowed.iter().map(|fp| fp.to_string()).collect(),
            algorithms: provider.signature_verification_algorithms,
        });
        let mut tls = rustls::ServerConfig::builder_with_provider(provider)
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(
                vec![server.cert.clone()],
                PrivateKeyDer::Pkcs8(server.key.clone_key()),
            )
            .unwrap();
        tls.alpn_protocols = vec![ALPN.to_vec()];
        let config = quinn::ServerConfig::with_crypto(Arc::new(
            quinn::crypto::rustls::QuicServerConfig::try_from(tls).unwrap(),
        ));
        let endpoint = quinn::Endpoint::server(config, "127.0.0.1:0".parse().unwrap()).unwrap();
        let address = endpoint.local_addr().unwrap().to_string();
        let accepting = endpoint.clone();
        tokio::spawn(async move {
            while let Some(incoming) = accepting.accept().await {
                tokio::spawn(async move {
                    let Ok(conn) = incoming.await else { return };
                    while let Ok((mut send, mut recv)) = conn.accept_bi().await {
                        tokio::spawn(async move {
                            let Ok(data) = recv.read_to_end(1 << 20).await else { return };
                            let _ = send.write_all(&data.to_ascii_uppercase()).await;
                            let _ = send.finish();
                            let _ = send.stopped().await;
                        });
                    }
                });
            }
        });
        (endpoint, address)
    }

    async fn round_trip(conn: &quinn::Connection, msg: &str) -> Result<String> {
        let DaemonStream { mut reader, mut writer } = open_stream(conn).await?;
        writer.write_all(msg.as_bytes()).await?;
        // Dropping the writer half-closes, as a detach does.
        drop(writer);
        let mut reply = String::new();
        tokio::time::timeout(Duration::from_secs(10), reader.read_to_string(&mut reply)).await??;
        Ok(reply)
    }

    #[tokio::test]
    async fn authorized_client_round_trips_on_many_streams() {
        let (server, client) = (identity(), identity());
        let (_endpoint, address) = start_echo(&server, &[&client.fingerprint]);
        let (_client_endpoint, conn) = dial(&address, &client, &server.fingerprint).await.unwrap();

        // A long-lived stream (like an attachment) does not hold up others.
        let mut long = open_stream(&conn).await.unwrap();
        long.writer.write_all(b"still open").await.unwrap();

        for msg in ["one", "two", "three"] {
            assert_eq!(round_trip(&conn, msg).await.unwrap(), msg.to_uppercase());
        }
    }

    #[tokio::test]
    async fn changed_daemon_key_is_refused() {
        let (server, client) = (identity(), identity());
        let (_endpoint, address) = start_echo(&server, &[&client.fingerprint]);
        let pinned = identity().fingerprint;
        match dial(&address, &client, &pinned).await {
            Err(DialError::KeyChanged { seen }) => assert_eq!(seen, server.fingerprint),
            other => panic!("dial with the wrong pin: {other:?}"),
        }
    }

    #[tokio::test]
    async fn unauthorized_client_gets_no_answer() {
        let (server, stranger) = (identity(), identity());
        let (_endpoint, address) = start_echo(&server, &[&identity().fingerprint]);
        // With TLS 1.3 the client may finish its side of the handshake before
        // the server rejects its certificate; the connection is then unusable.
        if let Ok((_client_endpoint, conn)) = dial(&address, &stranger, &server.fingerprint).await {
            assert!(round_trip(&conn, "hi").await.is_err());
        }
    }

    #[tokio::test]
    async fn probe_learns_the_key_before_authorization() {
        let (server, client) = (identity(), identity());
        let (_endpoint, address) = start_echo(&server, &[]);
        assert_eq!(probe_fingerprint(&address, &client).await.unwrap(), server.fingerprint);
    }
}
