package worker

import (
	"bytes"
	"testing"

	"go.mitchellh.com/libghostty"
)

// FuzzSnapshotRestore feeds arbitrary output into the shadow terminal,
// takes the reattach snapshot at a point where the worker could take it, and
// checks that a fresh terminal restored from it matches: screen, scrollback,
// cursor, modes, styles, colors, and the primary screen under the alternate
// one. Run it with
//
//	go test ./internal/worker -run '^$' -fuzz FuzzSnapshotRestore -fuzztime 5m
//
// Without -fuzz it runs the seed corpus (and testdata/fuzz) as a test.
func FuzzSnapshotRestore(f *testing.F) {
	for _, seed := range []string{
		"", "hello\r\nworld", "\x1b[31mred\x1b[0m", "\x1b[?1049h\x1b[2Jalt", "\x1b[3;10r\x1b[5;1Hx",
		"\x1b[?2026h", "\x1b]8;;https://example.com\x1b\\link", "界é", "\x1b[5 q", "\x1b(0lqk",
		"\x1b[?47h\x1b[44mx", "\x1b]4;1;rgb:12/34/56\x1b\\", "\x1b[>3u", "\x1b[?69h\x1b[5;20s",
	} {
		f.Add([]byte(seed))
	}
	// Slices of real programs' output, starting at arbitrary bytes.
	for _, s := range loadStreams(f) {
		for i := range 4 {
			from := len(s.data) * i / 4
			f.Add(s.data[from:min(from+4096, len(s.data))])
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		ts, err := newTerminalState(fuzzCols, fuzzRows, maxScrollbackBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer ts.close()
		ts.feed(data)
		// The worker only snapshots between sequences (ownerState.atGround);
		// a stream that ends inside one has nothing more to complete it.
		if !ts.atGround() {
			return
		}
		// A cursor in origin mode is a known limit of the formatter (see
		// TestSnapshotKnownLimits).
		if origin, _ := ts.term.Mode(libghostty.ModeOrigin); origin {
			return
		}
		snap, err := ts.snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		client, err := newClientTerminal(fuzzCols, fuzzRows)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		client.VTWrite([]byte("\x1b[2J\x1b[H"))
		client.VTWrite(snap)
		if d := diffRestored(ts.term, client); d != "" {
			if hasWideNarrowCell(ts.term) || pendingWrapOverBlank(ts.term) || alternateScrollback(ts.term) || graphemeModeChanged(ts.term, data) {
				return // a known limit; see TestSnapshotKnownLimits
			}
			t.Fatalf("restored terminal differs:\n%s\nsnapshot: %q", d, snap)
		}
		if screen, _ := ts.term.ActiveScreen(); screen == libghostty.ScreenAlternate {
			ts.term.VTWrite([]byte(exitAltScreen))
			client.VTWrite([]byte(exitAltScreen))
			if d := diffRestored(ts.term, client); d != "" {
				t.Fatalf("primary screen under the alternate screen differs:\n%s\nsnapshot: %q", d, snap)
			}
		}
	})
}

// hasWideNarrowCell reports whether the screen has a two-column cell holding
// an ASCII character. libghostty makes one when a wide character is printed
// while the DEC line-drawing charset is in use (it maps the character but
// keeps its width), and the formatter writes it as one column.
func hasWideNarrowCell(term *libghostty.Terminal) bool {
	cols, rows := must(term.Cols()), must(term.Rows())
	for y := range uint32(rows) {
		for x := range cols {
			cell := must(must(term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: x, Y: y})).Cell())
			if must(cell.Wide()) == libghostty.CellWideWide && must(cell.Codepoint()) < 0x80 {
				return true
			}
		}
	}
	return false
}

// graphemeModeChanged reports whether the stream may have switched
// grapheme cluster mode (2027): it is set now, or the stream mentions it
// (it may have been switched off again). Text printed before a switch was
// split into cells under the other mode, which the snapshot, setting the
// mode first, does not reproduce. Only checked once a restore differs, so
// a rough check is enough.
func graphemeModeChanged(term *libghostty.Terminal, data []byte) bool {
	if on, _ := term.Mode(libghostty.ModeGraphemeCluster); on {
		return true
	}
	return bytes.Contains(data, []byte("2027"))
}

// alternateScrollback reports whether the alternate screen holds
// scrollback, which only CSI 22 J (scroll the screen into scrollback) can
// give it and nothing replayed into a client can.
func alternateScrollback(term *libghostty.Terminal) bool {
	screen, _ := term.ActiveScreen()
	return screen == libghostty.ScreenAlternate && must(term.ScrollbackRows()) > 0
}

// pendingWrapOverBlank reports whether the cursor has a pending wrap over a
// blank cell, which the formatter cannot restore.
func pendingWrapOverBlank(term *libghostty.Terminal) bool {
	if !must(term.CursorPendingWrap()) {
		return false
	}
	ref := must(term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: must(term.CursorX()), Y: uint32(must(term.CursorY()))}))
	return len(must(ref.Graphemes())) == 0
}

// A small terminal makes wrapping, scrolling and scrollback cheap to reach.
const (
	fuzzCols = 40
	fuzzRows = 10
)
