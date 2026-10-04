package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// maxOutputBatch caps how much PTY output the pump hands the owner at once,
// and so the size of each chunk fanned out to clients.
const maxOutputBatch = 8192

// pumpPty reads the PTY and hands the output to the owner goroutine, until
// a read fails (the PTY's end, or the pump being stopped).
//
// PTY reads can be small: on macOS, a program writing in small pieces (a
// pipeline such as yes | head) gives mostly 30 to 130 bytes per read. Handed
// over one by one, each would cost a trip through the owner's
// queue, a VTWrite into libghostty, and a frame to every attached client.
// So reads accumulate in a batch while the owner is busy with the previous
// one, up to maxOutputBatch, and the owner takes everything pending at
// once. An idle owner gets each read straight away, so batching adds no
// latency. When a batch is full and the owner has not taken it, the pump
// stops reading, which leaves the agent blocked on a full PTY as before.
func pumpPty(reader io.Reader, o *owner) {
	b := &outputBatch{taken: make(chan struct{}, 1)}
	buf := make([]byte, maxOutputBatch)
	for {
		n, err := reader.Read(buf)
		if n > 0 && !b.add(buf[:n], o) {
			return
		}
		if err != nil {
			return
		}
	}
}

// pump runs pumpPty on the PTY master.
//
// It waits for the PTY to be readable before each read, instead of blocking
// in read(2), so that it can be stopped without consuming anything more: a
// handoff stops it and then snapshots the terminal, and output the agent
// writes from then on stays in the kernel's PTY buffer for the next worker
// image to read. A blocking read could not be interrupted, and whatever it
// returned after the snapshot would be lost.
//
// One goroutine runs it; it ends when the PTY reports EOF or an error (every
// process holding the terminal has exited), closing eof, or once stop is
// called. stop waits for it, and every byte it read has been handed to the
// owner by then (a take is queued on the owner goroutine before anything
// queued after stop returns).
type pump struct {
	ptmx *os.File
	// wakeR is watched alongside the PTY; stop writes to wakeW. Both are
	// close-on-exec, so they never outlive this image.
	wakeR, wakeW *os.File
	stopping     atomic.Bool
	done         chan struct{}
}

var errPumpStopped = errors.New("the pump was stopped")

// startPump starts reading ptmx. eof is closed when the PTY reports end of
// file (not when the pump is stopped).
func startPump(ptmx *os.File, o *owner, eof chan<- struct{}) (*pump, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create the pump's wake pipe: %w", err)
	}
	p := &pump{ptmx: ptmx, wakeR: r, wakeW: w, done: make(chan struct{})}
	// Fd puts ptmx in blocking mode, which it already is (creack/pty uses
	// Fd for its ioctls); reads only happen once it is readable.
	reader := &stoppableReader{p: p, fd: int(ptmx.Fd())}
	go func() {
		defer close(p.done)
		pumpPty(reader, o)
		if !p.stopping.Load() {
			close(eof)
		}
	}()
	return p, nil
}

// stoppableReader reads the PTY only once it is readable, and not at all
// once its pump is stopping.
type stoppableReader struct {
	p  *pump
	fd int
}

func (r *stoppableReader) Read(buf []byte) (int, error) {
	for {
		if r.p.stopping.Load() {
			return 0, errPumpStopped
		}
		ready, err := waitReadable(r.fd, int(r.p.wakeR.Fd()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "session worker: waiting for PTY output: %v\n", err)
			return 0, err
		}
		// Checked again after waking: once stop is called nothing more
		// is read, even if output is waiting.
		if r.p.stopping.Load() {
			return 0, errPumpStopped
		}
		if ready {
			return r.p.ptmx.Read(buf)
		}
	}
}

// stop makes the pump stop reading and waits for it. A read in progress
// completes and its data is handed to the owner first.
func (p *pump) stop() {
	p.stopping.Store(true)
	_, _ = p.wakeW.Write([]byte{0})
	<-p.done
	p.wakeR.Close()
	p.wakeW.Close()
}
