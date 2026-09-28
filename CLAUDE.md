You are working in the `agentd` repository: a daemon runtime for supervising coding agents as durable tasks. It is a Go daemon (`go/`: `agentd serve` plus one `agentd session-worker` per session) and a Rust `agent` CLI (`crates/`), with QUIC planned as the primary remote transport.

Before larger changes, read `README.md`, `ARCHITECTURE.md`, `ROADMAP.md` and `go/README.md`. The code is the source of truth for current behavior.

# Product direction

`agentd` is:

> A daemon runtime for supervising coding agents as durable tasks.

The central abstraction is not a terminal multiplexer.

`agentd` should own long-running coding-agent sessions and make those sessions durable, reconnectable, remotely accessible, and observable.

The machine where the work runs should run `agentd`.

Examples:

```text
MacBook
└── agentd
    ├── Claude Code
    └── Codex

Linux devbox
└── agentd
    ├── Claude Code
    ├── Codex
    └── shell
```

A client should eventually be able to connect to either daemon using the same application protocol.

The core runtime must not require a hosted control plane.

# Architecture

The architecture, with the remote transport still to come, is approximately:

```text
                    agent clients

               CLI / TUI / future UI
                       │
                       │
                agent protocol
                       │
             ┌─────────┴─────────┐
             │                   │
       Unix socket              QUIC
          local                 remote
             │                   │
             └─────────┬─────────┘
                       │
                       ▼
              ┌──────────────────┐
              │      agentd      │
              │        Go        │
              │                  │
              │ task manager     │
              │ session manager  │
              │ PTY ownership    │
              │ process lifecycle│
              │ persistence      │
              │ client fan-out   │
              │ attention model  │
              └─────────┬────────┘
                        │
                        │ coarse FFI
                        ▼
               ┌─────────────────┐
               │ libghostty-vt   │
               │      Zig        │
               └─────────────────┘
```

The eventual remote path should look like:

```text
MacBook

agent --host devbox attach foo

        │
        │ QUIC
        ▼

Linux devbox

agentd
 ├── PTY
 ├── terminal state
 └── Claude Code
```

If the client disconnects, the PTY and coding agent continue running.

A later connection should be able to reattach to the existing session.

# Important architectural decisions

## 1. Go owns the daemon

The daemon and its per-session workers are Go. Go owns:

- networking
- concurrency
- process supervision
- session lifecycle
- client management
- persistence coordination
- protocol handling
- remote connections
- PTY fan-out
- task/agent lifecycle

The `agent` CLI is Rust and speaks the same protocol. Keep the two codecs in step; golden-frame tests on both sides pin identical bytes.

Preserve existing user-visible behavior unless there is a compelling reason to change it.

## 2. libghostty-vt remains the terminal engine

Do not implement a terminal emulator in Go.

Use `libghostty-vt` through its C ABI / Go bindings for terminal state.

The intended model is:

```text
PTY output
   │
   ├──────────────► attached clients
   │
   └──────────────► libghostty-vt
                         │
                         ▼
                  terminal state
                         │
                         ▼
                  future snapshots
```

Keep the Go/Zig boundary coarse.

Prefer passing buffers into Zig rather than crossing the FFI boundary for individual cells, characters, or terminal operations.

## 3. QUIC is the primary remote transport

Do not design the remote architecture around HTTP, REST, WebSockets, gRPC, or a single TCP connection.

QUIC should eventually be the primary remote transport.

The application protocol must remain logically separate from QUIC.

Conceptually:

```text
agent protocol
     │
     ├── Unix socket
     └── QUIC
```

QUIC should eventually be used as a native multiplexed transport rather than merely putting one application byte stream over one QUIC stream.

Likely future stream roles include:

```text
QUIC connection
│
├── control stream
│
│   commands
│   responses
│   session discovery
│
├── event stream
│
│   task state changes
│   attention notifications
│
├── attachment stream: session A
│
│   terminal output
│   user input
│   resize
│   snapshot
│
├── attachment stream: session B
│
└── artifact streams
    diffs
    logs
    patches
    large outputs
```

Do not implement all of this immediately.

Design current abstractions so this is possible later.

## 4. Do not build a hosted control plane

A future optional service might eventually provide:

- identity
- machine discovery
- authorization
- organization membership
- audit metadata
- rendezvous
- relay

