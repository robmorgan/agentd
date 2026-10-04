package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// Events streams.
//
// Events live in state.db (internal/db/events.go), written by the daemon
// and by session workers. A subscriber is a cursor over the table: its
// handler reads a batch of at most eventBatch events after its cursor,
// writes them, and repeats; once it has caught up it waits for the feed to
// say something new may have been written. So the daemon holds at most one
// batch per subscriber and never buffers for a slow one: a subscriber that
// stops reading blocks only its own handler, in a write, while events keep
// accumulating in the table (pruned per session as usual, so a subscriber
// that falls very far behind skips what was pruned).
//
// Workers write the table directly, so the feed polls the newest event id
// (one indexed read of sqlite_sequence) every eventPollInterval while
// anyone is subscribed, and not at all otherwise. Events the daemon records
// itself wake subscribers at once.

const (
	// eventBatch bounds the events read and held per subscriber at once.
	eventBatch = 256
	// maxListEvents caps one ListEvents answer or SubscribeEvents tail;
	// defaultListEvents is the answer's size when the client asks for 0.
	maxListEvents     = 1000
	defaultListEvents = 100
)

// eventPollInterval is how often the feed looks for events written by
// session workers while anyone is subscribed: the most a worker's event
// waits before reaching subscribers. A variable so tests can shorten it.
var eventPollInterval = 250 * time.Millisecond

// eventFeed tells subscribers when new events may have been recorded.
//
// It is a broadcast without queues: changed returns a channel that is
// closed (and replaced) on the next change, so a subscriber that is busy
// writing simply finds it closed when it next looks, and no notification
// can pile up. One goroutine (run) polls the database while subscribers
// exist; it ends with the daemon.
type eventFeed struct {
	db *db.Database

	mu          sync.Mutex
	changed     chan struct{}
	subscribers int
	// arrived wakes run when the first subscriber arrives. Capacity 1;
	// a send that finds it full is dropped, since one wake-up suffices.
	arrived chan struct{}
}

func newEventFeed(store *db.Database) *eventFeed {
	return &eventFeed{db: store, changed: make(chan struct{}), arrived: make(chan struct{}, 1)}
}

// wait returns a channel closed when events may have been added since the
// call. Take it before reading the table, so nothing written meanwhile is
// missed.
func (f *eventFeed) wait() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

// notify wakes every subscriber.
func (f *eventFeed) notify() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *eventFeed) subscribe() {
	f.mu.Lock()
	f.subscribers++
	f.mu.Unlock()
	select {
	case f.arrived <- struct{}{}:
	default:
	}
}

func (f *eventFeed) unsubscribe() {
	f.mu.Lock()
	f.subscribers--
	f.mu.Unlock()
}

func (f *eventFeed) active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribers > 0
}

// run polls for events written by workers until stop is closed, and only
// while someone is subscribed.
func (f *eventFeed) run(stop <-chan struct{}) {
	last, _ := f.db.LastEventID()
	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()
	for {
		if !f.active() {
			select {
			case <-stop:
				return
			case <-f.arrived:
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		id, err := f.db.LastEventID()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentd: polling events: %v\n", err)
			continue
		}
		if id != last {
			last = id
			f.notify()
		}
	}
}

// eventSessionFilter checks an events request's optional session.
func (s *Server) eventSessionFilter(id *string) (string, error) {
	if id == nil {
		return "", nil
	}
	if !session.ValidName(*id) {
		return "", fmt.Errorf("session `%s` not found", *id)
	}
	rec, err := s.db.GetSession(*id)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", fmt.Errorf("session `%s` not found", *id)
	}
	return *id, nil
}

// listEvents answers ListEvents.
func (s *Server) listEvents(req *protocol.ListEvents) ([]session.Event, error) {
	sessionID, err := s.eventSessionFilter(req.SessionID)
	if err != nil {
		return nil, err
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = defaultListEvents
	}
	limit = min(limit, maxListEvents)
	if req.AfterID != nil {
		return s.db.EventsAfter(*req.AfterID, sessionID, limit)
	}
	return s.db.LatestEvents(0, sessionID, limit)
}

// serveEvents serves an events stream until the client closes it, the
// connection fails, or the daemon shuts down.
func (s *Server) serveEvents(conn transport.Stream, reader *bufio.Reader, req *protocol.SubscribeEvents) error {
	sessionID, err := s.eventSessionFilter(req.SessionID)
	if err != nil {
		return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
	}
	s.events.subscribe()
	defer s.events.unsubscribe()

	// The client sends nothing more; reading notices it closing its side.
	// The reader ends then, or when the handler returns and the stream is
	// closed.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, reader)
	}()

	send := func(events []session.Event) error {
		for i := range events {
			if err := protocol.WriteResponse(conn, &protocol.Response{Event: &events[i]}); err != nil {
				return err
			}
		}
		return nil
	}

	var cursor uint64
	if req.AfterID != nil {
		cursor = *req.AfterID
	} else {
		// Everything up to the newest id now is the past: send its tail,
		// then follow from there.
		if cursor, err = s.db.LastEventID(); err != nil {
			return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
		}
		if req.Tail > 0 {
			tail, err := s.db.LatestEvents(cursor, sessionID, min(int(req.Tail), maxListEvents))
			if err != nil {
				return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
			}
			if err := send(tail); err != nil {
				return err
			}
		}
	}

	for {
		changed := s.events.wait()
		batch, err := s.db.EventsAfter(cursor, sessionID, eventBatch)
		if err != nil {
			return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
		}
		if err := send(batch); err != nil {
			return err
		}
		if len(batch) > 0 {
			cursor = batch[len(batch)-1].ID
		}
		if len(batch) == eventBatch {
			continue
		}
		select {
		case <-changed:
		case <-gone:
			return nil
		case <-s.shutdown:
			return nil
		}
	}
}

// acknowledge records that the user has looked at a session, clearing its
// attention. how says what they did, for the event's summary.
func (s *Server) acknowledge(id, how string) {
	changed, err := s.db.Acknowledge(id, how)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd: acknowledging %s: %v\n", id, err)
		return
	}
	if changed {
		s.events.notify()
	}
}
