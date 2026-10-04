package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
)

// Duplicate and replayed requests.
//
// A client whose connection dies after it sent a request cannot know
// whether the daemon acted on it. For reads that does not matter: the
// client asks again. For requests with side effects (create, kill, rm,
// workspace add and rm) the client attaches a random token
// (protocol.CapRequestTokens) and sends the same request again, with the
// same token, once it has reconnected; the daemon answers a token it has
// seen with the original answer rather than acting twice (or failing with
// "already exists" or "not running").
//
// CreateSession's token is stored on the session row, so a retry finds the
// session even after a daemon restart (see createSession). The others are
// remembered here, in memory, for replayTTL: long enough to cover any
// reconnect a client attempts, short enough that the cache stays small. A
// daemon restart forgets them; a retried kill or rm then reports what it
// finds ("not running", "not found"), which is the state the client wanted.

const (
	// replayTTL is how long an answered token is remembered.
	replayTTL = 10 * time.Minute
	// maxReplayEntries bounds the cache. Beyond it the oldest answered
	// entries are forgotten early; requests still in progress are never
	// evicted (and are bounded by the control streams' in-flight limits).
	maxReplayEntries = 4096
)

type replayCache struct {
	mu      sync.Mutex
	entries map[string]*replayEntry
	now     func() time.Time
}

type replayEntry struct {
	// digest is what the request asked for; a token reused for a
	// different request is refused rather than answered.
	digest string
	// done is closed once resp is set.
	done     chan struct{}
	resp     *protocol.Response
	answered time.Time
}

func newReplayCache() *replayCache {
	return &replayCache{entries: make(map[string]*replayEntry), now: time.Now}
}

// do answers req, whose token is token: by running fn the first time, and
// with fn's answer for every later request with the same token while it is
// remembered. A duplicate that arrives while the first is still running
// waits for it.
func (c *replayCache) do(token string, req *protocol.Request, fn func() *protocol.Response) *protocol.Response {
	digest, err := req.Digest()
	if err != nil {
		return protocol.ErrorResponsef("%v", err)
	}
	c.mu.Lock()
	if e, ok := c.entries[token]; ok && !c.expired(e) {
		c.mu.Unlock()
		if e.digest != digest {
			return protocol.ErrorResponsef("request token %q was already used for a different request", token)
		}
		<-e.done
		return e.resp
	}
	c.evict()
	e := &replayEntry{digest: digest, done: make(chan struct{})}
	c.entries[token] = e
	c.mu.Unlock()

	resp := fn()
	c.mu.Lock()
	e.resp = resp
	e.answered = c.now()
	c.mu.Unlock()
	close(e.done)
	return resp
}

// expired reports whether e was answered more than replayTTL ago. Called
// with mu held.
func (c *replayCache) expired(e *replayEntry) bool {
	return !e.answered.IsZero() && c.now().Sub(e.answered) > replayTTL
}

// evict drops expired entries once the cache is full, and then, if it is
// still full, the oldest answered ones. Called with mu held.
func (c *replayCache) evict() {
	if len(c.entries) < maxReplayEntries {
		return
	}
	for token, e := range c.entries {
		if c.expired(e) {
			delete(c.entries, token)
		}
	}
	for len(c.entries) >= maxReplayEntries {
		oldest := ""
		var at time.Time
		for token, e := range c.entries {
			if !e.answered.IsZero() && (oldest == "" || e.answered.Before(at)) {
				oldest, at = token, e.answered
			}
		}
		if oldest == "" {
			return
		}
		delete(c.entries, oldest)
	}
}

func (c *replayCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// checkToken refuses a token that is not a plausible random id.
func checkToken(token string) error {
	if len(token) > protocol.MaxRequestTokenLen {
		return fmt.Errorf("request token is longer than %d bytes", protocol.MaxRequestTokenLen)
	}
	return nil
}
