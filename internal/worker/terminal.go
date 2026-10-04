package worker

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"go.mitchellh.com/libghostty"

	"github.com/robmorgan/agentd/internal/protocol"
)

// continuationMaxBytes bounds the unfinished escape sequence (or UTF-8
// character) the shadow terminal tracks so a handoff can carry it to the next
// worker image exactly. A longer one (a huge OSC 52 clipboard write, say)
// makes the continuation unavailable until the parser is back at ground, and
// a handoff attempted meanwhile is refused rather than restored inexactly.
const continuationMaxBytes = 1 << 20

// terminalState wraps a libghostty terminal that shadows the PTY stream so a
// reattaching client can be rehydrated with a snapshot of the screen. Its
// parser also reports the effects agents use to ask for attention (bell,
// desktop notifications, title changes); they are collected during a feed
// and returned with it, so the FFI boundary stays one call per PTY read.
type terminalState struct {
	term    *libghostty.Terminal
	pending [][]byte
	size    libghostty.SizeReportSize
	effects terminalEffects

	// lastFeed is when PTY output last reached the terminal, which
	// decides when its scrollback is idle enough to compress.
	lastFeed time.Time
	// compressed reports that a compression pass finished while the
	// terminal's compression activity token was compressedToken; until
	// the token changes there is nothing more to compress.
	compressed      bool
	compressedToken uint64
}

// Scrollback compression. libghostty can compress the pages of scrollback
// it holds, which a session that has scrolled far keeps for as long as it
// runs: a full 10 MB scrollback goes from about 9.4 MiB resident to under
// 1 MiB. Formatting a snapshot (an attach, a resync, history) still works on
// compressed pages, takes about 10% longer, and leaves them decompressed
// until the next quiet tick compresses them again.
//
// Compression runs on the owner goroutine (the terminal is not safe to share)
// from the worker's one-second activity tick, once output has been quiet for
// compressIdleAfter, in incremental steps that stop after compressStepBudget
// so PTY output and requests queued behind it wait at most that long. A busy
// session is never compressed: its pages would be decompressed again at once.
const (
	compressIdleAfter  = 3 * time.Second
	compressStepBudget = 5 * time.Millisecond
)

// maxNotificationsPerFeed bounds the notifications kept from one PTY read; a
// program flooding them gains nothing past the first few.
const maxNotificationsPerFeed = 4

// terminalEffects are the attention-related effects of one feed.
type terminalEffects struct {
	bells         int
	notifications []libghostty.TerminalDesktopNotification
	titleChanged  bool
}

func newTerminalState(cols, rows uint16, maxScrollbackBytes uint) (*terminalState, error) {
	s := &terminalState{size: libghostty.SizeReportSize{Rows: rows, Columns: cols}}
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackBytes(maxScrollbackBytes),
		libghostty.WithContinuationMaxBytes(continuationMaxBytes),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create terminal state: %w", err)
	}
	s.adopt(term)
	return s, nil
}

// adopt makes term the shadow terminal, wiring the callbacks through which
// it answers terminal queries and reports bells, notifications and title
// changes.
func (s *terminalState) adopt(term *libghostty.Terminal) {
	term.SetEffectWritePty(func(_ *libghostty.Terminal, data []byte) {
		s.pending = append(s.pending, append([]byte(nil), data...))
	})
	term.SetEffectSize(func(_ *libghostty.Terminal) (libghostty.SizeReportSize, bool) {
		return s.size, true
	})
	term.SetEffectBell(func(_ *libghostty.Terminal) { s.effects.bells++ })
	term.SetEffectDesktopNotification(func(_ *libghostty.Terminal, n libghostty.TerminalDesktopNotification) {
		if len(s.effects.notifications) < maxNotificationsPerFeed {
			s.effects.notifications = append(s.effects.notifications, n)
		}
	})
	term.SetEffectTitleChanged(func(_ *libghostty.Terminal) { s.effects.titleChanged = true })
	s.term = term
}

