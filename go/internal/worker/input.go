package worker

import (
	"errors"
	"io"
)

// inputQueueDepth bounds how many writes (keystroke batches, pastes,
// send-input requests, terminal query replies) may wait for the agent to
// read its input.
const inputQueueDepth = 256

var errInputQueueFull = errors.New("the agent is not reading its input; try again once it catches up")

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
	ch   chan []byte
	done chan struct{}
}

func newPTYInput(w io.Writer) *ptyInput {
	in := &ptyInput{ch: make(chan []byte, inputQueueDepth), done: make(chan struct{})}
	go func() {
		defer close(in.done)
		for data := range in.ch {
			if _, err := w.Write(data); err != nil {
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
	select {
	case in.ch <- data:
		return nil
	default:
		return errInputQueueFull
	}
}

func (in *ptyInput) close() {
	close(in.ch)
}
