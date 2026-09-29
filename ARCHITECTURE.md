# Architecture

`agentd` is a local daemon that owns coding-agent sessions: their PTYs, processes, terminal state,
and metadata. `agent` is a thin client that sends requests over the resolved runtime root's
`agentd.sock` and prints or streams the responses.

```text
agent (Rust CLI) ──unix socket──► agentd serve (Go) ──unix socket──► agentd session-worker (Go) ──PTY──► agent process
                                  session registry,                   one per session: PTY owner,
                                  request/attach proxy                shadow terminal, client fan-out
```

* The daemon (`go/internal/daemon`) owns the session registry in SQLite, spawns one worker per
  session, and proxies client requests and attach streams to it.
* Each session worker (`go/internal/worker`) owns one PTY and child process, feeds the output
  through `libghostty-vt`, and fans it out to attached clients over its own socket in
  `<runtime-root>/sessions/<id>.sock`.
* Control traffic and attach streams use a framed binary protocol.

## Sessions Belong To The Daemon, Not To Connections

A client connection is disposable. Disconnecting (or never detaching cleanly) only ends that
attachment; the agent keeps running and any later connection can reattach.

The daemon is disposable too. Each worker runs in its own process session, so stopping, crashing,
or restarting the daemon does not touch running sessions. A new daemon finds them again through
`state.db` and the per-session sockets. `agent daemon restart` is therefore always safe.

A session counts as live only if its worker accepts a connection on its socket. Pids recorded in
`state.db` are never trusted on their own: after a crash or reboot they may belong to unrelated
processes. A single daemon per root is enforced by an exclusive lock on `agentd.lock`, held for
the daemon's lifetime and released by the kernel however it exits; `agentd.pid` is informational.

The daemon supervises the workers it spawned: it reaps them and records a worker crash as a failed
session. Sessions whose worker disappeared while no daemon was running are marked
`unknown_recovered` at startup or on the next `ls`.

## Transports

The protocol runs over any bidirectional byte stream that supports half-close. Each stream carries
one request/response exchange or one attach session. Everything above `go/internal/transport` sees
only that `Stream` and a `Listener` that yields streams, so the daemon does not know or care which
transport a client used. The daemon-to-worker link is always a local Unix socket.

Two transports exist:

* **Unix socket** (`agentd.sock`): local clients, always on.
* **QUIC** (off unless `[remote] listen` is set): remote clients. Each client holds one QUIC
  connection and opens one bidirectional QUIC stream per request or attachment, so long-lived
  attachments and short requests are multiplexed without blocking each other. Keep-alives hold
  idle connections open, and each connection may have at most 256 streams. The daemon's first
  datagrams are 1200 bytes, QUIC's minimum, rather than quic-go's default 1280: Tailscale's
  interface MTU is 1280 including IP and UDP headers, so larger ones never leave it and the
  handshake times out. Path MTU discovery raises the size afterwards where the path allows.

QUIC connections authenticate both ways with pinned keys inside TLS 1.3 (ALPN `agentd`), the way
SSH uses host keys and `authorized_keys`, with no certificate authority:

* The daemon's key is `remote/daemon.key` (Ed25519, created on first use). Its fingerprint is
  `SHA256:` plus the base64 SHA-256 of the public key, and is what clients pin.
* `remote/authorized_clients` lists the client key fingerprints allowed in. It is read on every
  handshake, so authorizing and revoking take effect for new connections without a restart.
* An authorized client has the same access as the local socket owner.

The `agent` CLI is the client (`crates/agent-cli/src/transport.rs`, quinn and rustls). Its key is
`remote/client.key`, in the same format as the daemon's, and `hosts.toml` maps host names to an
address and the daemon fingerprint pinned by `agent host add`. One CLI process opens at most one
QUIC connection and one stream per request or attachment, so an attachment and the overlay's
requests share a connection. A remote host is never started, restarted or upgraded from the CLI,
and there is no local fallback for it. With TLS 1.3 a client can finish its half of the handshake
before the daemon rejects its key, so a refusal may only surface on the first stream; the CLI
recognises the TLS alert there and says how to authorize the machine. Both sides' tests pin one
key and its fingerprint, so the Go and Rust fingerprint definitions cannot drift apart.

