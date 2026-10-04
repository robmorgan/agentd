package transport

// Measurements of the QUIC transport: flow control, stream independence,
// stream costs, datagrams, loss and path migration. BENCHMARKS.md records the
// numbers and the decisions taken from them; run the tests with -v to see the
// numbers they log, and the benchmarks with
//
//	go test -run '^$' -bench . -benchtime 1x ./internal/transport/
//
// The tests assert only generous bounds, so they stay reliable on a loaded
// machine and under the race detector.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// Stream roles for the mux server, chosen by a stream's first byte. Each
// stands in for a kind of agentd traffic.
const (
	// roleEcho echoes what it reads, as a PTY echoes keystrokes.
	roleEcho = 'E'
	// roleFlood writes a counting pattern until the stream fails, as PTY
	// output to an attachment does. The byte after the role names the
	// counter of bytes written.
	roleFlood = 'F'
	// roleStall never reads again, as a stuck consumer would.
	roleStall = 'S'
	// roleIdle waits for the stream to end, holding no buffer, as an idle
	// attachment's handler does.
	roleIdle = 'I'
	// roleSink reads and discards everything.
	roleSink = 'D'
	// roleSend writes the number of bytes in the 8 bytes after the role,
	// then half-closes, as a large artifact transfer would.
	roleSend = 'L'
)

// muxServer is a QUIC listener serving the stream roles above.
type muxServer struct {
	l *QUICListener
	// done ends stalled handlers when the test ends.
	done chan struct{}
	// active counts running handlers.
	active atomic.Int64

	mu      sync.Mutex
	flooded map[byte]*atomic.Int64
}

func startMux(t testing.TB, server *Identity, client string) *muxServer {
	t.Helper()
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{Authorized: func(fp string) bool { return fp == client }})
	if err != nil {
		t.Fatal(err)
	}
	m := &muxServer{l: l, done: make(chan struct{}), flooded: map[byte]*atomic.Int64{}}
	// Cleanups run last first: stalled handlers end, then the listener.
	t.Cleanup(func() { l.Close() })
	t.Cleanup(func() { close(m.done) })
	go Serve(l, "test", nil, m.handle)
	return m
}

func (m *muxServer) handle(s Stream) {
	m.active.Add(1)
	defer m.active.Add(-1)
	defer s.Close()
	var role [1]byte
	if _, err := io.ReadFull(s, role[:]); err != nil {
		return
	}
	switch role[0] {
	case roleEcho:
		io.Copy(s, s)
	case roleFlood:
		if _, err := io.ReadFull(s, role[:]); err != nil {
			return
		}
		written := m.floodCounter(role[0])
		buf := make([]byte, 32<<10)
		for off := int64(0); ; {
			fillPattern(buf, off)
			n, err := s.Write(buf)
			off += int64(n)
			written.Store(off)
			if err != nil {
				return
			}
		}
	case roleStall:
		<-m.done
	case roleIdle:
		for {
			if _, err := s.Read(role[:]); err != nil {
				return
			}
		}
	case roleSink:
		io.Copy(io.Discard, s)
	case roleSend:
		var size [8]byte
		if _, err := io.ReadFull(s, size[:]); err != nil {
			return
		}
		buf := make([]byte, 32<<10)
		for off, end := int64(0), int64(binary.BigEndian.Uint64(size[:])); off < end; {
			n := min(int64(len(buf)), end-off)
			fillPattern(buf[:n], off)
			if _, err := s.Write(buf[:n]); err != nil {
				return
			}
			off += n
		}
		s.CloseWrite()
		io.Copy(io.Discard, s)
	}
}

func (m *muxServer) floodCounter(id byte) *atomic.Int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.flooded[id] == nil {
		m.flooded[id] = new(atomic.Int64)
	}
	return m.flooded[id]
}

// fillPattern fills buf with the pattern at stream offset off. A prime
// period means data delivered at the wrong offset does not match.
func fillPattern(buf []byte, off int64) {
	for i := range buf {
		buf[i] = byte((off + int64(i)) % 251)
	}
}

// checkPattern reports where buf, read at stream offset off, departs from
// the pattern.
func checkPattern(buf []byte, off int64) error {
	for i, b := range buf {
		if want := byte((off + int64(i)) % 251); b != want {
			return fmt.Errorf("byte %d is %d, want %d", off+int64(i), b, want)
		}
	}
	return nil
}

