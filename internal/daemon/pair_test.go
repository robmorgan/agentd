package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// startPair runs `agentd remote pair` against the harness daemon and
// returns the code it shows and where its result will arrive.
func startPair(h *harness, ctx context.Context) (string, <-chan error) {
	h.t.Helper()
	h.eventually("QUIC listener", func() bool { return h.srv.RemoteAddr() != "" })
	codes := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		paired, err := Pair(ctx, h.paths, func(w *protocol.ManagementPairing) { codes <- w.Code })
		if err == nil && paired.Name != "laptop" {
			err = errors.New("paired with " + paired.Name)
		}
		done <- err
	}()
	select {
	case code := <-codes:
		return code, done
	case err := <-done:
		h.t.Fatalf("pair: %v", err)
	}
	return "", nil
}

func TestPairingAuthorizesARemoteClient(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	client := newClientIdentity(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	code, done := startPair(h, ctx)

	paired, err := transport.Pair(ctx, h.srv.RemoteAddr(), client, code, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if paired.Fingerprint != h.srv.daemonID {
		t.Fatalf("paired with key %s, want %s", paired.Fingerprint, h.srv.daemonID)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	clients, _ := transport.ReadAuthorized(h.paths.AuthorizedClientsPath())
	if len(clients) != 1 || clients[0].Fingerprint != client.Fingerprint || clients[0].Name != "laptop" {
		t.Fatalf("authorized clients = %+v", clients)
	}
	if !remoteRequestWorks(h, client) {
		t.Fatal("paired client was refused")
	}
}

// Interrupting `agentd remote pair` closes its window at once.
func TestCancellingPairClosesTheWindow(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	ctx, cancel := context.WithCancel(context.Background())
	code, done := startPair(h, ctx)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("pair after cancel: %v", err)
	}
	h.eventually("window closed", func() bool {
		_, err := transport.Pair(context.Background(), h.srv.RemoteAddr(), newClientIdentity(t), code, "laptop")
		return errors.Is(err, transport.ErrNotPairing)
	})
	if clients, _ := transport.ReadAuthorized(h.paths.AuthorizedClientsPath()); len(clients) != 0 {
		t.Fatalf("authorized clients = %+v", clients)
	}
	// A new window can be opened.
	ctx, cancel = context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	startPair(h, ctx)
}

func TestPairNeedsRemoteAccess(t *testing.T) {
	h := newHarness(t)
	_, err := Pair(context.Background(), h.paths, func(*protocol.ManagementPairing) { t.Fatal("window opened") })
	if err == nil || !strings.Contains(err.Error(), "agentd remote enable") {
		t.Fatalf("err = %v", err)
	}
}

// Trusting a new machine is decided on the daemon's own machine, not by a
// remote client.
func TestRemoteClientCannotOpenAPairingWindow(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	client := newClientIdentity(t)
	if _, err := transport.Authorize(h.paths.AuthorizedClientsPath(), client.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dialRemote(client); err != nil {
		t.Fatal(err)
	}
	resp := h.management(&protocol.ManagementRequest{Pair: protocol.Empty})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "own machine") {
		t.Fatalf("remote pair = %#v", resp)
	}
}