`agentd remote enable` picks the listen address when none is given: first a Tailscale address
(`100.64.0.0/10` or `fd7a:115c:a1e0::/48` on an interface, or `tailscale ip -4`), used without
asking; otherwise the source address of the default route, found by connecting a UDP socket (which
sends nothing), which it asks about first, since that is a LAN address that may change or a public
one. It edits only the `listen` line of `config.toml`, keeping comments, and refuses layouts it
cannot edit safely. A daemon whose listen address cannot be bound keeps local service and retries
the bind every 5 seconds until shutdown, reporting the reason in its management status.

A client that disappears without closing (a laptop lid, a lost network) is noticed by the daemon's
60-second idle timeout; until then its attachment is still listed. The session is unaffected.

The daemon's tests run full sessions over QUIC, and over a TCP stand-in, to keep the seam honest.

## Wire Protocol

`agent` and `agentd` communicate over a custom framed binary protocol, implemented in
`go/internal/protocol` and `crates/agentd-shared/src/protocol.rs`. Golden-frame tests on both sides
keep the two byte-for-byte compatible. The current version is 33.

Each frame has a fixed 16-byte header followed by a payload:

* `magic` (`u32`, little-endian) identifies an `agentd` protocol frame
* `version` (`u16`, little-endian) must match the current protocol version
* `message_type` (`u16`, little-endian) identifies the request or response variant
* `flags` (`u16`, currently unused and set to `0`)
* `reserved` (`u16`, currently unused and set to `0`)
* `payload_len` (`u32`, little-endian) gives the number of payload bytes that follow

Payloads are binary-encoded field-by-field rather than serialized as JSON:

* strings => `u32 len` + UTF-8 bytes
* byte blobs => `u32 len` + raw bytes
* booleans => single `u8`
* optional values => presence `u8` followed by the encoded value
* lists => `u32 count` followed by elements

PTY snapshots, PTY output, and interactive input are sent as raw bytes. The current protocol
version is 1. A client on another version gets an `Error` frame in its own framing explaining the
mismatch, so the `Error` message kind and its payload never change.

A second, deliberately tiny daemon management protocol (framed at header version 0, JSON payloads)
carries daemon status and shutdown for `agent daemon info`, `restart` and `upgrade`, so those keep
working even when the CLI and daemon speak different versions of the main protocol.

Most commands use a simple request/response exchange:

1. client connects to the daemon socket
2. client writes one request frame
3. daemon writes one response frame or a stream of response frames
4. streaming commands terminate with an explicit `EndOfStream` or `SessionEnded` frame

`attach` is the bidirectional case. After the initial `AttachSession` request and `Attached`
response (which carries a snapshot of the current screen), the worker streams `PtyOutput` frames
while the client sends `AttachInput`, `AttachResize`, and `AttachSnapshot` frames on the same
socket until either side closes. The daemon forwards the attach stream as raw bytes and never
buffers PTY output itself.

## Creating a Session

When you create a session (`agent new [--cwd DIR] [NAME]`), the daemon:

1. Validates the name and agent, and that `cwd` is an existing absolute directory. It does not need
   to be a git repository.
2. Allocates a session id and stores the record in `<runtime-root>/state.db`.
3. Spawns `agentd session-worker` in a new process session.
4. Waits for the worker to report the session running and bind its socket.

The worker spawns the configured agent inside a PTY in `cwd`, with `AGENTD_SESSION_ID`,
`AGENTD_SOCKET` and `AGENTD_CWD` injected.

The daemon does not manage git worktrees or branches. That responsibility sits with whatever starts
the session (the user, a wrapper, a skill, or the agent itself); the README shows the worktree
recipe.

For each session there are three kinds of state:

