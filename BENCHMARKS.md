# Benchmarks

Measurements of `agentd`, the decisions taken from them, and how to rerun them. Numbers are from
one machine and are meant for comparison and for spotting regressions, not as guarantees.

## QUIC transport

Measured on an Apple M1 Max (10 cores), macOS (Darwin 27.0.0), Go 1.26.3, quic-go v0.63.0, over
loopback. Client and daemon run in the same process, so memory and CPU figures cover both ends.
macOS has no UDP segmentation offload, so QUIC throughput and CPU on Linux should be better.

The tests and benchmarks are in `internal/transport/quic_measure_test.go`. The tests run with
`go test ./...` and assert only generous bounds; their numbers are logged:

```sh
go test -v -run 'FlowControl|SlowAttachment|StalledAttachments|LargeTransfer|AddressChange|PacketLoss' ./internal/transport/
go test -run '^$' -bench 'StreamRequest|ConcurrentStreams|TransferCPU|SmallMessages' -benchtime 2s ./internal/transport/
go test -run '^$' -bench 'Memory' -benchtime 5x ./internal/transport/
go test -run '^$' -bench 'WindowAtRTT' -benchtime 1x ./internal/transport/
```

Lossy, delayed and rebinding networks are simulated by a UDP relay in the test (`udpRelay` in
`quic_test.go`).

### Flow control

`quicConfig` sets the receive windows explicitly:

| Window | Initial | Maximum | quic-go default |
| --- | --- | --- | --- |
| Stream | 512 KiB | 4 MiB | 512 KiB / 6 MiB |
| Connection | 4 MiB | 16 MiB | 768 KiB / 15 MiB |

What happens when a stream's reader stops reading:

* The sender can write exactly the stream's current window (524,288 bytes for a fresh stream),
  then `Write` blocks. Nothing is dropped: when the reader resumes, every byte arrives in order.
  A stream's window only grows while it is being read quickly, so a stalled stream stays at the
  window it had.
* Each stalled stream takes its share of the connection window, which is credited back only as
  the application reads. Once the stalled streams hold the whole connection window, nothing flows
  in that direction on any stream of the connection. With quic-go's defaults (768 KiB connection,
  512 KiB stream) that took **2** stalled streams; with agentd's windows it takes **8**, measured in
  both directions (`TestQUICFlowControlBoundsStalledStreams`,
  `TestQUICStalledAttachmentsShareTheConnectionWindow`).
* Closing a stalled stream returns its credit, but quic-go only advertises new connection credit
  once a quarter of the window is free: after 8 stalled attachments, closing 2 unblocked the
  connection. Reading a stalled flood again does not help, since its writer refills the window.
* Whatever a client does with its 256 streams, the daemon buffers at most the connection window
  for it: 4 MiB until its windows have grown, 16 MiB at most.

In agentd the realistic stall is daemon to client: the CLI stops reading its attachment (a
terminal paused with Ctrl-S, say). The daemon's writer then blocks and the worker's bounded queue
drops output for that attachment (see "Slow Clients" in ARCHITECTURE.md), while the overlay's
requests on the same connection keep working: round trips beside a stalled attachment took
p50 0.17-0.26 ms, max 1-2 ms (`TestQUICSlowAttachmentLeavesOthersInteractive`). A CLI holds one
attachment per connection, far from the 8 it would take to block the connection.

The maximum stream window decides how fast one large transfer can go on a long path, since the
sender waits for window updates once the window is smaller than the bandwidth-delay product. One
64 MiB transfer through a relay adding delay (`BenchmarkQUICWindowAtRTT`):

| Round trip | Stream 1 MiB | agentd (4 MiB) | Stream 16 MiB |
| --- | --- | --- | --- |
| 20 ms | 35 MB/s | 81 MB/s | 82 MB/s |
| 100 ms | 7.5 MB/s | 23 MB/s | 37 MB/s |

At 20 ms 4 MiB is as fast as 16 MiB (the relay and CPU are the limit); at 100 ms it still moves
a full 64 MiB snapshot or history in about 3 seconds. PTY output is orders of magnitude below
either. 4 MiB per stream and 16 MiB per connection keep a client's buffering in the daemon bounded
and small, and can be raised if transfers over long paths ever need it.

