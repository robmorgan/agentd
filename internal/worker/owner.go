package worker

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// ownerState is only ever touched by the owner goroutine. All other
// goroutines reach it through owner.do, which serialises access.
type ownerState struct {
	sessionID   string
	ptmx        *os.File
	terminal    *terminalState
	geometry    protocol.Geometry
	attachments map[string]*ownerAttachment
	output      *broadcaster
	input       *ptyInput
	// outputBytes and outputChunks count the PTY output published
	// (session stats).
	outputBytes  uint64
	outputChunks uint64
	// activity tracks what the agent is doing; nil in tests that build
	// an ownerState without a session.
	activity *activityTracker
	// owner is the goroutine this state belongs to, for timers that need
	// to get back onto it.
	owner *owner
	// groundWaiters run once the shadow terminal's parser is between
	// sequences; groundGen counts the times they have been run, so a
	// fallback timer only runs the ones it was started for. See atGround.
	groundWaiters []func()
	groundGen     uint64
	// restart is closed when a handoff begins, telling every attachment to
	// end (with SessionRestarting where the client asked for it); a
	// handoff that fails replaces it.
	restart chan struct{}
}

type ownerAttachment struct {
	kind        session.AttachmentKind
	connectedAt time.Time
	sub         *subscriber
	detach      chan struct{}
	detachOnce  sync.Once
}

func (a *ownerAttachment) signalDetach() {
	a.detachOnce.Do(func() { close(a.detach) })
}

// owner serialises all access to ownerState on a single goroutine. It is
// stopped by closing done rather than cmds, because connection handlers and
// the PTY pump may still try to reach it after the session has ended (for
// example a client blocked writing to a slow socket past shutdownGrace).
// Once stopped, do returns errOwnerStopped and post is a no-op.
type owner struct {
	cmds chan func(*ownerState)
	done chan struct{}
}

var errOwnerStopped = errors.New("session worker is shutting down")

func newOwner() *owner {
	return &owner{cmds: make(chan func(*ownerState), 1024), done: make(chan struct{})}
}

// do runs fn on the owner goroutine and waits for it to finish.
func (o *owner) do(fn func(*ownerState) error) error {
	result := make(chan error, 1)
	select {
	case o.cmds <- func(s *ownerState) { result <- fn(s) }:
	case <-o.done:
		return errOwnerStopped
	}
	select {
	case err := <-result:
		return err
	case <-o.done:
		return errOwnerStopped
	}
}

// doAtGround is do for work that takes a snapshot: fn runs at the next
// point in the PTY output where the snapshot is an exact boundary (see
// ownerState.atGround), which may be after later output arrives.
func (o *owner) doAtGround(fn func(*ownerState) error) error {
	result := make(chan error, 1)
	if err := o.do(func(s *ownerState) error {
		s.atGround(func() { result <- fn(s) })
		return nil
	}); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-o.done:
		return errOwnerStopped
	}
}

// post runs fn on the owner goroutine without waiting.
func (o *owner) post(fn func(*ownerState)) {
	select {
	case o.cmds <- fn:
	case <-o.done:
	}
}

// stop makes run return after the command in progress, if any.
func (o *owner) stop() {
	close(o.done)
}

func (o *owner) run(state *ownerState) {
	for {
		select {
		case fn := <-o.cmds:
			fn(state)
		case <-o.done:
			return
		}
	}
}

func cellWidthPx(g protocol.Geometry) uint32 {
	if g.Cols == 0 || g.PixelWidth == 0 {
		return 0
	}
	return uint32(g.PixelWidth) / uint32(g.Cols)
}

func cellHeightPx(g protocol.Geometry) uint32 {
	if g.Rows == 0 || g.PixelHeight == 0 {
		return 0
	}
	return uint32(g.PixelHeight) / uint32(g.Rows)
}

func (s *ownerState) hasLiveAttachTerminal() bool {
	for _, a := range s.attachments {
		if a.kind == session.AttachmentAttach {
			return true
		}
	}
	return false
}

// writeInput queues client input for the PTY; see ptyInput.
func (s *ownerState) writeInput(data []byte) error {
	if err := s.input.enqueue(data); err != nil {
		return err
	}
	if s.activity != nil {
		s.activity.input(time.Now(), s.watched())
	}
	return nil
}

