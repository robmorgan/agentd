package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

const quicTestTimeout = 10 * time.Second

// startEcho runs a QUIC listener whose streams echo their input back,
// upper-cased, once the client half-closes.
func startEcho(t *testing.T, server *Identity, authorized ...string) *QUICListener {
	t.Helper()
	allowed := map[string]bool{}
	for _, fp := range authorized {
		allowed[fp] = true
	}
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{Authorized: func(fp string) bool { return allowed[fp] }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go Serve(l, "test", nil, func(s Stream) {
		defer s.Close()
		data, err := io.ReadAll(s)
		if err != nil {
			return
		}
		s.Write([]byte(strings.ToUpper(string(data))))
		s.CloseWrite()
	})
	return l
}

func mustIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func roundTrip(t *testing.T, c *QUICClient, msg string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(quicTestTimeout))
	if _, err := s.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(reply)
}

func TestQUICAuthorizedClientRoundTrips(t *testing.T) {
	server, client := mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, client.Fingerprint)
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Several requests share one connection, one stream each.
	for _, msg := range []string{"one", "two", "three"} {
		if got := roundTrip(t, c, msg); got != strings.ToUpper(msg) {
			t.Fatalf("got %q for %q", got, msg)
		}
	}
}

func TestQUICRejectsUnauthorizedClient(t *testing.T) {
	server, stranger := mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, mustIdentity(t).Fingerprint)
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, l.Addr(), stranger, server.Fingerprint)
	if err == nil {
		// With TLS 1.3 the client may finish its side of the handshake
		// before the server rejects its certificate; the connection must
		// then be unusable.
		defer c.Close()
		s, err := c.OpenStream(ctx)
		if err == nil {
			s.SetDeadline(time.Now().Add(quicTestTimeout))
			s.Write([]byte("hi"))
			s.CloseWrite()
			reply, err := io.ReadAll(s)
			if err == nil {
				t.Fatalf("unauthorized client got a reply: %q", reply)
			}
			if !IsKeyRefused(err) {
				t.Fatalf("refusal on the stream is not recognised: %v", err)
			}
		}
	} else if !IsKeyRefused(err) {
		t.Fatalf("refusal at dial is not recognised: %v", err)
	}
}

// host add learns the daemon's key before this client is authorized.
func TestProbeLearnsTheKeyBeforeAuthorization(t *testing.T) {
	server, stranger := mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server)
	fp, err := ProbeFingerprint(context.Background(), l.Addr(), stranger)
	if err != nil || fp != server.Fingerprint {
		t.Fatalf("probe = %q, %v; want %s", fp, err, server.Fingerprint)
	}
}

// localhost resolving to ::1 first, with only 127.0.0.1 listening, still
// connects.
func TestDialTriesEveryResolvedAddress(t *testing.T) {
	ips, err := net.DefaultResolver.LookupHost(context.Background(), "localhost")
	if err != nil || len(ips) < 2 {
		t.Skipf("localhost resolves to %v (%v)", ips, err)
	}
	server, client := mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, client.Fingerprint)
	_, port, _ := net.SplitHostPort(l.Addr())
	c, err := DialQUIC(context.Background(), net.JoinHostPort("localhost", port), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "hi"); got != "HI" {
		t.Fatalf("got %q", got)
	}
}

func TestQUICClientRefusesUnpinnedDaemon(t *testing.T) {
	server, impostorPin, client := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, client.Fingerprint)
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	_, err := DialQUIC(ctx, l.Addr(), client, impostorPin.Fingerprint)
	var changed *KeyChangedError
	if !errors.As(err, &changed) || changed.Presented != server.Fingerprint || changed.Pinned != impostorPin.Fingerprint {
		t.Fatalf("dial to an unpinned daemon: %v", err)
	}
}

// A long-lived stream (like an attachment) must not hold up short request
// streams on the same connection.
func TestQUICStreamsAreIndependent(t *testing.T) {
	server, client := mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, client.Fingerprint)
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	long, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer long.Close()
	long.Write([]byte("still open")) // never half-closed

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := strings.Repeat("x", i+1)
			if got := roundTrip(t, c, msg); got != strings.ToUpper(msg) {
				t.Errorf("got %q", got)
			}
		}()
	}
	wg.Wait()
}

