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
// Termination: when the client stops sending (detach or disconnect) the
// worker side is half-closed so the worker drops the attachment and ends its
// stream. When the worker's stream ends (detach, session end, worker exit)
// both connections are closed, which also unblocks the client->worker copy.
func (s *Server) proxyAttach(client transport.Stream, clientReader *bufio.Reader, req *protocol.AttachSession) error {
	worker, err := s.dialWorker(req.SessionID)
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("%v", err))
	}
	defer worker.Close()
	if err := protocol.WriteRequest(worker, &protocol.Request{AttachSession: req}); err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("session `%s` runtime: %v", req.SessionID, err))
	}

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
