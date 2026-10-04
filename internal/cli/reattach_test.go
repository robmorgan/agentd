package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// How an attach stream ends decides whether the CLI attaches again: a
// worker restarting (an upgrade's live handoff) or a stream cut without a
// final frame (the daemon went away) resumes; a detach or the session
// ending does not.
func TestAttachStreamEndings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       frame
		wantErr error
		outcome attachOutcome
	}{
		{"restarting", frame{resp: &protocol.Response{SessionRestarting: &protocol.SessionRestarting{SessionID: "a"}}}, errSessionRestarting, 0},
		{"cut", frame{}, errAttachDropped, 0},
		{"detached", frame{resp: protocol.EndOfStreamResponse()}, nil, outcomeDetached},
		{"ended", frame{resp: &protocol.Response{SessionEnded: &protocol.SessionEnded{SessionID: "a"}}}, nil, outcomeEnded},
	} {
		for _, overlay := range []bool{false, true} {
			res, done, err := handleFrame(tc.f, overlay)
			if !done || !errors.Is(err, tc.wantErr) || (err == nil && res.outcome != tc.outcome) {
				t.Errorf("%s (overlay %v): %+v, %v, %v", tc.name, overlay, res, done, err)
			}
		}
	}
}

func TestReconnectable(t *testing.T) {
	local := testClient(t)
	remote := testClient(t)
	remote.host = &transport.Host{Name: "dev"}
	for _, c := range []*client{local, remote} {
		for _, err := range []error{errSessionRestarting, errAttachDropped, fmt.Errorf("wrapped: %w", errSessionRestarting)} {
			if !c.reconnectable(err) {
				t.Errorf("remote=%v: %v is not reconnectable", c.host != nil, err)
			}
		}
		if c.reconnectable(errors.New("session `a` not found")) {
			t.Errorf("remote=%v: a missing session is reconnectable", c.host != nil)
		}
	}
	// The local daemon's socket is refused or missing while it restarts.
	for _, err := range []error{syscall.ECONNREFUSED, syscall.ENOENT, syscall.ECONNRESET, io.ErrUnexpectedEOF} {
		if !local.reconnectable(fmt.Errorf("failed to connect: %w", err)) {
			t.Errorf("local: %v is not reconnectable", err)
		}
	}
	if !strings.Contains(local.reconnectStatus(errAttachDropped), "connection to agentd lost") ||
		!strings.Contains(remote.reconnectStatus(errAttachDropped), "connection to dev lost") ||
		!strings.Contains(local.reconnectStatus(errSessionRestarting), "session restarting") {
		t.Fatal("unexpected reconnect status")
	}
}