// dialMux starts a mux server and connects a client to it, directly or
// through the address via returns.
func dialMux(t testing.TB, via func(addr string) string) (*QUICClient, *muxServer) {
	t.Helper()
	server, client := mustIdentity(t), mustIdentity(t)
	m := startMux(t, server, client.Fingerprint)
	addr := m.l.Addr()
	if via != nil {
		addr = via(addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, addr, client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, m
}

// openRole opens a stream and sends its role, which is when the server
// first sees it.
func openRole(t testing.TB, c *QUICClient, role ...byte) Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Write(role); err != nil {
		t.Fatal(err)
	}
	return s
}

// echoOnce sends a 32-byte message on an echo stream and waits for it to
// come back, as a keystroke and its echo would.
func echoOnce(s Stream) (time.Duration, error) {
	msg := make([]byte, 32)
	start := time.Now()
	s.SetDeadline(start.Add(quicTestTimeout))
	if _, err := s.Write(msg); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(s, msg); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// latencies summarises round-trip times.
type latencies []time.Duration

func (l latencies) String() string {
	if len(l) == 0 {
		return "no samples"
	}
	s := slices.Clone(l)
	slices.Sort(s)
	q := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return fmt.Sprintf("n=%d p50=%v p99=%v max=%v", len(s), q(0.5), q(0.99), s[len(s)-1])
}

func (l latencies) max() time.Duration { return slices.Max(l) }

// settle waits until counter stops changing for a while, and returns its
// value: how much a writer got through before flow control blocked it.
func settle(counter *atomic.Int64) int64 {
	last, since := counter.Load(), time.Now()
	for deadline := time.Now().Add(quicTestTimeout); time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if v := counter.Load(); v != last {
			last, since = v, time.Now()
		} else if time.Since(since) > 300*time.Millisecond {
			break
		}
	}
	return last
}

// writeUntilBlocked writes to s until a write makes no progress for a
// while, and returns how much it wrote.
func writeUntilBlocked(s Stream) int64 {
	buf := make([]byte, 16<<10)
	var total int64
	for {
		s.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := s.Write(buf)
		total += int64(n)
		if err != nil {
			s.SetWriteDeadline(time.Time{})
			return total
		}
	}
}

// A stream whose reader stops reading takes only its own flow-control window
// of the connection's: the sender blocks once it has filled the stream
// window, and the connection window lets several such streams stall before
// a stream that is still being read is held up. This is what bounds what a
// client can make the daemon buffer, whatever it does with its 256 streams.
func TestQUICFlowControlBoundsStalledStreams(t *testing.T) {
	conf := quicConfig(DeadPeerTimeout)
	c, _ := dialMux(t, nil)

	// Client to daemon: a daemon handler that stops reading.
	var perStream []int64
	var total int64
	for len(perStream) < 64 {
		n := writeUntilBlocked(openRole(t, c, roleStall))
		if n == 0 {
			break
		}
		perStream = append(perStream, n)
		total += n
	}
	t.Logf("client to daemon: stalled streams accepted %v bytes, %d in all, before the connection blocked", perStream, total)
	if len(perStream) == 0 {
		t.Fatal("the first stream accepted nothing")
	}
	if perStream[0] > int64(conf.MaxStreamReceiveWindow) {
		t.Errorf("one stalled stream took %d bytes, over the %d-byte stream window", perStream[0], conf.MaxStreamReceiveWindow)
	}
	if total > int64(conf.MaxConnectionReceiveWindow) {
		t.Errorf("stalled streams took %d bytes, over the %d-byte connection window", total, conf.MaxConnectionReceiveWindow)
	}
	if want := int(conf.InitialConnectionReceiveWindow / conf.InitialStreamReceiveWindow); len(perStream) < want {
		t.Errorf("the connection blocked after %d stalled streams, want at least %d", len(perStream), want)
	}
}

// The same, daemon to client: an attachment whose client stops reading its
// output (a terminal paused with Ctrl-S, say) fills its stream window and
// then blocks the daemon's writer, while the connection's other streams keep
// flowing. Once the client reads again, all the output arrives, in order.
func TestQUICSlowAttachmentLeavesOthersInteractive(t *testing.T) {
	conf := quicConfig(DeadPeerTimeout)
	c, m := dialMux(t, nil)
	slow := openRole(t, c, roleFlood, 0)
	echo := openRole(t, c, roleEcho)

	buffered := settle(m.floodCounter(0))
	t.Logf("daemon wrote %d bytes to an attachment that is not being read before blocking", buffered)
	if buffered > int64(conf.MaxStreamReceiveWindow)+64<<10 {
		t.Errorf("daemon wrote %d bytes to a stalled stream, over the %d-byte stream window", buffered, conf.MaxStreamReceiveWindow)
	}

	var rtts latencies
	for range 50 {
		rtt, err := echoOnce(echo)
		if err != nil {
			t.Fatalf("interactive stream stopped while another stalled: %v", err)
		}
		rtts = append(rtts, rtt)
	}
	t.Logf("interactive round trips beside the stalled attachment: %v", rtts)
	if rtts.max() > 2*time.Second {
		t.Errorf("interactive round trip took %v beside a stalled stream", rtts.max())
	}

	// Reading again gets every byte, in order.
	slow.SetReadDeadline(time.Now().Add(quicTestTimeout))
	buf := make([]byte, 2*buffered)
	if _, err := io.ReadFull(slow, buf); err != nil {
		t.Fatal(err)
	}
	if err := checkPattern(buf, 0); err != nil {
		t.Fatalf("stalled stream lost or reordered output: %v", err)
	}
}

// Several attachments whose clients stop reading eventually take the whole
// connection window, and then nothing flows to the client on any stream
// until one of them reads again. This measures how many it takes.
func TestQUICStalledAttachmentsShareTheConnectionWindow(t *testing.T) {
	conf := quicConfig(DeadPeerTimeout)
	c, m := dialMux(t, nil)
	var perStream []int64
	var streams []Stream
	for i := byte(0); i < 64; i++ {
		streams = append(streams, openRole(t, c, roleFlood, i))
		n := settle(m.floodCounter(i))
		if n == 0 {
			break
		}
		perStream = append(perStream, n)
	}
	var total int64
	for _, n := range perStream {
		total += n
	}
	t.Logf("daemon to client: %d stalled attachments took %v bytes, %d in all, before the connection blocked", len(perStream), perStream, total)
	if want := int(conf.InitialConnectionReceiveWindow / conf.InitialStreamReceiveWindow); len(perStream) < want {
		t.Errorf("the connection blocked after %d stalled attachments, want at least %d", len(perStream), want)
	}
	if total > int64(conf.MaxConnectionReceiveWindow)+int64(len(perStream))*64<<10 {
		t.Errorf("stalled attachments took %d bytes, over the %d-byte connection window", total, conf.MaxConnectionReceiveWindow)
	}

	// Reading one of them again would only let its writer fill the window
	// again. Closing them (detaching the stuck clients) returns their
	// share of the connection window, which unblocks the rest; quic-go
	// only advertises more once a quarter of the window is free.
	for closed, s := range streams {
		s.Close()
		echo := openRole(t, c, roleEcho)
		echo.SetDeadline(time.Now().Add(time.Second))
		msg := make([]byte, 32)
		echo.Write(msg)
		if _, err := io.ReadFull(echo, msg); err == nil {
			t.Logf("the connection flowed again after %d stalled attachments were closed", closed+1)
			return
		}
	}
	t.Fatal("the connection stayed blocked after every stalled attachment was closed")
}

// A large transfer (a snapshot, history, or a future artifact) on one stream
// leaves an interactive stream on the same connection responsive.
func TestQUICLargeTransferLeavesEchoInteractive(t *testing.T) {
	size := int64(64 << 20)
	if testing.Short() {
		size = 8 << 20
	}
	c, _ := dialMux(t, nil)
	echo := openRole(t, c, roleEcho)
	idle, err := echoOnce(echo)
	if err != nil {
		t.Fatal(err)
	}

	var req [9]byte
	req[0] = roleSend
	binary.BigEndian.PutUint64(req[1:], uint64(size))
	large := openRole(t, c, req[:]...)
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		large.SetReadDeadline(time.Now().Add(time.Minute))
		buf := make([]byte, 64<<10)
		var off int64
		for {
			n, err := large.Read(buf)
			if cerr := checkPattern(buf[:n], off); cerr != nil {
				done <- cerr
				return
			}
			off += int64(n)
			if err == io.EOF && off == size {
				done <- nil
				return
			}
			if err != nil {
				done <- fmt.Errorf("after %d of %d bytes: %w", off, size, err)
				return
			}
		}
	}()

	var rtts latencies
	for transferring := true; transferring; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("large transfer: %v", err)
			}
			transferring = false
		case <-time.After(5 * time.Millisecond):
			rtt, err := echoOnce(echo)
			if err != nil {
				t.Fatalf("interactive stream failed during a large transfer: %v", err)
			}
			rtts = append(rtts, rtt)
		}
	}
	elapsed := time.Since(start)
	t.Logf("moved %d MiB in %v (%.0f MiB/s); idle round trip %v; round trips meanwhile: %v",
		size>>20, elapsed.Round(time.Millisecond), float64(size>>20)/elapsed.Seconds(), idle, rtts)
	if len(rtts) > 0 && rtts.max() > 2*time.Second {
		t.Errorf("interactive round trip took %v during a large transfer", rtts.max())
	}
}