### Interactive traffic beside other streams

* **A large transfer and an interactive stream** (`TestQUICLargeTransferLeavesEchoInteractive`):
  64 MiB moved in about 0.53 s (120 MiB/s) while another stream did 32-byte echo round trips:
  p50 0.23-0.24 ms, p99 0.7-6.2 ms, max 6.2 ms, against 0.3-0.5 ms when idle. Under the race
  detector: 38 MiB/s, p99 1.9 ms.
* **A stalled attachment and an interactive stream**: see above; no measurable effect.

### Stream costs

| Benchmark | Result |
| --- | --- |
| One request on a new stream (`BenchmarkQUICStreamRequest`) | 82 µs, 6.9 KB and 107 allocations per request |
| The same on a new Unix socket connection (`BenchmarkUnixStreamRequest`) | 27 µs, 2.8 KB, 28 allocations |
| 1,000 concurrent requests on one connection, at most 256 streams open (`BenchmarkQUICConcurrentStreams`) | 12.9 ms per 1,000 (12.9 µs per stream), 6.8 MB and 80,017 allocations per 1,000 |

Opening a stream costs no round trip; the cost is QUIC's per-stream state and framing on both
ends. 1,000 streams through the 256-stream limit queue in `OpenStream` without errors.

### Memory and CPU

| Measure | Result |
| --- | --- |
| Heap per open idle connection, both ends (`BenchmarkQUICMemoryPerConnection`, 100 connections) | 66 KB, 7 goroutines |
| Heap per open stream with a waiting handler, both ends (`BenchmarkQUICMemoryPerStream`, 200 streams) | 4.5 KB heap, plus the handler goroutine (1 goroutine, about 3 KB of stack) |
| CPU per MiB over one stream, both ends (`BenchmarkTransferCPU`) | QUIC 39 ms (110 MB/s); Unix socket 0.32 ms (3.3 GB/s) |

Each CLI connection has its own UDP socket and quic-go transport, which accounts for some of the
goroutines. QUIC costs about 120 times the CPU of a Unix socket per byte (encryption, a packet per
1.2-1.5 KB and acknowledgements, with no segmentation offload on macOS). That is irrelevant for
PTY output and requests, and is why local clients keep using the Unix socket.

### Priorities

quic-go has no stream priorities: it sends from streams with data in turn. The measurements do not
show a need for them. An interactive stream beside a 120 MiB/s transfer kept a p99 round trip of a
few milliseconds on loopback, and beside a stalled stream it was unaffected; flow control, not
scheduling, is what could block it, and the connection window covers that. **Decision: no
priorities.** Revisit if a real workload (large artifact streams over a slow link, say) shows
interactive latency suffering; prioritising would then mean either an upstream quic-go feature or
pacing large transfers in agentd.

### Datagrams

QUIC datagrams (RFC 9221) are unreliable and unordered, so only messages that are replaceable and
not worth retransmitting could use them. Going through the protocol (`internal/protocol`):

* **Must stay reliable:** `PtyOutput` (ordered terminal state transitions; a lost chunk corrupts
  the screen until a repaint), `AttachInput` and `SendInput` (keystrokes), `Attached` and
  `AttachSnapshot` (terminal state), `SessionEnded`, and every request and response (create, kill,
  attach, detach, list, history, workspaces, daemon info), which are commands with results.
* **Replaceable in principle:** `AttachResize`, since a newer size supersedes an older one. But the
  last one must arrive, and it must arrive in order with input and output on its attachment
  (input typed after a resize, and a snapshot asked for after one, assume the new size). Sending it
  as a datagram would need retransmission until acknowledged, which is what the stream already
  does; resizes are rare and tiny. Not worth it.
* **Latency probes and presence:** the protocol has neither. QUIC already pings every 5 seconds and
  ends a silent connection after 15, and quic-go's `ConnectionStats` reports the round-trip time,
  so a latency display would not need probes of its own.

No current message is a genuine datagram candidate.

