package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
)

// ownerState is only ever touched by the owner goroutine. All other
// goroutines reach it through owner.do, which serialises access.
type ownerState struct {
	sessionID           string
	ptmx                *os.File
	terminal            *terminalState
	hasClientDimensions bool
	geometry            protocol.Geometry
	nextAttachOrdinal   uint64
	attachments         map[string]*ownerAttachment
	output              *broadcaster
}

type ownerAttachment struct {
	kind        session.AttachmentKind
	connectedAt time.Time
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

func (s *ownerState) writeInput(data []byte) error {
	_, err := s.ptmx.Write(data)
	return err
}

func (s *ownerState) resize(g protocol.Geometry) error {
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Rows: g.Rows, Cols: g.Cols, X: g.PixelWidth, Y: g.PixelHeight}); err != nil {
		return fmt.Errorf("failed to resize pty: %w", err)
	}
	if err := s.terminal.resize(g.Cols, g.Rows, cellWidthPx(g), cellHeightPx(g)); err != nil {
		return err
	}
	s.hasClientDimensions = true
	s.geometry = g
	return nil
}

// publishOutput feeds PTY output through the shadow terminal, answers any
// terminal queries on behalf of absent clients, then fans the raw bytes out.
func (s *ownerState) publishOutput(data []byte) error {
	writes := s.terminal.feed(data)
	if !s.hasLiveAttachTerminal() {
		for _, response := range writes {
			if err := s.writeInput(response); err != nil {
				return err
			}
		}
	}
	s.output.publish(data)
	return nil
}

func (s *ownerState) history(vt bool) (string, error) {
	data, err := s.terminal.format(vt)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *ownerState) snapshot() ([]byte, error) {
	return s.terminal.format(true)
}

type attachResult struct {
	attachID    string
	snapshot    []byte
	connectedAt time.Time
	detach      chan struct{}
	sub         *subscriber
}

func (s *ownerState) attach(kind session.AttachmentKind, g protocol.Geometry) (*attachResult, error) {
	attachID := fmt.Sprintf("%s-%d", kind, s.nextAttachOrdinal)
	s.nextAttachOrdinal++
	connectedAt := time.Now().UTC()

	var snapshot []byte
	var err error
	if s.hasClientDimensions {
		if snapshot, err = s.snapshot(); err != nil {
			return nil, err
		}
		if err = s.resize(g); err != nil {
			return nil, err
		}
	} else {
		if err = s.resize(g); err != nil {
			return nil, err
		}
		if snapshot, err = s.snapshot(); err != nil {
			return nil, err
		}
	}

	a := &ownerAttachment{kind: kind, connectedAt: connectedAt, detach: make(chan struct{})}
	s.attachments[attachID] = a
	return &attachResult{
		attachID:    attachID,
		snapshot:    snapshot,
		connectedAt: connectedAt,
		detach:      a.detach,
		sub:         s.output.subscribe(),
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

func terminateProcess(sessionID string, agentPID *uint32) error {
	if agentPID == nil {
		return fmt.Errorf("session `%s` has no recorded agent pid", sessionID)
	}
	err := syscall.Kill(int(*agentPID), syscall.SIGTERM)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("failed to terminate `%s`: %w", sessionID, err)
}

func pumpPty(reader io.Reader, o *owner) {
	buf := make([]byte, 8192)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			o.post(func(s *ownerState) {
				if err := s.publishOutput(chunk); err != nil {
					fmt.Fprintf(os.Stderr, "session worker: failed to publish output: %v\n", err)
				}
			})
		}
		if err != nil {
			return
		}
	}
}