// A stream keeps working when the client's address changes mid-connection,
// as it does when a NAT rebinds or a laptop moves from Wi-Fi to cellular:
// the daemon validates the new path and moves the connection to it, without
// the client reconnecting.
func TestQUICSurvivesClientAddressChange(t *testing.T) {
	var relay *udpRelay
	var connects atomic.Int64
	server, client := mustIdentity(t), mustIdentity(t)
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{
		Authorized: func(fp string) bool { return fp == client.Fingerprint },
		OnConnect:  func(string, net.Addr) { connects.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go Serve(l, "test", nil, func(s Stream) {
		defer s.Close()
		io.Copy(s, s)
	})
	relay = startRelay(t, l.Addr(), relayOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, relay.addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := echoOnce(s); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		addr := relay.rebind()
		rtt, err := echoOnce(s)
		if err != nil {
			t.Fatalf("stream failed after the client's address changed to %v: %v", addr, err)
		}
		t.Logf("address change %d (now %v): first round trip %v", i+1, addr, rtt)
	}
	// A burst of data after the move arrives intact too.
	msg := make([]byte, 1<<20)
	fillPattern(msg, 0)
	go s.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatal(err)
	}
	if err := checkPattern(got, 0); err != nil {
		t.Fatal(err)
	}
	if n := connects.Load(); n != 1 {
		t.Fatalf("%d connections; the address change should not need a new one", n)
	}
}

// Packet loss slows streams down but never corrupts them: every byte arrives,
// in order. Datagrams are not retransmitted, so lost ones are gone.
func TestQUICUnderPacketLoss(t *testing.T) {
	for _, loss := range []float64{0.05, 0.20} {
		t.Run(fmt.Sprintf("%.0f%%", loss*100), func(t *testing.T) {
			var relay *udpRelay
			server, client := rawPair(t, true, func(addr string) string {
				relay = startRelay(t, addr, relayOptions{Loss: loss})
				return relay.addr()
			})

			// A stream: 1 MiB, byte-exact.
			const size = 1 << 20
			go func() {
				s, err := server.AcceptStream(context.Background())
				if err != nil {
					return
				}
				io.Copy(s, s)
				s.Close()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s, err := client.OpenStreamSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.SetDeadline(time.Now().Add(30 * time.Second))
			start := time.Now()
			go func() {
				msg := make([]byte, size)
				fillPattern(msg, 0)
				s.Write(msg)
				s.Close()
			}()
			got, err := io.ReadAll(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != size {
				t.Fatalf("echoed %d of %d bytes", len(got), size)
			}
			if err := checkPattern(got, 0); err != nil {
				t.Fatal(err)
			}
			streamTime := time.Since(start)

			// Datagrams: count how many of 500 arrive.
			const sent = 500
			received := make(chan int, 1)
			go func() {
				n := 0
				for {
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					_, err := server.ReceiveDatagram(ctx)
					cancel()
					if err != nil {
						received <- n
						return
					}
					n++
				}
			}()
			msg := make([]byte, 64)
			for range sent {
				if err := client.SendDatagram(msg); err != nil {
					t.Fatal(err)
				}
				time.Sleep(200 * time.Microsecond)
			}
			arrived := <-received
			t.Logf("%.0f%% loss (%d of %d packets lost): 1 MiB stream echoed intact in %v; %d of %d datagrams arrived (%.1f%% lost)",
				loss*100, relay.lost.Load(), relay.lost.Load()+relay.forwarded.Load(), streamTime.Round(time.Millisecond),
				arrived, sent, 100*float64(sent-arrived)/sent)
			if arrived == sent {
				t.Errorf("all %d datagrams arrived through a lossy link", sent)
			}
		})
	}
}

// rawPair connects two quic-go connections with agentd's QUIC settings,
// optionally with datagrams enabled, which agentd itself does not enable.
// The test's own handshake skips agentd's key checks, which other tests
// cover.
func rawPair(t testing.TB, datagrams bool, via func(addr string) string) (server, client *quic.Conn) {
	t.Helper()
	conf := quicConfig(DeadPeerTimeout)
	conf.EnableDatagrams = datagrams
	return rawPairWith(t, conf, via)
}

func rawPairWith(t testing.TB, conf *quic.Config, via func(addr string) string) (server, client *quic.Conn) {
	t.Helper()
	id := mustIdentity(t)
	l, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		NextProtos:   []string{ALPN},
	}, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	addr := l.Addr().String()
	if via != nil {
		addr = via(addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err = quic.DialAddr(ctx, addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		ServerName:         "agentd",
		InsecureSkipVerify: true,
	}, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseWithError(0, "") })
	server, err = l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.CloseWithError(0, "") })
	return server, client
}

