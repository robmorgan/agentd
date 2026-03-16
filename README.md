<h1>
<p align="center">
  <br>agentd
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

Then detach using `ctrl + ]` or `agent detach`.

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
- built-in Git worktree isolation under `~/.agentd/worktrees/`
- session metadata and structured events stored in SQLite at `~/.agentd/state.db`
- raw PTY logs stored per session in `~/.agentd/logs/`
- interactive reattach with `agent attach`
- background PTY input with `agent send`
- diff inspection against the base branch with `agent diff`

## Build

```sh
cargo build
```

## Install

```sh
make install
```

## Configure Agents

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
named by `AGENTD_SOCKET` using the `append_session_events` request. Consumers can read them with
`stream_events` or `agent events`.

Interactive PTY attach is available with `agent attach <session_id>`. Detach with `Ctrl-]`.
Only one interactive attacher is allowed per session. Background PTY writes are available with
`agent send-input <session_id> -- <text>`.

## Troubleshooting

Try restarting the daemon:

```sh
agent daemon info
agent daemon restart
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
