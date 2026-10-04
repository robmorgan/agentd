package daemon

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// controlClient is a test client for a control stream.
type controlClient struct {
	t      *testing.T
	conn   transport.Stream
	reader *bufio.Reader
	// welcome is the daemon's answer to Hello.
	welcome  *protocol.Welcome
	features protocol.Features
}

func (h *harness) control() *controlClient {
	h.t.Helper()
	conn := h.dial()
	h.t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(testTimeout))
	if err := protocol.WriteRequest(conn, &protocol.Request{Hello: protocol.NewHello("test")}); err != nil {
		h.t.Fatal(err)
	}
	c := &controlClient{t: h.t, conn: conn, reader: bufio.NewReader(conn)}
	resp, err := protocol.ReadResponse(c.reader)
	if err != nil || resp == nil || resp.Welcome == nil {
		h.t.Fatalf("hello: %#v, %v", resp, err)
	}
	c.welcome = resp.Welcome
	c.features = protocol.NegotiateFeatures(protocol.Capabilities(), resp.Welcome.Capabilities)
	return c
}

func (c *controlClient) send(id uint32, req *protocol.Request) {
	c.t.Helper()
	if err := protocol.WriteTaggedRequest(c.conn, c.features, id, req); err != nil {
		c.t.Fatal(err)
	}
}

// read returns the next tagged response.
func (c *controlClient) read() (uint32, *protocol.Response) {
	c.t.Helper()
	resp, id, tagged, err := protocol.ReadTaggedResponse(c.reader, c.features)
	if err != nil || resp == nil || !tagged {
		c.t.Fatalf("control response: %#v tagged=%v err=%v", resp, tagged, err)
	}
	return id, resp
}

func TestControlStreamHandshake(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	w := c.welcome
	if w.Version != protocol.ProtocolVersion || w.DaemonVersion != Version {
		t.Fatalf("welcome = %#v", w)
	}
	if !protocol.HasCapability(w.Capabilities, protocol.CapControlStream) {
		t.Fatalf("capabilities = %v", w.Capabilities)
	}
	if w.Host.Name == "" || w.Host.OS == "" || w.Host.CPUs == 0 || w.Host.MemoryBytes == 0 {
		t.Fatalf("host = %#v", w.Host)
	}
	if strings.Join(w.Host.Agents, ",") != "sh,stubborn" || w.Host.DefaultAgent != "sh" {
		t.Fatalf("agents = %v default %q", w.Host.Agents, w.Host.DefaultAgent)
	}

	// A client whose versions do not overlap is refused in its own framing.
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTimeout))
	protocol.WriteRequest(conn, &protocol.Request{Hello: &protocol.Hello{MinVersion: protocol.ProtocolVersion + 1, MaxVersion: protocol.ProtocolVersion + 3}})
	wantError(t, mustRead(t, conn), "speaks protocol versions")
}

func mustRead(t *testing.T, conn transport.Stream) *protocol.Response {
	t.Helper()
	resp, err := protocol.ReadResponse(bufio.NewReader(conn))
	if err != nil || resp == nil {
		t.Fatalf("response: %#v, %v", resp, err)
	}
	return resp
}

// Requests on one control stream run concurrently and are answered by id:
// a slow one (creating a session waits for its worker) does not hold up the
// quick ones sent after it.
func TestControlStreamPipelinesRequests(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	name := "piped"
	c.send(1, &protocol.Request{CreateSession: &protocol.CreateSession{Cwd: h.cwd, Name: &name, Agent: "sh"}})
	for id := uint32(2); id <= 20; id++ {
		c.send(id, &protocol.Request{ListWorkspaces: protocol.Empty})
	}
	seen := map[uint32]*protocol.Response{}
	for len(seen) < 20 {
		id, resp := c.read()
		if seen[id] != nil {
			t.Fatalf("request %d answered twice", id)
		}
		seen[id] = resp
	}
	if seen[1].CreateSession == nil {
		t.Fatalf("create = %#v", seen[1])
	}
	for id := uint32(2); id <= 20; id++ {
		if seen[id].Workspaces == nil {
			t.Fatalf("request %d = %#v", id, seen[id])
		}
	}

	// Errors are answered on the stream, which stays usable.
	c.send(21, &protocol.Request{GetSession: &protocol.SessionRef{SessionID: "missing"}})
	if id, resp := c.read(); id != 21 || resp.Error == nil {
		t.Fatalf("missing session: %d %#v", id, resp)
	}
	c.send(22, &protocol.Request{AttachSession: &protocol.AttachSession{SessionID: name, Kind: session.AttachmentAttach}})
	if id, resp := c.read(); id != 22 || resp.Error == nil || !strings.Contains(resp.Error.Message, "stream of its own") {
		t.Fatalf("attach on control stream: %d %#v", id, resp)
	}
	c.send(23, &protocol.Request{GetHistory: &protocol.GetHistory{SessionID: name}})
	if id, resp := c.read(); id != 23 || resp.Error == nil {
		t.Fatalf("history on control stream: %d %#v", id, resp)
	}
	c.send(24, &protocol.Request{GetSession: &protocol.SessionRef{SessionID: name}})
	if id, resp := c.read(); id != 24 || resp.Session == nil || resp.Session.SessionID != name {
		t.Fatalf("get session: %d %#v", id, resp)
	}

	// An untagged request ends the stream with an error.
	protocol.WriteRequest(c.conn, &protocol.Request{ListSessions: protocol.Empty})
	resp, err := protocol.ReadResponse(c.reader)
	if err != nil || resp == nil || resp.Error == nil {
		t.Fatalf("untagged request: %#v, %v", resp, err)
	}
	if resp, err := protocol.ReadResponse(c.reader); resp != nil || err != nil {
		t.Fatalf("stream should end: %#v, %v", resp, err)
	}
}

// A request that cannot be decoded is answered with an error carrying its
// id, and the stream carries on.
func TestControlStreamSurvivesUndecodableRequests(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	bad := []byte{0x50, 0x44, 0x47, 0x41, 1, 0, 0x0f, 0x27, 1, 0, 0, 0, 4, 0, 0, 0, 9, 0, 0, 0} // kind 9999, id 9
	if _, err := c.conn.Write(bad); err != nil {
		t.Fatal(err)
	}
	if id, resp := c.read(); id != 9 || resp.Error == nil || !strings.Contains(resp.Error.Message, "could not decode") {
		t.Fatalf("bad request: %d %#v", id, resp)
	}
	c.send(10, &protocol.Request{ListSessions: protocol.Empty})
	if id, resp := c.read(); id != 10 || resp.Sessions == nil {
		t.Fatalf("after bad request: %d %#v", id, resp)
	}
}

// Shutdown closes control streams too, without waiting for the client.
func TestShutdownEndsControlStreams(t *testing.T) {
	h := newHarness(t)
	c := h.control()
	h.stop()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	if resp, _, _, err := protocol.ReadTaggedResponse(c.reader, c.features); resp != nil {
		t.Fatalf("response after shutdown: %#v %v", resp, err)
	}
}
