# agentd (Go port)

This directory holds the Go implementation of the `agentd` server side: the
daemon (`agentd serve`) and the per-session worker it spawns. The Rust daemon
under `../crates/agentd` stays on disk as the behavioural reference until it
is deleted (stage 5 of `../docs/drop-worktrees.md`).

The Go side speaks protocol v33 and schema v8, which drop worktree management
in favour of a per-session working directory (see `../docs/drop-worktrees.md`).
The Rust daemon is on v32/v7, so the two no longer interoperate on the wire;
a runtime root created by the Rust daemon is migrated in place the first time
the Go side opens its `state.db`.

## Status

| Piece | State |
|---|---|
| `internal/protocol` | Complete. Same framing and primitive encodings as `crates/agentd-shared/src/protocol.rs`; version 33 removes the worktree, apply, discard and diff messages. Round-trip, golden-frame and removed-kind tests. |
| `internal/session`, `internal/paths` | Complete. Session records carry `Cwd` instead of repo/branch/worktree fields. |
| `internal/db` | Schema v8 init, v6/v7 migration (worktree path becomes `cwd`), and the session row operations the daemon and worker need. Uses `modernc.org/sqlite` (pure Go). |
| `internal/worker` | Complete session worker: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket speaking the worker protocol. Covered by real-PTY tests (attach/detach/reattach, survival across disconnect, multiple attachers, resize, exit, kill, slow and disconnecting clients, malformed frames) run under `-race`. |
| `cmd/agentd session-worker` | Done. Takes `--cwd`; injects `AGENTD_CWD` (and `AGENTD_WORKSPACE` as an alias). |
| `internal/daemon` | Complete daemon: socket and pid file lifecycle, create/kill/rm/ls/get, attach and request proxies to workers, history, daemon management protocol, worker supervision and startup reconciliation. Tests run the daemon in-process against real worker processes. |
| `cmd/agentd serve`, `upgrade` | Done. `serve --daemonize` is what the agent CLI runs. |

## How the daemon and workers fit together

```
agent CLI ──unix socket──► agentd serve ──unix socket──► agentd session-worker ──PTY──► agent
                           (registry, proxy)             (one per session)
```

- Each session's worker is started in its own process session, so stopping
  or restarting the daemon never stops a session. A new daemon finds live
  workers through `state.db` and `sessions/<id>.sock`.
- The daemon supervises the workers it spawned (reaping them and recording a
  crash as a failed session). Sessions whose worker disappeared while no
  daemon was running are marked `unknown_recovered` at startup or on `ls`.
- `kill` sends the worker SIGTERM; the worker stops the agent's process group
  (SIGKILL after 5s), writes the history logs, records the session as exited
  and sends `SessionEnded` to attached clients.
- Attach is a byte pipe through the daemon, so the daemon never buffers PTY
  output. Slow-consumer policy lives in the worker
  (`internal/worker/broadcast.go`).
- Worker stderr goes to `logs/<id>.worker.log`; a daemonized `serve` logs to
  `logs/agentd.log`.

## Running it

```sh
make -C go build
AGENTD_BIN=$PWD/go/bin/agentd agent new --cwd ~/src/project my-task
```

The agent CLI starts `agentd serve --daemonize` itself when no daemon is
running. `AGENTD_BIN` points it at this build instead of the `agentd` next
to the `agent` binary.

## Building

The Go bindings link `libghostty-vt` statically and track a specific ghostty
commit (pinned in `scripts/build-libghostty.sh`, matching the bindings'
`CMakeLists.txt`). That commit needs **Zig 0.16 or newer**, which is newer than
the Zig the Rust build uses for `vendor/ghostty`; the script therefore builds
its own checkout under `go/.build/` and never touches the submodule.

```sh
make -C go build      # builds libghostty-vt if needed, then bin/agentd
make -C go test
```

Point `ZIG=/path/to/zig` at a 0.16 toolchain if the one on `PATH` is older.
Zig is only needed when the library is (re)built; an up-to-date build under
`.build/` is reused as is.

libghostty-vt is built `ReleaseFast` by default (`GHOSTTY_OPTIMIZE` overrides
it). Zig's default Debug build parses PTY output at roughly 50 KB/s, which is
slow enough to throttle a busy agent; `BenchmarkTerminalFeed` shows ~640 MB/s
with ReleaseFast.

Only `libghostty` needs cgo; `modernc.org/sqlite` is pure Go, so cross
compiling is `zig cc` plus `CGO_ENABLED=1` as described in the go-libghostty
README.

## Layout

```
cmd/agentd            entry point: serve, upgrade, session-worker
internal/protocol     wire protocol
internal/session      shared session model
internal/paths        runtime root resolution
internal/db           SQLite state store
internal/daemon       agentd serve: session registry, proxies, lifecycle
internal/worker       session worker runtime
scripts/              libghostty-vt build helper
```
