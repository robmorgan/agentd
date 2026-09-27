package worker

import "sync"

const subscriberBuffer = 256

// broadcaster fans PTY output out to every attached client. A subscriber
// that falls more than subscriberBuffer chunks behind drops output rather
// than stalling the PTY, matching the lag semantics of the Rust worker.
type broadcaster struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch chan []byte
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

func (b *broadcaster) publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- data:
		default:
		}
	}
}
