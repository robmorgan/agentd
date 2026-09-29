<h1>
<p align="center">
  <img src="https://github.com/user-attachments/assets/fb16c2e0-fabb-4103-aed7-72e57ecc9659" alt="agentd logo" width="250" />
</h1>
  <p align="center">
    <strong>Run coding agents like processes. Supervise them like jobs.</strong>
  </p>
</p>

Developers are running **multiple coding agents** in parallel including Claude Code and Codex.
But once you run more than one, things get messy:

* terminals everywhere
* scrolling logs
* lost artifacts
* agents needing attention

`agentd` turns coding agents into **durable tasks with state, artifacts, and history.** Instead of babysitting terminal
tabs, you supervise work.

## How It Works

`tmux` multiplexes terminals. `agentd` supervises agents.
`agentd` is a daemon runtime for supervising coding agents as durable tasks.

Each task runs inside a managed session with:
* a working directory you choose: a git worktree, a plain checkout, or any directory
* a dedicated PTY
* retained terminal history
* persistent artifacts

This allows developers to supervise agent work without constantly switching between terminal sessions.

## Native Terminal First

`agentd` works WITH your terminal. Ghostty, iTerm2, Kitty, WezTerm... you chose your terminal for
a reason. `agentd` doesn't replace it.

To view an agent: open a new tab, run `agent attach my-agent`. Or use the built-in TUI (`agent`
with no args) for a quick overview of all sessions.

What `agentd` manages that your terminal can't:

* Daemon lifecycle: agents keep running after you disconnect
* Attention signals: know when an agent needs you without watching it
* Session metadata: persists across daemon restarts
* Live reattach: reconnect to running sessions from any terminal
* Vendor-neutral: Claude Code, Codex, any TTY-based agent

Native scrollback, native search, native copy/paste. For free.

## Example

Start a task:

```sh
agent new fix-tests
```

This creates a session, and starts the agent in an attached PTY.

List running tasks:

```sh
agent ls

NAME                 AGENT    STATUS      ELAPSED   TOKENS      COST
● fix-tests          codex    running     12m       2.3k/900    $0.18
⚠ dependency-bump    claude   running     4m        800/120     $0.07
✔ docs-readme        codex    completed   6m        1.1k/420    $0.05
```

You can attach to a running agent to open the underlying PTY session:

```sh
agent attach fix-tests
```

Multiple clients can attach to the same running session at once, including the TUI and one or
more `agent attach` processes.

