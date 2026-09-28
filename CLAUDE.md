You are working in the `agentd` repository.

Your task is to begin moving `agentd` toward a Go-based, distributed agent runtime with QUIC as its primary remote transport, while preserving the useful behavior and product direction that already exists.

Do not treat this as a greenfield rewrite.

First inspect the entire repository, understand the existing architecture and behavior, read `README.md`, `ARCHITECTURE.md`, `ROADMAP.md`, the protocol implementation, daemon/session lifecycle, PTY handling, persistence, libghostty integration, CLI/TUI, and tests.

Then design and implement the next incremental step toward the architecture below.

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

# Target architecture

The intended architecture is approximately:

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

## 1. Go is the target language for the daemon

The daemon should gradually move toward Go.

Go should own:

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

Do not rewrite everything merely to translate Rust into Go.

Use the migration to simplify the architecture where appropriate.

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

# Desired migration strategy

Do not attempt to complete the entire roadmap in one change.

Work incrementally.

The immediate goal is to establish a clean foundation for the Go daemon while preserving the existing implementation as a behavioral reference.

A sensible first milestone is:

```text
Go daemon
   │
   ├── starts and stops correctly
   ├── owns a Unix socket
   ├── understands a minimal versioned protocol
   ├── can spawn one PTY-backed process
   ├── can retain that process after client disconnect
   └── can reattach a client
```

Remote QUIC does not need to work in the first milestone.

# Before changing code

Perform a repository audit.

Identify:

1. Current crates/packages and their responsibilities.
2. Where daemon lifecycle lives.
3. Where PTYs are created and owned.
4. Where attached clients are tracked.
5. How multiple attachers currently work.
6. How PTY output is fanned out.
7. How the binary protocol works.
8. Which protocol messages exist.
9. How SQLite state is structured.
10. How worktrees are created and cleaned up.
11. How `libghostty-vt` is currently embedded.
12. How terminal state restoration currently works.
13. Which tests define important behavior.
14. Which parts can reasonably be reused during a Go migration.
15. Which parts should remain unchanged for now.

Do not assume the roadmap is perfectly aligned with the implementation.

The code is the source of truth for current behavior.

# Create a migration plan

Before large implementation changes, create or update a concise engineering document describing:

- current architecture
- target architecture
- migration boundaries
- compatibility strategy
- protocol strategy
- package layout
- risks
- what is deliberately deferred

Prefer a staged migration.

For example:

```text
Stage A
Freeze current behavior with tests.

Stage B
Introduce Go module and shared protocol definitions.

Stage C
Implement Go daemon lifecycle and Unix transport.

Stage D
Implement PTY session runtime.

Stage E
Integrate libghostty from Go.

Stage F
Move CLI operations to Go daemon.

Stage G
Remove superseded Rust daemon code.

Stage H
Introduce QUIC transport.
```

Adjust this based on what you discover in the repository.

# Suggested Go package boundaries

Do not blindly implement this layout, but prefer small packages with clear ownership.

Something approximately like:

```text
cmd/
  agent/
  agentd/

internal/
  daemon/
  session/
  task/
  agent/
  pty/
  protocol/
  transport/
    unix/
    quic/
  terminal/
  persistence/
  attention/
```

Avoid excessive package fragmentation.

Do not create interfaces merely for hypothetical extensibility.

Interfaces should exist where there are real boundaries such as transports or test seams.

# Protocol direction

Preserve the useful parts of the existing framed binary protocol.

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

# CLI compatibility

Preserve the existing user experience where practical.

Important commands include concepts such as:

```text
agent run
agent ls
agent attach
agent attachments
agent detach
agent history
agent diff
agent send
agent kill
agent daemon
```

Do not rename commands merely because implementation internals changed.

# Non-goals for this work

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

Do not turn this into a complete rewrite of the product.

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

# First task

Start by performing the repository audit.

Then propose the smallest coherent first implementation milestone that moves the project toward the target architecture without destroying existing functionality.

Unless the repository structure reveals a strong reason otherwise, prefer beginning with:

1. behavioral/integration tests around the existing daemon;
2. introduction of the Go module;
3. a minimal Go `agentd` process;
4. local Unix socket transport;
5. a small versioned protocol implementation;
6. one PTY-backed durable session;
7. attach → detach → reattach;
8. tests proving the child process survives client disconnection.

Do not begin with QUIC.

The point of the first milestone is to establish the correct ownership and session model.

QUIC becomes valuable once that model is sound.

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
