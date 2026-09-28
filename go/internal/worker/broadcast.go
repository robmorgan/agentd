package worker

import "sync"

const subscriberBuffer = 256

// broadcaster fans PTY output out to every attached client.
//
// Slow-consumer policy: publish never blocks. A subscriber that falls more
// than subscriberBuffer chunks (up to ~2 MiB of 8 KiB reads) behind silently
// loses the chunks that do not fit, matching the Rust worker's
// broadcast::RecvError::Lagged => continue. The PTY and every other client
// keep flowing, but the lagging client's screen is wrong until the program
// repaints or the user reattaches. The intended fix is to mark the
// subscriber lagged here and have serveAttach send a fresh snapshot once it
// drains; that needs the CLI to accept an unsolicited AttachSnapshot, which
// it currently rejects.
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
