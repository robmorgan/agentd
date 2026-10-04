# Architecture

`agentd` is a local daemon that owns coding-agent sessions: their PTYs, processes, terminal state,
and metadata. `agent` is a thin client that sends requests over the resolved runtime root's
`agentd.sock` and prints or streams the responses.

```text
agent (Go CLI) ──unix socket──► agentd serve (Go) ──unix socket──► agentd session-worker (Go) ──PTY──► agent process
                                session registry,                   one per session: PTY owner,
                                request/attach proxy                shadow terminal, client fan-out
```

* The daemon (`internal/daemon`) owns the session registry in SQLite, spawns one worker per
  session, and proxies client requests and attach streams to it.
* Each session worker (`internal/worker`) owns one PTY and child process, feeds the output
  through `libghostty-vt`, and fans it out to attached clients over its own socket in
  `<runtime-root>/sessions/<id>.sock`.
* Control traffic and attach streams use a framed binary protocol.

## Code Layout

Both binaries are one Go module at the repository root.

| Package | What it does |
|---|---|
| `cmd/agentd` | `serve [--daemonize]`, `upgrade`, `remote enable\|disable\|status` (set `[remote] listen` and restart the daemon), `remote id\|list\|authorize\|revoke`, `bench sessions` (`internal/bench`: measures sessions on a private daemon), `session-worker`. The agent CLI runs `serve --daemonize`. |
| `cmd/agent`, `internal/cli` | The `agent` CLI: commands, the session picker, attach and the Ctrl-Y overlay (plain ANSI), and `hosts.toml` for remote hosts. Pure Go: it never imports `worker`, `daemon` or `db`, and `cmd/agent`'s tests check that, then drive both real binaries through PTYs. |
| `internal/daemon` | `agentd serve`: lock/socket/pid file lifecycle, create/kill/rm/ls/get, attach and request proxies to workers, history, daemon management, worker supervision and startup reconciliation. Tests run the daemon in-process against real worker processes. |
| `internal/worker` | One session: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket, and activity and attention detection from the PTY stream. Real-PTY tests run under `-race`. |
| `internal/repo` | Read-only views of the git repository a session works in, for the daemon: git state, the diff and patch artifacts. Runs the system `git`; tests use real temporary repositories. |
| `internal/transport` | The seam between the protocol and the network: `Stream` (one request or attach session), `Listener`, the shared accept loop, the Unix socket transport, and the QUIC transport with pinned-key identities (`quic-go`). `transporttest` has a UDP relay that tests use as the network. |
| `internal/protocol` | The framed binary protocol and the small daemon management protocol, used by both binaries. Golden-frame tests pin the bytes. |
| `internal/session`, `internal/paths`, `internal/config` | The session model (and name rules), runtime-root resolution, and `config.toml`, shared by both binaries. |
| `internal/db` | `state.db`: schema, the guarded session state transitions the daemon and workers use, and session events. Uses `modernc.org/sqlite` (pure Go). Only the daemon and its workers open it. |
| `internal/procstat` | A process's resource usage from the OS (libproc on macOS, `/proc` on Linux) and the Go runtime's own numbers, for session stats and the bench. |
| `scripts/` | The libghostty-vt build helper. |

A few details of the daemon and workers that the sections below do not cover:

* `kill` asks the worker over its socket to stop; the worker stops the agent's process group
  (SIGKILL after 5s), writes the history logs, records the session as exited and sends
  `SessionEnded` to attached clients.
* PTY input goes through a bounded per-session queue and writer goroutine, so an agent that stops
  reading input never stalls output.
* Attach is a byte pipe through the daemon, so the daemon buffers no PTY output beyond one copy
  buffer per attachment. Slow-consumer policy lives in the worker (`internal/worker/broadcast.go`;
  see "Slow Clients, Buffering And Backpressure").
* Worker stderr goes to `logs/<id>.worker.log`; a daemonized `serve` logs to `agentd.log` in the
  root (not `logs/`, where it could collide with a session named `agentd`).

## Sessions Belong To The Daemon, Not To Connections

A client connection is disposable. Disconnecting (or never detaching cleanly) only ends that
attachment; the agent keeps running and any later connection can reattach.

The daemon is disposable too. Each worker runs in its own process session, so stopping, crashing,
or restarting the daemon does not touch running sessions. A new daemon finds them again through
`state.db` and the per-session sockets. `agent daemon restart` is therefore always safe.

A session counts as live only if its worker accepts a connection on its socket. Pids recorded in
`state.db` are never trusted on their own: after a crash or reboot they may belong to unrelated
processes. A single daemon per root is enforced by an exclusive lock on `agentd.lock`, held for
the daemon's lifetime and released by the kernel however it exits; `agentd.pid` is informational.

The daemon supervises the workers it spawned: it reaps them and records a worker crash as a failed
session. Sessions whose worker disappeared while no daemon was running are marked
`unknown_recovered` at startup or on the next `ls`.

## Lifecycles

### Tasks and sessions

Today a task is a session: one run of one agent in one working directory, under a name, with one
record in `state.db` and one worker process. The roadmap's task concepts (attention, elapsed time,
exit status, git state) are columns of that record or read from its working directory, so a task's
lifecycle is its session's, and there is no separate task table. If a task later spans several runs
(a retry, a follow-up agent), each run will still be a session and the task the grouping above
them; the session stays the unit that owns a PTY, a process and terminal state. Attachments, a
client's view of a session, are described in "Sessions, Incarnations And Attachments".

### Session states

A session's `status` in `state.db` changes only through the guarded updates in `internal/db`. Both
the daemon and the session's worker write it: the worker the outcome it knows first-hand, the
daemon what it can only infer.

```text
                 agent new (the daemon inserts the record)
                      │
                      ▼
                 ┌──────────┐   cwd missing, spawn failed, worker not up in 5 s,
                 │ creating │── worker exited first, or the daemon restarted ───► failed
                 └────┬─────┘
                      │ worker: agent spawned under the PTY, socket bound
                      ▼
                 ┌──────────┐   agent exits 0, or after kill ──────────────────► exited
                 │ running  │── agent exits non-zero or by a signal ──────────► failed
                 └──────────┘── worker crashes while its daemon supervises it ─► failed
                      │
                      │ worker gone, found by probing its socket
                      ▼ (it died while no daemon supervised it)
              unknown_recovered

  agent rm (or kill --rm): stops the session if it runs, then deletes the record and logs
```

* `creating`: the daemon has allocated the name and UID and is starting `agentd session-worker`.
  The worker passes the row's creation time to `MarkRunning`, which applies only to that
  incarnation and only while it is still `creating`, so a worker the daemon gave up on, or whose
  session was removed and recreated meanwhile, cannot claim it.
* `running`: the worker spawned the agent and bound `sessions/<id>.sock`. Only a worker that
  accepts a connection on that socket counts as live; recorded pids are informational.
