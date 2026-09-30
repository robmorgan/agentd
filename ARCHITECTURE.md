# Architecture

`agentd` is a local daemon that owns coding-agent sessions: their PTYs, processes, terminal state,
and metadata. `agent` is a thin client that sends requests over the resolved runtime root's
`agentd.sock` and prints or streams the responses.

```text
agent (Go CLI) ──unix socket──► agentd serve (Go) ──unix socket──► agentd session-worker (Go) ──PTY──► agent process
                                session registry,                   one per session: PTY owner,
                                request/attach proxy                shadow terminal, client fan-out
```

* The daemon (`internal/daemon`) owns the session registry in SQLite, spawns one worker per
  session, and proxies client requests and attach streams to it.
* Each session worker (`internal/worker`) owns one PTY and child process, feeds the output
  through `libghostty-vt`, and fans it out to attached clients over its own socket in
  `<runtime-root>/sessions/<id>.sock`.
* Control traffic and attach streams use a framed binary protocol.

## Code Layout

Both binaries are one Go module at the repository root.

| Package | What it does |
|---|---|
| `cmd/agentd` | `serve [--daemonize]`, `upgrade`, `remote enable\|disable\|status` (set `[remote] listen` and restart the daemon), `remote id\|list\|authorize\|revoke`, `session-worker`. The agent CLI runs `serve --daemonize`. |
| `cmd/agent`, `internal/cli` | The `agent` CLI: commands, the session picker, attach and the Ctrl-Y overlay (plain ANSI), and `hosts.toml` for remote hosts. Pure Go: it never imports `worker`, `daemon` or `db`, and `cmd/agent`'s tests check that, then drive both real binaries through PTYs. |
| `internal/daemon` | `agentd serve`: lock/socket/pid file lifecycle, create/kill/rm/ls/get, attach and request proxies to workers, history, daemon management, worker supervision and startup reconciliation. Tests run the daemon in-process against real worker processes. |
| `internal/worker` | One session: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket. Real-PTY tests run under `-race`. |
| `internal/transport` | The seam between the protocol and the network: `Stream` (one request or attach session), `Listener`, the shared accept loop, the Unix socket transport, and the QUIC transport with pinned-key identities (`quic-go`). |
| `internal/protocol` | The framed binary protocol and the small daemon management protocol, used by both binaries. Golden-frame tests pin the bytes. |
| `internal/session`, `internal/paths`, `internal/config` | The session model (and name rules), runtime-root resolution, and `config.toml`, shared by both binaries. |
| `internal/db` | `state.db`: schema, and the guarded session state transitions the daemon and workers use. Uses `modernc.org/sqlite` (pure Go). Only the daemon and its workers open it. |
| `scripts/` | The libghostty-vt build helper. |

A few details of the daemon and workers that the sections below do not cover:

* `kill` asks the worker over its socket to stop; the worker stops the agent's process group
  (SIGKILL after 5s), writes the history logs, records the session as exited and sends
  `SessionEnded` to attached clients.
* PTY input goes through a bounded per-session queue and writer goroutine, so an agent that stops
  reading input never stalls output.
* Attach is a byte pipe through the daemon, so the daemon never buffers PTY output. Slow-consumer
  policy lives in the worker (`internal/worker/broadcast.go`).
* Worker stderr goes to `logs/<id>.worker.log`; a daemonized `serve` logs to `agentd.log` in the
  root (not `logs/`, where it could collide with a session named `agentd`).

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
one request/response exchange or one attach session. Everything above `internal/transport` sees
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
* `remote/authorized_clients` lists the client key fingerprints allowed in. It is checked on every
  handshake, on every new stream, and every 5 seconds for each open connection, so revoking a key
  also closes that client's live connections, attachments included. TLS session tickets are
  disabled: a resumed session would skip the check. `agentd remote authorize|revoke` edit the file
  under a lock and replace it atomically, so concurrent edits are never lost.
* An authorized client has the same access as the local socket owner, except that only a local
  client may stop the daemon: a remote one could not start it again.
* Keys are created without ever replacing one that exists (written to a temporary file, then
  linked into place), so processes creating a key at the same moment all end up using the same one.

