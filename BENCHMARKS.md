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
