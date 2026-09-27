package protocol

import (
	"bytes"
	"reflect"
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
	roundTripRequest(t, &Request{CreateSession: &CreateSession{Workspace: "/tmp/x", Agent: "codex", Model: strp("m"), IntegrationPolicy: session.PolicyAutoApplySafe}})
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
		SessionID: "demo", Status: session.StatusExited, ApplyState: session.ApplyIdle, HasCommits: true,
		Branch: "agent/demo", Worktree: "/tmp/wt", ExitCode: i32p(0), Error: nil,
	}})
	roundTripResponse(t, &Response{Attachments: &[]session.AttachmentRecord{{
		AttachID: "attach-1", SessionID: "demo", Kind: session.AttachmentAttach, ConnectedAt: now,
	}}})
	roundTripResponse(t, &Response{Sessions: &[]session.Record{{
		SessionID: "demo", Agent: "codex", Model: strp("gpt-5.4"), Mode: session.ModeExecute,
		Workspace: "/tmp/demo", RepoPath: "/tmp/demo", RepoName: "demo", BaseBranch: "main",
		Branch: "agent/fix", Worktree: "/tmp/worktree", Status: session.StatusRunning,
		IntegrationPolicy: session.PolicyAutoApplySafe, ApplyState: session.ApplyIdle,
		WorkerPID: u32p(123), AgentPID: u32p(456), Attention: session.AttentionInfo,
		AttentionSummary: strp("fix"), CreatedAt: now, UpdatedAt: now,
	}}})
}

// TestAttachSessionGoldenFrame pins the exact bytes the Rust implementation
// produces for an AttachSession request so cross-language compatibility does
// not regress silently.
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
		32, 0, // protocol version
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

func TestLegacyStatusAndApplyStateValues(t *testing.T) {
	d := &decoder{buf: []byte{3}}
	if got := d.status(); got != session.StatusUnknownRecovered || d.err != nil {
		t.Fatalf("legacy paused status: got %q err %v", got, d.err)
	}
	d = &decoder{buf: []byte{3}}
	if got := d.applyState(); got != session.ApplyIdle || d.err != nil {
		t.Fatalf("legacy apply state: got %q err %v", got, d.err)
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