The `agent` CLI is the client (`internal/cli/client.go`, over the same `internal/transport`
QUIC code as the daemon). Its key is
`remote/client.key`, in the same format as the daemon's, and `hosts.toml` maps host names to an
address and the daemon fingerprint pinned by `agent host add`. One CLI process opens at most one
QUIC connection and one stream per request or attachment, so an attachment and the overlay's
requests share a connection. A remote host is never started, restarted or upgraded from the CLI.
With TLS 1.3 a client can finish its half of the handshake
before the daemon rejects its key, so a refusal may only surface on the first stream; the CLI
recognises the TLS alert there and says how to authorize the machine. A name that resolves to
several addresses (`localhost` is often `::1` and `127.0.0.1`) is dialled at each in turn within a
10-second budget. A test pins one key's fingerprint, since `hosts.toml` and `authorized_clients`
store fingerprints and they must never change.

`agentd remote enable` picks the listen address when none is given: first a Tailscale address,
used without asking; otherwise the source address of the default route, found by connecting a UDP
socket (which sends nothing), which it asks about first, since that is a LAN address that may
change, a shared carrier-grade NAT address, or a public one. An address in Tailscale's ranges
(`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) only counts as Tailscale's when Tailscale confirms it: it is
on the `tailscale0` interface, or `tailscale ip` reports it (macOS names the interface `utunN`).
Carriers and other VPNs hand out `100.64.0.0/10` addresses too.

`enable` edits only the `listen` line of `config.toml`, keeping comments and following a symlinked
file. The edit is checked before it is written: it must parse and leave every other setting as it
was, or the file is left alone. If the restarted daemon does not listen on the new address, the
previous setting is restored. A daemon whose listen address cannot be bound, or whose listener
stops, keeps local service and binds again every 5 seconds until shutdown, reporting the reason in
its management status.

A client that disappears without closing (a laptop lid, a lost network) is noticed by the daemon's
60-second idle timeout; until then its attachment is still listed. The session is unaffected.

The daemon's tests run full sessions over QUIC, and over a TCP stand-in, to keep the seam honest.

## Wire Protocol

`agent` and `agentd` communicate over a custom framed binary protocol, implemented in
`internal/protocol` and used by both binaries. Golden-frame tests pin its bytes, so a CLI and a
daemon of different builds, possibly on different machines, stay compatible. The current version
is 1.

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

1. Validates the name and agent, and resolves `cwd` on its own machine: an absolute path, a
   `~/` path expanded against the daemon user's home, or a path relative to a named workspace
   (stored in `state.db`, managed with `agent workspace`). The directory must exist; it does not
   need to be a git repository.
2. Allocates a session id and stores the record in `<runtime-root>/state.db`.
3. Spawns `agentd session-worker` in a new process session.
4. Waits for the worker to report the session running and bind its socket.

The worker spawns the configured agent inside a PTY in `cwd`, with `AGENTD_SESSION_ID`,
`AGENTD_SOCKET` and `AGENTD_CWD` injected.

The daemon does not manage git worktrees or branches. That responsibility sits with whatever starts
the session (the user, a wrapper, a skill, or the agent itself); the README shows the worktree
recipe.

Working directories are resolved by the daemon, never the client, because the client may be on
another machine. The local CLI still canonicalizes a plain `--cwd` itself, since it shares the
daemon's filesystem; with `--workspace` it sends the name and the relative path unchanged.

Workspaces are daemon state, not configuration: they live in the `workspaces` table of
`state.db` and clients add, list and remove them over the protocol, so a remote client can manage
them without touching the host's files. A session stores the workspace name next to the cwd
resolved from it at creation. Workspaces are addressing only: agentd does not clone, create or
sync them.

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
planned. See `internal/worker/broadcast.go`.

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
* `state.db` (schema v2; older versions are migrated forward, newer ones refused. Migrations
  must be additive, because session workers keep writing to the file across daemon upgrades)
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
built `ReleaseFast` under `.build/` by `scripts/build-libghostty.sh`. The FFI boundary is
coarse: whole PTY reads go in, whole snapshots come out. Only the worker links it: the `agent` CLI
never imports the worker (or the daemon and its SQLite store), and a test keeps it buildable with
`CGO_ENABLED=0`.
