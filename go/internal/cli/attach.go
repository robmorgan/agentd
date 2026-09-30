package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
	"github.com/robmorgan/agentd/go/internal/transport"
)

const (
	// attachEnterSequence pushes the kitty keyboard protocol's
	// "disambiguate" flag, so Ctrl-[ and Esc arrive as different keys.
	attachEnterSequence = "\x1b[>1u"
	// attachRestoreSequence undoes what an agent may have turned on: mouse
	// reporting, bracketed paste, focus events, the kitty keyboard flags
	// (popped) and a hidden cursor.
	attachRestoreSequence = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h"
	attachClearSequence   = "\x1b[2J\x1b[H"
	attachExitTitle       = "agentd"

	// frameQueue bounds the daemon frames read ahead of the terminal.
	frameQueue = 16
)

type attachOutcome int

const (
	outcomeDetached attachOutcome = iota
	outcomeEnded
	outcomeSwitch
)

type attachResult struct {
	outcome attachOutcome
	ended   *protocol.SessionEnded // outcomeEnded
	next    string                 // outcomeSwitch
}

// attachSession attaches the terminal to a session until the user detaches
// or the session ends, following switches to other sessions on the way.
func (c *client) attachSession(id string) error {
	titled := false
	defer func() {
		if titled {
			writeOut(terminalTitleBytes(attachExitTitle))
		}
	}()
	for {
		res, err := c.attachOnce(id, &titled)
		if err != nil {
			return err
		}
		switch res.outcome {
		case outcomeEnded:
			fmt.Println(formatSessionEnd(res.ended))
			return nil
		case outcomeSwitch:
			id = res.next
		default:
			return nil
		}
	}
}

// frame is one response read from an attach stream.
type frame struct {
	resp *protocol.Response
	err  error // nil resp and nil err: the stream ended
}

// attachStream is an attach session's stream, and the goroutine reading its
// frames.
//
// The reader goroutine belongs to the attachStream: stop closes the stream,
// which ends any read in progress, and waits for the goroutine to finish, so
// switching sessions never leaves one behind. The reader never blocks on a
// full channel after stop, since every send also watches done. While the
// channel is full it stops reading, and the stream's flow control pushes
// back on the daemon, which drops output for a lagging attachment rather
// than stall the PTY.
type attachStream struct {
	s      transport.Stream
	frames chan frame
	done   chan struct{}
	exited chan struct{}
}

func startAttachStream(s transport.Stream, r *bufio.Reader) *attachStream {
	a := &attachStream{s: s, frames: make(chan frame, frameQueue), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(a.exited)
		for {
			resp, err := protocol.ReadResponse(r)
			select {
			case a.frames <- frame{resp, err}:
			case <-a.done:
				return
			}
			if resp == nil || err != nil {
				return
			}
		}
	}()
	return a
}

func (a *attachStream) send(req *protocol.Request) error { return protocol.WriteRequest(a.s, req) }

// stop half-closes the stream, which tells the daemon this client detached,
// then closes it and waits for the reader.
func (a *attachStream) stop() {
	a.s.CloseWrite()
	close(a.done)
	a.s.Close()
	<-a.exited
}

// connectAttach opens an attach stream. A session that has already ended
// answers with SessionEnded instead.
func (c *client) connectAttach(id string) (*attachStream, *protocol.Attached, *protocol.SessionEnded, error) {
	s, err := c.open(context.Background())
	if err != nil {
		return nil, nil, nil, err
	}
	cols, rows, pw, ph := terminalGeometry()
	req := &protocol.Request{AttachSession: &protocol.AttachSession{
		SessionID: id,
		Kind:      session.AttachmentAttach,
		Geometry:  protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph},
	}}
	if err := protocol.WriteRequest(s, req); err != nil {
		s.Close()
		return nil, nil, nil, err
	}
	r := bufio.NewReader(s)
	resp, err := protocol.ReadResponse(r)
	switch {
	case err != nil:
		s.Close()
		return nil, nil, nil, err
	case resp == nil:
		s.Close()
		return nil, nil, nil, errors.New("agentd closed the connection")
	case resp.Attached != nil:
		return startAttachStream(s, r), resp.Attached, nil, nil
	case resp.SessionEnded != nil:
		s.Close()
		return nil, nil, resp.SessionEnded, nil
	case resp.Error != nil:
		s.Close()
		return nil, nil, nil, errors.New(resp.Error.Message)
	}
	s.Close()
	return nil, nil, nil, fmt.Errorf("unexpected attach response: %s", describeResponse(resp))
}