But this is explicitly out of scope right now.

The daemon must work directly between machines.

Assume connectivity can initially come from:

- localhost
- LAN
- Tailscale
- WireGuard
- directly reachable hosts

Do not implement NAT traversal, relays, accounts, organizations, or SaaS infrastructure.

## 5. Sessions belong to agentd, not connections

A network connection is ephemeral.

A session is durable.

Never make session identity depend on the lifetime of a Unix socket, TCP connection, or QUIC connection.

The model should be:

```text
client
   │
connection #1
   │
   ▼
agentd ─────── session
                  │
                  └── coding agent continues

connection disappears

client
   │
connection #2
   │
   ▼
agentd ─────── same session
```

This distinction should influence the internal API now.

## 6. Keep agentd agent-specific

Do not turn this into a generic distributed execution platform.

The differentiator is agent supervision.

Preserve and strengthen concepts such as:

- tasks
- coding agents
- PTY-backed execution
- artifacts
- diffs
- attention
- agent lifecycle
- durable interactive sessions

`agentd` should answer:

> Which agent needs my attention?

rather than merely:

> Which terminal is running?

# Package layout

```text
go/
  cmd/agentd/          serve, upgrade, session-worker
  internal/
    daemon/            session registry, proxies, lifecycle, supervision
    worker/            one session: PTY, terminal state, fan-out, input queue
    protocol/          framed protocol + daemon management protocol
    transport/         Stream/Listener seam, accept loop, Unix sockets, QUIC + identities
    db/                state.db
    session/           session model
    paths/             runtime root
crates/
  agent-cli/           the `agent` CLI and TUI
  agentd-shared/       Rust side of the protocol, schema, paths, config
```

Avoid excessive package fragmentation.

Do not create interfaces merely for hypothetical extensibility.

Interfaces should exist where there are real boundaries such as transports or test seams.

# Protocol direction

Keep the framed binary protocol (version 1; `go/internal/protocol` and `crates/agentd-shared/src/protocol.rs`).

Do not prematurely replace it with:

- JSON RPC
- protobuf
- gRPC
- HTTP APIs

The protocol should remain compact, explicit, versioned, and suitable for interactive streams.

Separate:

```text
protocol semantics
```

from:

```text
transport framing
```

where practical.

Eventually the same session semantics should be usable over Unix sockets and QUIC.

# Concurrency expectations

Treat concurrency design as a first-class concern.

Avoid uncontrolled goroutine creation and unbounded channels.

For every long-lived goroutine, make ownership and termination clear.

For every queue or buffer, define what happens when the consumer is slow.

Particularly consider:

- PTY output fan-out
- multiple attached clients
- blocked writers
- client disconnects
- session termination
- daemon shutdown
- resize races
- concurrent input
- slow remote clients

Run the Go race detector as part of development.

# Backpressure

Do not allow one slow attached client to stall the underlying PTY or every other client.

The architecture should make it possible to define an explicit policy for slow consumers.

Do not silently introduce unbounded buffering.

If the proper policy cannot yet be implemented, document the current behavior and leave a clear seam for it.

# Testing requirements

Preserve or add tests for:

- daemon startup/shutdown
- session creation
- PTY spawning
- attach
- detach
- reattach
- process survival after detach
- multiple attachers
- terminal resize
- process exit
- session cleanup
- malformed protocol frames
- protocol version mismatch
- client disconnect during output
- daemon shutdown during active session

Where possible use real PTYs rather than mocking away the interesting behavior.

Add benchmarks where they expose architectural costs, but do not optimize prematurely.

# Performance philosophy

Measure first.

Useful eventual metrics include:

- allocations per PTY write
- attach latency
- snapshot latency
- RSS per session
- Go heap per session
- goroutines per session
- fan-out throughput
- bytes copied per PTY write
- protocol framing overhead
- cgo overhead
- QUIC stream overhead

Do not introduce complexity for theoretical micro-optimizations without measurements.

However, avoid architectural decisions that obviously require unnecessary copying or serialization in hot paths.

# Open-source dependency philosophy

Prefer well-maintained focused libraries.

Do not build your own QUIC implementation.

When QUIC work begins, evaluate the current Go QUIC ecosystem and select an appropriate maintained implementation based on actual requirements.