Measured with datagrams enabled in the test configuration only (`BenchmarkQUICSmallMessages`,
64-byte messages, loopback, no loss):

| | Stream | Datagram |
| --- | --- | --- |
| Round trip | 46 µs | 57 µs |
| One way, back to back | 1.3 µs per message | 12.5 µs per message, 0% lost |

Datagrams are no faster: a stream round trip is as quick without loss, and a stream coalesces
small writes into full packets while quic-go sends datagrams from a 32-entry queue.

Under packet loss (`TestQUICUnderPacketLoss`, random loss in both directions): a 1 MiB stream echo
always arrived byte-exact and in order, taking 23-50 ms at 5% loss and 2.0-2.3 s at 20% (QUIC's
congestion control backs off hard). Of 500 datagrams, 3.4-4.4% were lost at 5% loss and 22-25% at
20%, as expected without retransmission.

**Decision: reliable streams stay the default and the only thing agentd uses.** Datagrams stay
disabled in `quicConfig` (`EnableDatagrams` is off) until a message exists that is replaceable,
time-sensitive and not worth retransmitting.

### Path migration

`TestQUICSurvivesClientAddressChange` moves the client's address mid-connection three times (the
relay switches to a new UDP socket, as a NAT rebinding or a move from Wi-Fi to cellular would).
The open stream keeps working without a reconnect: the daemon validates the new path and moves the
connection to it (RFC 9000 section 9, which quic-go implements for servers). The first round trip
after each move took 0.5-5 ms, a 1 MiB burst afterwards arrived intact, and the listener saw a
single connection throughout. A client whose address changes therefore keeps its attachments, as
long as packets flow again before the 15-second idle timeout; after that the CLI reconnects and
reattaches as before.

## Sessions at scale

What a session costs, measured end to end with `agentd bench sessions` (`internal/bench`): it
starts a private daemon in a temporary root, creates the sessions through the protocol, asks each
worker for its own numbers (`GetSessionStats`, see "Resource Usage" in ARCHITECTURE.md), reads CPU
time from outside the processes, and stops everything it started.

Measured on an Apple M1 Max (10 cores, 64 GiB), macOS (Darwin 27.0.0), Go 1.26.3, with the
commits that added the bench (on top of `d048e1e`). The machine was shared with other jobs (load
average 9-47 during the runs), so latency and throughput are pessimistic; memory, thread, file
and goroutine counts are not affected. The sessions run `/bin/sh -c 'exec cat'` (idle), `yes ... & exec cat` (output-heavy)
or print 200,000 lines and then `cat` (full scrollback), so the numbers are agentd's and not a
real agent's.

```sh
make agentd
bin/agentd bench sessions --count 1
bin/agentd bench sessions --count 10
bin/agentd bench sessions --count 100 --output-heavy 10 --attach 10
bin/agentd bench sessions --count 500
bin/agentd bench sessions --count 100 --duration 60s --fill-lines 0      # idle CPU
bin/agentd bench sessions --count 0 --output-heavy 100 --attach 100 --duration 10s --fill-lines 0
go test ./internal/worker -run '^$' -bench TerminalMemory -benchtime 1x
go test ./internal/db -run '^$' -bench .
```

1,000 sessions could not be run here: macOS allows 511 pseudo-terminals for all programs together
(`kern.tty.ptmx_max`; creating more fails with "device not configured", which agentd now explains).
500 was the most this machine allowed. Every per-session cost below is flat from 1 to 500 sessions,
so 1,000 idle sessions would cost about 8.7 GiB of worker memory, 1.2 GiB for the `cat` agents,
1,000 more daemon threads and about 100 MiB in the daemon.

### Per-session fixed costs

One idle session (mean over 500; the same at 1, 10 and 100):

