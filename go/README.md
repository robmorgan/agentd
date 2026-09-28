# agentd (Go port)

This directory holds the in-progress Go implementation of the `agentd`
server side. The Rust crates under `../crates` remain the behavioural
reference until the port is complete.

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
| `internal/db` | Schema v8 init, v6/v7 migration (worktree path becomes `cwd`), and the session row operations the worker needs. Uses `modernc.org/sqlite` (pure Go). |
| `internal/worker` | Complete session worker: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket speaking the worker protocol. Covered by real-PTY tests (attach/detach/reattach, survival across disconnect, multiple attachers, resize, exit, kill, slow and disconnecting clients, malformed frames) run under `-race`. |
| `cmd/agentd session-worker` | Done. Takes `--cwd`; injects `AGENTD_CWD` (and `AGENTD_WORKSPACE` as an alias). |
| `cmd/agentd serve` (daemon) | Not started. |

## Running the Go worker

Until `agentd serve` lands the worker is exercised by the tests under
`internal/worker`, which run it in-process against a real PTY. Pointing the
Rust daemon's `AGENTD_WORKER_BIN` at `go/bin/agentd` no longer works: the
worker rejects the old `--repo-root/--worktree/--branch` flags and the two
sides disagree on the protocol version.

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
cmd/agentd            entry point (session-worker today, serve later)
internal/protocol     wire protocol
internal/session      shared session model
internal/paths        runtime root resolution
internal/db           SQLite state store
internal/worker       session worker runtime
scripts/              libghostty-vt build helper
```
