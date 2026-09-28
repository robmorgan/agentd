package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/go/internal/session"
)

func strp(s string) *string { return &s }
func i32p(v int32) *int32   { return &v }
func u32p(v uint32) *uint32 { return &v }

func roundTripRequest(t *testing.T, req *Request) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, req)
	}
	if buf.Len() != 0 {
		t.Fatalf("trailing bytes after frame: %d", buf.Len())
	}
}

func roundTripResponse(t *testing.T, resp *Response) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteResponse(&buf, resp); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, resp)
	}
}

func TestRequestRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{SendInput: &SendInput{SessionID: "demo", Data: []byte{0, 1, 2, 255}, SourceSessionID: strp("origin")}})
	roundTripRequest(t, &Request{AttachResize: &Geometry{Cols: 120, Rows: 48, PixelWidth: 960, PixelHeight: 768}})
	roundTripRequest(t, &Request{AttachSnapshot: Empty})
	roundTripRequest(t, &Request{SwitchAttachedSession: &SwitchAttachedSession{SourceSessionID: "source", TargetSessionID: "target"}})
	roundTripRequest(t, &Request{DetachSession: &DetachSession{SessionID: "demo", All: true}})
	roundTripRequest(t, &Request{AttachSession: &AttachSession{SessionID: "demo", Kind: session.AttachmentTui, Geometry: Geometry{120, 48, 960, 768}}})
	roundTripRequest(t, &Request{DetachAttachment: &DetachAttachment{SessionID: "demo", AttachID: "attach-1"}})
	roundTripRequest(t, &Request{ListAttachments: &SessionRef{"demo"}})
	roundTripRequest(t, &Request{GetHistory: &GetHistory{SessionID: "demo", VT: true}})
	roundTripRequest(t, &Request{KillSession: &KillSession{SessionID: "demo", Remove: true, Force: true}})
	roundTripRequest(t, &Request{CreateSession: &CreateSession{Cwd: "/tmp/x", Name: strp("fix"), Agent: "codex", Model: strp("m")}})
	roundTripRequest(t, &Request{GetDaemonInfo: Empty})
	roundTripRequest(t, &Request{AttachInput: &Bytes{Data: []byte("hi")}})
}

func TestResponseRoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_456_789).UTC()
	roundTripResponse(t, &Response{Attached: &Attached{AttachID: "attach-1", Snapshot: []byte{0, 1, 2, 3, 4}}})
	roundTripResponse(t, &Response{AttachSnapshot: &Bytes{Data: []byte{4, 3, 2, 1}}})
	roundTripResponse(t, &Response{SwitchSession: &SessionRef{"target"}})
	roundTripResponse(t, &Response{History: &History{Data: "\x1b[31mhello\x1b[0m"}})
	roundTripResponse(t, &Response{Ok: Empty})
	roundTripResponse(t, &Response{EndOfStream: Empty})
	roundTripResponse(t, &Response{InputAccepted: Empty})
	roundTripResponse(t, &Response{Error: &ErrorResponse{Message: "boom"}})
	roundTripResponse(t, &Response{SessionEnded: &SessionEnded{
		SessionID: "demo", Status: session.StatusExited, ExitCode: i32p(0), Error: nil,
	}})
	roundTripResponse(t, &Response{CreateSession: &session.CreateResult{
		SessionID: "demo", Cwd: "/tmp/demo", Status: session.StatusRunning, Mode: session.ModeExecute,
	}})
	roundTripResponse(t, &Response{Attachments: &[]session.AttachmentRecord{{
		AttachID: "attach-1", SessionID: "demo", Kind: session.AttachmentAttach, ConnectedAt: now,
	}}})
	roundTripResponse(t, &Response{Sessions: &[]session.Record{{
		SessionID: "demo", Agent: "codex", Model: strp("gpt-5.4"), Mode: session.ModeExecute,
		Cwd: "/tmp/demo", Status: session.StatusRunning,
		WorkerPID: u32p(123), AgentPID: u32p(456), Attention: session.AttentionInfo,
		AttentionSummary: strp("fix"), CreatedAt: now, UpdatedAt: now,
	}}})
}