// handoffState is the shadow terminal's state carried across a worker
// handoff: libghostty's binary snapshot, which is exact (every screen and
// its scrollback, cursor, modes, and the parser's unfinished input), and as
// a fallback the VT rendering a reattaching client gets, in case the next
// image cannot decode the snapshot (its format is not yet stable across
// libghostty versions).
type terminalHandoff struct {
	Snapshot []byte
	VT       []byte
	Size     libghostty.SizeReportSize
}

func (s *terminalState) handoff() (*terminalHandoff, error) {
	snapshot, err := s.term.Snapshot()
	if err != nil {
		// Most likely an unfinished sequence longer than
		// continuationMaxBytes, which the snapshot cannot carry.
		return nil, fmt.Errorf("failed to snapshot the terminal (it may be in the middle of a very long escape sequence; try again): %w", err)
	}
	vt, err := s.format(true)
	if err != nil {
		return nil, err
	}
	return &terminalHandoff{Snapshot: snapshot, VT: vt, Size: s.size}, nil
}

// restoreTerminal rebuilds the shadow terminal from a handoff, from the
// exact binary snapshot when it decodes. Otherwise it rebuilds it from the VT
// rendering, which keeps the screen and scrollback text and most modes but
// not, for example, an unfinished escape sequence, and reports why in
// inexact. err is set only if neither worked.
func restoreTerminal(h *terminalHandoff) (s *terminalState, inexact error, err error) {
	term, snapErr := decodeSnapshot(h.Snapshot)
	if snapErr == nil {
		limit := maxScrollbackBytes
		if snapErr = term.SetScrollbackMaxBytes(&limit); snapErr != nil {
			term.Close()
		}
	}
	if snapErr == nil {
		s = &terminalState{size: h.Size}
		s.adopt(term)
		return s, nil, nil
	}
	cols, rows := h.Size.Columns, h.Size.Rows
	if cols == 0 || rows == 0 {
		cols, rows = defaultPtyCols, defaultPtyRows
	}
	s, err = newTerminalState(cols, rows, maxScrollbackBytes)
	if err != nil {
		return nil, nil, errors.Join(snapErr, err)
	}
	s.size = h.Size
	s.feed(h.VT)
	return s, snapErr, nil
}

func decodeSnapshot(data []byte) (*libghostty.Terminal, error) {
	if len(data) == 0 {
		return nil, errors.New("no terminal snapshot")
	}
	d, err := libghostty.NewSnapshotDecoderBytes(data)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	// Keep tracking the parser's unfinished input in the restored
	// terminal, so the next handoff carries it too.
	if err := d.SetMaxContinuationBytes(continuationMaxBytes); err != nil {
		return nil, err
	}
	if err := d.SetRetainContinuation(true); err != nil {
		return nil, err
	}
	// A restored terminal would otherwise hold all its scrollback
	// uncompressed, however compressed it was in the previous image.
	if err := d.SetCompressHistory(true); err != nil {
		return nil, err
	}
	term, err := d.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to decode the terminal snapshot: %w", err)
	}
	return term, nil
}

// feed parses PTY output and returns any responses the terminal wants to
// write back to the PTY (mode reports, size reports, and so on), and the
// attention effects the output had.
func (s *terminalState) feed(data []byte) ([][]byte, terminalEffects) {
	s.lastFeed = time.Now()
	s.term.VTWrite(data)
	return s.take()
}

// title is the title the program last set (OSC 0/2).
func (s *terminalState) title() string {
	t, err := s.term.Title()
	if err != nil {
		return ""
	}
	return t
}

// feedUntilGround parses only as much of data as it takes for the parser to
// be between sequences again, and returns how much that was (all of data if
// it never gets there) along with any responses and effects. See
// ownerState.atGround.
func (s *terminalState) feedUntilGround(data []byte) (int, [][]byte, terminalEffects) {
	s.lastFeed = time.Now()
	n, _ := s.term.VTWriteUntilGround(data)
	writes, effects := s.take()
	return n, writes, effects
}

// take returns and resets the responses and effects collected by a feed.
func (s *terminalState) take() ([][]byte, terminalEffects) {
	writes, effects := s.pending, s.effects
	s.pending, s.effects = nil, terminalEffects{}
	return writes, effects
}

