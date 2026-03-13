# agentd

Run multiple coding agents in parallel with durable sessions, isolated branches, live terminals, and artifact tracking.

## Status

V1 includes:

- local `agentd` daemon over a Unix socket
- `agentctl create`, `kill`, `attach`, `send-input`, `logs`, `events`, `sessions`, `status`, `diff`, `worktree`, and `daemon`
- PTY-backed agent processes that outlive client connections
- SQLite session metadata in `~/.agentd/state.db`
- SQLite artifact events in `~/.agentd/state.db`
- per-session PTY logs in `~/.agentd/logs/`
- Git worktree isolation in `~/.agentd/worktrees/`

## Configure agents

Create `~/.agentd/config.toml`:

```toml
[agents.claude]
command = "claude"
args = []

[agents.codex]
command = "codex"
args = []
```

The daemon injects:

- `AGENTD_SESSION_ID`
- `AGENTD_SOCKET`
- `AGENTD_WORKSPACE`
- `AGENTD_WORKTREE`
- `AGENTD_BRANCH`
- `AGENTD_TASK`

Instrumented agents can send structured event batches back to the daemon over the Unix socket
named by `AGENTD_SOCKET` using the `append_session_events` request, and consumers can read them
with `stream_events` or `agentctl events`.

Interactive PTY attach is available with `agentctl attach <session_id>`. Detach with `Ctrl-]`.
Only one interactive attacher is allowed per session. Background PTY writes are available with
`agentctl send-input <session_id> -- <text>`.

## Architecture

`agentd` is a local Unix-socket daemon that owns session state and PTY lifecycle. `agentctl` is a
thin client that sends JSON requests over `~/.agentd/agentd.sock` and prints or streams the
responses.

When you create a session, the daemon:

1. Resolves the repo root and current branch for the requested workspace.
2. Allocates a session id, branch name, and isolated git worktree under `~/.agentd/worktrees/`.
3. Stores session metadata in `~/.agentd/state.db`.
4. Spawns the configured agent inside a PTY with the session environment variables injected.

For each running session, the daemon keeps three kinds of state:

- durable metadata in SQLite for status, branch/worktree info, exit state, and structured events
- an append-only PTY log file in `~/.agentd/logs/<session_id>.log`
- an in-memory PTY runtime with the live writer handle and output fan-out used by `attach` and `send-input`

PTY output is copied into the log file and also broadcast to attached clients. PTY input can come
from either an interactive `attach` session or a background `send-input` request. Interactive
attach is exclusive per session; background writes do not steal focus from the current attached
client.

Structured events are separate from raw PTY logs. The daemon records lifecycle events such as
session start, finish, worktree creation/removal, and injected background input. Instrumented
agents can also append their own events, which makes `agentctl events` useful for machine-readable
progress while `agentctl logs` remains the raw terminal transcript.

On startup, the daemon reconciles session rows in SQLite against live processes. Metadata, logs,
and events survive daemon restarts, but live PTY handles do not. That means sessions created under
an earlier daemon lifetime may still appear in `status`, `logs`, and `events`, but they cannot be
reattached or accept new PTY input after the daemon restarts.

## Build

```sh
cargo build
```

## Install

```sh
make install
```

## Usage

```sh
agentctl create --workspace ~/Code/myrepo --task "fix failing tests" --agent claude
agentctl kill wrinkly-bears
agentctl kill --rm wrinkly-bears
agentctl attach wrinkly-bears
agentctl send-input wrinkly-bears -- "please rerun cargo test"$'\n'
agentctl logs wrinkly-bears
agentctl events wrinkly-bears
agentctl sessions
agentctl status wrinkly-bears
agentctl diff wrinkly-bears
agentctl worktree cleanup wrinkly-bears
agentctl worktree create wrinkly-bears
agentctl daemon info
agentctl daemon restart
```

`attach` and `send-input` only work for sessions created under the current daemon lifetime. If
`agentd` restarts, previously running sessions still keep their logs and metadata, but their live
PTY can no longer be reattached or written to.
