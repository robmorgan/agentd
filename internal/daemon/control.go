package daemon

import (
	"bufio"
	"errors"
	"os"
	"runtime"
	"sync"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// maxControlInFlight bounds the requests one control stream may have in
// progress at once. Once it is reached the daemon stops reading the stream
// until one finishes, so a client that pipelines requests is held back by
// the transport's flow control rather than queued in the daemon.
const maxControlInFlight = 16

// serveControl serves a control stream: the Hello handshake, then tagged
// one-shot requests until the client half-closes the stream (or it fails).
//
// Each request runs on its own goroutine, at most maxControlInFlight at a
// time, so a slow request (creating a session waits for its worker) does
// not hold up the quick ones behind it; responses carry the request's id
// and may go out in any order. Writes are serialised by writeMu. Once
// reading stops, serveControl waits for the requests in progress, so none
// outlives the handler. Shutdown closes the stream, which ends the read.
func (s *Server) serveControl(conn transport.Stream, reader *bufio.Reader, hello *protocol.Hello) error {
	version, err := protocol.NegotiateVersion(hello.MinVersion, hello.MaxVersion)
	if err != nil {
		return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
	}
	welcome := s.welcome(version)
	if protocol.HasCapability(hello.Capabilities, protocol.CapDaemonID) {
		welcome.DaemonID = s.daemonID
	}
	if err := protocol.WriteResponse(conn, &protocol.Response{Welcome: welcome}); err != nil {
		return err
	}
	features := protocol.NegotiateFeatures(protocol.Capabilities(), hello.Capabilities)

	var (
		writeMu  sync.Mutex
		writeErr error
		inFlight sync.WaitGroup
	)
	slots := make(chan struct{}, maxControlInFlight)
	reply := func(id uint32, resp *protocol.Response) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr != nil {
			return
		}
		if writeErr = protocol.WriteTaggedResponse(conn, features, id, resp); writeErr != nil {
			// The client is gone or not reading; closing the stream ends
			// the read loop too.
			conn.Close()
		}
	}
	defer inFlight.Wait()

	for {
		req, id, tagged, err := protocol.ReadTaggedRequest(reader, features)
		var decodeErr *protocol.DecodeError
		switch {
		case errors.As(err, &decodeErr) && tagged:
			// The frame was read whole, so the stream is still in step.
			reply(id, protocol.ErrorResponsef("agentd could not decode the request: %v", decodeErr.Err))
			continue
		case err != nil:
			return err
		case req == nil:
			return nil
		case !tagged:
			writeMu.Lock()
			err := protocol.WriteResponse(conn, protocol.ErrorResponsef("requests on a control stream need a request id"))
			writeMu.Unlock()
			return err
		case req.Role() != protocol.RoleRequest:
			reply(id, protocol.ErrorResponsef("a %s request needs a stream of its own", req.Role()))
			continue
		}
		slots <- struct{}{}
		inFlight.Add(1)
		go func() {
			defer inFlight.Done()
			defer func() { <-slots }()
			resp, after := s.respond(req, features)
			reply(id, resp)
			if after != nil {
				after()
			}
		}()
	}
}

// welcome describes this daemon and its machine.
func (s *Server) welcome(version uint16) *protocol.Welcome {
	host, _ := os.Hostname()
	return &protocol.Welcome{
		Version:       version,
		DaemonVersion: Version,
		Capabilities:  protocol.Capabilities(),
		Host: protocol.HostInfo{
			Name:         host,
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			CPUs:         uint32(runtime.NumCPU()),
			MemoryBytes:  physicalMemory(),
			Agents:       s.config.AgentNames(),
			DefaultAgent: s.config.DefaultAgent,
		},
	}
}
