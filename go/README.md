# agentd (Go port)

This directory holds the in-progress Go implementation of the `agentd`
server side. The Rust crates under `../crates` remain the source of truth
until the port is complete; the two are wired together so each piece can be
swapped over independently.

## Status

| Piece | State |
|---|---|
| `internal/protocol` | Complete. Byte-compatible with `crates/agentd-shared/src/protocol.rs` (protocol version 32), round-trip and golden-frame tests. |
| `internal/session`, `internal/paths` | Complete mirrors of the shared Rust types and runtime-root resolution. |
| `internal/db` | Schema v7 init/migration plus the session row operations the worker needs. Uses `modernc.org/sqlite` (pure Go). |
| `internal/worker` | Complete session worker: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket speaking the worker protocol. Covered by real-PTY tests (attach/detach/reattach, survival across disconnect, multiple attachers, resize, exit, kill, slow and disconnecting clients, malformed frames) run under `-race`. |
| `cmd/agentd session-worker` | Done. |
| `cmd/agentd serve` (daemon) | Not started. |

## Running the Go worker under the Rust daemon

The Rust daemon spawns whatever `AGENTD_WORKER_BIN` points at instead of its
own binary:

```sh
make -C go build
AGENTD_WORKER_BIN=$PWD/go/bin/agentd agent new my-task
```

Everything else (`agent attach`, `send-input`, `history`, `attachments`,
`kill`, `rm`) goes through the daemon unchanged.

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
