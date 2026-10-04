# agentd Roadmap

`agentd` is a daemon runtime for supervising coding agents as durable tasks.

The long-term goal is to make agent sessions behave more like infrastructure than terminal tabs:

* processes continue running when clients disconnect
* sessions can be reattached from another terminal or machine
* terminal state can be reconstructed without replaying an entire output stream
* agents can run locally or remotely using the same application protocol
* multiple clients can observe or interact with a session
* task state, artifacts, history, and attention survive beyond any individual terminal
* remote sessions use a transport designed for long-lived, multiplexed connections
* the daemon remains useful without requiring a hosted service

At its core:

```text
agent CLI
    │
    │ local or remote protocol
    ▼
┌──────────────────────┐
│        agentd        │
│                      │
│ session lifecycle    │
│ PTY ownership        │
│ process supervision  │
│ terminal state       │
│ persistence          │
│ client fan-out       │
└──────────┬───────────┘
           │
           ▼
      coding agents
```

`agentd` runs on the machine where the work runs.

A future hosted service may provide discovery, identity, authorization, audit, and connectivity, but it must not be required for the core runtime.

---

## Design principles

### The daemon owns the work

The machine running the agent also runs `agentd`.

If an agent runs on a remote Linux host, that host owns:

* the PTY
* the child process
* terminal state
* task metadata
* attachments
* session persistence

The client is disposable.

Disconnecting a laptop must not terminate work.

### Local and remote are the same system

Local:

```text
agent
  │
  │ Unix socket
  ▼
agentd
```

Remote:

```text
agent
  │
  │ QUIC
  ▼
agentd
```

The application protocol should not care which transport is underneath it.

### QUIC is the primary remote transport

Remote `agentd` connections should be designed around QUIC rather than treating remote sessions as HTTP requests or a single TCP byte stream.

QUIC gives the runtime useful primitives directly:

* encrypted transport
* multiplexed independent streams
* stream-level flow control
* connection-level flow control
* reduced head-of-line blocking between streams
* inexpensive creation of additional logical streams
* connection migration capabilities
* datagrams where appropriate
* one long-lived connection capable of carrying many sessions

The goal is not to expose QUIC details throughout the application.

QUIC should provide the transport substrate underneath the `agentd` session protocol.

### No mandatory control plane

The open-source runtime must work directly between machines.

Initial connectivity can rely on:

* LAN
* Tailscale
* WireGuard
* directly reachable hosts
* port forwarding

A hosted control plane can come later.

### The protocol is a first-class interface

`agentd` should not become tightly coupled to one CLI or TUI.

Clients should be able to:

* create tasks
* discover sessions
* attach and detach
* send input
* resize PTYs
* inspect terminal state
* inspect task metadata
* retrieve history
* observe events
* manage agent lifecycle

over a stable application protocol.

### Terminal state is not just a log

Raw PTY output is insufficient for reliable reconnect.

`agentd` should maintain actual terminal state so that attaching clients can receive a snapshot of the current terminal before consuming new output.

### Build for real workloads before generalized infrastructure

Optimize around actual Claude Code, Codex, shell, compiler, and test-runner workloads.

Avoid premature abstractions for arbitrary distributed workloads.

---

# Phase 0 — Stabilize the current model

Before changing implementation language or transport architecture, document the behavior that must survive the transition.

## Goals

* define the session lifecycle
* define task vs session semantics
* formalize the binary protocol
* identify current invariants
* build regression fixtures around PTY behavior

## Deliverables

* [ ] Document task lifecycle states
* [ ] Document session lifecycle states
* [ ] Document attachment lifecycle
* [ ] Version the binary protocol explicitly
* [ ] Create protocol fixture tests
* [ ] Record representative PTY streams from:

  * [ ] Claude Code
  * [ ] Codex
  * [ ] shell
  * [ ] Vim/Neovim
  * [ ] test runners
  * [ ] large compiler output
* [ ] Create attach/detach integration tests
* [ ] Create resize integration tests
* [ ] Test multiple simultaneous attachers
* [ ] Document daemon restart behavior
* [ ] Establish baseline benchmarks

Baseline metrics should include:

```text
daemon startup time
RSS at idle
RSS per session
goroutines/threads per session
file descriptors per session
attach latency
terminal snapshot size
terminal snapshot latency
PTY throughput
fan-out throughput
```

