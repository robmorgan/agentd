package cli

import (
	"bufio"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// fakeRemote is a remote daemon that hands every request on a control
// stream to the test, which answers it or drops the whole connection by
// restarting the listener on the same address, as a daemon restart or a
// network failure would.
type fakeRemote struct {
	t    *testing.T
	id   *transport.Identity
	addr string
	caps []string

	mu       sync.Mutex
	listener *transport.QUICListener
	requests chan fakeRequest
}

type fakeRequest struct {
	req    *protocol.Request
	answer func(*protocol.Response)
}

func newFakeRemote(t *testing.T, caps []string) (*fakeRemote, *client) {
	t.Helper()
	id, err := transport.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRemote{t: t, id: id, addr: "127.0.0.1:0", caps: caps, requests: make(chan fakeRequest, 16)}
	f.listen()
	t.Cleanup(func() {
		f.mu.Lock()
		f.listener.Close()
		f.mu.Unlock()
	})
	c := testClient(t)
	c.host = &transport.Host{Name: "dev", Address: f.addr, Fingerprint: id.Fingerprint}
	t.Cleanup(c.close)
	return f, c
}

func (f *fakeRemote) listen() {
	// The previous listener's socket may take a moment to be released.
	var l *transport.QUICListener
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		l, err = transport.ListenQUIC(f.addr, f.id, transport.QUICOptions{Authorized: func(string) bool { return true }})
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		f.t.Fatal(err)
	}
	f.addr = l.Addr()
	f.mu.Lock()
	f.listener = l
	f.mu.Unlock()
	go transport.Serve(l, "fake", nil, f.serve)
}

// restart drops every connection and listens again on the same address.
func (f *fakeRemote) restart() {
	f.mu.Lock()
	f.listener.Close()
	f.mu.Unlock()
	f.listen()
}

func (f *fakeRemote) serve(s transport.Stream) {
	defer s.Close()
	r := bufio.NewReader(s)
	req, err := protocol.ReadRequest(r)
	if err != nil || req == nil || req.Hello == nil {
		return
	}
	protocol.WriteResponse(s, &protocol.Response{Welcome: &protocol.Welcome{Version: 1, Capabilities: f.caps}})
	features := protocol.NegotiateFeatures(protocol.Capabilities(), f.caps)
	var writeMu sync.Mutex
	for {
		req, id, _, err := protocol.ReadTaggedRequest(r, features)
		if err != nil || req == nil {
			return
		}
		f.requests <- fakeRequest{req: req, answer: func(resp *protocol.Response) {
			writeMu.Lock()
			defer writeMu.Unlock()
			protocol.WriteTaggedResponse(s, features, id, resp)
		}}
	}
}

func (f *fakeRemote) next() fakeRequest {
	f.t.Helper()
	select {
	case r := <-f.requests:
		return r
	case <-time.After(10 * time.Second):
		f.t.Fatal("no request arrived")
		return fakeRequest{}
	}
}

type callResult struct {
	resp *protocol.Response
	err  error
}

func (c *client) callAsync(req *protocol.Request) <-chan callResult {
	done := make(chan callResult, 1)
	go func() {
		resp, err := c.request(req, 0)
		done <- callResult{resp, err}
	}()
	return done
}

func wait(t *testing.T, done <-chan callResult) callResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("request did not finish")
		return callResult{}
	}
}

// A lifecycle request whose connection is lost after it was sent is sent
// again on a new connection, with the same token; reads are sent again
// too, but a request without a token is not.
func TestLostConnectionRetries(t *testing.T) {
	f, c := newFakeRemote(t, protocol.Capabilities())

	done := c.callAsync(&protocol.Request{KillSession: &protocol.KillSession{SessionID: "demo", Remove: true}})
	first := f.next()
	token := first.req.KillSession.Token
	if len(token) != 32 {
		t.Fatalf("kill sent with token %q", token)
	}
	f.restart()
	again := f.next()
	if again.req.KillSession == nil || again.req.KillSession.Token != token {
		t.Fatalf("retry = %#v, want the same kill with token %q", again.req, token)
	}
	again.answer(&protocol.Response{KillSession: &protocol.KillSessionResult{Removed: true, WasRunning: true}})
	if r := wait(t, done); r.err != nil || r.resp.KillSession == nil || !r.resp.KillSession.Removed {
		t.Fatalf("kill = %#v, %v", r.resp, r.err)
	}

	done = c.callAsync(&protocol.Request{ListSessions: protocol.Empty})
	f.next()
	f.restart()
	f.next().answer(&protocol.Response{Sessions: &[]session.Record{}})
	if r := wait(t, done); r.err != nil || r.resp.Sessions == nil {
		t.Fatalf("list = %#v, %v", r.resp, r.err)
	}

	done = c.callAsync(&protocol.Request{SendInput: &protocol.SendInput{SessionID: "demo", Data: []byte("x")}})
	f.next()
	f.restart()
	if r := wait(t, done); r.err == nil {
		t.Fatalf("send-input was retried: %#v", r.resp)
	}
	select {
	case r := <-f.requests:
		t.Fatalf("send-input sent again: %#v", r.req)
	case <-time.After(200 * time.Millisecond):
	}
}

// Against a daemon that does not take tokens a lifecycle request is never
// sent twice, since the daemon may have acted on it.
func TestNoRetryWithoutRequestTokens(t *testing.T) {
	f, c := newFakeRemote(t, []string{protocol.CapControlStream})
	done := c.callAsync(&protocol.Request{CreateSession: &protocol.CreateSession{Cwd: "/w"}})
	f.next()
	f.restart()
	if r := wait(t, done); r.err == nil {
		t.Fatalf("create = %#v", r.resp)
	}
	select {
	case r := <-f.requests:
		t.Fatalf("create sent again: %#v", r.req)
	case <-time.After(200 * time.Millisecond):
	}
}

// A resync repaints the screen in passthrough, and is ignored while the
// overlay is open (its close repaints from a fresh snapshot); either way
// the attachment carries on.
func TestResyncFrames(t *testing.T) {
	if got := string(resyncBytes([]byte("screen"))); got != "\x1b[2J\x1b[Hscreen" || strings.Contains(got, attachEnterSequence) {
		t.Fatalf("resync bytes %q", got)
	}
	f := frame{resp: &protocol.Response{AttachResync: &protocol.Bytes{Data: []byte("s")}}}
	if _, done, err := handleFrame(f, true); done || err != nil {
		t.Fatalf("resync with the overlay open ended the attachment: %v", err)
	}
}
