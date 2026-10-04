package protocol

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

// The decoders read bytes straight off the network. These fuzz tests check
// that no input makes them panic or allocate without bound, and that
// whatever they accept encodes back to a frame that decodes to the same
// value. Run them longer with, for example:
//
//	go test ./internal/protocol -run '^$' -fuzz FuzzReadRequest -fuzztime 5m

func seedFrames(f *testing.F) {
	now := time.Unix(1_700_000_000, 5).UTC()
	reqs := []*Request{
		{Hello: NewHello("seed")},
		{CreateSession: &CreateSession{Cwd: "/w", Name: strp("fix"), Agent: "codex", Workspace: strp("ws")}},
		{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Geometry: Geometry{80, 24, 0, 0}}},
		{SendInput: &SendInput{SessionID: "ab", Data: []byte("hi\r")}},
		{ListSessions: Empty},
	}
	resps := []*Response{
		{Welcome: &Welcome{Version: 1, Capabilities: []string{CapControlStream}, Host: HostInfo{Agents: []string{"a"}}}},
		{Sessions: &[]session.Record{{SessionID: "ab", Agent: "sh", Mode: session.ModeExecute, Status: session.StatusRunning, Attention: session.AttentionInfo, CreatedAt: now, UpdatedAt: now}}},
		{Attached: &Attached{AttachID: "attach-1", Snapshot: []byte("\x1b[H")}},
		{Attachments: &[]session.AttachmentRecord{{AttachID: "a", SessionID: "b", Kind: session.AttachmentTui, ConnectedAt: now}}},
		ErrorResponsef("no"),
	}
	for _, r := range reqs {
		var buf bytes.Buffer
		WriteRequest(&buf, r)
		f.Add(buf.Bytes())
		buf.Reset()
		WriteTaggedRequest(&buf, Features{}, 3, r)
		f.Add(buf.Bytes())
	}
	for _, r := range resps {
		var buf bytes.Buffer
		WriteResponse(&buf, r)
		f.Add(buf.Bytes())
		buf.Reset()
		WriteTaggedResponse(&buf, Features{}, 3, r)
		f.Add(buf.Bytes())
	}
	var buf bytes.Buffer
	WriteManagementRequest(&buf, &ManagementRequest{Shutdown: &ManagementShutdown{Force: true}})
	f.Add(buf.Bytes())
}

func FuzzReadRequest(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		req, id, tagged, err := ReadTaggedRequest(bytes.NewReader(data), Features{})
		if err != nil || req == nil {
			return
		}
		var buf bytes.Buffer
		if tagged {
			err = WriteTaggedRequest(&buf, Features{}, id, req)
		} else {
			err = WriteRequest(&buf, req)
		}
		if err != nil {
			t.Fatalf("decoded request does not encode: %v (%#v)", err, req)
		}
		again, id2, tagged2, err := ReadTaggedRequest(&buf, Features{})
		if err != nil || id2 != id || tagged2 != tagged || !reflect.DeepEqual(again, req) {
			t.Fatalf("request round trip changed it:\n%#v\n%#v (%v)", req, again, err)
		}
	})
}

func FuzzReadResponse(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		resp, id, tagged, err := ReadTaggedResponse(bytes.NewReader(data), Features{})
		if err != nil || resp == nil {
			return
		}
		var buf bytes.Buffer
		if tagged {
			err = WriteTaggedResponse(&buf, Features{}, id, resp)
		} else {
			err = WriteResponse(&buf, resp)
		}
		if err != nil {
			t.Fatalf("decoded response does not encode: %v (%#v)", err, resp)
		}
		again, id2, tagged2, err := ReadTaggedResponse(&buf, Features{})
		if err != nil || id2 != id || tagged2 != tagged || !reflect.DeepEqual(again, resp) {
			t.Fatalf("response round trip changed it:\n%#v\n%#v (%v)", resp, again, err)
		}
	})
}

// FuzzReadIncoming covers the daemon's first read, which also accepts the
// JSON management protocol.
func FuzzReadIncoming(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		req, mgmt, err := ReadIncoming(bytes.NewReader(data))
		if err != nil {
			return
		}
		if req != nil && mgmt != nil {
			t.Fatal("ReadIncoming returned both a request and a management request")
		}
		if mgmt != nil {
			var buf bytes.Buffer
			if err := WriteManagementRequest(&buf, mgmt); err != nil {
				t.Fatalf("decoded management request does not encode: %v", err)
			}
		}
	})
}