| Cost | Per session | Notes |
| --- | --- | --- |
| Worker private memory | 8.9 MiB | Physical footprint. RSS is 19.7 MiB, but about 10.8 MiB of it is the `agentd` executable and system libraries, shared by every worker |
| Worker Go heap | 2.5 MiB | About 2 MiB of it is `modernc.org/libc`'s `/etc/services` and `/etc/protocols` tables, built at start-up (see the candidates below) |
| Worker Go runtime memory | 9.8 MiB mapped | Not all resident |
| libghostty terminal | 25 KB | `BenchmarkTerminalMemory`: 10,000 empty terminals at 160x48. At the 10 MB scrollback limit (6,900 rows at 160 columns) a terminal holds 9.4 MiB |
| Worker threads | 9 (8-10) | Includes the PTY reader blocked in `read(2)` and `cmd.Wait` blocked in `wait4` |
| Worker goroutines | 27 | 10 are the GC's mark workers (one per CPU), about 10 are the runtime's, 7 are agentd's |
| Worker open files | 9 | stdin, the log (stdout and stderr), the PTY, the session socket, the Go netpoller's kqueue, 2 pipes |
| Agent (`cat`) | 1.2 MiB, 1 thread, 3 files | A real agent costs far more: this is agentd's share only |
| Daemon | 1 goroutine, 1.0 thread, 85-112 KiB, 0 files | The supervisor goroutine blocked in `wait4`, which holds a thread |
| state.db | about 220 bytes | 112 KiB with 501 rows |
| Idle CPU | 0.4 µs/s per worker | 100 workers over 60 s: 0.004% of a core for all of them, daemon 0.0001%. Nothing wakes an idle worker |

At 1, 10, 100 and 500 sessions:

| Sessions | Create all (p50 / p99 each) | Daemon private / threads / goroutines | Worker private | Idle attach p50 / p99 | ListSessions through the daemon / state.db alone |
| --- | --- | --- | --- | --- | --- |
| 1 | 0.02 s (24 / 24 ms) | 9.4 MiB / 9 / 24 | 8.8 MiB | 0.49 / 0.49 ms | 0.29 / 0.13 ms |
| 10 | 0.06 s (32 / 58 ms) | 12.5 MiB / 24 / 33 | 9.0 MiB | 0.83 / 0.85 ms | 0.54 / 0.16 ms |
| 100 | 0.67 s (36 / 216 ms) | 19.8 MiB / 113 / 123 | 8.9 MiB | 0.53 / 1.04 ms | 2.4 / 0.44 ms |
| 500 | 2.6 s (34 / 111 ms) | 50.2 MiB / 514 / 523 | 8.9 MiB | 0.50 / 1.12 ms | 11 / 1.5 ms |

Sessions are created 8 at a time. The daemon starts (to its first answer) in 11-14 ms and holds
8.8 MiB private (19.4 MiB RSS), 8 threads, 23 goroutines and 9 files with no sessions.

### Snapshots and restore

The worker formats a snapshot on its owner goroutine and the bench replays it into a fresh
libghostty terminal of the session's size ("restore", what a client's terminal does with it).
Attach is the client's round trip through the daemon until the snapshot has arrived.

| Session | Snapshot | Format | Restore | Attach p50 (p99) |
| --- | --- | --- | --- | --- |
| Idle (empty screen) | 5.4 KiB | 38 µs | 75 µs | 0.5 ms (1.1 ms) |
| Full scrollback (6,903 rows) | 630 KiB | 2.0-2.7 ms | 1.3-1.6 ms | 3.1-4.7 ms (11 ms) |

A worker with full scrollback holds 26-27 MiB private, against 8.9 MiB idle: 9.4 MiB of
libghostty state, and the rest Go memory from formatting snapshots and history.

Since the worker compresses quiet scrollback (libghostty `Terminal.Compress`; see ARCHITECTURE.md,
"Resource Usage"), the libghostty part shrinks once the session has been quiet for three seconds:
a full 10 MB scrollback goes from 9.4 MiB to 0.6 MiB resident (23 of 24 pages compressed,
`TestIdleScrollbackIsCompressed`), and `agentd bench sessions` measured the full-scrollback worker
at 28.2 MiB private just after its snapshots and 19.4 MiB once idle. Formatting a snapshot from
compressed scrollback took 5.0 ms instead of 4.5-4.9 ms, and leaves the pages decompressed until
the next quiet tick.

### PTY throughput and fan-out

