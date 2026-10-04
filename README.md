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

  RUN  AGE    NAME            ACTIVITY     CWD                 ATTENTION
  ⚠    4m     dep-bump        waiting 1m   ~/src/app           Claude needs your permission to use Bash
  ●    12m    fix-tests       working      ~/src/app
  ○    6m     docs-readme     exited       ~/src/docs          finished (exit 0)
```

Sessions that need you come first; see [Attention model](#attention-model).

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

Review what the agent produced in its git repository: commits, staged and unstaged edits, and
new files, all since the session started:

```sh
agent diff fix-tests                   # the whole diff, coloured in a terminal
agent diff fix-tests --stat            # or a diffstat, or --name-only
agent status fix-tests                 # includes branch, base, commits and changed files
agent artifacts fix-tests              # what can be downloaded: diff, patch, history, history.vt
agent artifact fix-tests patch -o fix-tests.mbox    # the commits, for `git am`
```

See what sessions cost: memory, CPU, threads and open files of a session's worker and agent, and
its scrollback; or the daemon and every running session at once:

```sh
agent status --stats fix-tests
agent daemon stats
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

Instead of streaming logs constantly, sessions emit attention signals:

```
info      background update
notice    something meaningful happened (an agent went quiet, a session finished)
action    user intervention required (a bell, a permission prompt, a failure)
```

Clients surface sessions based on attention instead of raw output. `agentd` reads the signals from
each session's terminal output, without any cooperation from the agent beyond what terminals
already understand: the bell, desktop notifications (OSC 9 and OSC 777), output stopping and
starting, and the process in the foreground. They are recorded as events, and a session needs
attention until you look at it: attaching to it (or detaching from it) acknowledges it.

```sh
agent ls

  RUN  AGE    NAME            ACTIVITY     CWD                 ATTENTION
  ⚠    12m    fix-tests       waiting 2m   ~/src/app           Claude needs your permission to use Bash
  ●    40m    dep-bump        idle 5m      ~/src/app           idle after 4m of output
  ●    3m     docs            working      ~/src/docs
  ○    1h     refactor        exited       ~/src/app           finished (exit 0)
```

Sessions that need you come first, in `agent ls`, in the picker (`agent`) and in the `Ctrl-Y`
switcher. `agent status NAME` shows the details: activity, foreground command, terminal title,
elapsed time and what needs attention.

Every session's history of events is kept (the newest 500 per session):

```sh
agent events                       # the latest events of every session
agent events fix-tests --follow    # follow one session
agent events --follow --notify     # ring the bell and post a desktop notification when a session needs you
agent events --follow --level action --exec 'say "$AGENTD_EVENT_SESSION needs you"'
agent events --json --after 120    # for scripts: everything after event 120
```

`--notify` writes to your terminal, so it pops a desktop notification wherever your terminal
supports one (iTerm2, Ghostty, WezTerm, kitty and others use OSC 9; GNOME Terminal and other VTE
terminals, foot and urxvt use OSC 777; inside tmux, `set -g allow-passthrough on`). `--exec` runs a
command for each event with `AGENTD_EVENT_ID`, `_HOST`, `_SESSION`, `_KIND`, `_ATTENTION`,
`_SUMMARY` and `_TIME` set. Following reconnects by itself (after a daemon restart, or a lost
connection to a remote host) and resumes after the last event it printed.

### Making agents ask out loud

A bell or a desktop notification is the clearest signal that an agent is waiting for you. Without
one, `agentd` still notices an agent going quiet (`idle`), but cannot tell finishing from asking.

- **Claude Code** sends a notification when it needs permission and when it has been waiting for
  input for a minute. Its default channel depends on the terminal it thinks it runs in, which
  under `agentd` is whatever terminal the daemon was started from, and is often none. Choose one
  explicitly: run `/config` in Claude Code and set **Notifications** to **iTerm2 (OSC 9)**,
  **Ghostty (OSC 777)** or **Terminal Bell** (stored as `preferredNotifChannel` in
  `~/.claude.json`). Do not choose Kitty (OSC 99): `agentd` cannot read it.
- **Codex** sends notifications when a turn completes or it needs approval, once enabled in
  `~/.codex/config.toml`. Codex only notifies when it thinks its terminal is unfocused, which
  under `agentd` it cannot tell, so ask for always:

  ```toml
  [tui]
  notifications = true
  notification_method = "osc9"       # or "bel"
  notification_condition = "always"
  ```