Do not add QUIC merely to satisfy the roadmap before the protocol and session ownership boundaries are clean.

When real `agentd` workloads expose bugs or meaningful inefficiencies in dependencies, prefer producing clean upstream fixes rather than maintaining unnecessary forks.

# CLI

Preserve the existing user experience where practical.

The commands are:

```text
agent new [--cwd DIR] [NAME]
agent list | ls
agent attach
agent attachments
agent detach
agent history
agent send-input
agent status
agent kill
agent rm
agent daemon info | restart | upgrade
```

Do not rename commands merely because implementation internals changed.

# Non-goals

Do not implement:

- hosted control plane
- browser UI
- WebSocket transport
- NAT traversal
- relay network
- organization accounts
- billing
- Kubernetes integration
- distributed scheduling
- generic containers
- agent marketplace
- complex plugin systems
- arbitrary RPC framework

# Coding style

Favor:

- simple Go
- explicit ownership
- small understandable abstractions
- standard library where appropriate
- strong tests
- clear comments around lifecycle/concurrency
- measurable performance decisions

Avoid:

- clever generics
- abstract factories
- unnecessary dependency injection
- deep interface hierarchies
- speculative framework design
- premature distributed-systems machinery

# Documentation

As implementation progresses, keep these synchronized:

- `README.md`
- `ARCHITECTURE.md`
- `ROADMAP.md`

Document architectural decisions that would otherwise be difficult to infer from code.

# Current state and next milestone

Done:
- The Go daemon and per-session workers, on protocol v1 and state schema v1.
- Sessions survive client disconnects and daemon restarts.
- Liveness is checked through the worker sockets, and a flock enforces a single daemon.
- Attach fan-out and PTY input are bounded, and the runtime root is private to the user.
- The transport split: the daemon serves `transport.Stream`s from any `transport.Listener`.
- The daemon's QUIC listener (off unless `[remote] listen` is set): one QUIC stream per request or attachment, mutual TLS with pinned Ed25519 keys, managed with `agentd remote id|list|authorize|revoke`.
- The CLI's QUIC client: `agent --host NAME` and `NAME/session` addresses, `agent host add|ls|rm` (confirm-on-first-use key pinning in `hosts.toml`), and `agent remote id` (`remote/client.key`).

Next:
1. Automatic reattach after a network drop, and noticing dead clients faster than the 60s idle timeout.
2. Cross-host discovery (`agent ls` across hosts) and host health.
3. Per-client permissions and audit, if remote peers need to be limited.

Known gaps:
- History is only saved when a session exits.
- A lagging attach client loses output until the program repaints. The fix is an unsolicited snapshot resync, which needs CLI support.
- Agents receive `AGENTD_SOCKET` and are trusted peers of the daemon.

Do not begin QUIC before the transport split: the session and ownership model has to be clean first.

# Definition of the eventual vertical slice

Keep this future workflow in mind when making architectural decisions:

```sh
agent --host devbox run \
  --name auth-refactor \
  "refactor authentication and run the tests"
```

The remote machine runs:

```text
agentd
  │
  └── PTY
       │
       └── Claude Code
```

The user closes their laptop.

The client disappears.

The agent continues.

Hours later, possibly from another machine:

```sh
agent attach devbox/auth-refactor
```

The client establishes a new QUIC connection.

`agentd` locates the existing session, provides the current terminal state, attaches the client to the live PTY, and interaction continues.

That is the architectural destination.

Move toward it deliberately, incrementally, and with working software at every stage.

## Skill routing

When the user's request matches an available skill, ALWAYS invoke it using the Skill
tool as your FIRST action. Do NOT answer directly, do NOT use other tools first.
The skill has specialized workflows that produce better results than ad-hoc answers.

Key routing rules:
- Product ideas, "is this worth building", brainstorming → invoke office-hours
- Bugs, errors, "why is this broken", 500 errors → invoke investigate
- Ship, deploy, push, create PR → invoke ship
- QA, test the site, find bugs → invoke qa
- Code review, check my diff → invoke review
- Update docs after shipping → invoke document-release
- Weekly retro → invoke retro
- Design system, brand → invoke design-consultation
- Visual audit, design polish → invoke design-review
- Architecture review → invoke plan-eng-review
- Save progress, checkpoint, resume → invoke checkpoint
- Code quality, health check → invoke health
