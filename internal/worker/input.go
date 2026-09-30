package worker

import (
	"errors"
	"io"
	"sync/atomic"
)

const (
	// inputQueueDepth bounds how many writes (keystroke batches, pastes,
	// send-input requests, terminal query replies) may wait for the agent
	// to read its input.
	inputQueueDepth = 256
	// maxQueuedInput bounds the bytes waiting in the queue, so input that
	// the agent never reads cannot grow the worker's memory without limit.
	maxQueuedInput = 4 << 20
)

var (
	errInputQueueFull = errors.New("the agent is not reading its input; try again once it catches up")
	errInputTooLarge  = errors.New("input is larger than the session's input buffer; send it in smaller pieces")
)

// ptyInput owns all writes to the PTY. A write blocks for as long as the
// agent leaves its input unread, so it must not happen on the owner
// goroutine: that goroutine also drains PTY output, and an agent blocked on
// writing output would then never read its input again.
//
// enqueue is only called from the owner goroutine, which also owns close, so
// the channel is never sent on after it is closed. The writer goroutine ends
// once the channel is closed and drained, or at the first failed write (the
// PTY is gone), after which queued input is discarded.
type ptyInput struct {
	ch     chan []byte
	done   chan struct{}
	queued atomic.Int64 // bytes enqueued and not yet written
}

func newPTYInput(w io.Writer) *ptyInput {
	in := &ptyInput{ch: make(chan []byte, inputQueueDepth), done: make(chan struct{})}
	go func() {
		defer close(in.done)
		for data := range in.ch {
			_, err := w.Write(data)
			in.queued.Add(-int64(len(data)))
			if err != nil {
				for range in.ch {
				}
				return
			}
		}
	}()
	return in
}

// enqueue queues data for the PTY without blocking. When the queue is full
// the input is refused rather than stalling the session.
func (in *ptyInput) enqueue(data []byte) error {
	if len(data) > maxQueuedInput {
		return errInputTooLarge
	}
	if in.queued.Load()+int64(len(data)) > maxQueuedInput {
		return errInputQueueFull
	}
	select {
	case in.ch <- data:
		in.queued.Add(int64(len(data)))
		return nil
	default:
		return errInputQueueFull
	}
}

func (in *ptyInput) close() {
	close(in.ch)
}