// TestAttachSessionGoldenFrame pins the exact bytes of an AttachSession
// request. Apart from the version field this is the frame the Rust v32
// implementation produces, so framing compatibility does not regress silently.
func TestAttachSessionGoldenFrame(t *testing.T) {
	var buf bytes.Buffer
	err := WriteRequest(&buf, &Request{AttachSession: &AttachSession{
		SessionID: "ab", Kind: session.AttachmentTui, Geometry: Geometry{Cols: 0x0102, Rows: 0x0304, PixelWidth: 0x0506, PixelHeight: 0x0708},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x50, 0x44, 0x47, 0x41, // magic "AGDP" little-endian
		33, 0, // protocol version
		7, 0, // AttachSessionRequest
		0, 0, 0, 0, // flags, reserved
		15, 0, 0, 0, // payload length
		2, 0, 0, 0, 'a', 'b', // session id
		2,                      // AttachmentKind::Tui
		0x02, 0x01, 0x04, 0x03, // cols, rows
		0x06, 0x05, 0x08, 0x07, // pixel width, pixel height
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("golden mismatch\n got % x\nwant % x", buf.Bytes(), want)
	}
}

func TestLegacyStatusValues(t *testing.T) {
	d := &decoder{buf: []byte{3}}
	if got := d.status(); got != session.StatusUnknownRecovered || d.err != nil {
		t.Fatalf("legacy paused status: got %q err %v", got, d.err)
	}
}

// TestRemovedKindsAreRejected checks that frames using the kind numbers
// retired in v33 are refused rather than decoded into something else, and
// that a v32 frame is refused before its kind is even looked at.
func TestRemovedKindsAreRejected(t *testing.T) {
	e := &encoder{}
	e.str("demo")
	payload := bytes.NewBuffer(e.buf)

	for _, k := range []uint16{4, 5, 10, 19, 20} {
		var buf bytes.Buffer
		if err := writeFrame(&buf, ProtocolVersion, k, payload.Bytes()); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRequest(&buf); err == nil {
			t.Fatalf("request kind %d should be rejected", k)
		}
	}
	for _, k := range []uint16{106, 107} {
		var buf bytes.Buffer
		if err := writeFrame(&buf, ProtocolVersion, k, payload.Bytes()); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadResponse(&buf); err == nil {
			t.Fatalf("response kind %d should be rejected", k)
		}
	}

	var buf bytes.Buffer
	if err := writeFrame(&buf, 32, 7, payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	_, err := ReadRequest(&buf)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version `32`") {
		t.Fatalf("v32 frame: got %v", err)
	}
}

func TestCleanEOFReturnsNil(t *testing.T) {
	req, err := ReadRequest(bytes.NewReader(nil))
	if req != nil || err != nil {
		t.Fatalf("expected nil, nil; got %v %v", req, err)
	}
	if _, err := ReadRequest(bytes.NewReader([]byte{0x50, 0x44})); err == nil {
		t.Fatal("expected truncated header error")
	}
}

// The Rust CLI serialises DaemonManagementRequest::Shutdown with serde_json
// as {"force":true}; the frame must match byte for byte.
func TestManagementShutdownGoldenFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteManagementRequest(&buf, &ManagementRequest{Shutdown: &ManagementShutdown{Force: true}}); err != nil {
		t.Fatal(err)
	}
	payload := `{"force":true}`
	want := append([]byte{
		0x50, 0x44, 0x47, 0x41, // magic
		0x01, 0x00, // management version 1
		0x22, 0x4e, // kind 20002
		0, 0, 0, 0,
		byte(len(payload)), 0, 0, 0,
	}, payload...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got  % x\nwant % x", buf.Bytes(), want)
	}
	req, mgmt, err := ReadIncoming(&buf)
	if err != nil || req != nil || mgmt == nil || mgmt.Shutdown == nil || !mgmt.Shutdown.Force {
		t.Fatalf("ReadIncoming = %v, %#v, %v", req, mgmt, err)
	}
}

func TestManagementStatusResponseRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	status := &ManagementStatus{DaemonVersion: "0.1.0", ProtocolVersion: ProtocolVersion, PID: 42, Root: "/r", Socket: "/r/agentd.sock", RunningSessions: true}
	if err := WriteManagementResponse(&buf, &ManagementResponse{Status: status}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"running_sessions":true`)) {
		t.Fatalf("status payload uses unexpected field names: %s", buf.Bytes()[16:])
	}
	got, err := ReadManagementResponse(&buf)
	if err != nil || !reflect.DeepEqual(got.Status, status) {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
}

func TestReadIncomingVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteErrorAtVersion(&buf, 32, "nope"); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadIncoming(&buf)
	var ve *VersionError
	if !errors.As(err, &ve) || ve.Version != 32 {
		t.Fatalf("got %v", err)
	}
	// The error frame itself decodes at its own version.
	buf.Reset()
	WriteErrorAtVersion(&buf, ProtocolVersion, "nope")
	resp, err := ReadResponse(&buf)
	if err != nil || resp.Error == nil || resp.Error.Message != "nope" {
		t.Fatalf("got %#v, %v", resp, err)
	}
}
