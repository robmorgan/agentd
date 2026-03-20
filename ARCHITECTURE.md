# Architecture

`agentd` is a local Unix-socket daemon that owns session state and PTY lifecycle. `agent` is a
thin client that sends requests over the resolved runtime root's `agentd.sock` and prints or
streams the responses.

* Both daemon and client loops leverage `poll()`
* Each session creates its own unix socket file
* Control traffic uses a framed binary protocol
* We restore terminal state and output using `libghostty-vt`

## Wire Protocol

`agent` and `agentd` communicate over a custom framed binary protocol defined in
`crates/agentd-shared/src/protocol.rs`.

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

PTY snapshots, PTY output, and interactive input are sent as raw bytes. Structured events still
carry JSON payloads in the event body, but the socket transport itself is binary.

Most commands use a simple request/response exchange:

1. client connects to the daemon socket
2. client writes one request frame
3. daemon writes one response frame or a stream of response frames
4. streaming commands terminate with an explicit `EndOfStream` frame

`attach` is the bidirectional case. After the initial `AttachSession` request and `Attached`
response, the daemon streams `PtyOutput` frames while the client sends `AttachInput` frames on the
same socket until either side closes or the daemon emits `EndOfStream`.

## Creating a Session

When you create a session, the daemon:

1. Creates a new unix socket file 
2. Resolves the repo root and current branch for the requested workspace.
3. Allocates a session id, branch name, and isolated git worktree under `<runtime-root>/worktrees/`.
4. Stores session metadata in `<runtime-root>/state.db`.
5. Spawns the configured agent inside a PTY with the session environment variables injected.

For each running session, the daemon keeps three kinds of state:

- durable metadata in SQLite for status, branch/worktree info, exit state, and structured events
- an append-only PTY log file in `<runtime-root>/logs/<session_id>.log`
- an in-memory PTY runtime with the live writer handle and output fan-out used by `attach` and `send`

PTY output is copied into the log file and also broadcast to attached clients. PTY input can come
from either an interactive `attach` session or a background `send-input` request. Multiple
interactive attach clients may connect to the same session concurrently. PTY input is shared
across attached clients, and PTY resize follows last-writer-wins semantics.

Structured events are separate from raw PTY logs. The daemon records lifecycle events such as
session start, finish, worktree creation or removal, and injected background input. Instrumented
agents can also append their own events, which makes `agent events` useful for machine-readable
progress while `agent logs` remains the raw terminal transcript.

## Socket Files

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
* `state.db`
* `logs/`
* `worktrees/`
  
## libghostty-vt

We use `libghostty-vt` to restore the previous state of the terminal when a client re-attaches to a session.

How it works:

* user creates or re-attaches to a session with `agent attach <session_id>` or by focusing it in the TUI
* user interacts with terminal stdin
* stdin gets sent to pty via daemon
* daemon sends pty output to client and `ghostty-vt`
* `ghostty-vt` holds terminal state and scrollback
* user disconnects
* user re-attaches to session
* `ghostty-vt` sends terminal snapshot to client stdout

In this way, `ghostty-vt` doesn't sit in the middle of an active terminal session, it simply receives all the same data
the client receives so it can re-hydrate clients that connect to the session. This enables users to pick up where they
left off as if they didn't disconnect from the terminal session at all. It also has the added benefit of being very
fast, the only thing sitting in-between you and your PTY is a unix socket.
