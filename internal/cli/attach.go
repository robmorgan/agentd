package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/internal/config"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

const (
	// attachEnterSequence pushes the kitty keyboard protocol's
	// "disambiguate" flag, so Ctrl-[ and Esc arrive as different keys.
	attachEnterSequence = "\x1b[>1u"
	// attachRestoreSequence undoes what an agent may have turned on: the
	// alternate screen, mouse reporting, bracketed paste, focus events, the
	// kitty keyboard flags (popped) and a hidden cursor.
	attachRestoreSequence = "\x1b[?1049l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h"
	attachClearSequence   = "\x1b[2J\x1b[H"
	// attachDetachSequence clears the session's screen after a detach, so
	// the shell's prompt does not land in the middle of the agent's UI. The
	// session's output stays in the terminal's scrollback.
	attachDetachSequence = "\x1b[H\x1b[2J"
	attachExitTitle      = "agentd"

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
	return c.attachSessionUID(id, "")
}

// attachSessionUID is attachSession for a session known by its UID too
// (one just created), refusing any other incarnation under its name.
func (c *client) attachSessionUID(id, uid string) error {
	titled := false
	defer func() {
		if titled {
			writeOut(terminalTitleBytes(attachExitTitle))
		}
	}()
	for {
		res, err := c.attachOnce(id, uid, &titled)
		if err != nil {
			return err
		}
		switch res.outcome {
		case outcomeEnded:
			fmt.Println(formatSessionEnd(res.ended))
			return nil
		case outcomeSwitch:
			id, uid = res.next, ""
		default:
			return nil
		}
	}
}

var (
	// errSessionRestarting: the session's worker is restarting (a live
	// handoff to a new agentd binary) and ended the attach stream; the
	// session is still there to attach to again.
	errSessionRestarting = errors.New("the session is restarting")
	// errAttachDropped: the attach stream ended without a final frame, so
	// it was cut (the daemon stopped, say) rather than ended by the worker.
	errAttachDropped = errors.New("the attach stream ended unexpectedly")
)

// attachIdentity is what a reattach carries over from the attachment it
// replaces, so that it reaches the same session incarnation and the daemon
// can drop the old attachment at once (see protocol.CapAttachReplace).
type attachIdentity struct {
	// uid is the session incarnation, when the daemon reports it.
	uid string
	// attachID is the attachment the connection had.
	attachID string
}

