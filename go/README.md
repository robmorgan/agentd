# agentd (Go)

This directory holds the `agentd` daemon (`agentd serve`) and the
per-session worker it spawns (`agentd session-worker`). The `agent` CLI is
Rust (`../crates/agent-cli`) and speaks the same framed protocol (version 1)
and state schema (version 2).

## Layout at a glance

| Package | What it does |
|---|---|
| `internal/transport` | The seam between the protocol and the network: `Stream` (one request or attach session), `Listener`, the shared accept loop, the Unix socket transport, and the QUIC transport with pinned-key identities (`quic-go`). |
| `internal/protocol` | The framed binary protocol and the small daemon management protocol. The same framing and encodings as `crates/agentd-shared/src/protocol.rs`; golden-frame tests on both sides pin identical bytes. |
| `internal/session`, `internal/paths` | The session model and runtime-root resolution, shared in meaning with `crates/agentd-shared`. |
| `internal/db` | `state.db`: schema, and the guarded session state transitions the daemon and workers use. Uses `modernc.org/sqlite` (pure Go). |
| `internal/daemon` | `agentd serve`: lock/socket/pid file lifecycle, create/kill/rm/ls/get, attach and request proxies to workers, history, daemon management, worker supervision and startup reconciliation. Tests run the daemon in-process against real worker processes. |
| `internal/worker` | One session: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket. Real-PTY tests run under `-race`. |
| `cmd/agentd` | `serve [--daemonize]`, `upgrade`, `remote enable|disable|status` (set `[remote] listen` and restart the daemon), `remote id|list|authorize|revoke`, `session-worker`. The agent CLI runs `serve --daemonize`. |

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
- A session is live only if its worker answers on its socket; stored pids are
  never signalled on their own. One daemon per root is enforced by a flock on
  `agentd.lock`.
- `kill` asks the worker over its socket to stop; the worker stops the agent's
  process group (SIGKILL after 5s), writes the history logs, records the
  session as exited and sends `SessionEnded` to attached clients.
- PTY input goes through a bounded per-session queue and writer goroutine, so
  an agent that stops reading input never stalls output.
- Attach is a byte pipe through the daemon, so the daemon never buffers PTY
  output. Slow-consumer policy lives in the worker
  (`internal/worker/broadcast.go`).
- Worker stderr goes to `logs/<id>.worker.log`; a daemonized `serve` logs to
  `agentd.log` in the root (not `logs/`, where it could collide with a
  session named `agentd`).

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
`CMakeLists.txt`). That commit needs **Zig 0.16 or newer**; the script builds
its own checkout under `go/.build/`.

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