- **Anything else** that rings the bell (`printf '\a'`) or prints `\e]9;message\e\\` is heard the
  same way.

## Architecture

![](/.github/_docs/architecture.png)

`agentd` focuses purely on agent runtime semantics:

- durable PTY-backed agent sessions that outlive the client connection that started them
- session metadata stored in `state.db` under the resolved runtime root
- PTY scrollback held by each session's worker process and saved to `logs/` when the session ends
- interactive reattach with `agent attach`
- background PTY input with `agent send-input`
- git state and artifacts (`agent diff`, `agent artifacts`), read from the session's directory and
  streamed apart from terminal traffic

## Build

`agentd` is two Go binaries, built from one module at the repository root: the `agent` CLI and
the `agentd` daemon.

```sh
make build    # builds libghostty-vt if needed, then bin/agentd and bin/agent
make agent    # just the CLI: pure Go, no cgo or Zig needed
make test     # Go tests, with -race
```

The daemon's session workers link `libghostty-vt` statically, tracking the ghostty commit pinned in
`scripts/build-libghostty.sh` (matching the Go bindings' `CMakeLists.txt`). That commit needs
**Zig 0.16 or newer**; the script fetches it and builds it under `.build/`. Point `ZIG=` at a 0.16
toolchain if the one on `PATH` is older. Zig is only needed when the library is (re)built; an
up-to-date build under `.build/` is reused as is.

libghostty-vt is built `ReleaseFast` by default (`GHOSTTY_OPTIMIZE` overrides it). Zig's default
Debug build parses PTY output at roughly 50 KB/s, which is slow enough to throttle a busy agent;
`BenchmarkTerminalFeed` shows ~640 MB/s with ReleaseFast.

Only libghostty needs cgo; `modernc.org/sqlite` is pure Go, so cross compiling `agentd` is `zig cc`
plus `CGO_ENABLED=1` as described in the go-libghostty README.

To measure what sessions cost on this machine, `agentd bench sessions` starts a private daemon in a
temporary root (never `~/.agentd`), creates the sessions, prints a report, and stops everything it
started, also on Ctrl-C. [BENCHMARKS.md](BENCHMARKS.md) has the numbers and what they mean:

```sh
bin/agentd bench sessions --count 100                       # idle sessions
bin/agentd bench sessions --count 0 --output-heavy 100 --attach 100
bin/agentd bench sessions --count 500 --json-file report.json
```

`--output-heavy N` adds sessions that write output as fast as they can, `--attach K` attaches K
clients to one of them, `--duration` sets how long CPU, throughput and fan-out are measured, and
`--fill-lines` sizes the session used for the full-scrollback snapshot. macOS allows 511
pseudo-terminals for all programs together (`kern.tty.ptmx_max`), so about 500 sessions at most.

For local development, run the freshly built CLI against the freshly built daemon without reinstalling:

```sh
make dev-run ARGS="list"
```

## Install

```sh
make install
```

This installs `agent` and `agentd` side by side in `$GOBIN` (or `$(go env GOPATH)/bin`); set
`BINDIR` to choose another directory. If an older `agent` from `cargo install` is still in
`~/.cargo/bin`, remove it (`cargo uninstall agent-cli`) so the new one is found first.

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

### What A Session Produced

When a session starts in a git repository, `agentd` records HEAD's commit and branch as the
session's base. Everything is then measured from there:

- `agent diff NAME` prints one unified diff of everything that differs from the base: commits made
  since, staged and unstaged changes, and untracked files (respecting `.gitignore`). `--stat` and
  `--name-only` summarise it. The diff applies to a checkout of the base with `git apply` (binary files are
  only named in it; the `patch` artifact carries them).
- `agent status NAME` adds a git section: branch, HEAD, base, upstream with ahead/behind, the
  commits since the base, and the changed files counted by kind with line totals.
- `agent artifacts NAME` lists what can be downloaded, and `agent artifact NAME ARTIFACT [-o FILE]`
  downloads it: `diff`, `patch` (the commits since the base as an mbox, for `git am`), `history` and
  `history.vt` (the terminal history, plain or with escape sequences). With `-o` the file only
  appears once the artifact has arrived whole.

These work for ended sessions too, as long as their directory is still there. A session started
outside a repository, or before `agentd` recorded bases, is compared with HEAD instead.

`agentd` only reads: it runs `git` in the session's directory without taking locks, and never
writes to the repository, its index or its refs. Untracked files are included by marking them in a
private copy of the index. Lists are capped (200 commits, 1000 files) and say when they are. A
download that you stop reading (say, in a pager) simply waits; nothing piles up in the daemon.

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
- `AGENTD_CWD`
- `AGENTD_SESSION_NAME` (same as `AGENTD_SESSION_ID`)

The daemon socket is not passed to agents. They are not sandboxed, though: an agent runs as your
user, so it can still reach the daemon at its usual path. There is no structured event channel for
agents: `agentd` reads attention from what they print (see [Attention model](#attention-model)).
Session status, attention, events, and history are the supported runtime surfaces.

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
restored session was using the alternate screen, replay restores that state naturally. A client
that falls behind a fast agent (a slow link, a stalled terminal) never slows the agent down: it
skips output it cannot keep up with and is repainted from the current screen once it catches up.

Every session has a UID (`agent status` shows it) as well as its name. A name can be reused once
its session is removed; the UID never is, so a reconnecting client can tell that the session under
a name is no longer the one it was attached to.
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
agent diff devbox/auth-refactor --stat
agent --host devbox events --follow --notify             # tell me when a devbox session needs me
agent rm devbox/auth-refactor
```

Closing the laptop or losing the network leaves the session running; `agent attach` again, from any
authorized machine, restores the screen. An attachment that loses its connection reconnects by
itself: the bottom row says it is reconnecting, typing is ignored until it is back, and `Ctrl-\`
gives up. It keeps trying (every 5 seconds at most) until the devbox answers, so a laptop that slept
picks up where it was when it wakes. It stops if the devbox refuses this machine's key or the
session is gone, or was removed and replaced by a new session of the same name while you were away.
A network change that keeps the connection alive (a new address behind the same NAT) needs no
reconnect at all. Commands whose connection drops after they were sent are sent again once the
devbox answers: lookups always, and `new`, `kill`, `rm` and `workspace add|rm` because they carry a
token that lets the devbox do them only once and answer the retry with the first result.
`send-input` is never sent twice. Without `--agent`, `new` uses the devbox's `default_agent`.

With several hosts, `agent hosts` (or `agent host ls`) checks every one at once, this machine's
daemon included as `local`:

```text
NAME    ADDRESS          STATUS   LATENCY  SESSIONS  CPUS  AGENTS        VERSION
local   (this machine)   online   310µs    2/5       10    claude,codex  0.1.0
devbox  100.64.0.5:7433  online   23ms     3/7       32    claude        0.1.0
gpu-01  10.0.0.9:7433    offline  -        -         -     -             -
```

`SESSIONS` is running/total. A host can also be `unauthorized` (it refuses this machine's key) or
`key changed`. `agent host info devbox` shows its machine (OS, CPUs, memory), its agents, daemon
version, capabilities and pinned key. `--host local` (or `local/session`) is this machine.

Across every host at once:

```sh
agent ls --all             # one list, with a HOST column, what needs attention first
agent ls --all --json      # one object per session, with its address and global id
agent events --all -f      # follow every host's events (add --notify for alerts)
```

A session's global id (`agent status` shows it) is its host's daemon key plus the session's UID.
Unlike `host/name`, it never changes when a host is renamed and never refers to a newer session
that reused the name.

`agent --host auto new` lets the CLI choose: among the hosts that are online, have the agent, and
(with `--workspace`) have that workspace, it picks the one with the fewest running sessions per
CPU, and says which and why. It is a hint for spreading work, not a scheduler. It needs
`--workspace` or an absolute or `~/` `--cwd`, which mean the same on every host:

```sh
agent --host auto new --workspace mono fix-flaky-tests
```

Managing keys and hosts:

```sh
agent remote id                  # this machine's client key fingerprint
agent host ls                    # with --no-probe: hosts.toml as is, with pinned keys
agent host info devbox
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
AGENTD_BIN=$PWD/bin/agentd agent daemon restart
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
- git state and artifacts of a session's repository (`agent diff`, `agent artifacts`), locally and
  remotely, with large transfers on their own streams so they never stall an attachment
- attention and activity read from each session's terminal output, kept as events, followed with
  `agent events --follow` and surfaced in `agent ls`, `agent status` and the TUI

Sessions whose worker dies while no daemon is running are shown as `unknown_recovered`.