// attachOnce attaches to one session. Everything runs on this goroutine,
// fed by three sources: stdin chunks (the process-wide reader), the attach
// stream's frames, and SIGWINCH. Output is written to stdout as it
// arrives; while stdout is slow, the loop, and with it input, waits.
func (c *client) attachOnce(id string, titled *bool) (attachResult, error) {
	stream, attached, ended, err := c.connectAttach(id)
	if err != nil {
		return attachResult{}, err
	}
	if ended != nil {
		return attachResult{outcome: outcomeEnded, ended: ended}, nil
	}
	defer stream.stop()

	writeOut(terminalTitleBytes(id + " - " + attachExitTitle))
	*titled = true
	fmt.Fprintf(os.Stderr, "attached to %s (%s); Ctrl-Y overlay, Ctrl-\\\\ detaches, Ctrl-[/Ctrl-] switch running sessions\n", id, attached.AttachID)
	raw, err := enterRaw(true)
	if err != nil {
		return attachResult{}, fmt.Errorf("failed to set raw terminal mode: %w", err)
	}
	defer func() {
		raw.restore()
		writeOut([]byte(attachRestoreSequence))
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	writeOut(attachStartupBytes(attached.Snapshot))
	keys := stdinChunks()
	var parser attachParser
	var ov *overlay
	redraw := false
	for {
		// The overlay is drawn over the session's screen, which nothing
		// else writes to while it is open, so it only changes after input
		// or a resize.
		if ov != nil && redraw {
			ov.draw()
		}
		redraw = false
		select {
		case chunk, ok := <-keys:
			if !ok {
				return attachResult{outcome: outcomeDetached}, nil
			}
			redraw = true
			if ov != nil {
				res, closed, err := c.overlayInput(ov, chunk, stream)
				if err != nil || res != nil {
					return deref(res), err
				}
				if closed {
					ov = nil
				}
				continue
			}
			for _, action := range parser.push(chunk) {
				switch action.kind {
				case actionData:
					if err := stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: action.data}}); err != nil {
						return attachResult{}, err
					}
				case actionDetach:
					return attachResult{outcome: outcomeDetached}, nil
				case actionOpenOverlay:
					ov = newOverlay(c, id)
					if err := ov.open(); err != nil {
						return attachResult{}, err
					}
				case actionPreviousSession, actionNextSession:
					next, err := c.adjacentLiveSession(id, action.kind == actionPreviousSession)
					if err != nil {
						return attachResult{}, err
					}
					if next != "" {
						return attachResult{outcome: outcomeSwitch, next: next}, nil
					}
				}
				if ov != nil {
					// Like the terminal, the overlay takes over from here;
					// the rest of this chunk is dropped.
					break
				}
			}
		case <-winch:
			redraw = true
			if err := stream.send(resizeRequest()); err != nil {
				return attachResult{}, err
			}
		case f := <-stream.frames:
			res, done, err := handleFrame(f, ov != nil)
			if err != nil || done {
				return res, err
			}
		}
	}
}

func deref(r *attachResult) attachResult {
	if r == nil {
		return attachResult{}
	}
	return *r
}

// handleFrame handles one frame of a live attachment. While the overlay is
// open, output is dropped: the screen is redrawn from a snapshot when it
// closes.
func handleFrame(f frame, overlayOpen bool) (attachResult, bool, error) {
	switch {
	case f.err != nil:
		return attachResult{}, true, f.err
	case f.resp == nil || f.resp.EndOfStream != nil:
		return attachResult{outcome: outcomeDetached}, true, nil
	case f.resp.PtyOutput != nil:
		if !overlayOpen {
			writeOut(f.resp.PtyOutput.Data)
		}
		return attachResult{}, false, nil
	case f.resp.SessionEnded != nil:
		return attachResult{outcome: outcomeEnded, ended: f.resp.SessionEnded}, true, nil
	case f.resp.AttachSnapshot != nil:
		if overlayOpen {
			return attachResult{}, true, errors.New("unexpected attach snapshot response outside overlay restore")
		}
		return attachResult{}, true, errors.New("unexpected attach snapshot response during passthrough")
	case f.resp.Error != nil:
		return attachResult{}, true, errors.New(f.resp.Error.Message)
	}
	return attachResult{}, true, fmt.Errorf("unexpected response: %s", describeResponse(f.resp))
}