// atGround reports whether the parser is between sequences and UTF-8
// characters, so a snapshot taken now is an exact boundary in the stream.
func (s *terminalState) atGround() bool {
	ground, err := s.term.VTGround()
	return err != nil || ground
}

func (s *terminalState) resize(cols, rows uint16, cellWidthPx, cellHeightPx uint32) error {
	s.size = libghostty.SizeReportSize{Rows: rows, Columns: cols, CellWidth: cellWidthPx, CellHeight: cellHeightPx}
	return s.term.Resize(cols, rows, cellWidthPx, cellHeightPx)
}

// format renders the active screen and its scrollback for history: plain
// text, or VT sequences that reproduce the styles.
func (s *terminalState) format(vt bool) ([]byte, error) {
	// Formatting reads, and so decompresses, every page of scrollback
	// without changing the compression activity token: compress again
	// at the next quiet tick.
	s.compressed = false
	if !vt {
		return formatScreen(s.term, libghostty.FormatterFormatPlain, true)
	}
	colors, err := changedColors(s.term)
	if err != nil {
		return nil, err
	}
	screen, err := formatScreen(s.term, libghostty.FormatterFormatVT, true)
	if err != nil {
		return nil, err
	}
	return append(colors, screen...), nil
}

// snapshot returns the VT bytes that repaint a client's terminal, already
// cleared and of the same size, as this terminal is now: the primary screen
// and its scrollback, the alternate screen when it is active, the cursor
// (position, visibility, pen style), terminal modes, scrolling region, tab
// stops, keyboard modes, charsets and any colors the program changed.
// ARCHITECTURE.md lists what a VT stream cannot restore.
func (s *terminalState) snapshot() ([]byte, error) {
	out, err := changedColors(s.term)
	if err != nil {
		return nil, err
	}
	screen, err := s.term.ActiveScreen()
	if err != nil {
		return nil, err
	}
	if screen == libghostty.ScreenAlternate {
		primary, err := s.primaryUnderAlternate()
		if err != nil {
			return nil, err
		}
		out = append(out, primary...)
	}
	active, err := formatScreen(s.term, libghostty.FormatterFormatVT, true)
	if err != nil {
		return nil, err
	}
	if screen == libghostty.ScreenAlternate {
		active = resetPenAfterSwitch(active)
	}
	out = append(out, active...)
	return appendCursorShape(out, s.term)
}

// penReset puts the pen back to its defaults: SGR, hyperlink, charsets
// (G0 ASCII, shifted in) and character protection.
const penReset = "\x1b[0m\x1b]8;;\x1b\\\x1b(B\x0f\x1b[0\"q"

// resetPenAfterSwitch makes the alternate screen's contents start from a
// default pen. The formatter writes screen contents assuming one, but the
// client still has the pen the primary screen's formatting just restored:
// switching screens keeps it, and 1049 also saves it with the cursor, so it
// has to stay until the switch. The formatter emits the modes (which make
// the switch) and then the tab stops, which start by clearing them all
// (TBC, CSI 3 g); the reset goes in between.
func resetPenAfterSwitch(formatted []byte) []byte {
	i := bytes.Index(formatted, []byte("\x1b[3g"))
	if i < 0 {
		return formatted
	}
	out := make([]byte, 0, len(formatted)+len(penReset))
	out = append(out, formatted[:i]...)
	out = append(out, penReset...)
	return append(out, formatted[i:]...)
}

// appendCursorShape appends DECSCUSR for a bar or underline cursor, which
// the formatter does not emit. A block cursor is left alone: it is also
// what a terminal shows when no program has asked for a shape, so the
// client keeps its own default. The shape is only exposed through a render
// state, which copies the visible screen once.
func appendCursorShape(out []byte, term *libghostty.Terminal) ([]byte, error) {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	if err := rs.Update(term); err != nil {
		return nil, err
	}
	shape, err := rs.CursorVisualStyle()
	if err != nil {
		return nil, err
	}
	var n int
	switch shape {
	case libghostty.CursorVisualStyleBar:
		n = 6
	case libghostty.CursorVisualStyleUnderline:
		n = 4
	default:
		return out, nil
	}
	if blinking, _ := term.Mode(libghostty.ModeCursorBlinking); blinking {
		n-- // the blinking variant of each shape
	}
	return fmt.Appendf(out, "\x1b[%d q", n), nil
}

