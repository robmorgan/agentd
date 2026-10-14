package worker

import (
	"fmt"
	"os"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
)

// Size ownership. One attachment, the size owner, decides the session's
// size: only its geometry reaches the PTY and the shadow terminal. The
// others are pending: their geometry is recorded but not applied, so a
// second viewer does not reflow the agent's screen for everyone. The first
// attachment with a size becomes the owner; when the owner leaves, the
// pending attachment used most recently (its last input, else its attach)
// takes over and the session takes its size; a pending attachment can take
// over at any time (AttachSession.TakeSize, AttachTakeSize).
//
// A reattach that replaces the owner (AttachSession.Replaces) inherits the
// ownership, so a client that lost its connection keeps the size. The
// owner's stream can end before its reattach arrives: a handoff ends every
// attachment at once, and so does a daemon restart, which cuts the streams
// it proxies. So the owner's id is kept, reserved, when its stream ends
// during a handoff, or ends without the client saying it is leaving
// (AttachLeave, sent on a detach or a switch; a client without
// CapAttachSizeOwner never says, and leaves at once); its reattach
// inherits the size the same way, and if it has not come back within
// sizeOwnerGrace, the most recently used attachment takes over.
//
// Attachments learn of a change through their sizeNote, a one-slot signal
// that never blocks the owner goroutine: repeated changes merge, and the
// attachment's handler reads the current ownership when it gets to it.

// sizeOwnerGrace is how long the owner's id stays reserved for its
// reattach. It covers a handoff or daemon restart and the CLI's first
// reconnect attempts. A variable for tests.
var sizeOwnerGrace = 10 * time.Second

func (s *ownerState) ownership(attachID string) protocol.SizeOwnership {
	return protocol.SizeOwnership{Owner: attachID == s.sizeOwner, OwnerID: s.sizeOwner, Geometry: s.geometry}
}

// notifySize tells every attachment that the ownership or size changed.
func (s *ownerState) notifySize() {
	for _, a := range s.attachments {
		select {
		case a.sizeNote <- struct{}{}:
		default:
		}
	}
}

func hasSize(g protocol.Geometry) bool { return g.Cols != 0 && g.Rows != 0 }

// setSize records an attachment's geometry (AttachResize) and applies it
// if the attachment owns the size, or becomes the owner when there is none.
func (s *ownerState) setSize(attachID string, g protocol.Geometry) error {
	a, ok := s.attachments[attachID]
	if !ok {
		return nil
	}
	a.geometry = g
	switch {
	case attachID == s.sizeOwner:
		return s.resize(g)
	case s.sizeOwner == "" && hasSize(g):
		return s.takeSize(attachID, g)
	}
	return nil
}

// takeSize makes attachID the size owner at g.
func (s *ownerState) takeSize(attachID string, g protocol.Geometry) error {
	a, ok := s.attachments[attachID]
	if !ok {
		return nil
	}
	a.geometry = g
	changed := s.sizeOwner != attachID
	s.sizeOwner = attachID
	if err := s.resize(g); err != nil {
		return err
	}
	if changed {
		s.notifySize()
	}
	return nil
}

// noteInput records that an attachment was used, for promote. A terminal's
// echo of the status detection query is its reflex, not the user typing
// (see writeInput), so it does not count.
func (s *ownerState) noteInput(attachID string, data []byte, now time.Time) {
	if a, ok := s.attachments[attachID]; ok && !isStatusDetectionReply(data) {
		a.lastUsed = now
	}
}

// noteLeave records that an attachment's client is leaving on purpose.
func (s *ownerState) noteLeave(attachID string) {
	if a, ok := s.attachments[attachID]; ok {
		a.left = true
	}
}

// ownerLeft runs when the owning attachment is gone. During a handoff, or
// when its stream was cut (cut), its id stays reserved for its reattach;
// otherwise the next one takes over.
func (s *ownerState) ownerLeft(cut bool) {
	select {
	case <-s.restart:
		s.armSizeOwnerGrace()
	default:
		if cut {
			s.armSizeOwnerGrace()
		} else {
			s.promote()
		}
	}
}

// armSizeOwnerGrace promotes another attachment if the reserved owner has
// not reattached within sizeOwnerGrace.
func (s *ownerState) armSizeOwnerGrace() {
	if s.owner == nil {
		return
	}
	o := s.owner
	s.sizeReserve++
	reserve := s.sizeReserve
	time.AfterFunc(sizeOwnerGrace, func() {
		o.post(func(s *ownerState) {
			if _, live := s.attachments[s.sizeOwner]; !live && reserve == s.sizeReserve {
				s.promote()
			}
		})
	})
}

// promote makes the most recently used attachment with a size the owner and
// resizes the session to it. With none, there is no owner, the size stays,
// and the next attachment with a size takes it.
func (s *ownerState) promote() {
	next := ""
	var best time.Time
	for id, a := range s.attachments {
		if hasSize(a.geometry) && (next == "" || a.lastUsed.After(best)) {
			next, best = id, a.lastUsed
		}
	}
	if next == "" {
		if s.sizeOwner != "" {
			s.sizeOwner = ""
			s.notifySize()
		}
		return
	}
	if err := s.takeSize(next, s.attachments[next].geometry); err != nil {
		fmt.Fprintf(os.Stderr, "session worker: %v\n", err)
	}
}