* `exited`: the agent exited with status 0, or exited after a kill (the worker records a deliberate
  stop as `exited` whatever the status). The daemon also records `exited` if a worker it asked to
  stop disappears without recording an outcome.
* `failed`: the worker recorded why it could not start (missing directory, spawn or socket
  failure) or that the agent exited non-zero or by a signal; or the daemon recorded a failed start,
  a creation interrupted by a daemon restart, or a worker it spawned that died without recording an
  outcome (`MarkWorkerLost`, keyed on that worker's pid).
* `unknown_recovered`: the record says `running` but nothing listens on the socket, so the worker
  died while no daemon supervised it (SIGKILL, a reboot). This is found at daemon startup or on the
  next lookup (`ls`, `status` and the like), and is set only over `running`, so it never overwrites
  an outcome a worker wrote on its way out.

`exited`, `failed` and `unknown_recovered` are final: a session never restarts. Its history stays
readable (`agent history` reads the logs the worker wrote at exit) until `agent rm` deletes the
record and logs; the name can then be used again, by a new incarnation with a new UID.

### Daemon restarts

The daemon holds no session state that is not in `state.db` or in a worker, so restarting it is
safe:

* Workers run in their own process sessions and keep running, with their agents, PTYs and
  terminal state, while no daemon runs. A worker records its agent's exit in `state.db` itself.
* Stopping the daemon closes its client connections, which ends every attachment it was proxying
  (remote CLIs reconnect and reattach by themselves; local ones exit). Sessions are untouched.
* A starting daemon takes the lock, removes a stale socket, and reconciles: `running` sessions
  whose workers answer stay running and are reachable again; those whose workers are gone become
  `unknown_recovered`; sessions still `creating` become `failed` (a worker still starting is then
  refused by `MarkRunning`, stops its agent and exits).
* A daemon supervises only the workers it spawned. A worker that crashes under a later daemon is
  found by probing its socket, so its session becomes `unknown_recovered` rather than `failed`.
* `agent daemon restart` stops the daemon even with sessions running. A plain shutdown request is
  refused while sessions run, and `agent daemon upgrade` refuses too, since running workers are the
  old binary.

## Transports

The protocol runs over any bidirectional byte stream that supports half-close. Each stream has one
role (a request, the control stream, an attachment, a transfer, an event subscription; see
"Streams and their roles" below). Everything above `internal/transport` sees
only that `Stream` and a `Listener` that yields streams, so the daemon does not know or care which
transport a client used. The daemon-to-worker link is always a local Unix socket.

Two transports exist:

* **Unix socket** (`agentd.sock`): local clients, always on.
* **QUIC** (off unless `[remote] listen` is set): remote clients. Each client holds one QUIC
  connection and opens one bidirectional QUIC stream per role instance: a control stream for its
  one-shot requests, one per attachment, one per transfer. Long-lived attachments, transfers and
  short requests are multiplexed without blocking each other. Both peers ping
  every 5 seconds to hold idle connections open, and each connection may have at most 256 streams. The daemon's first
  datagrams are 1200 bytes, QUIC's minimum, rather than quic-go's default 1280: Tailscale's
  interface MTU is 1280 including IP and UDP headers, so larger ones never leave it and the
  handshake times out. Path MTU discovery raises the size afterwards where the path allows.

QUIC flow-control windows are set explicitly in `quicConfig`: 512 KiB growing to 4 MiB per
stream, 4 MiB growing to 16 MiB per connection. A stream whose reader stops (an attachment whose
terminal is paused) blocks only its own writer once its window is full; its data is kept, not
dropped, and the connection's other streams keep flowing until about 8 streams have stalled at
once. The connection window is therefore also the most one client can make the daemon buffer,
however many of its 256 streams it opens. A client whose address changes mid-connection (a NAT
rebinding, Wi-Fi to cellular) keeps its connection: the daemon validates the new path and moves to
it. Streams have no priorities (quic-go has none) and QUIC datagrams are disabled: measurements
showed no need for the first, and no protocol message is replaceable enough for the second.
[BENCHMARKS.md](BENCHMARKS.md) has the numbers and the reasoning.

QUIC connections authenticate both ways with pinned keys inside TLS 1.3 (ALPN `agentd`), the way
SSH uses host keys and `authorized_keys`, with no certificate authority:

* The daemon's key is `remote/daemon.key` (Ed25519, created on first use). Its fingerprint is
  `SHA256:` plus the base64 SHA-256 of the public key, and is what clients pin.
* `remote/authorized_clients` lists the client key fingerprints allowed in. It is checked on every
  handshake, on every new stream, and every 5 seconds for each open connection, so revoking a key
  also closes that client's live connections, attachments included. TLS session tickets are
  disabled: a resumed session would skip the check. `agentd remote authorize|revoke` edit the file
  under a lock and replace it atomically, so concurrent edits are never lost.
* An authorized client has the same access as the local socket owner, except that only a local
  client may stop the daemon: a remote one could not start it again.
* Keys are created without ever replacing one that exists (written to a temporary file, then
  linked into place), so processes creating a key at the same moment all end up using the same one.

The `agent` CLI is the client (`internal/cli/client.go`, over the same `internal/transport`
QUIC code as the daemon). Its key is
`remote/client.key`, in the same format as the daemon's, and `hosts.toml` maps host names to an
address and the daemon fingerprint pinned by `agent host add`. One CLI process opens at most one
QUIC connection, with one control stream for its requests and one stream per attachment or
transfer, so an attachment and the overlay's requests share a connection. A remote host is never started, restarted or upgraded from the CLI.
With TLS 1.3 a client can finish its half of the handshake
before the daemon rejects its key, so a refusal may only surface on the first stream; the CLI
recognises the TLS alert there and says how to authorize the machine. A name that resolves to
several addresses (`localhost` is often `::1` and `127.0.0.1`) is dialled at each in turn within a
10-second budget. A test pins one key's fingerprint, since `hosts.toml` and `authorized_clients`
store fingerprints and they must never change.

`agentd remote enable` picks the listen address when none is given: first a Tailscale address,
used without asking; otherwise the source address of the default route, found by connecting a UDP
socket (which sends nothing), which it asks about first, since that is a LAN address that may
change, a shared carrier-grade NAT address, or a public one. An address in Tailscale's ranges
(`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) only counts as Tailscale's when Tailscale confirms it: it is
on the `tailscale0` interface, or `tailscale ip` reports it (macOS names the interface `utunN`).
Carriers and other VPNs hand out `100.64.0.0/10` addresses too.

`enable` edits only the `listen` line of `config.toml`, keeping comments and following a symlinked
file. The edit is checked before it is written: it must parse and leave every other setting as it
was, or the file is left alone. If the restarted daemon does not listen on the new address, the
previous setting is restored. A daemon whose listen address cannot be bound, or whose listener
stops, keeps local service and binds again every 5 seconds until shutdown, reporting the reason in
its management status.

A client that disappears without closing (a laptop lid, a lost network) is noticed once the daemon
has heard nothing from it for 15 seconds (`transport.DeadPeerTimeout`); until then its attachment
is still listed. Closing the connection ends its attachments; the session is unaffected. QUIC uses
the smaller of the two peers' idle timeouts, so this holds for older clients too. Both ends notice a
silent peer this way, so an outage longer than that ends the attachment even if the network comes
back.

The CLI hides that: when an attach stream fails because its connection was lost
(`transport.IsConnectionLost`: an idle or handshake timeout, an unreachable network, or the daemon
closing the connection, say to restart), it stays in raw mode, dials a new connection and attaches
to the same session again, then repaints from the new snapshot. Session identity never depended on
the connection, so nothing on the daemon is resumed. The reattach names the session incarnation it
expects (its UID, so it can never land on a new session that reused the name) and the attachment it
replaces, which the worker drops at once rather than when the daemon notices the old connection is
dead (see "Sessions, Incarnations And Attachments"). Retries back off from 0.5 to 5 seconds and
continue until they succeed or fail for a reason a retry cannot fix (a refused or changed key, a
session that is gone or was replaced, a protocol error). Meanwhile the bottom row shows the status,
and typed input is dropped rather than delivered late; only the detach key acts. Output written
while disconnected is not replayed; the snapshot shows the screen as it is now. Local attachments do
not reconnect: the Unix socket only fails when the daemon itself goes away.

A path change that QUIC can follow needs no reconnect at all: when a client's address changes
mid-connection (NAT rebinding, or Wi-Fi to cellular behind the same NAT), quic-go validates the new
path and moves the connection to it, and every stream on it carries on. A reconnect is only needed
when the old path dies outright, and the attach above copes with that.

One-shot requests recover the same way. A remote request whose connection is lost after it was
sent is sent again on a new connection, with the same backoff, if that is safe: a read always, and a
request with side effects (`new`, `kill`, `rm`, `workspace add|rm`) only because it carries a
request token the daemon recognises (see "Duplicate And Replayed Requests"). Other requests
(`send-input`, `detach`) are never sent twice. A daemon that was never reached fails at once.

The daemon's tests run full sessions over QUIC, and over a TCP stand-in, to keep the seam honest.
The QUIC ones also run through a UDP relay standing in for the network
(`internal/transport/transporttest`), which can black-hole traffic, block new connections, or move
a client to a new address: they cover an address change mid-attachment, a hard network switch, an
outage longer than the dead-peer timeout, a client that stops reading while its session writes
about 67 MB (checking the daemon's heap and the worker's RSS stay flat), and fifty abrupt reconnects in a row, counting goroutines, file descriptors, streams and attachments
before and after.

## Wire Protocol

`agent` and `agentd` communicate over a custom framed binary protocol, implemented in
`internal/protocol` and used by both binaries. Golden-frame tests pin its bytes, so a CLI and a
daemon of different builds, possibly on different machines, stay compatible. The current version
is 1.

Each frame has a fixed 16-byte header followed by a payload:

* `magic` (`u32`, little-endian) identifies an `agentd` protocol frame
* `version` (`u16`, little-endian) is the protocol version the frame is written in
* `message_type` (`u16`, little-endian) identifies the request or response variant
* `flags` (`u16`): bit 0 marks a frame whose payload starts with a `u32` request id (only on
  control streams, below). A frame with a flag the reader does not know is refused, since flags
  change the payload's layout
* `reserved` (`u16`, currently unused and set to `0`)
* `payload_len` (`u32`, little-endian) gives the number of payload bytes that follow (at most
  64 MiB; a longer declared length is refused before anything is allocated)

Payloads are binary-encoded field-by-field rather than serialized as JSON:

* strings => `u32 len` + UTF-8 bytes (invalid UTF-8 is refused)
* byte blobs => `u32 len` + raw bytes
* booleans => single `u8`
* optional values => presence `u8` followed by the encoded value
* lists => `u32 count` followed by elements (the count never sizes an allocation by itself)

PTY snapshots, PTY output, and interactive input are sent as raw bytes. A client on an unsupported
version gets an `Error` frame in its own framing explaining the mismatch, so the `Error` message
kind and its payload never change. Fuzz tests (`internal/protocol/fuzz_test.go`) check that no
input makes a decoder panic or allocate without bound, and that whatever decodes re-encodes to the
same value.

A second, deliberately tiny daemon management protocol (framed at header version 0, JSON payloads)
carries daemon status and shutdown for `agent daemon info`, `restart` and `upgrade`, so those keep
working even when the CLI and daemon speak different versions of the main protocol. Its status
includes connection-level metrics: the streams the daemon is serving, and for each open QUIC
connection the client key, remote address, stream counts, QUIC's smoothed RTT, bytes sent and
received, and packets lost (`agent daemon info`, `agentd remote status`).

### Streams and their roles

The unit of the protocol is a stream (a Unix connection, or one QUIC stream on a client's QUIC
connection). The first frame a client sends on a stream decides its role for its whole life, so no
separate negotiation round trip is needed (`protocol.Request.Role`):

| Role | First frame | Then |
|---|---|---|
| request | any one-shot request | one response, then the stream ends |
| control | `Hello` | `Welcome`, then any number of tagged one-shot requests and their tagged responses, in any order |
| attach | `AttachSession` | `Attached` with a snapshot, then `PtyOutput` (and `AttachResync`, if negotiated) frames one way and `AttachInput`, `AttachResize`, `AttachSnapshot` the other, until either side ends it |
| events | `SubscribeEvents` | `Event` frames, server to client, in id order: the backlog the request asked for, then live ones, until the client closes the stream |
| artifact | `GetArtifact` | `ArtifactChunk` frames, then `EndOfStream` (or an `Error`: the artifact is incomplete). On a stream of its own, a large transfer never delays the control stream or an attachment |
| artifact | `GetHistory` | one `History` response holding the whole history (the `history` artifacts stream the same content) |
| management | a version-0 frame | one JSON response |

This is the stream taxonomy, and each kind of traffic has its place in it: small commands share the
control stream; each interactive attachment, being latency-sensitive and long-lived, has a stream of
its own; notice that something happened flows on events streams; and anything large (history,
diffs, patches, logs) goes on an artifact stream, so a big transfer never sits in front of a
keystroke or a small request. Over QUIC each stream has its own flow control, so
a stalled attachment or a large transfer never blocks another stream on the same connection, and a
client never needs more than one connection.

### Handshake, versions and capabilities

`Hello` is the connection handshake. The client sends the range of protocol versions it speaks
(framed at the lowest), its name, and the optional features ("capabilities") it supports. The
daemon answers `Welcome` with the newest version in both ranges, its own capabilities, and a
description of its machine (host name, OS, architecture, CPUs, memory, configured agents and the
default one), or with an `Error` when the ranges do not overlap. Both sides then use only the
features both listed. Today there is one protocol version (1), and these capabilities:

| Capability | Adds |
|---|---|
| `control-stream` | tagged requests on the stream that started with `Hello` |
| `git-state` | `GetGitState` |
| `artifacts` | `ListArtifacts`, `GetArtifact`, `ArtifactChunk` |
| `session-uid` | the session UID appended to session records and to `CreateSession`'s answer; on an attach stream, `AttachSession.ExpectUID` and `Attached.SessionUID` |
| `request-tokens` | a request token appended to `CreateSession`, `KillSession`, `AddWorkspace`, `RemoveWorkspace` |
| `attach-features` | `AttachSession` may end with the attach stream's own feature list (below) |
| `attach-replace` | attach stream: `AttachSession.Replaces`, the attachment this one replaces |
| `attach-resync` | attach stream: the worker may send `AttachResync` |
| `runtime-stats` | `GetSessionStats`, `GetDaemonStats` |
| `events` | events streams (`SubscribeEvents`), `ListEvents` |
| `session-activity` | the activity fields appended to session records (after the UID): activity, foreground command, title, last output and attention times |

The protocol grows without breaking older peers this way: a new message kind, or a field appended
to the end of an existing message or struct (a session record, even inside a list), comes with a
capability, and neither side sends it unless the other advertised that capability. The
capabilities both sides listed are the stream's `protocol.Features`, which tell the encoder which
optional fields to write and the decoder which to expect. Streams without a handshake use the base
encoding. Decoders reject unknown kinds, unknown flags and trailing bytes, so nothing is ever
silently misread. Changing an existing encoding needs a new protocol version.

Attach streams have no `Hello`, so `AttachSession` carries their handshake: when the daemon's
`Welcome` listed `attach-features`, the CLI appends the attach-stream capabilities it wants that the
daemon also listed (`session-uid`, `attach-replace`, `attach-resync`), followed by the fields of the
ones listed. The daemon cuts the list down to what the session's worker supports (it asks the worker
with a `Hello` on the worker socket; a worker started by an older daemon build may support fewer,
and one from before attach features supports none), and the worker's `Attached` echoes the list in
effect, followed by its fields. Each side uses only what that list names. An older CLI sends no list
and gets the base `Attached`; an older daemon never advertises the capability, so a newer CLI sends
none.

The CLI opens one control stream per process, right after connecting, and sends all its one-shot
requests on it (`internal/cli/control.go`). Each request carries a `u32` id that its response
echoes; the daemon runs up to 16 requests of one control stream at once and stops reading the
stream beyond that, so a client that pipelines requests is held back by flow control rather than
queued in the daemon. A request that cannot be decoded is answered with an error under its id and
the stream carries on. Requests that need their own stream (attach, artifacts, history) are refused on a
control stream. An artifact stream has no handshake of its own: the CLI opens one only after its
control stream's `Welcome` listed `artifacts`. If the control stream has ended when the CLI next uses it (the daemon restarted,
say), it opens a new one. A request it already wrote is resent only if that is safe: a read, or a
tokened request after a lost remote connection (see "Duplicate And Replayed Requests").

### Messages

| Request | Response | Role |
|---|---|---|
| `Hello` | `Welcome` | control |
| `GetDaemonInfo` | `DaemonInfo` | request |
| `ShutdownDaemon` | `Ok` | request |
| `CreateSession` | `CreateSession` | request |
| `KillSession` | `KillSession` | request |
| `GetSession` | `Session` | request |
| `ListSessions` | `Sessions` | request |
| `ListAttachments` | `Attachments` | request |
| `DetachSession`, `DetachAttachment` | `Ok` | request |
| `SendInput` | `InputAccepted` | request |
| `ListWorkspaces` | `Workspaces` | request |
| `AddWorkspace` | `Workspace` | request |
| `RemoveWorkspace` | `Ok` | request |
| `ListEvents` | `Events` | request |
| `GetHistory` | `History` | artifact |
| `GetGitState` | `GitState` | request |
| `ListArtifacts` | `Artifacts` | request |
| `GetArtifact` | `ArtifactChunk` frames, then `EndOfStream` (or `Error`) | artifact |
| `GetSessionStats` | `SessionStats` (`runtime-stats`) | request |
| `GetDaemonStats` | `DaemonStats` (`runtime-stats`) | request |
| `SubscribeEvents` | `Event`, repeated | events |
| `AttachSession` | `Attached` (or `SessionEnded` for a finished session), then `PtyOutput`, `AttachSnapshot`, `AttachResync`, and finally `SessionEnded`, `EndOfStream` or `Error` | attach |

Any request may instead be answered with `Error`. Kinds are numbered by area so that parallel work
does not collide: 1-19 and 101-118 are the base protocol, 30-39 and 130-139 git state and
artifacts, 150-159 session resumption (`AttachResync` is 150).

`attach` is the bidirectional case. After the initial `AttachSession` request and `Attached`
response (which carries a snapshot of the current screen), the worker streams `PtyOutput` frames
while the client sends `AttachInput`, `AttachResize`, and `AttachSnapshot` frames on the same
stream until either side closes. The daemon forwards the attach stream as raw bytes and never
buffers PTY output itself.

## Events and Attention

`agentd` answers "which agent needs me?" with events. An event (`session.Event`) is something that
happened to a session: an id, the session, a time, a kind, an attention level (`info`, `notice` or
`action`) and a one-line summary. The kinds and their default levels are defined in
`internal/session/event.go`:

| Kind | Level | Recorded by, when |
|---|---|---|
| `created` | info | the daemon, when it creates the session row |
| `started` | info | the worker, once the agent runs in its PTY |
| `exited` | notice | the worker, when the agent exits with status 0 |
| `killed` | info | the worker (or the daemon), when the agent stopped on request |
| `failed` | action | the worker or daemon, when the session fails to start or the agent exits non-zero |
| `worker_lost` | action | the daemon, when a worker died without recording an outcome |
| `recovered` | info | a starting daemon, for each session whose worker kept running |
| `bell` | action | the worker, when the program rings the bell |
| `notification` | action | the worker, for a desktop notification (OSC 9, OSC 777); the summary is its text |
| `idle` | notice (info while a client is attached) | the worker, when output stops for 10s after activity |
| `working` | info | the worker, when output resumes after an idle event |
| `stalled` | notice | the worker, after 30 minutes without output while a command other than the agent holds the PTY's foreground |
| `acknowledged` | info | the daemon, when the user looked at the session (below) |

Kinds travel as strings, so a client shows kinds newer than itself instead of failing to decode them.

**Persistence.** Events are rows of the `events` table in `state.db`, written by the daemon and
the workers. Ids come from `AUTOINCREMENT`, so they increase per runtime root and are never reused;
because every write is an immediate transaction, ids are committed in order, and a reader that has
seen id N never later finds a new event below N. An id is therefore a cursor. Each session keeps
its newest 500 events, pruned in the transaction that inserts a new one, and removing a session
removes its events, so the table is bounded by the sessions retained. A lifecycle change and its
event are written in one transaction.

**Attention.** A session record's `attention`, `attention_summary` and `attention_at` are the
highest-level unacknowledged event since the user last looked, and that event's summary. An event
raises the attention to its level unless something more urgent is pending (an equal level replaces
the summary; info events never change it). Lifecycle endings (exited, killed, failed, worker lost)
replace the attention instead, since an agent that has stopped is no longer waiting for whatever it
asked. Acknowledging resets the attention to info and records an `acknowledged` event, if there was
anything to clear. Attaching interactively (`attach`, or focusing a session in the picker)
acknowledges, and so does detaching from a session that is still running: either way the user has
seen its screen. An attachment that ends because the session ended does not, and output reaching
an attached client does not either, since an attached terminal may be a background tab. An ended
session's attention stays until it is removed.

**Activity.** The worker also keeps the session's `activity` (`working`; `idle` after 10 seconds
without output; `waiting` after a bell or notification, until someone types; `exited`), the name of
the process in the PTY's foreground (`tcgetpgrp` on the master, sampled every second: the agent
itself, or a command a shell runs there under job control), the terminal title, and when the program
last wrote output. It writes them on transitions only, never per output chunk, and never bumps
`updated_at`, so lists do not reshuffle as agents go idle and back. Elapsed time is derived from
`created_at` and `exited_at`.

**Detection.** Bells, desktop notifications (OSC 9 and OSC 777) and title changes come from
libghostty's parser, through its effect callbacks, out of the `VTWrite` the worker already makes
for each PTY read; the added cost per chunk is a clock read. The owner goroutine never touches the
database for this: events and activity go to a recorder goroutine through a queue of at most 64
events (dropped, and logged, while it is full) and a single activity slot (a newer snapshot
replaces an unwritten one). Noise is bounded: a bell is recorded unless another was seen in the
last 10 seconds with no input since, so a program ringing in a loop records one event; a repeated
notification text likewise; a bell within 2 seconds of a notification is the same request; and
idle events are at most one per 30 seconds. OSC 99 (kitty's notification protocol) is not parsed
by libghostty and is not detected.

**Events streams.** `SubscribeEvents{AfterID, SessionID, Tail}` opens an events stream: the daemon
sends every retained event after `AfterID` (or, without it, the newest `Tail` events), then new
ones as they are recorded, until the client closes the stream. Each subscriber is a cursor over the
table: its handler reads at most 256 events after its cursor, writes them, and repeats, waiting
once it has caught up. The daemon thus holds at most one batch per subscriber and never buffers for
a slow one: a subscriber that stops reading blocks only its own handler, in a write, while events
accumulate in the table (one that falls behind past the retention limit skips what was pruned).
Workers write the table directly, so while anyone is subscribed the daemon polls the newest id
(`sqlite_sequence`) every 250ms and wakes subscribers when it changes; events the daemon records
itself wake them at once. A poll costs about 130µs (`BenchmarkLastEventID`, mostly opening the
connection), under 0.1% of a core, and none is made without subscribers. `ListEvents{AfterID,
SessionID, Limit}` is the one-shot form on the control stream (at most 1000 per answer).

**Clients.** `agent events [SESSION] [--follow]` prints events or follows them; following keeps
the id of the last event it handled and, when the stream ends (a daemon restart, a lost
connection), subscribes again after it, so nothing is missed or repeated. `--notify` rings the bell
and asks the user's terminal for a desktop notification (OSC 9, or OSC 777 on VTE terminals, foot
and urxvt; passed through tmux) for action-level events, and `--exec CMD` runs a command per event
with the event in `AGENTD_EVENT_*` variables: these are the task-level notifications, and they work
the same against a remote host. `agent ls`, `agent status`, the picker and the Ctrl-Y overlay show
attention and activity, and order live sessions needing action first.

## Creating a Session

When you create a session (`agent new [--cwd DIR] [NAME]`), the daemon:

1. Validates the name and agent, and resolves `cwd` on its own machine: an absolute path, a
   `~/` path expanded against the daemon user's home, or a path relative to a named workspace
   (stored in `state.db`, managed with `agent workspace`). The directory must exist; it does not
   need to be a git repository.
2. If `cwd` is in a git repository with a commit, reads HEAD's commit and branch: the session's
   base (see "Git State And Artifacts"). Nothing else about git is touched.
3. Allocates a session id (the name) and a new UID, and stores the record, base included, in
   `<runtime-root>/state.db`.
4. Spawns `agentd session-worker` in a new process session.
5. Waits for the worker to report the session running and bind its socket.

The worker spawns the configured agent inside a PTY in `cwd`, with `AGENTD_SESSION_ID`,
`AGENTD_SESSION_NAME` and `AGENTD_CWD` injected. The daemon socket is deliberately not passed in.
That is not a boundary: agents run as the socket owner and can still find the socket at its
well-known path, so isolating them needs a sandbox around the agent process.

The daemon does not manage git worktrees or branches. That responsibility sits with whatever starts
the session (the user, a wrapper, a skill, or the agent itself); the README shows the worktree
recipe.

Working directories are resolved by the daemon, never the client, because the client may be on
another machine. The local CLI still canonicalizes a plain `--cwd` itself, since it shares the
daemon's filesystem; with `--workspace` it sends the name and the relative path unchanged.

Workspaces are daemon state, not configuration: they live in the `workspaces` table of
`state.db` and clients add, list and remove them over the protocol, so a remote client can manage
them without touching the host's files. A session stores the workspace name next to the cwd
resolved from it at creation. Workspaces are addressing only: agentd does not clone, create or
sync them.

For each session there are three kinds of state:

- durable metadata in SQLite for status, working directory, attention, activity, exit state, and
  the session's events
- terminal state and scrollback in the worker's `libghostty-vt` instance, written to
  `logs/<id>.log` (VT) and `logs/<id>.rendered.log` (plain) when the session ends
- the worker's live PTY and output fan-out used by `attach` and `send-input`

PTY input can come from an interactive `attach` or a background `send-input`. Multiple clients may
attach to the same session concurrently; input is shared and resize is last-writer-wins.

Killing a session asks the worker, over its socket, to stop (the worker handles SIGTERM the same
way). The worker stops the agent's whole process group (SIGKILL after five seconds), writes the
history logs, records the session as exited, and sends `SessionEnded` to attached clients. The
daemon signals a worker directly only if it is wedged, and only while its socket still answers.

## Git State And Artifacts

`agentd` does not manage git, but it reports what an agent produced in git, read from the
session's directory on the daemon's machine (`internal/repo`, `internal/daemon/artifacts.go`). It
only ever reads: the repository, its index and its refs are never written.

**Base.** When a session is created in a repository whose HEAD has a commit, the daemon records
that commit and the branch (`sessions.git_base`, `git_base_branch`, schema v3). What the session
produced is measured from there: its commits are those reachable from HEAD but not from the base,
and its changed files are everything that differs from the base: committed since, staged,
unstaged, and untracked. A session without a base (created before v3, outside a repository, or on
a branch with no commits yet) is measured from HEAD instead (or from the empty tree on an unborn
branch); one whose base has since vanished from the repository is reported as such and measured
from HEAD. The base is never moved: a rebase, a branch switch or a reset by the agent is reported
against where the session started.

**`GetGitState`** (a one-shot request) answers with the repository root, branch (or detached HEAD),
HEAD, base, upstream with ahead/behind counts, the commits since the base (hash, subject, author,
time) and the changed files (path, old path for renames, status, added and deleted lines, binary).
Lists are capped (200 commits, with the total count; 1000 files) and say when they were cut; git is
stopped as soon as enough has been read, so a huge repository costs a bounded read. Failures (git
not installed, the directory gone, a timeout after 30s) are reported inside the answer rather than
failing the request, so `agent status` always works. It works for ended sessions too, while their
directory exists.

**Artifacts.** `ListArtifacts` names what can be downloaded: `diff` (a unified diff of everything
since the base, untracked files included), `patch` (`git format-patch --stdout` of the commits since
the base: an mbox for `git am`), and `history` and `history.vt` (the terminal history, with its size
once saved). `GetArtifact` streams one on an artifact stream.

**How git runs.** The system `git`, in the session's directory, with `GIT_OPTIONAL_LOCKS=0`,
`core.fsmonitor=false`, no pager or colour, no external diff or textconv, and explicit flags for
everything a user's config could change (rename detection, `a/` and `b/` prefixes, no relative
paths). The daemon's own `GIT_*` variables are removed. Each git runs in its own process group,
killed as a group when its request ends or its client goes away.

`git diff` leaves out untracked files. To include them without touching the repository, the daemon
copies the index to a private temporary directory, marks the untracked files intent-to-add in the
copy (`GIT_INDEX_FILE`), and gives those commands a private object directory with the repository's
as an alternate, since intent-to-add stores the empty blob. Git then handles untracked files like
any other: binary detection and attributes apply, and a file moved without `git mv` is a rename. At
most 20,000 untracked files are included (`GitState` says when there were more).

**Streaming and backpressure.** An artifact is sent as `ArtifactChunk` frames of at most 256 KiB,
built in place around one reused buffer as git (or the history file) produces them, then
`EndOfStream`. The daemon never holds an artifact: a client that stops reading blocks the daemon's
write, which stops it reading git's stdout, which blocks git on its pipe. In memory per transfer:
one chunk, plus the pipe and the transport's buffers (over QUIC, the stream's flow-control window).
The transfer resumes when the client reads again and ends when it closes the stream, its connection
dies, or the daemon shuts down, killing git. There is no stall timeout, since a pager may pause as
long as its reader does. Live history is the exception: the worker answers with the whole history
in one frame (at most 64 MiB), which the daemon then sends in chunks.

Large transfers are separated from terminal traffic by giving each its own stream: over QUIC each
stream has its own flow control, so a transfer, stalled or at full speed, does not delay an
attachment on the same connection. `TestInteractiveDuringLargeArtifactOverQUIC` measures it on
loopback with a 48 MiB diff. Over six runs on a machine shared with other test suites, input
echoed through a live session over one QUIC connection took a p50 of 0.3-0.7 ms idle, 0.3-1.2 ms
with a stalled transfer open, and 0.35-0.9 ms while the transfer ran at 40-57 MiB/s (git diff
included), with a p99 of 1-12 ms and never more than 15 ms.

## Resource Usage

Each session's worker measures itself on request (`GetSessionStats`, which the daemon forwards):
its process from the OS (resident and private memory, CPU time, threads, open files) and its Go
runtime (goroutines, heap, memory held from the OS), the agent's top process, and its terminal and
fan-out (size, scrollback rows, PTY bytes and reads, chunks dropped for lagging clients). Asked
to, it also formats a reattach snapshot and replays it into a fresh terminal, timing both. The
daemon answers `GetDaemonStats` for its own process. `agent status --stats` and `agent daemon
stats` show them, and `agentd bench sessions` uses them to measure sessions at scale
(BENCHMARKS.md, "Sessions at scale").

Private memory is reported beside RSS because RSS counts the pages every worker shares (the
`agentd` executable and system libraries, about 11 MiB of each idle worker's 20 MiB), so summing
RSS over many sessions overstates their cost. libghostty's memory is outside the Go runtime and is
measured in-process (`BenchmarkTerminalMemory`): about 25 KB for a new terminal, and the
scrollback limit (10 MB) once full.

## Sessions, Incarnations And Attachments

A session's id is its name. Names are unique among the sessions that exist, but a name is free
again once its session is removed, so a name alone cannot tell a client whether the session it
knew is the one there now. Every session therefore also has a UID: 128 random bits, generated when
the session is created, stored in `state.db` and never changed or reused. One UID is one
incarnation of a name. `agent status` shows it. When several hosts are known to one client, a
global id is the host's identity (its key fingerprint) plus the UID; neither part depends on the
name.

An attachment is one client's live view of one incarnation, from `AttachSession` until it ends. Its
lifecycle, all in the worker (`internal/worker/worker.go`, `serveAttach`):

1. **Attach.** The worker numbers it `<kind>-<n>` (`attach-3`, `tui-1`), where `n` counts in the
   session's row in `state.db`: ids are never reused within an incarnation, not even by a later
   worker for the same session. (Reuse would let a client that names an old id, to replace it or
   `agent detach --attach` it, hit someone else's attachment.) On the owner goroutine it then applies
   the client's size, takes the snapshot (laid out for that size), subscribes the attachment to
   output and lists it, in one step, so the snapshot is an exact boundary: the output that follows
   continues it. If the output so far ends inside an escape sequence, that step waits for the rest
   of it (at most 100 ms; see "libghostty-vt"). `Attached`
   carries the id and the snapshot (and with `session-uid`, the UID).
2. **Live.** Output flows to the client through its bounded queue (below); its input, resizes and
   snapshot requests flow back. Several attachments may be live at once; input is shared and resize
   is last-writer-wins.
3. **End.** Any of: the client half-closes the stream (detach) or its stream fails (disconnect, a
   dead peer noticed after the 15-second timeout, a write that fails); `DetachAttachment` or
   `DetachSession` names it; a later attach replaces it; the session ends (`SessionEnded` follows
   the last output); the agent stops reading input and the input queue overflows (`Error`). The
   attachment is unsubscribed and unlisted on the owner goroutine as its handler returns. A handler
   blocked writing to a client that stopped reading is given 2 seconds to finish once it is told to
   end, then its write is failed.

The session never depends on any of this: it runs with no attachments at all.

A reattach after a lost connection is a new attachment with a new id; nothing of the old one is
resumed, and the screen is restored from the new snapshot. It carries two things from the old one.
`ExpectUID` makes the daemon (checking `state.db`) and the worker (checking its own UID) refuse the
attach if the name now belongs to another incarnation: the CLI reports that the session was
replaced and stops, rather than attaching the user's terminal and keystrokes to a session they never
saw. `Replaces` names the old attachment, which the worker drops at once (it is unlisted and stops
receiving output immediately; its handler ends within the 2-second grace) instead of keeping it
until its connection is noticed dead. A replace is honoured only together with a matching
`ExpectUID`, since ids are only unique within an incarnation.

### Output positions

Attach streams carry no output offsets or sequence numbers, deliberately. What a terminal client
needs after any gap (a reconnect, or output dropped because it lagged) is the current screen, and
the worker supplies exactly that: a snapshot taken on the owner goroutine, where output is
published, is an exact boundary in that client's stream, with everything before it inside the
snapshot and everything after it following as output. Offsets would only help a client that wants
the missed bytes themselves, which would mean the worker retaining a replay buffer per session, and
a terminal does not need them: replaying them would only redraw what the snapshot already shows. A
consumer that wants the full transcript reads history instead.

## Duplicate And Replayed Requests

A client whose connection dies after it sent a request cannot know whether the daemon acted on it.
Reads are simply asked again. Requests with side effects carry a client-chosen token
(`request-tokens`): `CreateSession`, `KillSession` (stop and `rm`), `AddWorkspace` and
`RemoveWorkspace`. The CLI generates one random token per command and, if the connection to a
remote daemon is lost after sending, sends the same request with the same token on a new
connection. The daemon answers a token it has seen with the original answer: a retried `new`
returns the session it created (waiting for it to finish starting if need be), not "already
exists"; a retried `kill` or `rm` returns what the first one did, not "not running" or "not found".
A duplicate that arrives while the first is still running waits for it. A token reused for a
different request is refused.

`CreateSession`'s token is stored on the session's row (`sessions.create_token`, unique), so a
retry finds the session even across a daemon restart. The others are remembered in memory for 10
minutes, at most 4096 of them (`internal/daemon/replay.go`); after a daemon restart a retried
`kill` or `rm` reports what it finds, which is the state the client asked for. `send-input` and
`detach` carry no token and are never resent.

## Slow Clients, Buffering And Backpressure

The PTY is never blocked by a client, and nothing buffers without bound. Every stage on the path
between the PTY and a client either blocks its producer (backpressure) or, at exactly one place,
drops with a defined recovery.

Output, per attachment, from the PTY to the screen:

| Stage | Bound | When full |
|---|---|---|
| PTY reads (worker) | batched into chunks of up to 8 KiB, published on the owner goroutine | a full batch stops the PTY reader until the owner takes it; publish never blocks |
| Subscriber queue (worker, `broadcast.go`) | 1024 chunks and 1 MiB, whichever first | the chunk is dropped and the attachment marked lagged |
| Attach handler (worker) | one frame being written: 8 KiB of output, or one snapshot | blocks: the queue above fills |
| Worker to daemon Unix socket | kernel socket buffers (8 KiB each way on macOS, about 200 KiB on Linux) | blocks the handler |
| Daemon proxy (`proxy.go`) | one 32 KiB copy buffer | blocks: stops reading the worker |
| QUIC stream (remote only) | the client's stream receive window (`quicConfig`): 512 KiB, grown towards 4 MiB only for a reader that keeps up; 16 MiB per connection | blocks the daemon's copy for this stream only |
| CLI frame queue (`attach.go`) | 16 decoded frames plus a 4 KiB read buffer | stops reading: the window above fills |
| Terminal | the CLI writes to stdout synchronously | stops the CLI's loop: the queue above fills |

So a remote client that stops reading holds at most about 1 MiB plus one snapshot in the worker,
32 KiB plus QUIC's unacknowledged data (at most its window) in the daemon, and the window plus 16
frames in the CLI. The rest of its output is dropped in the worker. Over QUIC each attachment is its
own stream with its own flow control, so the stall stops at that stream: other attachments and
requests on the same connection, and other clients, keep flowing. Over the Unix socket the same
holds per connection.

Recovery from the drop is a resync. The client asks for it in its attach feature list
(`attach-resync`). Once its queue overflows, the worker sends the next thing it can, at the moment
the client has drained what was in flight (the blocked write went through), as an `AttachResync`:
a fresh snapshot taken on the owner goroutine, discarding what was queued, which is an exact
boundary like any other snapshot. The CLI clears the screen and paints it, so the screen is correct
again however much was dropped; with the overlay open it ignores it, since closing the overlay
repaints from a snapshot anyway. If output keeps arriving faster than the client takes it, it gets
a resync each time it drains, which keeps it as current as its link allows. A client that stalls
outright may do so with a resync already in its blocked write; when it reads again it gets that
stale one first and, since it lagged meanwhile, a fresh one right after. A client that did not
ask for resyncs (an older CLI) keeps the old behaviour: its screen is wrong until the program
repaints or it reattaches. A snapshot is the screen plus retained scrollback (scrollback memory is
capped at 10 MB), typically some hundreds of KiB.

Input goes the other way: the CLI's stdin reader queues up to 32 reads of 4 KiB; the CLI writes
input frames to the stream as the user types, blocking when the stream's window is full; the
daemon's proxy copies through one 32 KiB buffer; the worker's attach handler reads one request at
a time and hands input to one bounded queue per session (256 writes and 4 MiB), drained by a
dedicated writer. An agent that stops reading its input therefore never stalls PTY output or other
requests. Once the queue is full, further input is refused with an error until the agent catches
up: `send-input` gets the error, and an attachment ends with it rather than silently dropping
keystrokes.

## Runtime Root

All state lives under a single root directory. The root is resolved in this priority order:

* AGENTD_DIR => uses exact path (e.g., /custom/path)
* home => uses `~/.agentd`, on every platform
* XDG_RUNTIME_DIR => uses `{XDG_RUNTIME_DIR}/agentd`, only without a home directory
* TMPDIR => uses `{TMPDIR}/agentd-{uid}` (appends uid for multi-user safety)
* /tmp => uses `/tmp/agentd-{uid}` (default fallback, appends uid for multi-user safety)

The root is deliberately not a per-boot runtime directory by default. `XDG_RUNTIME_DIR` (typically
`/run/user/{uid}`, a tmpfs) is wiped on reboot, and on Linux also once the user's last session
ends, which would take `state.db`, session history and the daemon's remote key with it; every
remote client would then see a changed key. The runtime files in the root (the sockets, lock and
pid file) are harmless after a reboot: a starting daemon removes a stale socket while holding the
lock, and session liveness is judged by connecting to the session's socket.

The selected root contains:

* `config.toml`
* `agentd.sock`
* `agentd.lock` (held by the running daemon)
* `agentd.pid` (informational)
* `state.db` (schema v5; older versions are migrated forward, newer ones refused. Migrations
  must be additive, because session workers keep writing to the file across daemon upgrades)
* `sessions/` (one socket per live session)
* `agentd.log` (output of a daemonized daemon)
* `remote/` (`daemon.key`, `authorized_clients`, and the CLI's `client.key`)
* `hosts.toml` (remote hosts known to the CLI, with pinned daemon keys)
* `logs/` (session history and `<id>.worker.log`)

The root and everything in it are private to the user (directories 0700, files and sockets 0600),
since logs hold full agent transcripts. `agentd` refuses a root owned by another user.

## libghostty-vt

We use `libghostty-vt` to restore the previous state of the terminal when a client re-attaches to a session.

How it works:

* user creates or re-attaches to a session with `agent attach <name>` or by focusing it in the TUI
* user interacts with terminal stdin
* stdin gets sent to the PTY via the daemon and the session worker
* the worker sends PTY output to attached clients and to `ghostty-vt`
* `ghostty-vt` holds terminal state and scrollback
* user disconnects
* user re-attaches to session
* `ghostty-vt` produces a terminal snapshot that is sent to the client before live output

In this way, `ghostty-vt` doesn't sit in the middle of an active terminal session, it simply receives all the same data
the client receives so it can re-hydrate clients that connect to the session. This enables users to pick up where they
left off as if they didn't disconnect from the terminal session at all.

The worker uses `go.mitchellh.com/libghostty`, whose pinned `libghostty-vt` needs Zig 0.16 and is
built `ReleaseFast` under `.build/` by `scripts/build-libghostty.sh`. Only the worker links it:
the `agent` CLI never imports the worker (or the daemon and its SQLite store), and a test keeps it
buildable with `CGO_ENABLED=0`.

### The FFI boundary

The boundary is coarse: output goes in as whole buffers, snapshots come out whole. Nothing on the
output path crosses it per cell or per character. PTY reads can be small (on macOS, a program
that writes in small pieces, such as `yes | head`, gives mostly 30 to 130 bytes per read), so the
PTY pump batches them: reads that arrive while the owner
goroutine is busy are handed over together, up to 8 KiB, as one `VTWrite` and one chunk for each
client; an idle owner gets each read at once, so batching adds no latency. A snapshot is a few
getters, one or two formatter runs and one render-state update. [BENCHMARKS.md](BENCHMARKS.md)
has the measurements: a `VTWrite` costs about 50 ns plus the parsing, and at 1 KiB per call the
call overhead is under 1%, while output through the worker is limited by the kernel's PTY.

### The reattach snapshot

The snapshot is VT bytes that the CLI writes to the user's terminal after clearing it, laid out
for that terminal's size: the worker resizes the PTY and its terminal to the attaching client
first. It is built from libghostty's VT formatter, with these additions (`terminal.go`):

* **An exact boundary.** It is taken only where the parser is between escape sequences and UTF-8
  characters (`VTGround`). If an attach arrives mid-sequence, it waits for the next PTY read and
  feeds only the bytes that finish the sequence (`VTWriteUntilGround`) before taking it, at most
  100 ms. Otherwise the client would get the sequence's tail as live output and print it.
* **The primary screen under the alternate one.** The formatter only formats the active screen,
  so while a program has the alternate screen up the worker copies the terminal (its binary
  snapshot, decoded), leaves the alternate screen in the copy, and formats the primary screen and
  scrollback from that first. The alternate screen's formatting then switches the client over with
  the mode the program used, so when the program exits, the shell and its scrollback are back. The
  scrolling region is emitted once, after the switch (it is terminal-wide), and the pen is reset
  after the switch so the alternate screen does not inherit the primary one's.
* **Blank rows at the bottom.** The formatter leaves them out, so with scrollback the rows it
  writes would land too low (after a clear screen, the prompt at the bottom, under old output).
  The missing line feeds are added after the screen contents.
* **Only changed colors.** The formatter's palette option sets all 256 colors to libghostty's
  defaults, overwriting the user's theme; the snapshot sets only the palette entries and default
  foreground, background and cursor colors that a program changed.
* **Cursor shape.** A bar or underline cursor (DECSCUSR) is restored; a block cursor is left to
  the client's default.

Tests restore snapshots into a second terminal and compare them cell by cell
(`restore_test.go`), replay recorded streams from Claude Code, Codex, a shell, Vim, Neovim,
`go test` and clang cut at many points (`streams_test.go`, `testdata/streams`), and fuzz it
(`FuzzSnapshotRestore`).

What a snapshot does not restore, because the formatter does not emit it or because a VT stream
written to someone else's terminal cannot carry it (`TestSnapshotKnownLimits` demonstrates the
formatter's):

* **Colors of blank cells.** The formatter skips cells that hold only a background (written by an
  erase while a background color is set) and writes a row of them as an empty line, so an
  editor's background shows the client's default on empty rows until the program redraws them.
  It also writes the gap before later text on a row as spaces in the pen of the text before the
  gap. Both are fixable in libghostty's formatter (`PageFormatter`), which is where the fix
  belongs.
* **The cursor in origin mode** (DECOM): the formatter enables the mode before an absolute cursor
  position, which then lands relative to the top margin.
* **Soft wraps and hyperlinks of text already on screen.** Every row is written with a line
  break after it, so a wrapped line comes back as separate lines that a later resize does not
  reflow; hyperlinks (OSC 8) are only emitted for the pen, not for text.
* **A pending wrap over a blank cell** (possible after restoring a saved cursor): the formatter
  restores a pending wrap by printing the cell under the cursor again, which does nothing when
  that cell is blank, so the next character lands on the same row instead of the next.
* **A wide character printed in the DEC line-drawing charset**: libghostty maps it but keeps it
  two columns wide, and the formatter writes the mapped character in one, so the rest of the row
  lands a column to the left.
* **The saved cursor** (DECSC, mode 1048), the kitty keyboard flag stack (only the current flags
  are set), kitty graphics and sixel images.
* **The window title and working directory** (OSC 0/2 and 7), deliberately: the CLI sets its own
  title, and the directory is a path on the session's host.
* **The client's own state.** The snapshot assumes a terminal in its default state: modes are
  emitted only where they differ from the defaults, and a block cursor is not emitted. The CLI
  clears the screen first and resets the common modes when it detaches.
* **Other clients' sizes.** The snapshot fits the attaching client; any other client attached at
  another size keeps getting output laid out for the PTY's size, which the last resize set.
