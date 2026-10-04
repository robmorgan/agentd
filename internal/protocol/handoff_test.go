package protocol

import (
	"slices"
	"testing"

	"github.com/robmorgan/agentd/internal/session"
)

func TestHandoffRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{HandoffSession: &HandoffSession{SessionID: "demo", Executable: "/usr/local/bin/agentd"}})
	roundTripRequest(t, &Request{AttachSession: &AttachSession{
		SessionID: "demo", Kind: session.AttachmentAttach, Geometry: Geometry{80, 24, 0, 0},
		Features: []string{CapSessionRestart},
	}})
	roundTripResponse(t, &Response{HandedOff: &HandedOff{SessionID: "demo", WorkerPID: 4242, Executable: "/bin/agentd", Handoffs: 3}})
	roundTripResponse(t, &Response{SessionRestarting: &SessionRestarting{SessionID: "demo", Reason: "upgrading"}})
	if got := (&Request{HandoffSession: &HandoffSession{}}).Role(); got != RoleRequest {
		t.Fatalf("handoff role = %v", got)
	}
}

// TestHandoffGoldenFrames pins the handoff messages.
func TestHandoffGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{HandoffSession: &HandoffSession{SessionID: "s", Executable: "/x"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x3c, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 60
		0x0b, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 's',
		0x02, 0x00, 0x00, 0x00, '/', 'x',
	})
	assertResponseGolden(t, &Response{HandedOff: &HandedOff{SessionID: "s", WorkerPID: 0x01020304, Executable: "/x", Handoffs: 2}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0xa0, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 160
		0x13, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 's',
		0x04, 0x03, 0x02, 0x01, // worker pid
		0x02, 0x00, 0x00, 0x00, '/', 'x',
		0x02, 0x00, 0x00, 0x00, // handoffs
	})
	assertResponseGolden(t, &Response{SessionRestarting: &SessionRestarting{SessionID: "s", Reason: "r"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0xa1, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 161
		0x0a, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 's',
		0x01, 0x00, 0x00, 0x00, 'r',
	})
}

// Session restart is an attach-stream feature: a client asks for it in
// AttachSession.Features once the daemon's Welcome listed it.
func TestSessionRestartIsAnAttachFeature(t *testing.T) {
	if !slices.Contains(AttachCapabilities(), CapSessionRestart) || !slices.Contains(Capabilities(), CapSessionRestart) {
		t.Fatal("session-restart is not offered as an attach feature")
	}
	control := NegotiateFeatures(Capabilities(), []string{CapAttachFeatures, CapSessionRestart})
	if got := AttachFeatures(control); !slices.Equal(got, []string{CapSessionRestart}) {
		t.Fatalf("attach features = %v", got)
	}
	if !slices.Contains(Capabilities(), CapWorkerHandoff) || slices.Contains(AttachCapabilities(), CapWorkerHandoff) {
		t.Fatal("worker-handoff is a worker capability, not an attach feature")
	}
}
