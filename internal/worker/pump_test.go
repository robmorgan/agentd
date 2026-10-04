package worker

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"
	"time"
)

// smallReads returns data in reads of 1 to 200 bytes, like a macOS PTY
// under heavy output.
type smallReads struct {
	data  []byte
	rng   *rand.Rand
	reads int
}

func (r *smallReads) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(1+r.rng.IntN(200), len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	r.reads++
	return n, nil
}

// TestPumpBatchesSmallReads checks that the pump hands the owner every byte
// in order, in chunks of at most maxOutputBatch, and that reads arriving
// while the owner is busy share a chunk.
func TestPumpBatchesSmallReads(t *testing.T) {
	var want bytes.Buffer
	for i := 0; want.Len() < 512<<10; i++ {
		fmt.Fprintf(&want, "\x1b[3%dmline %d\x1b[0m padding padding\r\n", i%8, i)
	}
	ts := newTestTerminal(t, 80, 24)
	o := newOwner()
	state := &ownerState{terminal: ts, output: newBroadcaster(), attachments: map[string]*ownerAttachment{}, owner: o}
	// Room for every read as its own chunk, and less output than the
	// subscriber's byte bound, so nothing is dropped however the
	// scheduling goes.
	sub := &subscriber{ch: make(chan []byte, 1<<16)}
	state.output.subs[sub] = struct{}{}
	go o.run(state)
	defer o.stop()

	r := &smallReads{data: want.Bytes(), rng: rand.New(rand.NewPCG(1, 2))}
	pumpDone := make(chan struct{})
	go func() {
		pumpPty(r, o)
		close(pumpDone)
	}()

	var got bytes.Buffer
	chunks := 0
	for got.Len() < want.Len() {
		select {
		case c := <-sub.ch:
			sub.took(c)
			if len(c) > maxOutputBatch {
				t.Fatalf("chunk of %d bytes, more than %d", len(c), maxOutputBatch)
			}
			got.Write(c)
			chunks++
			// Keep the owner busy now and then, as a slow client or a
			// snapshot would, so reads pile up behind it.
			if chunks%8 == 0 {
				o.post(func(*ownerState) { time.Sleep(time.Millisecond) })
			}
		case <-time.After(testTimeout):
			t.Fatalf("got %d of %d bytes", got.Len(), want.Len())
		}
	}
	<-pumpDone
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatal("output reordered or corrupted")
	}
	if chunks >= r.reads {
		t.Fatalf("%d reads became %d chunks; expected fewer", r.reads, chunks)
	}
	t.Logf("%d reads, %d chunks", r.reads, chunks)
}