// update records what the daemon answered an attach with.
func (a *attachIdentity) update(attached *protocol.Attached) {
	if attached.SessionUID != "" {
		a.uid = attached.SessionUID
	}
	a.attachID = attached.AttachID
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
// answers with SessionEnded instead. Cancelling ctx abandons the attempt,
// closing the stream if it was opened.
//
// The attach stream asks for the attach features the daemon's Welcome
// offered (protocol.AttachFeatures): the session's UID, replacing prev's
// attachment when prev knows the UID, resyncs after lag, and at most
// scrollback rows of scrollback in the snapshot (-1 for all).
func (c *client) connectAttach(ctx context.Context, id string, prev attachIdentity, scrollback int) (*attachStream, *protocol.Attached, *protocol.SessionEnded, error) {
	cs, err := c.controlStream(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := c.open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	defer stopWatch()
	cols, rows, pw, ph := terminalGeometry()
	attach := &protocol.AttachSession{
		SessionID: id,
		Kind:      session.AttachmentAttach,
		Geometry:  protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph},
		Features:  protocol.AttachFeatures(cs.features),
	}
	if prev.uid != "" && attach.HasFeature(protocol.CapSessionUID) {
		attach.ExpectUID = prev.uid
		if attach.HasFeature(protocol.CapAttachReplace) {
			attach.Replaces = prev.attachID
		}
	}
	if attach.HasFeature(protocol.CapAttachScrollback) {
		attach.ScrollbackRows = protocol.AllScrollbackRows
		if scrollback >= 0 {
			attach.ScrollbackRows = uint32(scrollback)
		}
	}
	req := &protocol.Request{AttachSession: attach}
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
	case !stopWatch():
		// ctx was cancelled after the answer arrived, closing s.
		return nil, nil, nil, ctx.Err()
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
//
// When the attachment is cut while the session lives on (the connection to
// the daemon is lost, the daemon restarts, or the session's worker restarts
// for an upgrade) the terminal stays in raw mode while attachOnce attaches
// to the same session again, repainting the screen from the new snapshot;
// see reattach.
func (c *client) attachOnce(id, uid string, titled *bool) (res attachResult, err error) {
	ident := attachIdentity{uid: uid}
	stream, attached, ended, err := c.connectAttach(context.Background(), id, ident, c.attachScrollbackRows())
	if err != nil {
		return attachResult{}, err
	}
	if ended != nil {
		return attachResult{outcome: outcomeEnded, ended: ended}, nil
	}
	ident.update(attached)
	defer func() {
		if stream != nil {
			stream.stop()
		}
	}()

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
		if err == nil && res.outcome == outcomeDetached {
			writeOut([]byte(attachDetachSequence))
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	keys := stdinChunks()
	var parser attachParser
	snapshot := attached.Snapshot
	for {
		writeOut(attachStartupBytes(snapshot))
		res, err := c.runAttachment(id, stream, keys, &parser, winch)
		if err == nil || !c.reconnectable(err) {
			return res, err
		}
		stream.stop()
		stream = nil
		var r *attachResult
		stream, attached, r, err = c.reattach(id, ident, keys, &parser, err)
		if stream == nil {
			return deref(r), err
		}
		ident.update(attached)
		snapshot = attached.Snapshot
		// An overlay that was open when the connection dropped may have
		// hidden the cursor.
		writeOut([]byte("\x1b[?25h"))
	}
}

// attachScrollbackRows is the configured cap on the scrollback an attach
// copies into the terminal ([attach] scrollback_rows; -1 for all of it).
func (c *client) attachScrollbackRows() int {
	cfg, err := config.Load(c.paths.Config)
	if err != nil {
		return config.DefaultAttachScrollbackRows
	}
	return cfg.Attach.AttachScrollbackRows()
}

// runAttachment runs one attach stream until it ends, the user detaches or
// switches, or it fails.
func (c *client) runAttachment(id string, stream *attachStream, keys <-chan []byte, parser *attachParser, winch <-chan os.Signal) (attachResult, error) {
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

// Reconnection backs off from the first retry up to the maximum, and keeps
// trying until it succeeds, fails for good, or the user detaches. A laptop
// that slept for hours reconnects when it wakes.
const (
	reconnectFirstRetry = 500 * time.Millisecond
	reconnectMaxRetry   = 5 * time.Second
	// reconnectStartDaemon is the attempt at which a local CLI starts the
	// daemon itself, as any command would, if it has not come back (it
	// crashed, say). A restart or upgrade brings it back well before.
	reconnectStartDaemon = 4
)

// reconnectable reports whether an attachment that failed with err can be
// resumed by attaching again: the session is fine, only the path to it
// broke. Errors a retry cannot fix (a refused or changed key, a session that
// is gone or replaced, a protocol error) are not.
func (c *client) reconnectable(err error) bool {
	switch {
	case errors.Is(err, errSessionRestarting), errors.Is(err, errAttachDropped):
		return true
	case c.host != nil:
		return transport.IsConnectionLost(err)
	}
	// The local daemon's socket is refused or missing while it restarts.
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF)
}

// reattach attaches to session id again after its attachment was cut
// (cause): the connection was lost, the daemon restarted, or the session's
// worker restarted for an upgrade. It shows a status line on the
// bottom row, which the new snapshot paints over. Typed input is dropped
// rather than sent late to an agent the user cannot see; only the detach
// key acts, at any point. It gives up on errors a retry cannot fix: a
// refused or changed key, a session that is gone or was replaced by
// another under the same name (prev's UID), a protocol error.
//
// It returns the new stream and its Attached, or with a nil stream, the
// result or error that ends the attachment.
func (c *client) reattach(id string, prev attachIdentity, keys <-chan []byte, parser *attachParser, cause error) (*attachStream, *protocol.Attached, *attachResult, error) {
	detached := &attachResult{outcome: outcomeDetached}
	delay := time.Duration(0)
	for try := 1; ; try++ {
		if !errors.Is(cause, errSessionRestarting) {
			// The old connection is dead; the next stream dials a new
			// one. A restarting worker leaves the connection alone.
			c.close()
		}
		if c.host == nil && try == reconnectStartDaemon && !c.localDaemonAnswers() {
			_ = c.spawnDaemon()
		}
		writeOut(reconnectStatusBytes(c.reconnectStatus(cause), try, cause))

		retry := make(chan struct{})
		timer := time.AfterFunc(delay, func() { close(retry) })
		if !waitUnlessDetached(retry, keys, parser) {
			timer.Stop()
			return nil, nil, detached, nil
		}

		// The attempt runs on its own goroutine so the detach key works
		// while it waits; cancelling ctx ends it promptly, and it is
		// always waited for, so none outlives reattach.
		type attempt struct {
			stream   *attachStream
			attached *protocol.Attached
			ended    *protocol.SessionEnded
			err      error
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan attempt, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			var a attempt
			// The terminal already holds the scrollback the first
			// snapshot copied, so a reattach repaints only the screen.
			a.stream, a.attached, a.ended, a.err = c.connectAttach(ctx, id, prev, 0)
			result <- a
		}()
		finished := waitUnlessDetached(done, keys, parser)
		cancel()
		a := <-result
		switch {
		case !finished:
			if a.stream != nil {
				a.stream.stop()
			}
			return nil, nil, detached, nil
		case a.err == nil && a.ended != nil:
			return nil, nil, &attachResult{outcome: outcomeEnded, ended: a.ended}, nil
		case a.err == nil:
			return a.stream, a.attached, nil, nil
		case !c.reconnectable(a.err):
			return nil, nil, nil, a.err
		}
		cause = a.err
		delay = min(max(2*delay, reconnectFirstRetry), reconnectMaxRetry)
	}
}

// waitUnlessDetached waits until done fires, dropping typed input. It
// returns false if the user pressed the detach key first, or stdin ended.
func waitUnlessDetached(done <-chan struct{}, keys <-chan []byte, parser *attachParser) bool {
	for {
		select {
		case <-done:
			return true
		case chunk, ok := <-keys:
			if !ok {
				return false
			}
			for _, action := range parser.push(chunk) {
				if action.kind == actionDetach {
					return false
				}
			}
		}
	}
}

// reconnectStatus says what happened to the attachment.
func (c *client) reconnectStatus(cause error) string {
	switch {
	case errors.Is(cause, errSessionRestarting):
		return "session restarting, reattaching"
	case c.host != nil:
		return fmt.Sprintf("connection to %s lost, reconnecting", c.host.Name)
	}
	return "connection to agentd lost, reconnecting"
}

// reconnectStatusBytes draws the reconnect status on the bottom row,
// leaving the cursor where it was.
func reconnectStatusBytes(status string, try int, cause error) []byte {
	cols, rows, _, _ := terminalGeometry()
	msg := status
	if try > 1 {
		msg += fmt.Sprintf(" (attempt %d)", try)
	}
	msg += fmt.Sprintf(": %v. Ctrl-\\ detaches", cause)
	line := []rune(" " + strings.Join(strings.Fields(msg), " ") + " ")
	if cols > 0 && len(line) > int(cols) {
		line = line[:cols]
	}
	return []byte(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", rows, string(line)))
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
	case f.resp == nil:
		return attachResult{}, true, errAttachDropped
	case f.resp.EndOfStream != nil:
		return attachResult{outcome: outcomeDetached}, true, nil
	case f.resp.SessionRestarting != nil:
		return attachResult{}, true, errSessionRestarting
	case f.resp.PtyOutput != nil:
		if !overlayOpen {
			writeOut(f.resp.PtyOutput.Data)
		}
		return attachResult{}, false, nil
	case f.resp.SessionEnded != nil:
		return attachResult{outcome: outcomeEnded, ended: f.resp.SessionEnded}, true, nil
	case f.resp.AttachResync != nil:
		// The worker dropped output while this client lagged; the
		// snapshot replaces the screen, and output continues from it.
		if !overlayOpen {
			writeOut(resyncBytes(f.resp.AttachResync.Data))
		}
		return attachResult{}, false, nil
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
			return nil, nil, errAttachDropped
		case f.resp.SessionRestarting != nil:
			return nil, nil, errSessionRestarting
		case f.resp.AttachSnapshot != nil:
			return f.resp.AttachSnapshot.Data, nil, nil
		case f.resp.PtyOutput != nil, f.resp.AttachResync != nil:
			// Superseded by the snapshot asked for.
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

// resyncBytes repaints the screen from a resync snapshot. Unlike
// attachStartupBytes it pushes no keyboard flags: the attachment's are
// already in place.
func resyncBytes(snapshot []byte) []byte {
	return append([]byte(attachClearSequence), snapshot...)
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
