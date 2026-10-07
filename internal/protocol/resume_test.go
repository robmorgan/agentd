package protocol

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

func featuresOf(caps ...string) Features { return NegotiateFeatures(caps, caps) }

// assertGoldenWith is assertRequestGolden/assertResponseGolden for frames
// whose optional fields depend on negotiated features: it also checks that
// a peer without them refuses the frame rather than misreading it.
func assertGoldenWith(t *testing.T, f Features, req *Request, resp *Response, golden []byte) {
	t.Helper()
	var buf bytes.Buffer
	var err error
	if req != nil {
		err = WriteRequestWith(&buf, f, req)
	} else {
		err = WriteResponseWith(&buf, f, resp)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Fatalf("encoding changed\n got % x\nwant % x", buf.Bytes(), golden)
	}
	if req != nil {
		got, err := ReadRequestWith(bytes.NewReader(golden), f)
		if err != nil || !reflect.DeepEqual(got, req) {
			t.Fatalf("decode = %#v, %v", got, err)
		}
		if _, err := ReadRequest(bytes.NewReader(golden)); err == nil {
			t.Fatal("decoded without the features it needs")
		}
		return
	}
	got, err := ReadResponseWith(bytes.NewReader(golden), f)
	if err != nil || !reflect.DeepEqual(got, resp) {
		t.Fatalf("decode = %#v, %v", got, err)
	}
	if _, err := ReadResponse(bytes.NewReader(golden)); err == nil {
		t.Fatal("decoded without the features it needs")
	}
}

func TestRequestTokenGoldenFrames(t *testing.T) {
	f := featuresOf(CapRequestTokens)
	assertGoldenWith(t, f, &Request{CreateSession: &CreateSession{Cwd: "/w", Name: strp("fix"), Agent: "codex", Workspace: strp("ws"), Token: "t"}}, nil, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x24, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x2f, 0x77, 0x01, 0x03,
		0x00, 0x00, 0x00, 0x66, 0x69, 0x78, 0x05, 0x00, 0x00, 0x00, 0x63, 0x6f,
		0x64, 0x65, 0x78, 0x00, 0x01, 0x02, 0x00, 0x00, 0x00, 0x77, 0x73,
		0x01, 0x00, 0x00, 0x00, 't', // token
	})
	assertGoldenWith(t, f, &Request{KillSession: &KillSession{SessionID: "ab", Remove: true, Token: "t"}}, nil, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0c, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x01,
		0x01, 0x00, 0x00, 0x00, 't', // token
	})
	assertGoldenWith(t, f, &Request{AddWorkspace: &AddWorkspace{Name: "m", Path: "/w", Token: "t"}}, nil, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x11, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x6d, 0x02, 0x00, 0x00,
		0x00, 0x2f, 0x77,
		0x01, 0x00, 0x00, 0x00, 't', // token
	})
	assertGoldenWith(t, f, &Request{RemoveWorkspace: &WorkspaceRef{Name: "m", Token: "t"}}, nil, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x12, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0a, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x6d,
		0x01, 0x00, 0x00, 0x00, 't', // token
	})
	// Without the capability the token is not sent at all.
	var buf bytes.Buffer
	WriteRequest(&buf, &Request{KillSession: &KillSession{SessionID: "ab", Token: "t"}})
	if bytes.Contains(buf.Bytes()[16:], []byte("t")) {
		t.Fatalf("token sent without the capability: % x", buf.Bytes())
	}
}

func TestSessionUIDGoldenFrames(t *testing.T) {
	f := featuresOf(CapSessionUID)
	assertGoldenWith(t, f, nil, &Response{CreateSession: &session.CreateResult{SessionID: "ab", UID: "u", Cwd: "/w", Status: session.StatusRunning, Mode: session.ModePlan}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x66, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x13, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x02, 0x00,
		0x00, 0x00, 0x2f, 0x77, 0x02, 0x02,
		0x01, 0x00, 0x00, 0x00, 'u', // uid
	})
	// The UID is appended to every record, even inside a list.
	now := time.Unix(1_700_000_000, 0).UTC()
	recs := []session.Record{
		{SessionID: "a", UID: "1", Agent: "sh", Mode: session.ModeExecute, Status: session.StatusRunning, Attention: session.AttentionInfo, CreatedAt: now, UpdatedAt: now},
		{SessionID: "b", UID: "2", Agent: "sh", Mode: session.ModeExecute, Status: session.StatusExited, Attention: session.AttentionNotice, CreatedAt: now, UpdatedAt: now},
	}
	var buf bytes.Buffer
	if err := WriteTaggedResponse(&buf, f, 1, &Response{Sessions: &recs}); err != nil {
		t.Fatal(err)
	}
	frame := append([]byte(nil), buf.Bytes()...)
	resp, _, _, err := ReadTaggedResponse(&buf, f)
	if err != nil || !reflect.DeepEqual(*resp.Sessions, recs) {
		t.Fatalf("sessions = %#v, %v", resp, err)
	}
	if _, _, _, err := ReadTaggedResponse(bytes.NewReader(frame), Features{}); err == nil {
		t.Fatal("records with UIDs decoded without the capability")
	}
}