// --- Benchmarks ---

// One request: open a stream, send a small request, read the reply. The
// stream is new each time, as every agent request's is.
func BenchmarkQUICStreamRequest(b *testing.B) {
	server, client := mustIdentity(b), mustIdentity(b)
	l := startEcho(b, server, client.Fingerprint)
	c, err := DialQUIC(context.Background(), l.Addr(), client, server.Fingerprint)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	b.ReportAllocs()
	for b.Loop() {
		if err := request(c, "ping"); err != nil {
			b.Fatal(err)
		}
	}
}

// The same over a Unix socket, where each request is a new connection.
func BenchmarkUnixStreamRequest(b *testing.B) {
	path := shortSocketPath(b)
	l, err := ListenUnix(path, UnixOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	go Serve(l, "test", nil, func(s Stream) {
		defer s.Close()
		data, _ := io.ReadAll(s)
		s.Write(data)
		s.CloseWrite()
	})
	b.ReportAllocs()
	for b.Loop() {
		s, err := DialUnix(path, time.Second)
		if err != nil {
			b.Fatal(err)
		}
		s.Write([]byte("ping"))
		s.CloseWrite()
		if _, err := io.ReadAll(s); err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}

func request(c *QUICClient, msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	s, err := c.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(quicTestTimeout))
	if _, err := s.Write([]byte(msg)); err != nil {
		return err
	}
	s.CloseWrite()
	reply, err := io.ReadAll(s)
	if err == nil && len(reply) != len(msg) {
		err = fmt.Errorf("reply %q to %q", reply, msg)
	}
	return err
}

// 1,000 requests on one connection from 1,000 goroutines at once. The
// daemon allows a client 256 open streams, so OpenStream waits for the rest.
func BenchmarkQUICConcurrentStreams(b *testing.B) {
	const streams = 1000
	server, client := mustIdentity(b), mustIdentity(b)
	l := startEcho(b, server, client.Fingerprint)
	c, err := DialQUIC(context.Background(), l.Addr(), client, server.Fingerprint)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	b.ReportAllocs()
	for b.Loop() {
		var wg sync.WaitGroup
		var failed atomic.Int64
		for range streams {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if request(c, "ping") != nil {
					failed.Add(1)
				}
			}()
		}
		wg.Wait()
		if n := failed.Load(); n > 0 {
			b.Fatalf("%d of %d requests failed", n, streams)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*streams), "ns/stream")
}