func TestQUICCloseEndsAcceptAndConnections(t *testing.T) {
	server, client := mustIdentity(t), mustIdentity(t)
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{Authorized: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("hello"))
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()

	l.Close()
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v", err)
	}
	s.SetReadDeadline(time.Now().Add(quicTestTimeout))
	if _, err := io.ReadAll(s); err == nil {
		t.Fatal("client stream survived the listener closing")
	}
}

func TestIdentityPersistsAndFingerprintIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote", "daemon.key")
	first, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", info.Mode(), err)
	}
	// Loading regenerates the certificate but keeps the key.
	second, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint || !ValidFingerprint(first.Fingerprint) {
		t.Fatalf("fingerprint changed: %s vs %s", first.Fingerprint, second.Fingerprint)
	}
	if other := mustIdentity(t); other.Fingerprint == first.Fingerprint {
		t.Fatal("two keys share a fingerprint")
	}
}

func TestAuthorizedClientsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_clients")
	a, b := mustIdentity(t).Fingerprint, mustIdentity(t).Fingerprint
	if ok, _ := IsAuthorized(path, a); ok {
		t.Fatal("missing file authorized a client")
	}
	if _, err := Authorize(path, "not-a-fingerprint", ""); err == nil {
		t.Fatal("malformed fingerprint accepted")
	}
	if added, err := Authorize(path, a, "laptop"); err != nil || !added {
		t.Fatalf("authorize a: %v %v", added, err)
	}
	if added, _ := Authorize(path, a, "again"); added {
		t.Fatal("duplicate authorization added")
	}
	Authorize(path, b, "")
	clients, err := ReadAuthorized(path)
	if err != nil || len(clients) != 2 || clients[0].Name != "laptop" {
		t.Fatalf("clients = %+v, %v", clients, err)
	}
	if removed, _ := Revoke(path, a); !removed {
		t.Fatal("revoke a")
	}
	if ok, _ := IsAuthorized(path, a); ok {
		t.Fatal("revoked client still authorized")
	}
	if ok, _ := IsAuthorized(path, b); !ok {
		t.Fatal("revoking a dropped b")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_clients mode = %v", info.Mode())
	}
}

