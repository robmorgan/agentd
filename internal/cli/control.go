package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// controlStream carries the CLI's one-shot requests to a daemon over one
// long-lived stream (see protocol.RoleControl): it is opened with Hello, and
// each request is tagged with an id that its response echoes, so requests
// from the picker, the overlay and commands share it without waiting on
// each other.
//
// Ownership: one reader goroutine reads every response and hands it to the
// caller waiting on its id. It ends when the stream fails or closes, after
// failing every pending call; close waits for it. Writes are serialised by
// writeMu. A failed stream stays failed: the client opens a new one for the
// next request.
type controlStream struct {
	s       transport.Stream
	welcome *protocol.Welcome
	// features are the optional features both sides support.
	features protocol.Features

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]chan *protocol.Response
	err     error // why the stream ended; set once

	done chan struct{} // closed when the reader ends
}

// errControlClosed means the control stream ended before a response
// arrived.
var errControlClosed = errors.New("agentd closed the control stream")

// openControl opens a stream and performs the handshake on it. The daemon
// answering anything but Welcome (an older daemon answers Hello with an
// error) is returned as an error.
func openControl(ctx context.Context, s transport.Stream) (*controlStream, error) {
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	reader := bufio.NewReader(s)
	err := protocol.WriteRequest(s, &protocol.Request{Hello: protocol.NewHello("agent " + Version)})
	var resp *protocol.Response
	if err == nil {
		resp, err = protocol.ReadResponse(reader)
	}
	watching := stopWatch()
	switch {
	case !watching:
		err = errors.Join(ctx.Err(), err)
	case err != nil:
	case resp == nil:
		err = errors.New("agentd closed the connection")
	case resp.Error != nil:
		err = errors.New(resp.Error.Message)
	case resp.Welcome == nil:
		err = fmt.Errorf("unexpected response to hello: %s", describeResponse(resp))
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.SetDeadline(time.Time{})
	cs := &controlStream{
		s:        s,
		welcome:  resp.Welcome,
		features: protocol.NegotiateFeatures(protocol.Capabilities(), resp.Welcome.Capabilities),
		pending:  make(map[uint32]chan *protocol.Response),
		done:     make(chan struct{}),
	}
	go cs.read(reader)
	return cs, nil
}

func (cs *controlStream) read(r *bufio.Reader) {
	defer close(cs.done)
	for {
		resp, id, tagged, err := protocol.ReadTaggedResponse(r, cs.features)
		switch {
		case err == nil && resp == nil:
			err = errControlClosed
		case err == nil && !tagged:
			// Only a refusal of the stream itself comes untagged.
			err = fmt.Errorf("control stream refused: %s", describeResponse(resp))
			if resp.Error != nil {
				err = errors.New(resp.Error.Message)
			}
		}
		if err != nil {
			cs.fail(err)
			return
		}
		cs.mu.Lock()
		ch := cs.pending[id]
		delete(cs.pending, id)
		cs.mu.Unlock()
		if ch != nil {
			ch <- resp // buffered; a caller that gave up never reads it
		}
	}
}

// fail ends the stream for every pending and future call.
func (cs *controlStream) fail(err error) {
	cs.mu.Lock()
	if cs.err == nil {
		cs.err = err
	}
	cs.pending = nil
	cs.mu.Unlock()
	cs.s.Close()
}

// failed reports whether the stream has ended.
func (cs *controlStream) failed() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.err != nil
}

// sentError is a failure after a request was written: the daemon may have
// acted on it, so it must not be retried blindly.
type sentError struct{ err error }

func (e *sentError) Error() string { return e.err.Error() }
func (e *sentError) Unwrap() error { return e.err }

// roundTrip sends req and waits for its response, ctx's deadline, or the
// stream ending. An error wrapped in *sentError means the request was sent.
func (cs *controlStream) roundTrip(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	ch := make(chan *protocol.Response, 1)
	cs.mu.Lock()
	if cs.err != nil {
		err := cs.err
		cs.mu.Unlock()
		return nil, err
	}
	cs.nextID++
	id := cs.nextID
	cs.pending[id] = ch
	cs.mu.Unlock()
	forget := func() {
		cs.mu.Lock()
		if cs.pending != nil {
			delete(cs.pending, id)
		}
		cs.mu.Unlock()
	}

	cs.writeMu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		cs.s.SetWriteDeadline(deadline)
	}
	err := protocol.WriteTaggedRequest(cs.s, cs.features, id, req)
	cs.s.SetWriteDeadline(time.Time{})
	cs.writeMu.Unlock()
	if err != nil {
		// A partial frame leaves the stream unusable.
		cs.fail(err)
		forget()
		return nil, &sentError{err}
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-cs.done:
		// The reader may have delivered the response just before ending.
		select {
		case resp := <-ch:
			return resp, nil
		default:
		}
		cs.mu.Lock()
		err := cs.err
		cs.mu.Unlock()
		return nil, &sentError{err}
	case <-ctx.Done():
		forget()
		return nil, &sentError{fmt.Errorf("agentd did not answer in time: %w", ctx.Err())}
	}
}

// close ends the stream and waits for its reader.
func (cs *controlStream) close() {
	cs.s.CloseWrite()
	cs.fail(errControlClosed)
	<-cs.done
}