// AttachSession's feature list is the attach stream's handshake: it and the
// fields it enables follow Geometry, and are absent for older clients.
func TestAttachFeaturesGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{AttachSession: &AttachSession{
		SessionID: "ab", Kind: session.AttachmentAttach, Geometry: Geometry{Cols: 80, Rows: 24},
		Features: []string{CapSessionUID, CapAttachReplace}, ExpectUID: "u", Replaces: "attach-1",
	}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x45, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 'a', 'b', // session id
		0x01,                                           // attach
		0x50, 0x00, 0x18, 0x00, 0x00, 0x00, 0x00, 0x00, // geometry
		0x02, 0x00, 0x00, 0x00, // features
		0x0b, 0x00, 0x00, 0x00, 's', 'e', 's', 's', 'i', 'o', 'n', '-', 'u', 'i', 'd',
		0x0e, 0x00, 0x00, 0x00, 'a', 't', 't', 'a', 'c', 'h', '-', 'r', 'e', 'p', 'l', 'a', 'c', 'e',
		0x01, 0x00, 0x00, 0x00, 'u', // expected uid
		0x08, 0x00, 0x00, 0x00, 'a', 't', 't', 'a', 'c', 'h', '-', '1', // replaces
	})
	assertResponseGolden(t, &Response{Attached: &Attached{
		AttachID: "attach-1", Snapshot: []byte("s"), Features: []string{CapSessionUID}, SessionUID: "u",
	}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x68, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x29, 0x00, 0x00, 0x00,
		0x08, 0x00, 0x00, 0x00, 'a', 't', 't', 'a', 'c', 'h', '-', '1',
		0x01, 0x00, 0x00, 0x00, 's', // snapshot
		0x01, 0x00, 0x00, 0x00, // features
		0x0b, 0x00, 0x00, 0x00, 's', 'e', 's', 's', 'i', 'o', 'n', '-', 'u', 'i', 'd',
		0x01, 0x00, 0x00, 0x00, 'u', // session uid
	})
	assertResponseGolden(t, &Response{AttachResync: &Bytes{Data: []byte("hi")}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x96, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 150
		0x06, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 'h', 'i',
	})
	// A feature with no field of its own adds only its name.
	roundTripRequest(t, &Request{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Features: []string{CapAttachResync}}})
	// The scrollback cap follows the fields of the features before it.
	assertRequestGolden(t, &Request{AttachSession: &AttachSession{
		SessionID: "ab", Kind: session.AttachmentAttach, Geometry: Geometry{Cols: 80, Rows: 24},
		Features: []string{CapAttachScrollback}, ScrollbackRows: 1000,
	}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x2c, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 'a', 'b', // session id
		0x01,                                           // attach
		0x50, 0x00, 0x18, 0x00, 0x00, 0x00, 0x00, 0x00, // geometry
		0x01, 0x00, 0x00, 0x00, // features
		0x11, 0x00, 0x00, 0x00, 'a', 't', 't', 'a', 'c', 'h', '-', 's', 'c', 'r', 'o', 'l', 'l', 'b', 'a', 'c', 'k',
		0xe8, 0x03, 0x00, 0x00, // scrollback rows
	})
	roundTripRequest(t, &Request{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach,
		Features: AttachCapabilities(), ExpectUID: "u", Replaces: "attach-1", ScrollbackRows: AllScrollbackRows}})
}

func TestAttachFeaturesFollowTheControlStream(t *testing.T) {
	if got := AttachFeatures(featuresOf(CapSessionUID, CapAttachResync)); got != nil {
		t.Fatalf("attach features without %s: %v", CapAttachFeatures, got)
	}
	got := AttachFeatures(NegotiateFeatures(Capabilities(), []string{CapAttachFeatures, CapAttachResync, CapControlStream, "future"}))
	if !reflect.DeepEqual(got, []string{CapAttachResync}) {
		t.Fatalf("attach features = %v", got)
	}
	if got := IntersectCapabilities([]string{"a", "b", "a", "c"}, []string{"c", "a"}); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("intersect = %v", got)
	}
}

func TestRequestTokensAndDigests(t *testing.T) {
	kill := &Request{KillSession: &KillSession{SessionID: "a", Remove: true}}
	slot := kill.TokenSlot()
	if slot == nil {
		t.Fatal("kill takes no token")
	}
	before, _ := kill.Digest()
	*slot = "tok"
	after, _ := kill.Digest()
	if before != after || !strings.HasPrefix(before, "4:") {
		t.Fatalf("digest depends on the token: %q %q", before, after)
	}
	other, _ := (&Request{KillSession: &KillSession{SessionID: "b", Remove: true}}).Digest()
	if other == before {
		t.Fatal("different requests share a digest")
	}
	for _, req := range []*Request{{ListSessions: Empty}, {SendInput: &SendInput{}}, {AttachSession: &AttachSession{}}} {
		if req.TokenSlot() != nil {
			t.Fatalf("%#v takes a token", req)
		}
	}
}
