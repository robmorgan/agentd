// Package transport carries the agent protocol. The protocol's unit is one
// bidirectional byte stream holding a single request/response exchange or one
// attach session, ended by half-closing. A Unix socket connection is one such
// stream; a QUIC stream will be another, with many streams multiplexed over
// one long-lived QUIC connection. Everything above this package works in
// terms of Stream and Listener, so it does not care which transport a client
// used.
package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Stream is one bidirectional byte stream carrying a single request or one
// attach session.
type Stream interface {
	io.ReadWriteCloser
	// CloseWrite half-closes the stream: the peer reads EOF while the
	// other direction keeps working.
	CloseWrite() error
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Listener yields Streams. Accept returns an error wrapping net.ErrClosed
// once Close has been called.
type Listener interface {
	Accept() (Stream, error)
	Close() error
	// Addr describes where the listener listens, for logs.
	Addr() string
}

// Counter counts running handlers for Serve. *sync.WaitGroup is one.
type Counter interface {
	Add(delta int)
	Done()
}

var _ Counter = (*sync.WaitGroup)(nil)

const maxAcceptBackoff = time.Second

// Serve accepts streams until the listener is closed and runs handle for
// each one on its own goroutine. Temporary Accept failures, such as running
// out of file descriptors, are retried with capped backoff rather than
// ending the loop. Serve returns once Accept reports that the listener is
// closed.
//
// If handlers is non-nil, Serve counts each handler in it (Add(1), then
// Done when it returns) before starting it, so once Serve has returned,
// waiting on handlers (a *sync.WaitGroup, say) covers every handler it
// started.
func Serve(l Listener, logPrefix string, handlers Counter, handle func(Stream)) {
	backoff := time.Duration(0)
	for {
		stream, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), maxAcceptBackoff)
			fmt.Fprintf(os.Stderr, "%s: accept on %s: %v; retrying in %v\n", logPrefix, l.Addr(), err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		if handlers == nil {
			go handle(stream)
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			handle(stream)
		}()
	}
}