Detach the local `agent attach` client using `ctrl + \`. Switch to the previous attached session
with `ctrl + [` and the next one with `ctrl + ]`. To inspect or manage other attached
clients:

```sh
agent attachments fix-tests
agent detach fix-tests --attach attach-1
agent detach fix-tests --all
```

Inspect a session's scrollback (live from the session, or from its saved log once it has ended):

```sh
agent history fix-tests
agent history fix-tests --vt
```

Stop a task:

```sh
agent kill fix-tests
```

Remove a session and clean up its artifacts:

```sh
agent rm fix-tests
```

Or use the compatibility form:

```sh
agent kill --rm fix-tests
```

Run the `agent` command without any arguments to open the TUI.

## Core Concepts

`agentd` introduces three core primitives.

- **Tasks.** A long-running unit of work. A task may spawn one or more agents and has a lifecycle (running, completed, failed).
- **Threads.** A sequence of reasoning associated with a task. Threads capture prompts, tool calls, intermediate outputs, and final results.
- **Artifacts.** Outputs produced by agents, such as commits, files, patches, test results, or screenshots.

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

`agentd` focuses purely on agent runtime semantics:

- durable PTY-backed agent sessions that outlive the client connection that started them
- session metadata stored in `state.db` under the resolved runtime root
- PTY scrollback held by each session's worker process and saved to `logs/` when the session ends
- interactive reattach with `agent attach`
- background PTY input with `agent send-input`

## Build

`agentd` is two binaries: the `agent` CLI (Rust) and the `agentd` daemon (Go). The daemon
links `libghostty-vt`, which needs **Zig 0.16 or newer**; `go/scripts/build-libghostty.sh`
fetches the pinned ghostty commit and builds it under `go/.build/` (point `ZIG=` at a 0.16
toolchain if the one on `PATH` is older). See `go/README.md` for details.

```sh
make build    # go/bin/agentd and target/debug/agent
make test     # Go tests (with -race) and cargo test
```

For local development, run the debug CLI against the freshly built daemon without reinstalling:

```sh
make dev-run ARGS="list"
```

## Install

```sh
make install
```

This installs `agent` with `cargo install` and copies `agentd` next to it.

## Working Directories And Worktrees

A session runs in the directory you give it. `agent new` defaults to the current directory;
pass `--cwd DIR` to pick another (it must exist). `agentd` does not create git worktrees,
branches, or merge anything back. If you want isolation between agents working on the same
repository, create the worktree yourself and point the session at it:

```sh
git worktree add -b agent/auth-refactor ../wt/auth-refactor main
agent new --cwd ../wt/auth-refactor auth-refactor
```

A skill or a wrapper script can package this recipe. Two agents started in the same checkout
will step on each other; that is your call, not the daemon's.

### Workspaces

Working directories are resolved on the machine running `agentd`, not the client. Give the
directories you work in a name, then start sessions by name:

```sh
agent workspace add mono '~/src/mono'                         # or: agent workspace add mono .
agent workspace ls
agent new --workspace mono auth-refactor                      # runs in ~/src/mono
agent new --workspace mono --cwd services/api api-fix         # runs in ~/src/mono/services/api
agent workspace rm mono
```

Workspaces are stored by the daemon in `state.db`, so they take effect immediately and are
managed entirely from the CLI. A quoted `~/` path is expanded by the daemon against its own
user's home; other paths are resolved by the CLI, which today always shares the daemon's
filesystem. The directory must exist: `agentd` does not clone, create, or sync workspaces.

With `--workspace`, `--cwd` is relative to the workspace root and may not leave it. A session
records the workspace it was started in (`agent status` shows it); removing or re-adding a
workspace does not move sessions that are already running.

This is the addressing a remote client will use: once `agent --host` exists, a laptop can start
a session in a devbox's checkout without knowing the devbox's paths.

## Configure Agents

Create `<runtime-root>/config.toml`:

```toml
default_agent = "claude"

[agents.claude]
command = "claude"
args = []

