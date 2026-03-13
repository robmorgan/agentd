# agentd

Run multiple coding agents in parallel with durable sessions, isolated branches, live terminals, and artifact tracking.

## Status

V1 includes:

- local `agentd` daemon over a Unix socket
- `agentctl create`, `logs`, `sessions`, `status`, `diff`, and `worktree`
- PTY-backed agent processes that outlive client connections
- SQLite session metadata in `~/.agentd/state.db`
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
- `AGENTD_WORKSPACE`
- `AGENTD_WORKTREE`
- `AGENTD_BRANCH`
- `AGENTD_TASK`

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
agentctl logs wrinkly-bears
agentctl sessions
agentctl status wrinkly-bears
agentctl diff wrinkly-bears
agentctl worktree cleanup wrinkly-bears
agentctl worktree create wrinkly-bears
```