// A fixed key and its fingerprint. The fingerprint is what users compare and
// what hosts.toml and authorized_clients store, so it must never change.
const (
	parityKeyPEM = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIBjksdA/xBFa67gw4s1UxuZHtUs8lCcbF6PTgueUIoCc
-----END PRIVATE KEY-----
`
	parityFingerprint = "SHA256:AOUfC64ic5/SwRe6zJIVSbIBIuiehNsWrqsT/Af5gtY"
)

// The fingerprint of a fixed key never changes: existing hosts.toml pins
// and authorized_clients entries depend on it.
func TestFingerprintIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parity.key")
	if err := os.WriteFile(path, []byte(parityKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.Fingerprint != parityFingerprint {
		t.Fatalf("fingerprint = %s, want %s", id.Fingerprint, parityFingerprint)
	}
}

// quic-go's default first datagram (1280 bytes of UDP payload) does not fit
// through a Tailscale interface (MTU 1280 including IP and UDP headers), and
// the handshake then times out; see quicConfig.
func TestFirstDatagramFitsTailscaleMTU(t *testing.T) {
	const tailscaleMTU, ipv6UDPHeaders = 1280, 48
	if size := int(quicConfig(DeadPeerTimeout).InitialPacketSize); size == 0 || size+ipv6UDPHeaders > tailscaleMTU {
		t.Fatalf("InitialPacketSize %d does not fit a %d-byte MTU", size, tailscaleMTU)
	}
}

// Revoking a key ends that client's open connections, attachments
// included, and it cannot come back through session resumption either.
func TestQUICRevokeEndsLiveConnections(t *testing.T) {
	server, client, other := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	var mu sync.Mutex
	allowed := map[string]bool{client.Fingerprint: true, other.Fingerprint: true}
	authorized := func(fp string) bool { mu.Lock(); defer mu.Unlock(); return allowed[fp] }
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{Authorized: authorized, RecheckInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go Serve(l, "test", nil, func(s Stream) {
		defer s.Close()
		io.Copy(s, s) // an attachment: open until either side ends it
	})

	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	c, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stay, err := DialQUIC(ctx, l.Addr(), other, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer stay.Close()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(quicTestTimeout))
	s.Write([]byte("x"))
	if _, err := io.ReadFull(s, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	delete(allowed, client.Fingerprint)
	mu.Unlock()
	if _, err := io.ReadAll(s); err == nil {
		t.Fatal("revoked client's attachment stayed open")
	}
	// With TLS 1.3 the client may finish its side of the handshake before
	// the refusal arrives; the connection must then be unusable.
	if again, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint); err == nil {
		defer again.Close()
		if roundTripErr(ctx, again) == nil {
			t.Fatal("revoked client reconnected")
		}
	}
	// Other clients are untouched.
	s2, err := stay.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2.SetDeadline(time.Now().Add(quicTestTimeout))
	s2.Write([]byte("y"))
	if _, err := io.ReadFull(s2, make([]byte, 1)); err != nil {
		t.Fatalf("authorized client lost its connection: %v", err)
	}
}

func roundTripErr(ctx context.Context, c *QUICClient) error {
	s, err := c.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(2 * time.Second))
	s.Write([]byte("z"))
	_, err = io.ReadFull(s, make([]byte, 1))
	return err
}

// Concurrent first use publishes one key that everyone ends up using.
func TestConcurrentIdentityCreationAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote", "daemon.key")
	const n = 8
	fps := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := LoadOrCreateIdentity(path)
			if err != nil {
				t.Error(err)
				return
			}
			fps <- id.Fingerprint
		}()
	}
	wg.Wait()
	close(fps)
	onDisk, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	for fp := range fps {
		if fp != onDisk.Fingerprint {
			t.Fatalf("a process uses %s but the key on disk is %s", fp, onDisk.Fingerprint)
		}
	}
}

// Concurrent authorize and revoke commands never lose each other's changes.
func TestConcurrentAuthorizedEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_clients")
	var fps []string
	for range 10 {
		fps = append(fps, mustIdentity(t).Fingerprint)
	}
	var wg sync.WaitGroup
	for _, fp := range fps {
		wg.Add(1)
		go func() { defer wg.Done(); Authorize(path, fp, "") }()
	}
	wg.Wait()
	if clients, _ := ReadAuthorized(path); len(clients) != len(fps) {
		t.Fatalf("%d of %d authorizations kept", len(clients), len(fps))
	}
	for _, fp := range fps[:5] {
		wg.Add(1)
		go func() { defer wg.Done(); Revoke(path, fp) }()
	}
	wg.Wait()
	clients, _ := ReadAuthorized(path)
	if len(clients) != 5 {
		t.Fatalf("after 5 concurrent revokes, %d clients remain", len(clients))
	}
	for _, c := range clients {
		for _, gone := range fps[:5] {
			if c.Fingerprint == gone {
				t.Fatalf("revoked %s came back", gone)
			}
		}
	}
}

// A client that goes silent without closing its connection (a laptop lid
// shut, a network dropped) is noticed within the idle timeout, which ends
// its attachment on the daemon; one that is merely idle is kept alive by
// pings.
func TestQUICNoticesSilentClient(t *testing.T) {
	server, client := mustIdentity(t), mustIdentity(t)
	const idle = time.Second
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{
		Authorized:  func(fp string) bool { return fp == client.Fingerprint },
		IdleTimeout: idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ended := make(chan error, 1)
	go Serve(l, "test", nil, func(s Stream) {
		defer s.Close()
		_, err := io.Copy(s, s) // an attachment: open until either side ends it
		ended <- err
	})
	relay := startRelay(t, l.Addr())

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
	s.SetDeadline(time.Now().Add(quicTestTimeout))
	echo := func() error {
		if _, err := s.Write([]byte("x")); err != nil {
			return err
		}
		_, err := io.ReadFull(s, make([]byte, 1))
		return err
	}
	if err := echo(); err != nil {
		t.Fatal(err)
	}

	// Idle for longer than the timeout: pings keep the attachment open.
	time.Sleep(3 * idle)
	select {
	case err := <-ended:
		t.Fatalf("idle attachment was closed: %v", err)
	default:
	}
	if err := echo(); err != nil {
		t.Fatalf("idle attachment stopped working: %v", err)
	}

	// Silence the client without it closing anything.
	relay.drop.Store(true)
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("attachment ended cleanly; want a connection error")
		}
	case <-time.After(5 * idle):
		t.Fatal("daemon did not notice the silent client")
	}
}

// udpRelay forwards datagrams between one client and a server until drop
// is set, after which it silently discards them in both directions, as a
// dead network would.
type udpRelay struct {
	front *net.UDPConn
	drop  atomic.Bool
}

func (r *udpRelay) addr() string { return r.front.LocalAddr().String() }

func startRelay(t *testing.T, serverAddr string) *udpRelay {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	raddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	back, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { front.Close(); back.Close() })
	r := &udpRelay{front: front}
	var mu sync.Mutex
	var clientAddr *net.UDPAddr
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := front.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			clientAddr = from
			mu.Unlock()
			if !r.drop.Load() {
				back.Write(buf[:n])
			}
		}
	}()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := back.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			to := clientAddr
			mu.Unlock()
			if to != nil && !r.drop.Load() {
				front.WriteToUDP(buf[:n], to)
			}
		}
	}()
	return r
}

// Only failures a new connection may get past count as a lost connection.
func TestIsConnectionLost(t *testing.T) {
	for _, c := range []struct {
		err  error
		lost bool
	}{
		{nil, false},
		{errors.New("unexpected response"), false},
		{&quic.IdleTimeoutError{}, true},
		{&quic.HandshakeTimeoutError{}, true},
		{&quic.StatelessResetError{}, true},
		{fmt.Errorf("could not reach agentd: %w", &net.DNSError{Err: "no such host"}), true},
		{fmt.Errorf("dial: %w", context.DeadlineExceeded), true},
		{&quic.ApplicationError{Remote: true, ErrorMessage: "agentd is shutting down"}, true},
		// This client closed it.
		{&quic.ApplicationError{Remote: false}, false},
		// The daemon refused this client's key.
		{&quic.TransportError{Remote: true, ErrorCode: quic.TransportErrorCode(0x100 + 42)}, false},
	} {
		if got := IsConnectionLost(c.err); got != c.lost {
			t.Errorf("IsConnectionLost(%v) = %v, want %v", c.err, got, c.lost)
		}
	}
}

// The listener reports each open connection with its stream counts and
// QUIC's own statistics.
func TestQUICConnectionInfo(t *testing.T) {
	server, client := mustIdentity(t), mustIdentity(t)
	l, err := ListenQUIC("127.0.0.1:0", server, QUICOptions{Authorized: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	qc, err := DialQUIC(ctx, l.Addr(), client, server.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer qc.Close()
	streams := make([]Stream, 3)
	for i := range streams {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := qc.OpenStream(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		s.Write([]byte("x")) // a stream reaches the daemon with its first byte
		streams[i] = s
	}
	accepted := make([]Stream, 3)
	for i := range accepted {
		s, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepted[i] = s
	}
	accepted[0].Close()
	conns := l.Connections()
	if len(conns) != 1 {
		t.Fatalf("connections = %#v", conns)
	}
	c := conns[0]
	if c.Fingerprint != client.Fingerprint || c.StreamsOpened != 3 || c.StreamsOpen != 2 || c.Remote == "" ||
		c.BytesReceived == 0 || c.BytesSent == 0 || c.RTT <= 0 || time.Since(c.ConnectedAt) > time.Minute {
		t.Fatalf("connection info = %#v", c)
	}
	accepted[0].Close() // closing twice counts once
	if got := l.Connections()[0].StreamsOpen; got != 2 {
		t.Fatalf("open streams after double close = %d", got)
	}
}
