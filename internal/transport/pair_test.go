package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/robmorgan/agentd/internal/protocol"
)

// pairingHarness is an echo listener whose Authorized check is a set that a
// successful pairing adds to.
type pairingHarness struct {
	server  *Identity
	l       *QUICListener
	pairing *Pairing

	mu         sync.Mutex
	authorized map[string]string
}

func startPairing(t *testing.T) *pairingHarness {
	t.Helper()
	h := &pairingHarness{server: mustIdentity(t), pairing: NewPairing("devbox"), authorized: map[string]string{}}
	l, err := ListenQUIC("127.0.0.1:0", h.server, QUICOptions{
		Authorized: func(fp string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			_, ok := h.authorized[fp]
			return ok
		},
		Pairing: h.pairing,
	})
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
	h.l = l
	return h
}

func (h *pairingHarness) open(t *testing.T, ttl time.Duration) *PairingWindow {
	t.Helper()
	w, err := h.pairing.Open(ttl, func(fp, name string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.authorized[fp] = name
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func pairCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestPairingAuthorizesTheClientAndPinsTheDaemon(t *testing.T) {
	h := startPairing(t)
	w := h.open(t, time.Minute)
	client := mustIdentity(t)

	// Typed with spaces instead of dashes.
	paired, err := Pair(pairCtx(t), h.l.Addr(), client, strings.ReplaceAll(w.Code, "-", " "), "laptop\nevil")
	if err != nil {
		t.Fatal(err)
	}
	if paired.Fingerprint != h.server.Fingerprint || paired.Host != "devbox" {
		t.Fatalf("paired with %+v", paired)
	}
	select {
	case <-w.Done():
	case <-time.After(quicTestTimeout):
		t.Fatal("window still open")
	}
	r := w.Result()
	// The name is one line, so it cannot add a line to the authorized file.
	if r.Err != nil || r.Fingerprint != client.Fingerprint || r.Name != "laptop?evil" {
		t.Fatalf("result = %+v", r)
	}

	// The client now connects normally, under the key it paired with.
	c, err := DialQUIC(pairCtx(t), h.l.Addr(), client, paired.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "hi"); got != "HI" {
		t.Fatalf("got %q", got)
	}

	// The window is closed: the same code does not work twice.
	if _, err := Pair(pairCtx(t), h.l.Addr(), mustIdentity(t), w.Code, "other"); !errors.Is(err, ErrNotPairing) {
		t.Fatalf("second pairing: %v", err)
	}
}

func TestPairingWithoutAWindowIsRefused(t *testing.T) {
	h := startPairing(t)
	_, err := Pair(pairCtx(t), h.l.Addr(), mustIdentity(t), "123-456-789", "laptop")
	if !errors.Is(err, ErrNotPairing) {
		t.Fatalf("err = %v", err)
	}
}

// A daemon that does not offer pairing at all (an older one) refuses the
// same way.
func TestPairingWithADaemonThatDoesNotPair(t *testing.T) {
	l := startEcho(t, mustIdentity(t))
	_, err := Pair(pairCtx(t), l.Addr(), mustIdentity(t), "123-456-789", "laptop")
	if !errors.Is(err, ErrNotPairing) {
		t.Fatalf("err = %v", err)
	}
}

func TestPairingClosesAfterTooManyWrongCodes(t *testing.T) {
	h := startPairing(t)
	w := h.open(t, time.Minute)
	wrong := "000-000-000"
	if w.Code == wrong {
		wrong = "000-000-001"
	}
	stranger := mustIdentity(t)
	for i := 1; i <= PairingAttempts; i++ {
		_, err := Pair(pairCtx(t), h.l.Addr(), stranger, wrong, "guess")
		var refused *PairRefusedError
		if !errors.As(err, &refused) || !strings.Contains(refused.Message, "wrong pairing code") {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	<-w.Done()
	if r := w.Result(); r.Err == nil || !strings.Contains(r.Err.Error(), "wrong codes") {
		t.Fatalf("result = %+v", r)
	}
	// Even the right code is too late now.
	if _, err := Pair(pairCtx(t), h.l.Addr(), stranger, w.Code, "guess"); !errors.Is(err, ErrNotPairing) {
		t.Fatalf("right code after closing: %v", err)
	}
	if len(h.authorized) != 0 {
		t.Fatalf("authorized %v", h.authorized)
	}
}

func TestPairingWindowExpiresAndCloses(t *testing.T) {
	h := startPairing(t)
	w := h.open(t, 50*time.Millisecond)
	<-w.Done()
	if r := w.Result(); !errors.Is(r.Err, errPairingExpired) {
		t.Fatalf("result = %+v", r)
	}
	if _, err := Pair(pairCtx(t), h.l.Addr(), mustIdentity(t), w.Code, "late"); !errors.Is(err, ErrNotPairing) {
		t.Fatalf("after expiry: %v", err)
	}

	// Only one window at a time; a closed one makes room for the next.
	w2 := h.open(t, time.Minute)
	if _, err := h.pairing.Open(time.Minute, func(string, string) error { return nil }); err == nil {
		t.Fatal("opened a second window")
	}
	w2.Close()
	<-w2.Done()
	if r := w2.Result(); !errors.Is(r.Err, errPairingClosed) {
		t.Fatalf("result = %+v", r)
	}
	h.open(t, time.Minute)
}

// A pairing connection carries the pairing exchange and nothing else: its
// client cannot reach the agent protocol, even while a window is open.
func TestPairingConnectionServesNoAgentStreams(t *testing.T) {
	h := startPairing(t)
	h.open(t, time.Minute)
	stranger := mustIdentity(t)
	conn, _, err := dialEach(pairCtx(t), h.l.Addr(), stranger, "", PairALPN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	s, err := conn.OpenStreamSync(pairCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(quicTestTimeout))
	// Not a pairing message: the echo handler would upper-case it.
	s.Write([]byte("hello"))
	s.Close()
	if reply, err := io.ReadAll(s); err == nil && len(reply) > 0 {
		t.Fatalf("pairing connection got %q", reply)
	}
	if len(h.l.Connections()) != 0 {
		t.Fatalf("pairing connection listed as a client: %+v", h.l.Connections())
	}
}

// A daemon impersonated by a machine in the middle cannot complete the
// exchange without the code: it can only relay, and the exchange is bound to
// each TLS connection, so the relayed proofs do not match.
func TestPairingRefusesARelayedExchange(t *testing.T) {
	h := startPairing(t)
	w := h.open(t, time.Minute)
	client := mustIdentity(t)
	middle := startRelayDaemon(t, h.l.Addr())
	_, err := Pair(pairCtx(t), middle, client, w.Code, "laptop")
	var refused *PairRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Message, "wrong pairing code") {
		t.Fatalf("pairing through a relay: %v", err)
	}
	if len(h.authorized) != 0 {
		t.Fatalf("authorized %v", h.authorized)
	}
}

// startRelayDaemon listens for pairing connections under its own key and
// relays each pairing message, unchanged, to the daemon at target over a
// pairing connection of its own: a machine in the middle that does not know
// the code.
func startRelayDaemon(t *testing.T, target string) string {
	t.Helper()
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{mustIdentity(t).cert},
		NextProtos:   []string{PairALPN},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	l, err := quic.ListenAddr("127.0.0.1:0", tlsConf, quicConfig(DeadPeerTimeout))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	upID := mustIdentity(t)
	go func() {
		for {
			conn, err := l.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				defer conn.CloseWithError(0, "")
				down, err := conn.AcceptStream(context.Background())
				if err != nil {
					return
				}
				upConn, _, err := dialEach(context.Background(), target, upID, "", PairALPN)
				if err != nil {
					return
				}
				defer upConn.CloseWithError(0, "")
				up, err := upConn.OpenStreamSync(context.Background())
				if err != nil {
					return
				}
				for {
					m, err := protocol.ReadPairMessage(down)
					if err != nil || protocol.WritePairMessage(up, m) != nil {
						return
					}
					m, err = protocol.ReadPairMessage(up)
					if err != nil || protocol.WritePairMessage(down, m) != nil {
						return
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

func TestPairingCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		code, err := NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 11 || code[3] != '-' || code[7] != '-' {
			t.Fatalf("code %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 19 {
		t.Fatalf("codes repeat: %v", seen)
	}
	for in, want := range map[string]string{"123-456-789": "123456789", "123 456 789": "123456789", "123456789": "123456789"} {
		if got, err := NormalizePairingCode(in); err != nil || got != want {
			t.Errorf("NormalizePairingCode(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "12-345", "123-456-7890", "abc-def-ghi", "SHA256:x"} {
		if _, err := NormalizePairingCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