// overlayInput feeds input to the open overlay. closed reports that it
// closed and the screen was restored.
func (c *client) overlayInput(ov *overlay, chunk []byte, stream *attachStream) (res *attachResult, closed bool, err error) {
	for _, ev := range ov.keys.push(chunk) {
		out, err := ov.handle(ev)
		if err != nil {
			return nil, false, err
		}
		switch out.kind {
		case overlayClose:
			restored, ended, err := restoreSnapshot(stream)
			if err != nil {
				return nil, false, err
			}
			if ended != nil {
				return &attachResult{outcome: outcomeEnded, ended: ended}, false, nil
			}
			writeOut([]byte("\x1b[?25h"))
			writeOut(attachStartupBytes(restored))
			return nil, true, nil
		case overlaySwitch:
			return &attachResult{outcome: outcomeSwitch, next: out.session}, false, nil
		}
	}
	return nil, false, nil
}

// restoreSnapshot asks for the current screen after the overlay closes,
// skipping output that arrives first. Input waits (bounded, in the stdin
// channel) until it is done.
func restoreSnapshot(stream *attachStream) ([]byte, *protocol.SessionEnded, error) {
	if err := stream.send(&protocol.Request{AttachSnapshot: protocol.Empty}); err != nil {
		return nil, nil, err
	}
	for f := range stream.frames {
		switch {
		case f.err != nil:
			return nil, nil, f.err
		case f.resp == nil:
			return nil, nil, errors.New("agentd closed the attach connection")
		case f.resp.AttachSnapshot != nil:
			return f.resp.AttachSnapshot.Data, nil, nil
		case f.resp.PtyOutput != nil:
		case f.resp.SessionEnded != nil:
			return nil, f.resp.SessionEnded, nil
		case f.resp.EndOfStream != nil:
			return nil, nil, errors.New("attach stream ended while restoring snapshot")
		case f.resp.Error != nil:
			return nil, nil, errors.New(f.resp.Error.Message)
		default:
			return nil, nil, fmt.Errorf("unexpected response while restoring attach snapshot: %s", describeResponse(f.resp))
		}
	}
	return nil, nil, errors.New("agentd closed the attach connection")
}

func resizeRequest() *protocol.Request {
	cols, rows, pw, ph := terminalGeometry()
	return &protocol.Request{AttachResize: &protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph}}
}

func attachStartupBytes(snapshot []byte) []byte {
	out := make([]byte, 0, len(attachEnterSequence)+len(attachClearSequence)+len(snapshot))
	out = append(out, attachEnterSequence...)
	out = append(out, attachClearSequence...)
	return append(out, snapshot...)
}

// terminalTitleBytes sets both the window and icon titles.
func terminalTitleBytes(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07\x1b]2;" + title + "\x07")
}

// writeOut writes to the terminal. Errors are ignored: there is nowhere to
// report them, and a terminal that went away ends the attachment through
// its input.
func writeOut(b []byte) {
	for len(b) > 0 {
		n, err := os.Stdout.Write(b)
		if err != nil {
			return
		}
		b = b[n:]
	}
}

// adjacentLiveSession is the running session before or after id in list
// order, wrapping around, or "" when there is none.
func (c *client) adjacentLiveSession(id string, previous bool) (string, error) {
	sessions, err := c.listSessions(0)
	if err != nil {
		return "", err
	}
	var running []session.Record
	for _, s := range sessions {
		if s.Status == session.StatusRunning {
			running = append(running, s)
		}
	}
	return adjacentSessionIn(orderedSessions(running), id, previous), nil
}

func adjacentSessionIn(sessions []*session.Record, id string, previous bool) string {
	if len(sessions) <= 1 {
		return ""
	}
	current := -1
	for i, s := range sessions {
		if s.SessionID == id {
			current = i
		}
	}
	if current < 0 {
		return ""
	}
	target := (current + 1) % len(sessions)
	if previous {
		target = (current + len(sessions) - 1) % len(sessions)
	}
	if target == current {
		return ""
	}
	return sessions[target].SessionID
}

func formatSessionEnd(e *protocol.SessionEnded) string {
	switch e.Status {
	case session.StatusFailed:
		if e.Error != nil {
			return fmt.Sprintf("session %s failed: %s", e.SessionID, *e.Error)
		}
		return fmt.Sprintf("session %s failed", e.SessionID)
	case session.StatusExited, session.StatusUnknownRecovered:
		if e.ExitCode != nil {
			return fmt.Sprintf("session %s finished (exit %d)", e.SessionID, *e.ExitCode)
		}
		return fmt.Sprintf("session %s finished", e.SessionID)
	}
	return fmt.Sprintf("session %s ended", e.SessionID)
}
