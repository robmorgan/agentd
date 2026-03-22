<h1>
<p align="center">
  <img src="https://github.com/user-attachments/assets/476f5730-2ff6-47ea-af53-ea27f76699f3" alt="agentd Logo" width="250" />
</h1>
  <p align="center">
    <strong>Run coding agents like processes. Supervise them like jobs.</strong>
  </p>
</p>

Developers are starting to run **multiple coding agents** in parallel including Claude Code and Codex.
But once you run more than one, things get messy:

* terminals everywhere
* scrolling logs
* lost artifacts
* agents needing attention

`agentd` turns coding agents into **durable tasks with state, artifacts, and events.** Instead of babysitting terminal
tabs, you supervise work.

## How It Works

`tmux` multiplexes terminals. `agentd` supervises agents.
`agentd` is a daemon runtime for supervising coding agents as durable tasks.

Each task runs inside a managed session with:
* its own git worktree and branch
* a dedicated PTY
* structured logs
* persistent artifacts
* a machine-readable event stream

This allows developers to supervise agent work without constantly switching between terminal sessions.

## Example

Start a task:

```sh
agent run --name fix-tests "fix failing tests in auth service"
```

This creates a task, assigns a session, and starts the agent in a detached PTY.

List running tasks:

```sh
agent ls

NAME                 AGENT    STATUS      ELAPSED   TOKENS      COST
● fix-tests          codex    running     12m       2.3k/900    $0.18
⚠ dependency-bump    claude   blocked     4m        800/120     $0.07
✔ docs-readme        codex    completed   6m        1.1k/420    $0.05
```

You can attach to a running agent to open the underlying PTY session:

```sh
agent attach fix-tests
```

Multiple clients can attach to the same running session at once, including the TUI and one or
more `agent attach` processes.

Detach the local `agent attach` client using `ctrl + ]`. To inspect or manage other attached
clients:

```sh
agent attachments fix-tests
agent detach fix-tests --attach attach-1
agent detach fix-tests --all
```

Stop a task:

```sh
agent kill fix-tests
```

And explicitly cleanup any artifacts and worktrees:

```sh
agent kill --rm fix-tests
```

Run the `agent` command without any arguments to open the TUI.

## Core Concepts

`agentd` introduces four core primitives.

- **Tasks.** A long-running unit of work. A task may spawn one or more agents and has a lifecycle (running, blocked, completed, failed).
- **Threads.** A sequence of reasoning associated with a task. Threads capture prompts, tool calls, intermediate outputs, and final results.
- **Artifacts.** Outputs produced by agents, such as commits, files, patches, test results, or screenshots.
- **Events.** Structured runtime events emitted by agentd. These allow external clients to build UIs, dashboards, or automation.

## Attention model

Multiple agents create an **attention problem**.

Instead of streaming logs constantly, tasks emit attention signals:

```
info      background update
notice    something meaningful happened
action    user intervention required
```

Clients surface tasks based on attention instead of raw output.

## Architecture

![](/.github/_docs/architecture.png)

`agentd` is not a terminal multiplexer. Terminal layout (splits, panes, tabs) should remain the responsibility of the host terminal or multiplexer. Instead, it focuses purely on agent runtime semantics.

- durable PTY-backed agent sessions that outlive the client connection that started them
- built-in Git worktree isolation under the resolved runtime root
- session metadata and structured events stored in `state.db` under the resolved runtime root
- raw PTY logs stored in `logs/` under the resolved runtime root
- interactive reattach with `agent attach`
- background PTY input with `agent send`
- diff inspection against the base branch with `agent diff`

## Build

Bootstrap the pinned Ghostty checkout first:

```sh
make bootstrap-ghostty
```

This clones `ghostty-org/ghostty` into `vendor/ghostty` and checks out the
exact commit recorded in `third_party/ghostty.lock`.

Then build:

```sh
cargo build
```

For local development, run the debug binaries directly without reinstalling:

```sh
make dev-run ARGS="sessions"
```

## Install

```sh
make install
```

## Configure Agents

Create `<runtime-root>/config.toml`:

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
named by `AGENTD_SOCKET` using the `append_session_events` request. Consumers can read them with
`stream_events` or `agent events`.

Runtime paths are resolved in this order:

- `AGENTD_DIR` as the exact runtime root
- `XDG_RUNTIME_DIR/agentd`
- on macOS, `~/.agentd`
- `TMPDIR/agentd-<uid>`
- `/tmp/agentd-<uid>`

The selected root contains `config.toml`, `agentd.sock`, `agentd.pid`, `state.db`, `logs/`, and
`worktrees/`.

macOS typically does not set `XDG_RUNTIME_DIR`, so the default root on macOS becomes `~/.agentd`
unless `AGENTD_DIR` is set explicitly.

Interactive PTY attach is available with `agent attach <session_id>`. Detach with `Ctrl-]` or
`agent detach <session_id> --attach <attach_id>` for a specific client, or
`agent detach <session_id> --all` to disconnect every attached client on the session.
Use `agent attachments <session_id>` to inspect the current attachment ids.
Multiple interactive attachers are allowed per session, and the TUI uses the same shared attach
path when a worker is focused. Background PTY writes are still available with
`agent send-input <session_id> -- <text>`.

## Troubleshooting

Try restarting the daemon:

```sh
agent daemon info
agent daemon restart
agent daemon upgrade
```

## Status And Limitations

Current capabilities include:

- local `agentd` daemon over a Unix socket
- PTY-backed agent processes that outlive client connections
- SQLite-backed session metadata and event storage
- per-session PTY log persistence
- Git worktree isolation per session

`attach` and `send` only work for sessions created under the current daemon lifetime. If
`agentd` restarts, previously running sessions still keep their metadata, logs, and events, but
their live PTY can no longer be reattached or written to.