// watched reports whether an interactive client is attached.
func (s *ownerState) watched() bool { return len(s.attachments) > 0 }

// tickActivity checks idleness and samples the PTY's foreground process,
// and compresses the scrollback of a terminal that has gone quiet.
func (s *ownerState) tickActivity() {
	now := time.Now()
	if s.activity != nil {
		s.activity.tick(now, foregroundPGID(s.ptmx), s.watched())
	}
	s.terminal.compressIdle(now)
}

// resize applies a client's geometry. A zero row or column count (a client
// whose terminal reports no size) keeps the current size instead: the PTY
// and libghostty both reject it.
func (s *ownerState) resize(g protocol.Geometry) error {
	if g.Cols == 0 || g.Rows == 0 {
		return nil
	}
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Rows: g.Rows, Cols: g.Cols, X: g.PixelWidth, Y: g.PixelHeight}); err != nil {
		return fmt.Errorf("failed to resize pty: %w", err)
	}
	if err := s.terminal.resize(g.Cols, g.Rows, cellWidthPx(g), cellHeightPx(g)); err != nil {
		return err
	}
	s.geometry = g
	return nil
}

// publishOutput feeds PTY output through the shadow terminal, answers any
// terminal queries on behalf of absent clients, then fans the raw bytes out.
//
// While snapshots are waiting for the parser to be between sequences (see
// atGround), it feeds only up to that point first, runs them, and then
// handles the rest: subscribers get the chunk in two pieces, and a client
// attaching there gets only the second, after its snapshot.
func (s *ownerState) publishOutput(data []byte) error {
	s.outputBytes += uint64(len(data))
	s.outputChunks++
	if len(s.groundWaiters) > 0 {
		n, writes, effects := s.terminal.feedUntilGround(data)
		s.afterFeed(writes, effects)
		if n > 0 {
			s.output.publish(data[:n])
		}
		if s.terminal.atGround() {
			s.runGroundWaiters()
		}
		data = data[n:]
		if len(data) == 0 {
			return nil
		}
	}
	s.afterFeed(s.terminal.feed(data))
	s.output.publish(data)
	return nil
}

// afterFeed passes the attention effects of output to activity tracking,
// and writes the shadow terminal's replies to terminal queries (cursor
// position, device attributes and so on) to the PTY when no client terminal
// is attached to answer them itself.
func (s *ownerState) afterFeed(writes [][]byte, effects terminalEffects) {
	if s.activity != nil {
		title := ""
		if effects.titleChanged {
			title = s.terminal.title()
		}
		s.activity.output(time.Now(), effects, title, s.watched())
	}
	if s.hasLiveAttachTerminal() {
		return
	}
	for _, response := range writes {
		if err := s.input.enqueue(response); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: dropped terminal reply: %v\n", err)
		}
	}
}

// groundTimeout bounds how long a snapshot waits for a sequence that output
// left unfinished. Programs write whole sequences, so the rest of one split
// across PTY reads follows at once; a program that stops mid-sequence gets
// a snapshot taken there instead (and its client may print the sequence's
// tail as text). A variable so tests can lengthen it.
var groundTimeout = 100 * time.Millisecond

// atGround runs fn on the owner goroutine at the next point in the PTY
// output where the shadow terminal's parser is between escape sequences and
// UTF-8 characters: now, if it already is. A snapshot is a boundary in the
// stream (the client gets the snapshot, then the output after it), and a
// boundary inside a sequence would hand the client the sequence's tail
// without its start, which it would print as text. PTY reads split
// sequences routinely under heavy output, so snapshots wait for the next
// boundary, at most groundTimeout.
func (s *ownerState) atGround(fn func()) {
	if len(s.groundWaiters) == 0 && s.terminal.atGround() {
		fn()
		return
	}
	s.groundWaiters = append(s.groundWaiters, fn)
	if len(s.groundWaiters) == 1 {
		gen := s.groundGen
		time.AfterFunc(groundTimeout, func() {
			s.owner.post(func(s *ownerState) {
				if s.groundGen == gen {
					s.runGroundWaiters()
				}
			})
		})
	}
}

