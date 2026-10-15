package daemon

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robmorgan/agtd/internal/protocol"
	"github.com/robmorgan/agtd/internal/session"
	"github.com/robmorgan/agtd/internal/transport"
)

// hostEvents reads the host events straight from the database, since a
// test's remote client may since have been revoked.
func hostEvents(h *harness) []session.Event {
	h.t.Helper()
	all, err := h.srv.db.EventsAfter(0, "", 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []session.Event
	for _, ev := range all {
		if ev.SessionID == "" {
			out = append(out, ev)
		}
	}
	return out
}

// Value: protects=a record of someone guessing pairing codes, with where
// the guesses came from; fails_when=servePair stops recording
// ErrTooManyWrongCodes, or the window's result loses the guesser's
// address; why_new=wrong codes were only counted, never recorded;
// seam=none
func TestTooManyWrongPairingCodesAreRecorded(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	code, done := startPair(h, ctx)
	wrong := "000-000-000"
	if code == wrong {
		wrong = "000-000-001"
	}
	stranger := newClientIdentity(t)
	for range transport.PairingAttempts {
		if _, err := transport.Pair(ctx, h.srv.RemoteAddr(), stranger, wrong, "guess"); err == nil {
			t.Fatal("wrong code accepted")
		}
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "wrong codes") {
		t.Fatalf("pair = %v", err)
	}
	evs := hostEvents(h)
	if kinds(evs) != "pair_failed" || evs[0].Attention != session.AttentionNotice ||
		!strings.HasSuffix(evs[0].Summary, "from 127.0.0.1") {
		t.Fatalf("host events = %+v", evs)
	}
}

// Value: protects=remote connections on record without one per command,
// and a revoked key's live connection being dropped on record;
// fails_when=OnConnect/OnRevoked stop recording, the dedupe keys on the
// port (every connection has a new one) or never expires, or OnRevoked
// fires for each place that notices the revocation; why_new=connections
// were only logged to stderr; seam=connectAudit.now, so the interval
// passes without sleeping
func TestRemoteConnectionsAreRecorded(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	var elapsed atomic.Int64
	start := time.Now()
	h.srv.connects.mu.Lock()
	h.srv.connects.now = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	h.srv.connects.mu.Unlock()
	client := newClientIdentity(t)
	if _, err := transport.Authorize(h.paths.AuthorizedClientsPath(), client.Fingerprint, "laptop"); err != nil {
		t.Fatal(err)
	}

	// Every command connects anew; within the interval, one is recorded.
	for range 2 {
		if !remoteRequestWorks(h, client) {
			t.Fatal("authorized client was refused")
		}
	}
	evs := hostEvents(h)
	if kinds(evs) != "connected" || evs[0].Attention != session.AttentionInfo ||
		evs[0].Summary != "laptop ("+client.Fingerprint+") connected from 127.0.0.1" {
		t.Fatalf("host events = %+v", evs)
	}
	elapsed.Store(int64(connectAuditInterval))
	if !remoteRequestWorks(h, client) {
		t.Fatal("authorized client was refused")
	}
	if got := kinds(hostEvents(h)); got != "connected connected" {
		t.Fatalf("after the interval: %s", got)
	}

	// Revoking the key drops its live connection, on record once.
	if _, err := h.dialRemote(client); err != nil {
		t.Fatal(err)
	}
	// A request served shows the daemon has the connection; the dial can
	// return before the daemon's side of the handshake is done.
	if resp := h.request(&protocol.Request{ListSessions: protocol.Empty}); resp.Sessions == nil {
		t.Fatalf("list over QUIC = %#v", resp)
	}
	if _, err := transport.Revoke(h.paths.AuthorizedClientsPath(), client.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if conn, err := h.dialer(); err == nil {
		// Opening a stream is when the daemon notices; the request fails.
		protocol.WriteRequest(conn, &protocol.Request{ListSessions: protocol.Empty})
		conn.Close()
	}
	h.eventually("a disconnected event", func() bool {
		return strings.HasSuffix(kinds(hostEvents(h)), "disconnected")
	})
	evs = hostEvents(h)
	if last := evs[len(evs)-1]; last.Summary != client.Fingerprint+" at 127.0.0.1 disconnected: key revoked" {
		t.Fatalf("disconnected event = %+v", last)
	}
	if got := kinds(evs); got != "connected connected disconnected" {
		t.Fatalf("host events: %s", got)
	}
}
