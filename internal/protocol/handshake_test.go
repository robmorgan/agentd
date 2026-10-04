package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestHandshakeRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{Hello: NewHello("agent 0.1.0")})
	roundTripRequest(t, &Request{Hello: &Hello{MinVersion: 1, MaxVersion: 3, Client: "x"}})
	roundTripResponse(t, &Response{Welcome: &Welcome{
		Version: 1, DaemonVersion: "0.1.0", Capabilities: []string{CapControlStream, "future"},
		Host: HostInfo{Name: "devbox", OS: "linux", Arch: "amd64", CPUs: 16, MemoryBytes: 64 << 30,
			Agents: []string{"claude", "codex"}, DefaultAgent: "claude"},
	}})
}

// TestHelloGoldenFrame pins Hello and Welcome: they are what CLIs and
// daemons of different builds exchange first.
func TestHelloGoldenFrame(t *testing.T) {
	assertRequestGolden(t, &Request{Hello: &Hello{MinVersion: 1, MaxVersion: 2, Client: "a", Capabilities: []string{"c"}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x13, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 19
		0x12, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x02, 0x00, // min, max
		0x01, 0x00, 0x00, 0x00, 'a', // client
		0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 'c', // capabilities
	})
	assertResponseGolden(t, &Response{Welcome: &Welcome{Version: 1, DaemonVersion: "v", Capabilities: []string{"c"},
		Host: HostInfo{Name: "h", OS: "o", Arch: "a", CPUs: 2, MemoryBytes: 3, Agents: []string{"x"}, DefaultAgent: "x"}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x76, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 118
		0x39, 0x00, 0x00, 0x00,
		0x01, 0x00, // version
		0x01, 0x00, 0x00, 0x00, 'v',
		0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 'c',
		0x01, 0x00, 0x00, 0x00, 'h',
		0x01, 0x00, 0x00, 0x00, 'o',
		0x01, 0x00, 0x00, 0x00, 'a',
		0x02, 0x00, 0x00, 0x00, // cpus
		0x03, 0, 0, 0, 0, 0, 0, 0, // memory
		0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 'x',
		0x01, 0x00, 0x00, 0x00, 'x',
	})
}

// A tagged frame sets flag bit 0 and puts the u32 request id first in the
// payload, counted in its length.
func TestTaggedGoldenFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTaggedRequest(&buf, 0x01020304, &Request{ListSessions: Empty}); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x0d, 0x00, // ListSessions
		0x01, 0x00, 0x00, 0x00, // flags: request id
		0x04, 0x00, 0x00, 0x00, // payload length
		0x04, 0x03, 0x02, 0x01, // request id
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got  % x\nwant % x", buf.Bytes(), want)
	}
	req, id, tagged, err := ReadTaggedRequest(&buf)
	if err != nil || !tagged || id != 0x01020304 || req.ListSessions == nil {
		t.Fatalf("ReadTaggedRequest = %#v, %x, %v, %v", req, id, tagged, err)
	}

	buf.Reset()
	if err := WriteTaggedResponse(&buf, 7, ErrorResponsef("no")); err != nil {
		t.Fatal(err)
	}
	resp, id, tagged, err := ReadTaggedResponse(&buf)
	if err != nil || !tagged || id != 7 || resp.Error == nil || resp.Error.Message != "no" {
		t.Fatalf("ReadTaggedResponse = %#v, %d, %v, %v", resp, id, tagged, err)
	}
}

// Streams that do not use request ids refuse tagged frames, and every
// reader refuses flags it does not know, since they change the payload's
// layout.
func TestTagsAndUnknownFlagsAreRefused(t *testing.T) {
	var buf bytes.Buffer
	WriteTaggedRequest(&buf, 1, &Request{ListSessions: Empty})
	if _, err := ReadRequest(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("ReadRequest accepted a tagged frame")
	}
	if _, _, err := ReadIncoming(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("ReadIncoming accepted a tagged first frame")
	}

	frame := buf.Bytes()
	frame[8] = 0x02 // an unknown flag
	_, _, _, err := ReadTaggedRequest(bytes.NewReader(frame))
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("unknown flag: got %v", err)
	}

	// A decode failure on a control stream still reports the id and
	// leaves the stream at the next frame.
	buf.Reset()
	writeFrameTagged(&buf, ProtocolVersion, 9999, true, 42, nil)
	WriteTaggedRequest(&buf, 43, &Request{ListSessions: Empty})
	if _, id, tagged, err := ReadTaggedRequest(&buf); !errors.As(err, &de) || id != 42 || !tagged {
		t.Fatalf("bad kind: id %d tagged %v err %v", id, tagged, err)
	}
	if req, id, _, err := ReadTaggedRequest(&buf); err != nil || id != 43 || req.ListSessions == nil {
		t.Fatalf("next frame: %#v %d %v", req, id, err)
	}
}

func TestNegotiateVersion(t *testing.T) {
	for _, tc := range []struct {
		min, max uint16
		want     uint16
		ok       bool
	}{
		{1, 1, 1, true},
		{1, 9, ProtocolVersion, true},
		{ProtocolVersion + 1, ProtocolVersion + 2, 0, false},
		{0, 0, 0, false},
		{2, 1, 0, false},
	} {
		got, err := NegotiateVersion(tc.min, tc.max)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NegotiateVersion(%d, %d) = %d, %v", tc.min, tc.max, got, err)
		}
	}
}

func TestStreamRoles(t *testing.T) {
	for req, want := range map[*Request]StreamRole{
		{Hello: NewHello("x")}:                      RoleControl,
		{AttachSession: &AttachSession{}}:           RoleAttach,
		{GetHistory: &GetHistory{}}:                 RoleArtifact,
		{ListSessions: Empty}:                       RoleRequest,
		{CreateSession: &CreateSession{}}:           RoleRequest,
		{SendInput: &SendInput{}}:                   RoleRequest,
		{DetachAttachment: &DetachAttachment{}}:     RoleRequest,
		{RemoveWorkspace: &WorkspaceRef{Name: "w"}}: RoleRequest,
	} {
		if got := req.Role(); got != want {
			t.Errorf("%#v: role %v, want %v", req, got, want)
		}
	}
	if !reflect.DeepEqual(NewHello("x").Capabilities, Capabilities()) {
		t.Fatal("NewHello does not offer this build's capabilities")
	}
}

// A list's count comes off the wire; a frame claiming billions of elements
// must fail on its missing bytes, not allocate room for them first.
func TestLyingListCountsDoNotAllocate(t *testing.T) {
	for _, k := range []kind{kSessionsResponse, kAttachmentsResponse, kWorkspacesResponse} {
		var buf bytes.Buffer
		writeFrame(&buf, ProtocolVersion, uint16(k), []byte{0xff, 0xff, 0xff, 0xff})
		allocs := testing.AllocsPerRun(1, func() {
			if _, err := ReadResponse(bytes.NewReader(buf.Bytes())); err == nil {
				t.Fatalf("kind %d: lying count accepted", k)
			}
		})
		if allocs > 20 {
			t.Fatalf("kind %d: %v allocations", k, allocs)
		}
	}
}