func (s *ownerState) runGroundWaiters() {
	waiters := s.groundWaiters
	s.groundWaiters = nil
	s.groundGen++
	for _, fn := range waiters {
		fn()
	}
}

func (s *ownerState) history(vt bool) (string, error) {
	data, err := s.terminal.format(vt)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *ownerState) snapshot() ([]byte, error) {
	return s.terminal.snapshot()
}

type attachResult struct {
	attachID string
	snapshot []byte
	detach   chan struct{}
	restart  chan struct{}
	sub      *subscriber
}

// attach registers a new attachment under attachID, which the caller
// numbered (see db.NextAttachID), and takes its snapshot. If replaces names
// a live attachment, that one is dropped at once: the client says it was
// its own, on a connection it has lost.
func (s *ownerState) attach(attachID string, kind session.AttachmentKind, g protocol.Geometry, replaces string) (*attachResult, error) {
	connectedAt := time.Now().UTC()
	if old, ok := s.attachments[replaces]; ok && replaces != "" {
		// Its handler may be blocked writing to the dead connection; it
		// ends within shutdownGrace and cleans up after itself, but the
		// attachment stops receiving output and leaves the list now.
		old.signalDetach()
		s.output.unsubscribe(old.sub)
		delete(s.attachments, replaces)
	}

	// Resize first, so the snapshot is laid out for the client's terminal:
	// one formatted at the previous size lands wrapped and with the cursor
	// on the wrong row in a terminal of another size.
	if err := s.resize(g); err != nil {
		return nil, err
	}
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}

	a := &ownerAttachment{kind: kind, connectedAt: connectedAt, sub: s.output.subscribe(), detach: make(chan struct{})}
	s.attachments[attachID] = a
	return &attachResult{
		attachID: attachID,
		snapshot: snapshot,
		detach:   a.detach,
		restart:  s.restart,
		sub:      a.sub,
	}, nil
}

func (s *ownerState) removeAttachment(attachID string) {
	delete(s.attachments, attachID)
}

func (s *ownerState) listAttachments() []session.AttachmentRecord {
	out := make([]session.AttachmentRecord, 0, len(s.attachments))
	for id, a := range s.attachments {
		out = append(out, session.AttachmentRecord{
			AttachID: id, SessionID: s.sessionID, Kind: a.kind, ConnectedAt: a.connectedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

func (s *ownerState) detachAttachment(attachID string) error {
	a, ok := s.attachments[attachID]
	if !ok {
		return fmt.Errorf("attachment `%s` not found", attachID)
	}
	a.signalDetach()
	return nil
}

func (s *ownerState) detachAll() {
	for _, a := range s.attachments {
		a.signalDetach()
	}
}

// outputBatch is PTY output read by the pump and not yet taken by the
// owner goroutine.
type outputBatch struct {
	mu   sync.Mutex
	data []byte
	// posted is set while a take is queued on the owner goroutine and has
	// not run; data added meanwhile goes out with it.
	posted bool
	// taken is signalled (without blocking) whenever the owner takes the
	// batch, to wake a pump waiting for room.
	taken chan struct{}
}

// add appends data to the batch, queueing a take on the owner unless one is
// already queued. It returns false if the owner has stopped.
func (b *outputBatch) add(data []byte, o *owner) bool {
	b.mu.Lock()
	for b.posted && len(b.data)+len(data) > maxOutputBatch {
		b.mu.Unlock()
		select {
		case <-b.taken:
		case <-o.done:
			return false
		}
		b.mu.Lock()
	}
	// A fresh slice per batch: subscribers keep the one they were given.
	b.data = append(b.data, data...)
	post := !b.posted
	b.posted = true
	b.mu.Unlock()
	if post {
		o.post(func(s *ownerState) {
			if err := s.publishOutput(b.take()); err != nil {
				fmt.Fprintf(os.Stderr, "session worker: failed to publish output: %v\n", err)
			}
		})
	}
	return true
}

func (b *outputBatch) take() []byte {
	b.mu.Lock()
	data := b.data
	b.data = nil
	b.posted = false
	b.mu.Unlock()
	select {
	case b.taken <- struct{}{}:
	default:
	}
	return data
}
