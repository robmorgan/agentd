package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// workerRequestTimeout bounds a one-shot request to a worker so a wedged
// worker cannot hang the client forever.
const workerRequestTimeout = 30 * time.Second

// dialWorker connects to a session's worker, explaining failures in terms of
// the session rather than the socket.
func (s *Server) dialWorker(id string) (*net.UnixConn, error) {
	conn, err := net.Dial("unix", s.paths.SessionSocketPath(id))
	if err == nil {
		return conn.(*net.UnixConn), nil
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

// proxyRequest forwards a single request to the session's worker and relays
// its single response.
func (s *Server) proxyRequest(client net.Conn, id string, req *protocol.Request) error {
	worker, err := s.dialWorker(id)
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("%v", err))
	}
	defer worker.Close()
	worker.SetDeadline(time.Now().Add(workerRequestTimeout))
	if err := protocol.WriteRequest(worker, req); err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("session `%s` runtime: %v", id, err))
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(worker))
	if err == nil && resp == nil {
		err = errors.New("closed the connection")
	}
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("session `%s` runtime: %v", id, err))
	}
	return protocol.WriteResponse(client, resp)
}

// history serves live history from the worker, falling back to the logs a
// worker writes when its session ends.
func (s *Server) history(client net.Conn, req *protocol.Request) error {
	id := req.GetHistory.SessionID
	if worker, err := net.Dial("unix", s.paths.SessionSocketPath(id)); err == nil {
		worker.Close()
		return s.proxyRequest(client, id, req)
	}
	rec, err := s.getSession(id)
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("%v", err))
	}
	if rec == nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("session `%s` not found", id))
	}
	path := s.paths.RenderedLogPath(id)
	if req.GetHistory.VT {
		path = s.paths.LogPath(id)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.WriteResponse(client, protocol.ErrorResponsef(
			"history for session `%s` is not available; it is only retained until the daemon restarts", id))
	}
	if err != nil {
		return protocol.WriteResponse(client, protocol.ErrorResponsef("failed to read %s: %v", path, err))
	}
	return protocol.WriteResponse(client, &protocol.Response{History: &protocol.History{Data: string(data)}})
}

// proxyAttach splices an attaching client onto the worker. After forwarding
// the AttachSession request it is a plain byte pipe in both directions; the
// worker speaks the attach stream directly to the client.
//
// Termination: when the client stops sending (detach or disconnect) the
// worker side is half-closed so the worker drops the attachment and ends its
// stream. When the worker's stream ends (detach, session end, worker exit)
// both connections are closed, which also unblocks the client->worker copy.
func (s *Server) proxyAttach(client net.Conn, clientReader *bufio.Reader, req *protocol.AttachSession) error {
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
	if uc, ok := client.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	client.Close()
	worker.Close()
	<-clientDone
	if copyErr != nil && !isDisconnect(copyErr) {
		return copyErr
	}
	return nil
}
