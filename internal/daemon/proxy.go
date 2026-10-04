package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// workerRequestTimeout bounds a one-shot request to a worker so a wedged
// worker cannot hang the client forever.
const workerRequestTimeout = 30 * time.Second

// dialWorker connects to a session's worker, explaining failures in terms of
// the session rather than the socket.
func (s *Server) dialWorker(id string) (transport.Stream, error) {
	conn, err := transport.DialUnix(s.paths.SessionSocketPath(id), workerDialTimeout)
	if err == nil {
		return conn, nil
	}
	rec, lookupErr := s.getSession(id)
	switch {
	case lookupErr != nil:
		return nil, lookupErr
	case rec == nil:
		return nil, fmt.Errorf("session `%s` not found", id)
	case rec.Status != session.StatusRunning:
		return nil, fmt.Errorf("session `%s` is not running", id)
	}
	return nil, fmt.Errorf("failed to connect to runtime for session `%s`: %w", id, err)
}

// forward sends a single request to the session's worker and returns its
// single response.
func (s *Server) forward(id string, req *protocol.Request) *protocol.Response {
	worker, err := s.dialWorker(id)
	if err != nil {
		return protocol.ErrorResponsef("%v", err)
	}
	defer worker.Close()
	resp, err := exchange(worker, req)
	if err != nil {
		return protocol.ErrorResponsef("session `%s` runtime: %v", id, err)
	}
	return resp
}

// sessionStats asks the session's worker for its resource usage. A worker
// started by an older agentd (workers outlive daemon upgrades) does not
// know the request.
func (s *Server) sessionStats(req *protocol.Request) *protocol.Response {
	id := req.GetSessionStats.SessionID
	resp := s.forward(id, req)
	if resp.Error != nil && resp.Error.Message == "unsupported worker request" {
		return protocol.ErrorResponsef("session `%s` was started by an older agentd that cannot report resource usage; it will once restarted", id)
	}
	return resp
}

// history serves a session's whole history in one response (GetHistory):
// live from the worker, or from the logs a worker writes when its session
// ends. The `history` artifacts stream the same content in chunks.
func (s *Server) history(req *protocol.Request) *protocol.Response {
	src, err := s.historySource(req.GetHistory.SessionID, req.GetHistory.VT)
	if err != nil {
		return protocol.ErrorResponsef("%v", err)
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return protocol.ErrorResponsef("failed to read history: %v", err)
	}
	return &protocol.Response{History: &protocol.History{Data: string(data)}}
}

// exchange sends one request to a worker and reads its one response.
func exchange(worker transport.Stream, req *protocol.Request) (*protocol.Response, error) {
	worker.SetDeadline(time.Now().Add(workerRequestTimeout))
	if err := protocol.WriteRequest(worker, req); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(worker))
	if err == nil && resp == nil {
		err = errors.New("closed the connection")
	}
	return resp, err
}

// proxyAttach splices an attaching client onto the worker. After forwarding
// the AttachSession request it is a plain byte pipe in both directions; the
// worker speaks the attach stream directly to the client.
//
// Attaching acknowledges the session's attention, as does detaching from a
// session that is still running: either way the user has seen its screen.
// An attachment that ends because the session ended leaves the end's
// attention in place, since a client may be attached in a terminal nobody
// is looking at.
//
// Termination: when the client stops sending (detach or disconnect) the
// worker side is half-closed so the worker drops the attachment and ends its
// stream. When the worker's stream ends (detach, session end, worker exit)
// both connections are closed, which also unblocks the client->worker copy.
func (s *Server) proxyAttach(client transport.Stream, clientReader *bufio.Reader, req *protocol.AttachSession) error {
	if len(req.Features) > 0 {
		if err := s.negotiateAttach(req); err != nil {
			return protocol.WriteResponse(client, protocol.ErrorResponsef("%v", err))
		}
	}
	worker, err := s.dialWorker(req.SessionID)
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("%v", err))
	}
	defer worker.Close()
	if err := protocol.WriteRequest(worker, &protocol.Request{AttachSession: req}); err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("session `%s` runtime: %v", req.SessionID, err))
	}
	s.acknowledge(req.SessionID, "seen: attached")
	defer func() {
		if rec, err := s.db.GetSession(req.SessionID); err == nil && rec != nil && rec.Status == session.StatusRunning {
			s.acknowledge(req.SessionID, "seen: detached")
		}
	}()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		// clientReader may already hold bytes the client sent after its
		// AttachSession frame, so copy from it rather than the raw conn.
		_, _ = io.Copy(worker, clientReader)
		_ = worker.CloseWrite()
	}()

	_, copyErr := io.Copy(client, worker)
	_ = client.CloseWrite()
	client.Close()
	worker.Close()
	<-clientDone
	if copyErr != nil && !isDisconnect(copyErr) {
		return copyErr
	}
	return nil
}

// negotiateAttach settles an attach stream's features before the request
// goes to the worker. The client lists only features this daemon
// advertised, but the session's worker may be an older build (workers keep
// running across daemon upgrades), so the list is cut down to what the
// worker supports; fields of dropped features are cleared, and the
// worker's Attached tells the client what is in effect.
//
// An ExpectUID is checked here too, against state.db, so a reattach never
// reaches another incarnation even through a worker too old to check it;
// a current worker checks again itself, closing the gap between this
// lookup and the dial.
func (s *Server) negotiateAttach(req *protocol.AttachSession) error {
	if req.HasFeature(protocol.CapSessionUID) && req.ExpectUID != "" {
		rec, err := s.db.GetSession(req.SessionID)
		switch {
		case err != nil:
			return err
		case rec == nil:
			return fmt.Errorf("session `%s` not found", req.SessionID)
		case rec.UID != req.ExpectUID:
			return errors.New(protocol.SessionReplacedMessage(req.SessionID))
		}
	}
	req.Features = protocol.IntersectCapabilities(req.Features, s.workerAttachCapabilities(req.SessionID))
	if !req.HasFeature(protocol.CapSessionUID) {
		req.ExpectUID = ""
	}
	if !req.HasFeature(protocol.CapAttachReplace) {
		req.Replaces = ""
	}
	return nil
}

// workerAttachCapabilities asks a session's worker which attach-stream
// features it supports. A worker from before attach features answers Hello
// with an error, and supports none.
func (s *Server) workerAttachCapabilities(id string) []string {
	caps, _ := workerCapabilities(s.paths.SessionSocketPath(id))
	return caps
}