With no client attached, output-heavy sessions are bound by the worker feeding libghostty:

| Output-heavy sessions | Total | Per session | Worker CPU | CPU per MiB |
| --- | --- | --- | --- | --- |
| 10 | 99 MiB/s | 9.8 MiB/s | 554% | 56 ms |
| 100 | 89-91 MiB/s | 0.9 MiB/s | 647-727% | 71-82 ms |

The machine's CPUs, shared with other jobs, are the limit: two sessions alone reached 46-64 MiB/s
each at 17-23 ms of worker CPU per MiB. macOS PTYs return about 1 KiB per read under a flood
(1,012 bytes on average). An output-heavy worker holds 21-22 MiB private (its scrollback is
full) and 15 threads.

Fan-out, one output-heavy session with K clients attached through the daemon's Unix socket:

| Clients | PTY output | Each client receives | Dropped chunks | Echo to every client p50 / p99 / max | Worker / daemon CPU |
| --- | --- | --- | --- | --- | --- |
| 10 | 17.4 MiB/s | 17.4 MiB/s (100%) | 59 | 0.37 / 2.9 / 5.5 ms | 184% / 234% |
| 100 | 19.2 MiB/s | 6.7 MiB/s (35%) | 13.1 million | 35 / 71 / 110 ms, 34% lost | 303% / 290% |

Echo is a marker typed every 100 ms into the session, timed until the PTY's echo of it reaches
each client: input latency behind a stream of output. With 100 clients each PTY read becomes 100
frames of about 1 KiB, nearly 2 million a second through the worker and the daemon's proxy; the
clients fall behind, the worker drops what does not fit in their queues (the slow-consumer policy
in `internal/worker/broadcast.go`), and echoes are lost with the dropped chunks.

### Database

`go test ./internal/db -bench .` (every call opens its own SQLite connection, about 120 µs of it):

| Operation | Time | Allocations |
| --- | --- | --- |
| ListSessions, 10 / 100 / 1,000 rows | 0.16 / 0.39 / 3.0 ms | 464 / 3,527 / 34,131 |
| GetSession, 1,000 rows | 0.13 ms | 158 |
| InsertSession, 1,000 rows | 0.55 ms | 113 |
| InsertSession + MarkRunning (a session start) | 1.0 ms | 220 |

Through the daemon, ListSessions also probes every running worker's socket (about 19 µs each),
which is most of its 11 ms at 500 sessions.

### Candidates

The largest per-session fixed costs, from the numbers above. None of them was changed here: the
largest is in a dependency and belongs upstream, and the others are not simple or not permanent
costs.

1. **`modernc.org/libc` parses `/etc/services` at start-up: 3.9 MiB of every worker's 8.9 MiB.**
   Its `netdb` package reads `/etc/services` (678 KB on macOS) and `/etc/protocols` in `init`:
   3.6 MB and 44,000 allocations and about 3 ms in every `agentd` process (`GODEBUG=inittrace=1`),
   keeping about 2 MB of tables alive that agentd never uses. With a local copy that loads the
   tables on first use (a `sync.Once` in its four lookup functions; nothing outside the package
   reads them), an idle worker held 5.0 MiB private instead of 8.9 MiB, a 150 KiB heap instead of
   2.5 MiB, and 17 goroutines instead of 27 (no garbage collection had to run, so the GC's workers
   never started). That change should go to `modernc.org/libc` upstream rather than into a fork
   here.
2. **Scrollback, up to 10 MB of libghostty state per session** (`maxScrollbackBytes`). A session
   whose output fills it costs 12-18 MiB more than an idle one (about 9 MiB less once it is quiet,
   now that quiet scrollback is compressed). It is a variable cost, but the
   largest one: 500 such sessions would need about 13 GiB. A configurable or smaller limit is a
   product decision.
3. **One daemon thread per session.** Each worker's supervisor blocks in `cmd.Wait` (`wait4`),
   which holds an OS thread: 514 daemon threads at 500 sessions, and 85-112 KiB per session. One
   reaper driven by SIGCHLD would make it constant, but the daemon's share is about 1% of a
   session's cost.