// Heap and goroutines per open, idle connection: N clients connect and make
// one request each. Client and daemon share this process, so the numbers
// cover both ends of each connection; each client also has its own UDP
// socket, as a CLI process does.
func BenchmarkQUICMemoryPerConnection(b *testing.B) {
	const conns = 100
	server, client := mustIdentity(b), mustIdentity(b)
	l := startEcho(b, server, client.Fingerprint)
	var heap, goroutines float64
	for b.Loop() {
		heapBefore, goroutinesBefore := heapInUse(), runtime.NumGoroutine()
		var open []*QUICClient
		for range conns {
			c, err := DialQUIC(context.Background(), l.Addr(), client, server.Fingerprint)
			if err != nil {
				b.Fatal(err)
			}
			if err := request(c, "ping"); err != nil {
				b.Fatal(err)
			}
			open = append(open, c)
		}
		time.Sleep(100 * time.Millisecond) // let finished streams be released
		heap += float64(heapInUse()-heapBefore) / conns
		goroutines += float64(runtime.NumGoroutine()-goroutinesBefore) / conns
		for _, c := range open {
			c.Close()
		}
		time.Sleep(100 * time.Millisecond) // let the daemon see them close
	}
	b.ReportMetric(heap/float64(b.N), "heap-B/conn")
	b.ReportMetric(goroutines/float64(b.N), "goroutines/conn")
}

