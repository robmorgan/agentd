package worker

import (
	"sync"
	"sync/atomic"
)

const (
	// subscriberBuffer bounds the PTY reads (up to 8 KiB each) queued for
	// one attachment. Line-at-a-time output can come in reads of a few
	// dozen bytes, so it is generous: the byte bound below is the real one.
	subscriberBuffer = 1024
	// ...and subscriberMaxBytes the bytes they hold, whichever is reached
	// first.
	subscriberMaxBytes = 1 << 20
)

// broadcaster fans PTY output out to every attached client.
//
// Slow-consumer policy: publish never blocks. A subscriber whose queue is
// full (subscriberBuffer chunks or subscriberMaxBytes bytes) loses the
// chunks that do not fit and is marked lagged. The PTY and every other
// client keep flowing. What happens next is serveAttach's business: a
// client that negotiated attach-resync is sent a fresh snapshot once it
// drains (see resync), which replaces its now-wrong screen; an older client
// keeps a wrong screen until the program repaints or it reattaches.
//
// publish only runs on the owner goroutine, which also discards a
// subscriber's queue and clears its lag when a snapshot is taken, so a
// snapshot is an exact boundary in the subscriber's stream.
type broadcaster struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	// dropped counts the chunks dropped for lagging subscribers, summed
	// over all of them (reported in session stats).
	dropped uint64
}

type subscriber struct {
	ch chan []byte
	// queued is the bytes in ch. publish adds, the reader subtracts.
	queued atomic.Int64
	// lagged is set when output was dropped for this subscriber, and
	// cleared when a snapshot replaces what it missed.
	lagged atomic.Bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[*subscriber]struct{})}
}

func (b *broadcaster) subscribe() *subscriber {
	s := &subscriber{ch: make(chan []byte, subscriberBuffer)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *broadcaster) unsubscribe(s *subscriber) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

func (b *broadcaster) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *broadcaster) publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		// Only the reader lowers queued meanwhile, so this check cannot
		// let the queue overshoot its byte bound.
		if s.queued.Load()+int64(len(data)) > subscriberMaxBytes {
			b.dropped++
			s.lagged.Store(true)
			continue
		}
		select {
		case s.ch <- data:
			s.queued.Add(int64(len(data)))
		default:
			b.dropped++
			s.lagged.Store(true)
		}
	}
}

// droppedChunks is the number of chunks dropped so far for lagging
// subscribers.
func (b *broadcaster) droppedChunks() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// next takes the next queued chunk off the subscriber's queue, if any.
func (s *subscriber) next() ([]byte, bool) {
	select {
	case data := <-s.ch:
		s.queued.Add(-int64(len(data)))
		return data, true
	default:
		return nil, false
	}
}

// took accounts for a chunk the reader received from ch directly.
func (s *subscriber) took(data []byte) { s.queued.Add(-int64(len(data))) }

// discard drops everything queued and clears the lag. It must run on the
// owner goroutine, together with taking the snapshot that supersedes the
// dropped output.
func (s *subscriber) discard() {
	for {
		if _, ok := s.next(); !ok {
			break
		}
	}
	s.lagged.Store(false)
}
