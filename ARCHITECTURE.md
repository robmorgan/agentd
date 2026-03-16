# Architecture

`agentd` is a local Unix-socket daemon that owns session state and PTY lifecycle. `agent` is a
thin client that sends requests over `~/.agentd/agentd.sock` and prints or streams the responses.

* Both daemon and client loops leverage `poll()`
* Each session creates its own unix socket file
* We restore terminal state and output using `libghostty-vt`

## Creating a Session

When you create a session, the daemon:

1. Creates a new unix socket file 
2. Resolves the repo root and current branch for the requested workspace.
3. Allocates a session id, branch name, and isolated git worktree under `~/.agentd/worktrees/`.
4. Stores session metadata in `~/.agentd/state.db`.
5. Spawns the configured agent inside a PTY with the session environment variables injected.

For each running session, the daemon keeps three kinds of state:

- durable metadata in SQLite for status, branch/worktree info, exit state, and structured events
- an append-only PTY log file in `~/.agentd/logs/<session_id>.log`
- an in-memory PTY runtime with the live writer handle and output fan-out used by `attach` and `send`

PTY output is copied into the log file and also broadcast to attached clients. PTY input can come
from either an interactive `attach` session or a background `send-input` request. Interactive
attach is exclusive per session; background writes do not steal focus from the current attached
client.

Structured events are separate from raw PTY logs. The daemon records lifecycle events such as
session start, finish, worktree creation or removal, and injected background input. Instrumented
agents can also append their own events, which makes `agent events` useful for machine-readable
progress while `agent logs` remains the raw terminal transcript.

## Socket Files

Each session gets its own unix socket file. The default location depends on your environment variables (checked in priority order):

* AGENTD_DIR => uses exact path (e.g., /custom/path)
* XDG_RUNTIME_DIR => uses {XDG_RUNTIME_DIR}/zmx (recommended on Linux, typically results in /run/user/{uid}/agentd)
* TMPDIR => uses {TMPDIR}/agentd-{uid} (appends uid for multi-user safety)
* /tmp => uses /tmp/agentd-{uid} (default fallback, appends uid for multi-user safety)
  
## libghostty-vt

We use `libghostty-vt` to restore the previous state of the terminal when a client re-attaches to a session.

How it works:

* user creates session `zmx attach `
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