---

# Phase 1 — Go daemon

Move the core daemon architecture toward Go.

The purpose is not merely a language rewrite. The Go implementation should become the foundation for remote sessions, concurrency, networking, and process supervision.

## Target architecture

```text
                 agent
                   │
                   │ framed protocol
                   ▼
          ┌──────────────────┐
          │      agentd      │
          │        Go        │
          │                  │
          │ task manager     │
          │ session manager  │
          │ PTY runtime      │
          │ persistence      │
          │ fan-out          │
          └────────┬─────────┘
                   │
                   │ C ABI
                   ▼
          ┌──────────────────┐
          │  libghostty-vt   │
          │       Zig        │
          └──────────────────┘
```

## Deliverables

* [x] Introduce Go workspace/module
* [x] Implement daemon lifecycle in Go
* [x] Implement local Unix socket listener
* [x] Implement existing framed protocol in Go
* [x] Implement client library
* [x] Implement CLI transport using Go client (the `agent` CLI is Go, in `internal/cli`)
* [x] Spawn agent processes under PTYs
* [x] Support detach without process termination
* [x] Support interactive reattach
* [x] Support PTY resize
* [x] Support multiple attached clients
* [x] Sessions run in a per-session `cwd`; agentd does not manage git worktrees
* [x] Port SQLite-backed metadata
* [x] Preserve compatibility with existing session semantics where practical
* [x] Sessions survive daemon restart (each session's worker is its own process)

## Exit criteria

The following must work entirely through the Go daemon:

```sh
agent new --cwd ~/src/project feature-x

agent ls

agent attach <task>

# detach

agent attach <task>

agent kill <task>
```

No remote support is required yet.

---

# Phase 2 — Go + libghostty

Use `libghostty-vt` as the terminal-state engine behind agent sessions.

The daemon should maintain terminal state independently from attached clients.

## Runtime model

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
                   snapshot/replay
                         │
                         ▼
                    new client
```

## Deliverables

* [x] Integrate Go bindings for `libghostty-vt` (`go.mitchellh.com/libghostty`)
* [x] Feed every PTY output stream into terminal state
* [x] Generate terminal snapshots for newly attached clients
* [ ] Restore:

  * [ ] primary screen
  * [ ] alternate screen
  * [ ] cursor position
  * [ ] styles
  * [ ] scrollback
  * [ ] terminal dimensions
* [ ] Validate behavior against real TUIs
* [ ] Benchmark cgo boundary overhead
* [ ] Minimize high-frequency Go ↔ Zig calls
* [ ] Add fuzz/property tests around snapshot restoration

## Important constraint

Prefer coarse operations across the Go/Zig boundary.

Avoid:

```text
Go
  ↓
Zig per character
  ↓
Go
  ↓
Zig per character
```

Prefer:

```text
Go
  ↓
write buffer
  ↓
Zig processes buffer
```

## Exit criteria

A user can:

1. start an interactive application
2. disconnect
3. allow substantial output/state changes
4. reconnect
5. immediately see the correct terminal state

without replaying the entire PTY history.

---

# Phase 3 — Transport-independent application protocol

Separate session semantics from the underlying connection transport.

The protocol should operate over explicit message and stream abstractions rather than assuming everything is one ordered byte stream.

## Target

```text
                    agent protocol

                         ▲
                         │
              ┌──────────┴──────────┐
              │                     │
         Unix socket               QUIC
              │                     │
              ▼                     ▼
            local                 remote
            agentd                agentd
```

## Deliverables

* [x] Extract transport-neutral protocol package (`internal/protocol` over the stream/listener seam in `internal/transport`)
* [x] Resolve session working directories on the daemon (`~/` paths and named workspaces managed
      with `agent workspace`), so a remote client does not need the host's paths
* [x] Define connection handshake (`Hello`/`Welcome`, opening the control stream)
* [x] Define protocol capability negotiation (capability lists in `Hello`/`Welcome`)
* [x] Define protocol version negotiation (version ranges in `Hello`, the daemon picks)
* [x] Add request IDs (frame flag + `u32` id on control streams)
* [x] Define request/response messages (catalog in ARCHITECTURE.md)
* [ ] Define long-lived event streams
* [x] Define bidirectional attachment streams
* [x] Define structured error frames
* [x] Add connection-level metrics (per QUIC connection: streams, RTT, bytes, loss)
* [x] Add explicit payload limits
* [x] Fuzz protocol decoding (`internal/protocol/fuzz_test.go`)
* [x] Reject malformed and oversized messages safely

## Protocol concepts

The application protocol should support:

```text
commands
responses
events
attachments
terminal snapshots
terminal output
terminal input
resize events
task events
attention events
```

The protocol does not need to become a generic RPC framework.

---

# Phase 4 — QUIC remote transport

Add direct secure remote connectivity between an `agent` client and `agentd`.

QUIC is the primary remote transport.

## Example

```text
MacBook

agent --host devbox attach auth-refactor

          │
          │ QUIC
          ▼

Linux devbox

agentd
 ├── Claude Code
 ├── Codex
 └── shell
```

## Connection model

Prefer one long-lived QUIC connection per client ↔ daemon relationship.

Within that connection, use multiple streams for independent activities.

For example:

```text
QUIC connection
│
├── control stream
│     commands
│     responses
│     session discovery
│
├── event stream
│     task updates
│     attention events
│
├── attach stream: session A
│     terminal snapshot
│     PTY output
│     user input
│     resize
│
├── attach stream: session B
│     terminal snapshot
│     PTY output
│     user input
│
└── artifact stream
      diffs
      logs
      files
```

One slow session should not unnecessarily stall unrelated sessions.

## Deliverables

* [x] Add QUIC listener to `agentd`
* [x] Add QUIC client transport (`agent --host`)
* [x] One-command setup (`agentd remote enable`, detecting a Tailscale address)
* [x] Establish TLS identity model (self-signed Ed25519 keys, pinned by fingerprint)
* [x] Authenticate machines/clients (mutual TLS: authorized client keys, pinned daemon key)
* [x] Define stream roles (one bidirectional stream per request or attachment)
* [x] Define stream negotiation (a stream's first frame sets its role)
* [x] Support remote:

  * [x] `ls`
  * [x] `new` (`agent --host H new --cwd DIR`)
  * [x] `attach`
  * [x] `send`
  * [x] `history`
  * [x] `diff` (`agent diff HOST/SESSION`, and `agent artifact` for the other artifacts)
  * [x] `kill`
* [ ] Preserve sessions across network loss
* [ ] Reconnect cleanly
* [ ] Restore terminal state after reconnect
* [ ] Handle slow consumers safely
* [ ] Bound buffering
* [ ] Implement stream-level backpressure
* [x] Implement connection-level resource limits (per-connection stream limit, idle timeout)
* [x] Add heartbeat/liveness semantics where necessary (QUIC keep-alive)

## Initial deployment model

Do not build NAT traversal yet.

Use existing networking:

* Tailscale
* WireGuard
* LAN/private network
* public host connectivity

---

# Phase 5 — QUIC-native session architecture

Once basic remote connectivity works, take advantage of QUIC rather than merely using it as “TCP with different plumbing.”

## Goals

Design stream ownership around `agentd` concepts.

Avoid funneling every operation through a single connection mutex or application-level multiplexer.

## Potential stream model

### Control stream

Long-lived ordered stream for low-volume control messages:

```text
list sessions
start task
kill task
host metadata
capability negotiation
```

### Attachment streams

Each interactive attachment receives its own bidirectional stream.

```text
client input ───────► PTY

PTY output ─────────► client

resize ─────────────► PTY
```

### Event stream

A long-lived server-to-client stream:

```text
task started
task completed
attention required
session exited
host state changed
```

### Artifact streams

Large transfers should not block terminal interaction:

```text
diffs
logs
patches
screenshots
large artifacts
```

Use independent streams.

## Deliverables

* [ ] Define stream taxonomy
* [ ] Make attachment streams independent
* [x] Separate large transfers from latency-sensitive terminal traffic (artifacts stream on their
      own stream as bounded `ArtifactChunk` frames, held back by flow control)
* [x] Measure flow-control behavior
* [x] Test slow attachment while another remains interactive
* [x] Test simultaneous large artifact transfer and interactive PTY (end to end through the daemon
      too: `TestInteractiveDuringLargeArtifactOverQUIC`, a 48 MiB diff, stalled and at full speed,
      over one QUIC connection with a live attachment)
* [x] Benchmark stream creation cost
* [x] Benchmark many concurrent streams
* [x] Investigate priority requirements if real workloads demonstrate a need

---

# Phase 6 — Reconnection and session resumption

QUIC connections are not themselves the durable session.

The session belongs to `agentd`.

A client must be able to lose its connection completely and create another one later.

## Model

```text
client
   │
   │ QUIC connection #1
   ▼
agentd
   │
   └── session continues

      network disappears

client
   │
   │ QUIC connection #2
   ▼
agentd
   │
   └── same session
```

The application must not depend on transport connection identity as session identity.

## Deliverables

* [ ] Stable session IDs
* [ ] Stable attachment semantics
* [x] Reattachment after complete connection loss
* [x] Snapshot on reattach
* [ ] Sequence/output position tracking if required
* [ ] Duplicate/replayed request handling where necessary
* [ ] Idempotency for lifecycle commands where useful
* [ ] Test network switching
* [ ] Test extended offline periods
* [ ] Test repeated reconnect loops

QUIC connection migration may improve transient network changes, but `agentd` must remain correct even when migration is unavailable and a completely new connection is required.

---

# Phase 7 — QUIC datagrams experimentation

QUIC datagrams may be useful for information that is:

* time-sensitive
* replaceable
* not worth retransmitting

They should not be used merely because they exist.

Potential candidates:

```text
presence
latency probes
ephemeral telemetry
superseded resize hints
```

Do not use unreliable datagrams for terminal output unless there is a clear protocol design proving that lost data cannot corrupt terminal state.

PTY output is generally ordered state transition data and should remain reliable.

## Deliverables

* [x] Identify genuinely replaceable messages
* [x] Benchmark datagrams vs streams
* [x] Validate behavior under packet loss
* [x] Keep reliable streams as the default

---

# Phase 8 — Durable daemon lifecycle

A durable session should ideally survive more than a client disconnect.

Eventually it should survive daemon maintenance as well.

## Goal

Upgrade or restart `agentd` without unnecessarily destroying active agent work.

## Areas to investigate

* inherited file descriptors
* Unix descriptor passing
* `SCM_RIGHTS`
* supervisor/worker models
* socket activation
* PTY descriptor transfer
* process adoption
* handoff of terminal state
* QUIC listener handoff where feasible
* daemon upgrade coordination

## Potential model

```text
agentd v1
   │
   ├── PTY fd
   ├── child process
   └── terminal state
          │
          │ handoff
          ▼
agentd v2
   │
   ├── same PTY
   ├── same child
   └── restored terminal state
```

## Deliverables

* [ ] Document restart guarantees
* [ ] Preserve task metadata across restart
* [ ] Investigate live PTY handoff
* [ ] Prototype FD transfer
* [ ] Prototype daemon upgrade
* [ ] Ensure clients can reconnect after upgrade
* [ ] Add crash-recovery tests

This phase should be driven by feasibility rather than becoming a blocker for earlier releases.

---

# Phase 9 — Scale and efficiency

Treat resource usage as part of the product.

Agent workflows may eventually involve dozens or hundreds of dormant sessions.

## Primary benchmark

```sh
agentd bench sessions --count N
```

## Measure

* [ ] RSS per idle session
* [ ] Go heap per session
* [ ] libghostty memory per session
* [ ] goroutines per session
* [ ] file descriptors per session
* [ ] idle CPU
* [ ] snapshot size
* [ ] snapshot latency
* [ ] restore latency
* [ ] fan-out cost
* [x] QUIC memory per connection
* [x] QUIC memory per stream
* [x] QUIC CPU overhead
* [ ] database overhead

## Target workloads

```text
100 sessions
1,000 sessions
10,000 terminal-state objects

100 concurrent output-heavy sessions

100 simultaneous attachments

1 QUIC connection
1,000 concurrent streams

large artifact transfer
+
interactive PTY traffic
```

Large counts do not necessarily represent expected deployments. They help expose fixed costs and architectural bottlenecks.

## Optimization principles

Optimize only after measurement.

Useful tools include:

```text
pprof
runtime metrics
execution tracing
allocation profiling
race detector
benchstat
system-level profiling
packet captures
QUIC transport metrics
```

Prefer simple optimizations that reduce permanent per-session cost.

---

# Phase 10 — Agent-native runtime features

Once durable remote sessions are solid, build agent-specific behavior above them.

This is where `agentd` should differentiate itself from a terminal multiplexer.

## Attention model

Sessions emit attention states rather than requiring users to continuously watch terminal output.

```text
info
notice
action
```

Potential signals:

```text
agent completed
agent failed
agent is waiting for input
tests failed
merge conflict
permission required
large diff created
agent appears stalled
```

## Deliverables

* [ ] Formalize attention events
* [ ] Add event persistence
* [ ] Surface attention in CLI
* [ ] Surface attention in TUI
* [ ] Track task elapsed time
* [ ] Track agent/process status
* [x] Expose Git state (`GetGitState`, `agent status`; the base is recorded at session creation)
* [x] Expose produced commits (commits since the base; the `patch` artifact)
* [x] Expose changed files (staged, unstaged and untracked since the base; `agent diff --stat`)
* [x] Expose artifacts (`agent artifacts`, `agent artifact`: diff, patch, history)
* [ ] Support task-level notifications

## Principle

The user should not need to watch every agent.

The runtime should help answer:

> Which task needs me now?

---

# Phase 11 — Multiple machines

Once remote `agentd` is stable, make multiple hosts feel like one workspace without centralizing session execution.

## Example

```text
agent hosts

NAME        STATUS     SESSIONS
local       online     2
desktop     online     5
devbox-1    online     3
gpu-01      offline    0
```

Potential commands:

```sh
agent --host devbox-1 new --cwd /srv/repo auth-tests

agent sessions --all

agent attach devbox-1/auth-tests
```

## Deliverables

* [x] Host configuration (`agent host add|ls|rm`, `hosts.toml`)
* [x] Host aliases (`--host NAME`, `NAME/session`)
* [x] Host health (`agent hosts` / `agent host ls`: online, offline, unauthorized, latency, load)
* [ ] Cross-host session discovery
* [ ] Stable global session identifiers
* [ ] Host-aware CLI UX
* [x] Machine capabilities (`Welcome` host description; `agent host info`)
* [x] Simple placement hints (`agent --host auto new`: least running sessions per CPU among hosts with the agent and workspace)

Avoid implementing a scheduler until real usage requires one.

---

# Phase 12 — Optional control plane

Only introduce a centralized service once multiple-machine usage demonstrates a clear need.

The control plane must not become the owner of execution.

## Responsibility split

### Data plane

`agentd` on each machine owns:

```text
PTYs
processes
terminal state
agent execution
artifacts
session runtime
QUIC endpoint
```

### Control plane

An optional hosted service may own:

```text
identity
organizations
machine enrollment
machine discovery
authorization
session directory
audit metadata
connection rendezvous
relay
policy
```

## Architecture

```text
                  optional control plane

              identity / discovery / policy
                         │
          ┌──────────────┼──────────────┐
          │              │              │
          ▼              ▼              ▼

       laptop          devbox          prod

       agentd          agentd          agentd
         │               │               │
       agents          agents          agents
```

Where possible, the client should establish a direct QUIC connection to the machine running `agentd`.

```text
client ═════════════ QUIC ═════════════► agentd
```

rather than proxying session data through the control plane:

```text
client ──► cloud service ──► agentd
```

A relay may eventually exist as a fallback when direct connectivity is impossible.

---

# Phase 13 — Connectivity and rendezvous

Once direct QUIC connections are proven useful, investigate making them easier across NAT and dynamic networks.

This should come after the runtime itself is strong.

Potential architecture:

```text
                     control plane
                    rendezvous only
                    /             \
                   /               \
                  ▼                 ▼
              client             agentd
                  \                 /
                   \               /
                    ══ direct QUIC ══
```

Fallback:

```text
client ══ QUIC ══► relay ══ QUIC ══► agentd
```

Potential areas:

* host discovery
* ephemeral connection credentials
* NAT traversal
* ICE-like candidate negotiation
* UDP hole punching
* relay fallback
* network migration
* short-lived certificates
* device authorization

Do not build these before direct networking demonstrates the need.

---

# Optional compatibility transports

QUIC is the primary remote transport.

Other transports may eventually exist for compatibility rather than defining the architecture.

Possible examples:

```text
Unix socket     local
QUIC            preferred remote
WebTransport    browser-facing QUIC semantics
WebSocket       legacy/proxy fallback
stdio/SSH       bootstrap or debugging
```

These should adapt to the `agentd` application protocol rather than cause that protocol to be redesigned around their limitations.

---

# Long-term architecture

```text
                            clients

                   CLI      TUI      Web
                    │        │        │
                    └────────┼────────┘
                             │
                       agent protocol
                             │
                    QUIC / local socket
                             │
              ┌──────────────┼──────────────┐
              │              │              │
              ▼              ▼              ▼

          MacBook         devbox-01      server-03
          agentd           agentd          agentd
             │               │               │
          sessions        sessions        sessions
             │               │               │
           agents          agents          agents
```

With an optional service layered above:

```text
                   agentd control plane

                identity / discovery
                access / audit / relay

              /           |           \
             /            |            \
         MacBook        devbox-01      server-03
          agentd          agentd          agentd
```

The hosted service enhances the runtime.

It does not define it.

---

# Non-goals

For the foreseeable future, `agentd` is not trying to become:

* a general container orchestrator
* Kubernetes for agents
* a generic RPC framework
* a terminal emulator GUI
* a full terminal multiplexer
* a replacement for Git
* a cloud scheduler
* a mandatory hosted platform
* a distributed database
* an HTTP-first application platform

Some of those areas may eventually overlap with the project, but they should not drive the core design prematurely.

---

# Engineering areas to explore

Development of `agentd` should deliberately build expertise in the systems underneath durable agent runtimes.

## Go systems engineering

* goroutine lifecycle
* cancellation
* backpressure
* synchronization
* memory ownership
* profiling
* networking
* process supervision

## PTYs

* Unix PTY lifecycle
* process groups
* signals
* resize behavior
* descriptor ownership
* process adoption

## Terminal state

* VT parsing
* alternate screens
* scrollback
* snapshots
* terminal modes
* synchronized output

## QUIC

* connection lifecycle
* streams
* flow control
* congestion control
* TLS identity
* connection migration
* datagrams
* stream limits
* resource exhaustion
* backpressure
* network failure behavior

## Protocol design

* framing
* stream roles
* capability negotiation
* versioning
* idempotency
* reconnect semantics
* bounded messages
* state resynchronization

## Go / Zig interoperability

* cgo
* C ABI design
* pointer ownership
* allocation boundaries
* cross-compilation
* static linking

## Durability

* SQLite
* session recovery
* process state
* daemon upgrades
* file descriptor handoff

---

# Near-term focus

The immediate objective is intentionally narrow:

> Run an agent on another machine over QUIC, disconnect completely, reconnect later, and immediately recover the correct live session.

The path to that milestone is:

```text
Go daemon
   ↓
PTY ownership
   ↓
libghostty terminal state
   ↓
transport-neutral application protocol
   ↓
QUIC transport
   ↓
remote agentd
   ↓
disconnect
   ↓
agent continues running
   ↓
new QUIC connection
   ↓
reattach
   ↓
terminal snapshot
   ↓
continue working
```

Until that flow is excellent, avoid adding unnecessary infrastructure around it.

---

# Definition of success

`agentd` should eventually make this ordinary:

```sh
agent --host devbox new --cwd /srv/repo auth-refactor
```

Ask the agent to refactor the authentication middleware and run the test suite. Close the laptop.

Come back later from another machine:

```sh
agent sessions --all
```

See:

```text
HOST       NAME             AGENT    STATUS      ATTENTION
devbox     auth-refactor    claude   running     action
desktop    docs             codex    completed   notice
```

Then:

```sh
agent attach devbox/auth-refactor
```

and immediately continue interacting with the exact session that was already running.

Underneath that experience:

```text
client
   │
   ║ QUIC
   │
   ▼
agentd
   │
   ├── durable task
   ├── PTY
   ├── terminal state
   └── coding agent
```

No terminal babysitting.

No dependency on the original client.

No mandatory cloud service.

No HTTP request lifecycle pretending to be a persistent session protocol.

Just durable agent work.