// Heap per open stream: one connection holds N streams open, each with a
// daemon handler waiting to read from it, as attachments do. The handler
// goroutines' stacks are counted separately from the heap.
func BenchmarkQUICMemoryPerStream(b *testing.B) {
	const streams = 200
	c, m := dialMux(b, nil)
	var heap, stack, goroutines float64
	for b.Loop() {
		heapBefore, stackBefore, goroutinesBefore := heapInUse(), stackInUse(), runtime.NumGoroutine()
		active := m.active.Load()
		var open []Stream
		for range streams {
			ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
			s, err := c.OpenStream(ctx)
			cancel()
			if err != nil {
				b.Fatal(err)
			}
			s.Write([]byte{roleIdle})
			open = append(open, s)
		}
		for m.active.Load() < active+streams {
			time.Sleep(time.Millisecond)
		}
		heap += float64(heapInUse()-heapBefore) / streams
		stack += float64(stackInUse()-stackBefore) / streams
		goroutines += float64(runtime.NumGoroutine()-goroutinesBefore) / streams
		for _, s := range open {
			s.Close()
		}
		for m.active.Load() > active {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // let finished streams be released
	}
	b.ReportMetric(heap/float64(b.N), "heap-B/stream")
	b.ReportMetric(stack/float64(b.N), "stack-B/stream")
	b.ReportMetric(goroutines/float64(b.N), "goroutines/stream")
}

func heapInUse() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

func stackInUse() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.StackInuse)
}

// CPU per MiB moved over one stream: QUIC over loopback (encryption, packet
// handling and acknowledgements, both ends) against a Unix socket. cpu-ms/MiB
// is the process's user and system time, both ends included.
func BenchmarkTransferCPU(b *testing.B) {
	b.Run("quic", func(b *testing.B) {
		c, _ := dialMux(b, nil)
		benchmarkTransfer(b, openRole(b, c, roleSink))
	})
	b.Run("unix", func(b *testing.B) {
		path := shortSocketPath(b)
		l, err := ListenUnix(path, UnixOptions{})
		if err != nil {
			b.Fatal(err)
		}
		defer l.Close()
		go Serve(l, "test", nil, func(s Stream) {
			defer s.Close()
			io.Copy(io.Discard, s)
		})
		s, err := DialUnix(path, time.Second)
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		benchmarkTransfer(b, s)
	})
}