// primaryUnderAlternate formats the primary screen while a program (an
// editor, or a TUI such as Claude Code) has the alternate screen up.
// libghostty's formatter only formats the active screen, so this copies the
// terminal through its binary snapshot, leaves the alternate screen in the
// copy (which restores the cursor 1049 saved), and formats that. The client
// gets the primary screen first; the alternate screen's own formatting then
// switches it over with the same mode the program used, so the primary
// screen and its scrollback are there again when the program exits.
//
// This costs a copy of the whole terminal, scrollback included, on each
// snapshot taken while the alternate screen is up; BENCHMARKS.md has the
// numbers.
func (s *terminalState) primaryUnderAlternate() ([]byte, error) {
	data, err := s.term.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	dec, err := libghostty.NewSnapshotDecoderBytes(data)
	if err != nil {
		return nil, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	defer dec.Close()
	cp, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	defer cp.Close()

	exit := "\x1b[?47l"
	if on, _ := s.term.Mode(libghostty.ModeAltScreenSave); on {
		exit = "\x1b[?1049l"
	} else if on, _ := s.term.Mode(libghostty.ModeAltScreen); on {
		exit = "\x1b[?1047l"
	}
	cp.VTWrite([]byte(exit))
	// The scrolling region belongs to the terminal, not to a screen, and
	// the alternate screen's formatting sets it. Set here as well, it
	// would already be in force while the alternate screen's contents are
	// written from the top, and scroll them.
	return formatScreen(cp, libghostty.FormatterFormatVT, false)
}

// formatScreen formats term's active screen. In VT form it also emits the
// state that affects how later output renders (modes, cursor, pen, the
// scrolling region if region is set, and so on), but not the palette:
// changedColors emits only the colors a program
// changed, where the formatter would overwrite all 256 of the client
// terminal's colors with libghostty's defaults. It does not emit the
// working directory (OSC 7) either, which names a directory on the
// session's host, not the client's.
//
// The formatter leaves out blank rows at the bottom of the screen. Written
// into a client from the top, the rows it does emit then end up too low
// whenever there is scrollback: after a clear screen, say, the prompt
// would land on the bottom row instead of the top, with old output above
// it. So formatScreen adds the missing line feeds after the screen
// contents, before the cursor and pen are restored, so the client scrolls
// exactly as far as this terminal has. The formatter separates rows with
// CRLF and emits no other CRLF, so the count of rows it emitted is the
// count of CRLFs plus one.
func formatScreen(term *libghostty.Terminal, format libghostty.FormatterFormat, region bool) ([]byte, error) {
	out, err := runFormatter(term, format, true, region)
	if err != nil || format != libghostty.FormatterFormatVT {
		return out, err
	}
	total, err := term.TotalRows()
	if err != nil {
		return nil, err
	}
	missing := int(total) - 1 - bytes.Count(out, crlf)
	if missing <= 0 {
		return out, nil
	}
	// The same formatting without the trailing extras is the prefix of
	// out that ends with the screen contents.
	contents, err := runFormatter(term, format, false, false)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(out, contents) {
		return out, nil
	}
	padded := make([]byte, 0, len(out)+missing*len(crlf))
	padded = append(padded, contents...)
	padded = append(padded, bytes.Repeat(crlf, missing)...)
	return append(padded, out[len(contents):]...), nil
}

var crlf = []byte("\r\n")

// runFormatter formats term's active screen. The modes and tab stops come
// before the screen contents; with trailing set, the state emitted after
// them (the scrolling region if region is also set, keyboard modes, then
// cursor, pen, hyperlink, protection, kitty keyboard flags and charsets)
// follows.
func runFormatter(term *libghostty.Terminal, format libghostty.FormatterFormat, trailing, region bool) ([]byte, error) {
	f, err := libghostty.NewFormatter(term,
		libghostty.WithFormatterFormat(format),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterUnwrap(false),
		libghostty.WithFormatterExtraModes(true),
		// Tab stops also matter for the alternate screen: setting them
		// ends with CUP home, which is where the screen contents start.
		libghostty.WithFormatterExtraTabstops(true),
		libghostty.WithFormatterExtraScrollingRegion(trailing && region),
		libghostty.WithFormatterExtraKeyboard(trailing),
		libghostty.WithFormatterExtraCursor(trailing),
		libghostty.WithFormatterExtraStyle(trailing),
		libghostty.WithFormatterExtraHyperlink(trailing),
		libghostty.WithFormatterExtraProtection(trailing),
		libghostty.WithFormatterExtraKittyKeyboard(trailing),
		libghostty.WithFormatterExtraCharsets(trailing),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create formatter: %w", err)
	}
	defer f.Close()
	return f.Format()
}

// compressIdle compresses scrollback if output has been quiet long enough
// and the scrollback changed (or was read by format) since the last complete
// pass; see compressIdleAfter.
func (s *terminalState) compressIdle(now time.Time) {
	if now.Sub(s.lastFeed) < compressIdleAfter {
		return
	}
	token, err := s.term.CompressionActivity()
	if err != nil || (s.compressed && token == s.compressedToken) {
		return
	}
	deadline := now.Add(compressStepBudget)
	for {
		result, err := s.term.Compress(libghostty.TerminalCompressionIncremental)
		if err != nil || result != libghostty.TerminalCompressionPending {
			// Done, or not possible on this platform (or failing):
			// nothing more to do until the scrollback changes.
			s.compressed = true
			s.compressedToken, _ = s.term.CompressionActivity()
			return
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// memory is what the terminal's screens hold, from libghostty.
func (s *terminalState) memory() (*protocol.TerminalMemory, error) {
	m, err := s.term.MemoryUsage()
	if err != nil {
		return nil, err
	}
	return &protocol.TerminalMemory{
		Pages:           m.Primary.Pages + m.Alternate.Pages,
		ResidentBytes:   m.Primary.ResidentBytes + m.Alternate.ResidentBytes,
		CompressedPages: m.Primary.CompressedPages + m.Alternate.CompressedPages,
		CompressedBytes: m.Primary.CompressedBytes + m.Alternate.CompressedBytes,
	}, nil
}

// scrollbackRows is the number of rows of history above the screen.
func (s *terminalState) scrollbackRows() uint64 {
	n, err := s.term.ScrollbackRows()
	if err != nil {
		return 0
	}
	return uint64(n)
}

// changedColors returns OSC sequences setting the palette entries and the
// default foreground, background and cursor colors that the program has
// changed (OSC 4, 10, 11, 12), and nothing for colors it left alone, so the
// client keeps its own theme.
func changedColors(term *libghostty.Terminal) ([]byte, error) {
	current, err := term.ColorPalette()
	if err != nil {
		return nil, err
	}
	defaults, err := term.ColorPaletteDefault()
	if err != nil {
		return nil, err
	}
	var out []byte
	for i := range current {
		if current[i] != defaults[i] {
			c := current[i]
			out = fmt.Appendf(out, "\x1b]4;%d;rgb:%02x/%02x/%02x\x1b\\", i, c.R, c.G, c.B)
		}
	}
	for _, dyn := range []struct {
		osc               int
		current, fallback func() (*libghostty.ColorRGB, error)
	}{
		{10, term.ColorForeground, term.ColorForegroundDefault},
		{11, term.ColorBackground, term.ColorBackgroundDefault},
		{12, term.ColorCursor, term.ColorCursorDefault},
	} {
		c, err := dyn.current()
		if err != nil {
			return nil, err
		}
		d, err := dyn.fallback()
		if err != nil {
			return nil, err
		}
		if c != nil && (d == nil || *c != *d) {
			out = fmt.Appendf(out, "\x1b]%d;rgb:%02x/%02x/%02x\x1b\\", dyn.osc, c.R, c.G, c.B)
		}
	}
	return out, nil
}

func (s *terminalState) close() {
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
}