4. **Worker threads.** 9 per idle worker: the PTY reader blocks in `read(2)` (the PTY is not in
   Go's poller) and `cmd.Wait` in `wait4`, beside the runtime's own. `GOMAXPROCS=1` or `2` saved
   1-2 threads and 0.2-0.4 MiB per worker, and would cap a busy one; not worth it.
5. **One frame per PTY read per client.** Fan-out to 100 clients collapses because every 1 KiB
   read becomes a frame for every client. Writing the chunks queued for a client as one frame
   would cut frames and system calls by about an order of magnitude; it changes throughput, not
   per-session cost, and fits with the planned resync of lagging clients. The worker now batches PTY
   reads that arrive while it is busy into one chunk (see "Terminal and worker"), which cut frames
   2.7 times for small reads; coalescing what is queued for a slow client is still open.
6. **ListSessions dials every running worker** to check it is alive: 11 ms at 500 sessions, against
   1.5 ms for state.db.

## Terminal and worker

In-process measurements of a session worker: the libghostty boundary, the reattach snapshot, PTY
output through a real worker, fan-out and attach. The benchmarks are in `internal/worker`
(`terminal_test.go`, `worker_bench_test.go`); the PTY and attach ones run `Run` against a real PTY
and `/bin/sh`, and talk to the worker over its socket as the daemon does.

Measured on an Apple M1 Max (10 cores, 64 GiB), macOS 27.0.1, Go 1.26.3, libghostty-vt
`ReleaseFast` at ghostty `27e8b3f`, on the branch that added them (on top of `651a4cd`, rebased onto `c154564`). The
machine was shared with other jobs (load average 5-40), so the median of three runs is given.

```sh
make libghostty
export PKG_CONFIG_PATH=$PWD/.build/ghostty-out/share/pkgconfig
go test ./internal/worker -run '^$' -bench 'VTWrite|TerminalFeed|TerminalSnapshot|RecordedStream|NewTerminal' -benchmem -count 3
go test ./internal/worker -run '^$' -bench 'PTYThroughput|FanOut|Attach' -benchmem -count 3
```

### The libghostty boundary

`BenchmarkVTWriteChunkSize` feeds the same 64 KiB of colored clang diagnostics in chunks of each
size, one `VTWrite` per chunk:

| Chunk | Throughput | Per call |
| --- | --- | --- |
| 1 B | 18.6 MB/s | 54 ns |
| 64 B | 193 MB/s | 332 ns |
| 1 KiB | 239 MB/s | 4.3 µs |
| 8 KiB | 258 MB/s | 31.8 µs |
| 64 KiB | 267 MB/s | 245 µs |

A call costs about 50 ns of its own (the 1-byte case is almost all call), so at 1 KiB per call the
overhead is about 1% and at 64 bytes about 25%. Plain line output parses at 630-680 MB/s
(`BenchmarkTerminalFeed`, 8 KiB chunks); creating and freeing a terminal takes 9.6 µs.

The worker makes one `VTWrite` per chunk of PTY output, and nothing on the output path crosses
the boundary per cell or per character. A snapshot is a few getters, one formatter run (two when
blank rows at the bottom need padding), one render-state update for the cursor shape, and with
the alternate screen up a binary snapshot and decode of the terminal.

PTY reads can be much smaller than a chunk. With `yes 'line padding…' | head -c 8M` writing to a
PTY, a bare read loop on macOS got 480,000 reads, almost all of 33 to 128 bytes, at 25.8 MB/s.
(The "Sessions at scale" numbers above saw about 1 KiB per read from `yes` writing directly; read
sizes follow how the program writes.) So the worker's PTY pump batches reads that arrive while the
owner goroutine is busy, up to 8 KiB, and hands them over as one chunk: one trip through the owner's
queue, one `VTWrite` and one frame per client. An idle owner gets every read at once, so batching
adds no latency.

### PTY throughput

`BenchmarkPTYThroughput`: the agent writes 8 MiB of short lines; one client attached over the
worker's socket reads until the end marker. CPU is the test process's (worker and client), not the
agent's.

| Pump | Throughput | Frames per 8 MiB | CPU per 8 MiB | Allocations |
| --- | --- | --- | --- | --- |
| One chunk per read (before) | 20-27 MB/s | 85,000 | 1.11 s | 1.03 million |
| Batched (now) | 26.4 MB/s | 31,400 | 0.79 s | 0.41 million |

Throughput is the kernel's: about what a bare read loop gets from the same PTY. Batching cuts the
frames (and with them `VTWrite` calls, owner trips and socket writes) by 2.7 times, CPU by 30% and
allocations by 60%. A profile of the batched worker is almost all system calls (PTY reads 22%,
socket writes 16%, reads 10%) and scheduler wake-ups; libghostty's parsing is a few percent.

### Snapshots

`BenchmarkTerminalSnapshot` (160x48, 1 MB of line output in scrollback) and
`BenchmarkRecordedStreamSnapshot` (each recorded stream at 120x40, stopped before the program
exits, so the editors and agents still have the alternate screen up):

| Terminal | Snapshot | Time |
| --- | --- | --- |
| 1 MB of scrollback, primary screen | 288 KiB | 1.9 ms |
| 1 MB of scrollback, alternate screen up | 289 KiB | 3.7 ms |
| clang, 500 KB of diagnostics | 539 KiB | 7.2 ms |
| Neovim | 11 KiB | 127 µs |
| Vim | 4.0 KiB | 117 µs |
| Claude Code | 2.1 KiB | 76 µs |
| Codex | 0.5 KiB | 66 µs |
| bash | 2.3 KiB | 40 µs |
| `go test -v` | 2.0 KiB | 29 µs |

Formatting costs about 2 ms per MB of plain scrollback, more for styled output (clang's 500 KB
take 7 ms). With the alternate screen up the primary screen
under it is formatted from a copy of the terminal, which doubles the cost; at the 10 MB scrollback
limit that is about 40 ms per attach, against 20 ms. The snapshot no longer carries the 256-entry
palette (about 6 KiB) unless a program changed colors.

### Attach

`BenchmarkAttach`: connect to the worker's socket, send `AttachSession`, receive `Attached` with
the snapshot, disconnect. The session is a shell script at 160x48.

| Session | Snapshot | Round trip |
| --- | --- | --- |
| Idle, a line on screen | 255 B | 0.50 ms |
| 1 MB of scrollback | 298 KiB | 4.9 ms |

The idle round trip was 0.09 ms before attachment ids were numbered in `state.db` (see "Sessions,
Incarnations And Attachments" in ARCHITECTURE.md), and 0.50 ms with a database write per attach.
Workers now reserve ids in blocks, so an attach no longer writes.

### Attach latency through the daemon

`agentd bench sessions --count 10 --fill-lines 0`, attaching to each idle session once, one at a
time (`--parallel 1`) or eight at once (`--parallel 8`, the default). Later work had put two
database writes on every attach (numbering the attachment, acknowledging the session's attention),
in SQLite's default rollback journal, where every access waits for any write by sleeping in steps
of up to 100 ms. Moving state.db to write-ahead logging with a long-lived connection per process,
reserving attachment ids in blocks, and writing the acknowledgement beside the attach brought it
back:

| | One at a time (p50) | Eight at once (p50) |
| --- | --- | --- |
| Before | 3.3 ms | 26-470 ms (one run's p99: 950 ms) |
| After | 0.58-0.81 ms | 1.7-2.9 ms |

Same machine, under a load average of 3-9 from other work, so the spread between runs is wide.

### Fan-out

`BenchmarkFanOut`: publishing an 8 KiB chunk to 1, 10 and 100 subscribers, each drained by its own
goroutine as attached clients are:

| Subscribers | Per publish |
| --- | --- |
| 1 | 150 ns |
| 10 | 1.5 µs |
| 100 | 3.6 µs |

Publishing never blocks or copies (every subscriber gets the same slice), so it is cheap; what
limits many clients is writing a frame to each of them (see "PTY throughput and fan-out" above).
The share of chunks that reach the in-process subscribers varies from run to run (25-90%):
publishing outruns them, and the slow-consumer policy drops what does not fit.