func benchmarkTransfer(b *testing.B, s Stream) {
	buf := make([]byte, 32<<10)
	b.SetBytes(1 << 20)
	cpu := processCPU()
	for b.Loop() {
		for range (1 << 20) / len(buf) {
			if _, err := s.Write(buf); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64((processCPU()-cpu).Microseconds())/1000/float64(b.N), "cpu-ms/MiB")
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Throughput of one 64 MiB transfer at 20 and 100 ms round-trip times, for
// several stream and connection window limits: the window must cover the
// bandwidth-delay product, or the transfer waits on window updates. agentd's
// own limits are the "agentd" case.
func BenchmarkQUICWindowAtRTT(b *testing.B) {
	const size = 64 << 20
	agentd := quicConfig(DeadPeerTimeout)
	for _, rtt := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond} {
		for _, w := range []struct {
			name         string
			stream, conn uint64
		}{
			{"stream=1MiB", 1 << 20, 4 << 20},
			{"agentd", agentd.MaxStreamReceiveWindow, agentd.MaxConnectionReceiveWindow},
			{"stream=16MiB", 16 << 20, 32 << 20},
		} {
			b.Run(fmt.Sprintf("rtt=%v/%s", rtt, w.name), func(b *testing.B) {
				benchmarkWindow(b, size, rtt, w.stream, w.conn)
			})
		}
	}
}

func benchmarkWindow(b *testing.B, size int, rtt time.Duration, stream, conn uint64) {
	conf := quicConfig(DeadPeerTimeout)
	conf.MaxStreamReceiveWindow = stream
	conf.MaxConnectionReceiveWindow = conn
	server, client := rawPairWith(b, conf, func(addr string) string {
		return startRelay(b, addr, relayOptions{Delay: rtt / 2}).addr()
	})
	go func() {
		for {
			s, err := server.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				io.Copy(io.Discard, s)
				s.Close()
			}()
		}
	}()
	buf := make([]byte, 64<<10)
	b.SetBytes(int64(size))
	for b.Loop() {
		s, err := client.OpenStreamSync(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		for range size / len(buf) {
			if _, err := s.Write(buf); err != nil {
				b.Fatal(err)
			}
		}
		s.Close()
		// Wait for the daemon's side to finish reading.
		s.SetReadDeadline(time.Now().Add(time.Minute))
		if _, err := io.ReadAll(s); err != nil {
			b.Fatal(err)
		}
	}
}

// Small-message round trips and one-way rates, over a stream and as
// datagrams (enabled here only; agentd does not enable them).
func BenchmarkQUICSmallMessages(b *testing.B) {
	const size = 64
	b.Run("stream/round-trip", func(b *testing.B) {
		server, client := rawPair(b, true, nil)
		go func() {
			s, err := server.AcceptStream(context.Background())
			if err == nil {
				io.Copy(s, s)
			}
		}()
		s, err := client.OpenStreamSync(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		msg := make([]byte, size)
		for b.Loop() {
			s.Write(msg)
			if _, err := io.ReadFull(s, msg); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("datagram/round-trip", func(b *testing.B) {
		server, client := rawPair(b, true, nil)
		go func() {
			for {
				p, err := server.ReceiveDatagram(context.Background())
				if err != nil {
					return
				}
				server.SendDatagram(p)
			}
		}()
		msg := make([]byte, size)
		lost := 0
		for b.Loop() {
			if err := client.SendDatagram(msg); err != nil {
				b.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_, err := client.ReceiveDatagram(ctx)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				lost++
			} else if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(lost), "lost")
	})
	b.Run("stream/one-way", func(b *testing.B) {
		server, client := rawPair(b, true, nil)
		received := make(chan int64, 1)
		go func() {
			s, err := server.AcceptStream(context.Background())
			if err != nil {
				return
			}
			n, _ := io.Copy(io.Discard, s)
			received <- n
		}()
		s, err := client.OpenStreamSync(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		msg := make([]byte, size)
		for b.Loop() {
			if _, err := s.Write(msg); err != nil {
				b.Fatal(err)
			}
		}
		s.Close()
		if n := <-received; n != int64(b.N*size) {
			b.Fatalf("received %d of %d bytes", n, b.N*size)
		}
	})
	b.Run("datagram/one-way", func(b *testing.B) {
		server, client := rawPair(b, true, nil)
		var received atomic.Int64
		go func() {
			for {
				if _, err := server.ReceiveDatagram(context.Background()); err != nil {
					return
				}
				received.Add(1)
			}
		}()
		msg := make([]byte, size)
		for b.Loop() {
			if err := client.SendDatagram(msg); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		time.Sleep(200 * time.Millisecond)
		b.ReportMetric(100*float64(int64(b.N)-received.Load())/float64(b.N), "%lost")
	})
}