- durable metadata in SQLite for status, working directory, attention, and exit state
- terminal state and scrollback in the worker's `libghostty-vt` instance, written to
  `logs/<id>.log` (VT) and `logs/<id>.rendered.log` (plain) when the session ends
- the worker's live PTY and output fan-out used by `attach` and `send-input`

PTY input can come from an interactive `attach` or a background `send-input`. Multiple clients may
attach to the same session concurrently; input is shared and resize is last-writer-wins.

Killing a session asks the worker, over its socket, to stop (the worker handles SIGTERM the same
way). The worker stops the agent's whole process group (SIGKILL after five seconds), writes the
history logs, records the session as exited, and sends `SessionEnded` to attached clients. The
daemon signals a worker directly only if it is wedged, and only while its socket still answers.

## Slow Clients

The PTY is never blocked by a client. Each attachment has a bounded queue in the worker; a client
that falls too far behind loses output until it catches up, and its screen is wrong until the
program repaints or the client reattaches. Resyncing a lagging client from a fresh snapshot is
planned. See `go/internal/worker/broadcast.go`.

Input goes the other way through one bounded queue per session, drained by a dedicated writer.
An agent that stops reading its input therefore never stalls PTY output or other requests; once
the queue is full, further input is refused with an error until the agent catches up.

## Runtime Root

All state lives under a single root directory. The root is resolved in this priority order:

* AGENTD_DIR => uses exact path (e.g., /custom/path)
* home => uses `~/.agentd`, on every platform
* XDG_RUNTIME_DIR => uses `{XDG_RUNTIME_DIR}/agentd`, only without a home directory
* TMPDIR => uses `{TMPDIR}/agentd-{uid}` (appends uid for multi-user safety)
* /tmp => uses `/tmp/agentd-{uid}` (default fallback, appends uid for multi-user safety)

The root is deliberately not a per-boot runtime directory by default. `XDG_RUNTIME_DIR` (typically
`/run/user/{uid}`, a tmpfs) is wiped on reboot, and on Linux also once the user's last session
ends, which would take `state.db`, session history and the daemon's remote key with it; every
remote client would then see a changed key. The runtime files in the root (the sockets, lock and
pid file) are harmless after a reboot: a starting daemon removes a stale socket while holding the
lock, and session liveness is judged by connecting to the session's socket.

The selected root contains:

* `config.toml`
* `agentd.sock`
* `agentd.lock` (held by the running daemon)
* `agentd.pid` (informational)
* `state.db` (schema v1; the daemon refuses any other version)
* `sessions/` (one socket per live session)
* `agentd.log` (output of a daemonized daemon)
* `remote/` (`daemon.key`, `authorized_clients`, and the CLI's `client.key`)
* `hosts.toml` (remote hosts known to the CLI, with pinned daemon keys)
* `logs/` (session history and `<id>.worker.log`)

The root and everything in it are private to the user (directories 0700, files and sockets 0600),
since logs hold full agent transcripts. `agentd` refuses a root owned by another user.

## libghostty-vt

We use `libghostty-vt` to restore the previous state of the terminal when a client re-attaches to a session.

How it works:

* user creates or re-attaches to a session with `agent attach <name>` or by focusing it in the TUI
* user interacts with terminal stdin
* stdin gets sent to the PTY via the daemon and the session worker
* the worker sends PTY output to attached clients and to `ghostty-vt`
* `ghostty-vt` holds terminal state and scrollback
* user disconnects
* user re-attaches to session
* `ghostty-vt` produces a terminal snapshot that is sent to the client before live output

In this way, `ghostty-vt` doesn't sit in the middle of an active terminal session, it simply receives all the same data
the client receives so it can re-hydrate clients that connect to the session. This enables users to pick up where they
left off as if they didn't disconnect from the terminal session at all.

The worker uses `go.mitchellh.com/libghostty`, whose pinned `libghostty-vt` needs Zig 0.16 and is
built `ReleaseFast` under `go/.build/` by `go/scripts/build-libghostty.sh`. The FFI boundary is
coarse: whole PTY reads go in, whole snapshots come out.
