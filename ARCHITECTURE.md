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

The daemon supervises the workers it spawned: it reaps them and records a worker crash as a failed
session. Sessions whose worker disappeared while no daemon was running are marked
`unknown_recovered` at startup or on the next `ls`.

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

PTY snapshots, PTY output, and interactive input are sent as raw bytes. Message kind numbers are
stable across versions; numbers retired in v33 are left unassigned. A client on another version
gets an `Error` frame in its own framing explaining the mismatch.

A second, deliberately tiny daemon management protocol (framed at version 1, JSON payloads) carries
`agent daemon info` status and shutdown, so those keep working across main protocol bumps.

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
`AGENTD_SOCKET`, `AGENTD_CWD` (and `AGENTD_WORKSPACE` as an alias) injected.

The daemon does not manage git worktrees or branches. That responsibility sits with whatever starts
the session (the user, a wrapper, a skill, or the agent itself). See `docs/drop-worktrees.md`.

For each session there are three kinds of state:

- durable metadata in SQLite for status, working directory, attention, and exit state
- terminal state and scrollback in the worker's `libghostty-vt` instance, written to
  `logs/<id>.log` (VT) and `logs/<id>.rendered.log` (plain) when the session ends
- the worker's live PTY and output fan-out used by `attach` and `send-input`

PTY input can come from an interactive `attach` or a background `send-input`. Multiple clients may
attach to the same session concurrently; input is shared and resize is last-writer-wins.

Killing a session sends the worker SIGTERM. The worker stops the agent's whole process group
(SIGKILL after five seconds), writes the history logs, records the session as exited, and sends
`SessionEnded` to attached clients.

## Slow Clients

The PTY is never blocked by a client. Each attachment has a bounded queue in the worker; a client
that falls too far behind loses output until it catches up, and its screen is wrong until the
program repaints or the client reattaches. Resyncing a lagging client from a fresh snapshot is
planned. See `go/internal/worker/broadcast.go`.

## Runtime Root

All runtime state lives under a single root directory. The root is resolved in this priority order:

* AGENTD_DIR => uses exact path (e.g., /custom/path)
* XDG_RUNTIME_DIR => uses `{XDG_RUNTIME_DIR}/agentd` (recommended on Linux, typically `/run/user/{uid}/agentd`)
* macOS default => uses `~/.agentd` when `AGENTD_DIR` and `XDG_RUNTIME_DIR` are unset
* TMPDIR => uses `{TMPDIR}/agentd-{uid}` (appends uid for multi-user safety)
* /tmp => uses `/tmp/agentd-{uid}` (default fallback, appends uid for multi-user safety)

The selected root contains:

* `config.toml`
* `agentd.sock`
* `agentd.pid`
* `state.db` (schema v8; a v6 or v7 database from the former Rust daemon is migrated in place)
* `sessions/` (one socket per live session)
* `logs/` (session history, `<id>.worker.log`, and `agentd.log` for a daemonized daemon)

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