[agents.codex]
command = "codex"
args = []
```

Agent picker order follows the order of the `[agents.*]` tables in this file. `default_agent`
must name one of those configured agents. Without it, the default is `claude` if configured,
otherwise the first agent listed. With no `config.toml` at all, `claude` and `codex` are
configured and `claude` is the default.

The daemon injects:

- `AGENTD_SESSION_ID`
- `AGENTD_SOCKET`
- `AGENTD_CWD`
- `AGENTD_SESSION_NAME` (same as `AGENTD_SESSION_ID`)

Instrumented agents can use the injected session environment to locate the daemon socket, but
there is no separate structured event channel. Session status, attention, and history are the
supported runtime surfaces.

Everything lives under one root, `~/.agentd` on macOS and Linux alike, unless `AGENTD_DIR` names
another. Without a home directory, `XDG_RUNTIME_DIR/agentd`, `TMPDIR/agentd-<uid>` and then
`/tmp/agentd-<uid>` are used instead; those may not survive a reboot.

The root contains `config.toml`, `state.db`, `logs/`, `remote/` (keys for remote access),
`hosts.toml`, and the runtime files `agentd.sock`, `agentd.lock`, `agentd.pid` and `sessions/` (one
socket per live session). It is created private to your user (0700), and `agentd` refuses a root
owned by someone else.

Interactive PTY attach is available with `agent attach <name>`. Detach with `Ctrl-\`, switch to
the previous running session with `Ctrl-[`, or switch to the next running session with `Ctrl-]`, or
`agent detach <name> --attach <attach_id>` for a specific client, or
`agent detach <name> --all` to disconnect every attached client on the session.
Use `agent attachments <name>` to inspect the current attachment ids.
Attach clears the visible screen and repaints from the daemon's retained terminal state; if the
restored session was using the alternate screen, replay restores that state naturally.
Multiple interactive attachers are allowed per session, and the TUI uses the same shared attach
path when a worker is focused. Background PTY writes are still available with
`agent send-input <name> -- <text>`.

## Remote Access (Preview)

`agentd` can accept remote clients over QUIC, so sessions on a devbox can be reached from a laptop
over a LAN, Tailscale or WireGuard, without any hosted service. Both sides authenticate with pinned
keys, like SSH host keys and `authorized_keys`.

**On the devbox**, remote access is off by default. Turn it on:

```sh
agentd remote enable
```

This finds the devbox's Tailscale address and listens on UDP port 7433 there. It sets
`[remote] listen` in `config.toml`, restarts the daemon (sessions keep running), and prints the
`agent host add` command to run on the laptop, with the daemon's key fingerprint included.

Without Tailscale, it offers the address of the interface holding the default route and asks first.
A private LAN address is only reachable on that network and may change with DHCP; a public address
is reachable from the whole internet, so it gets a stronger warning. To choose the address
yourself (for example a WireGuard one), or to skip the question in a script:

```sh
agentd remote enable 10.8.0.2          # port 7433
agentd remote enable 10.8.0.2:9000
agentd remote enable --yes             # accept the detected address
agentd remote status
agentd remote disable
```

If the address does not exist yet when the daemon starts (say, Tailscale is still coming up at
boot), the daemon keeps retrying every few seconds. `agentd remote status` and `agent daemon info`
show why it is not listening.

**On the laptop**, run the command `enable` printed:

```sh
agent host add devbox 100.64.0.5:7433 --fingerprint SHA256:...
```

Without `--fingerprint`, `agent host add` shows the key the daemon presents and asks you to confirm
it against `agentd remote id`. Either way it then prints the command that authorizes the laptop.

**Back on the devbox**, run the command it printed:

```sh
agentd remote authorize SHA256:... my-laptop
```

Now any command can run on the devbox, with `--host` or a `host/session` address:

```sh
agent --host devbox new --cwd ~/repo auth-refactor      # a path on the devbox (absolute or ~/...)
agent --host devbox workspace add mono ~/src/mono       # name a directory on the devbox
agent --host devbox new --workspace mono auth-tests      # and start sessions in it
agent --host devbox ls
agent attach devbox/auth-refactor
agent send-input devbox/auth-refactor -- "run the tests"
agent history devbox/auth-refactor
agent rm devbox/auth-refactor
```

Closing the laptop or losing the network leaves the session running; `agent attach` again, from any
authorized machine, restores the screen. Without `--agent`, `new` uses the devbox's `default_agent`.

Managing keys and hosts:

```sh
agent remote id                  # this machine's client key fingerprint
agent host ls
agent host rm devbox
agentd remote list               # on the devbox: authorized clients
agentd remote revoke SHA256:...
```

An authorized client has the same access as you have locally, except that only the devbox itself
can stop its daemon. Authorizing takes effect immediately; revoking a key also disconnects that
client within a few seconds, ending any attachment it has open. `agent daemon info` works remotely; `restart` and `upgrade` only
manage the local daemon.

Troubleshooting:

- **"refused this machine's key"**: the laptop is not authorized; run the `agentd remote authorize`
  command from the error on the devbox.
- **"the key of host ... has changed"**: the devbox presented a different key from the pinned one.
  If you know why (for example `remote/daemon.key` was recreated), run `agent host rm` and
  `agent host add` again. Otherwise, treat it as a possible interception.
- **"timed out connecting"**: check `[remote] listen` on the devbox, that the daemon was restarted,
  and that UDP reaches the port (firewalls often allow TCP only).

## Troubleshooting

Try restarting the daemon:

```sh
agent daemon info
agent daemon restart
agent daemon upgrade
```

`agent daemon restart` is safe while sessions run: they keep running and reattach to the new
daemon. `agent daemon upgrade` still refuses while live sessions are active, since their workers
run the old binary; stop them first.

`agent` starts the daemon on demand with `agentd serve --daemonize`, using the `agentd` binary
installed next to `agent`. Set `AGENTD_BIN` to use a different daemon binary, for example a local
build:

```sh
AGENTD_BIN=$PWD/go/bin/agentd agent daemon restart
```

## Status And Limitations

Current capabilities include:

- local `agentd` daemon over a Unix socket
- remote sessions over QUIC with `agent --host` and pinned keys (preview)
- PTY-backed agent processes that outlive client connections
- sessions that survive the daemon stopping, restarting, or being upgraded: each runs in its
  own worker process, and a new daemon picks it up again
- SQLite-backed session metadata
- per-session PTY history held by the session while it runs and saved to `logs/` when it ends

Sessions whose worker dies while no daemon is running are shown as `unknown_recovered`.
