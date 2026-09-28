package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
			if reply, err := io.ReadAll(s); err == nil {
				t.Fatalf("unauthorized client got a reply: %q", reply)
			}
		}
	}
}

func TestQUICClientRefusesUnpinnedDaemon(t *testing.T) {
	server, impostorPin, client := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	l := startEcho(t, server, client.Fingerprint)
	ctx, cancel := context.WithTimeout(context.Background(), quicTestTimeout)
	defer cancel()
	_, err := DialQUIC(ctx, l.Addr(), client, impostorPin.Fingerprint)
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned key") {
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
