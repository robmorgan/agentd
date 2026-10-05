package worker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.mitchellh.com/libghostty"
)

// These tests check that the reattach snapshot restores a terminal: feed a
// stream into the worker's shadow terminal, take the snapshot, write it into
// a fresh terminal of the same size (as the CLI writes it into the user's
// terminal) and compare the two.

// terminalImage is everything about a terminal the snapshot should restore.
type terminalImage struct {
	Cols, Rows     uint16
	Screen         libghostty.TerminalScreen
	CursorX        uint16
	CursorY        uint16
	CursorVisible  bool
	PendingWrap    bool
	CursorStyle    string
	CursorShape    libghostty.CursorVisualStyle
	CursorBlinking bool
	Modes          string
	KittyFlags     libghostty.KittyKeyFlags
	ScrollbackRows uint
	Palette        libghostty.Palette
	DynamicColors  string
	// Text is the screen and its scrollback as plain text, with trailing
	// blanks trimmed: a blank cell and a space look the same. Styled is
	// the same rendered with SGR styles, which covers the styles in the
	// scrollback.
	Text   string
	Styled string
	// Cells is every visible cell's text and resolved colors and style,
	// read cell by cell rather than through the formatter that made the
	// snapshot, so the formatter cannot hide its own omissions. Blank
	// cells (empty, or a space) only show as a space there; Blanks lists
	// those whose colors or attributes still show, such as a background.
	Cells  string
	Blanks string
	// RowFlags marks the visible rows that are soft-wrapped onto the
	// next (w) and that hold hyperlinks (h).
	RowFlags string
}

var restoredModes = []struct {
	name string
	mode libghostty.Mode
}{
	{"KAM", libghostty.ModeKAM}, {"IRM", libghostty.ModeInsert}, {"SRM", libghostty.ModeSRM},
	{"LNM", libghostty.ModeLinefeed}, {"DECCKM", libghostty.ModeDECCKM},
	{"DECCOLM", libghostty.Mode132Column}, {"DECSCLM", libghostty.ModeSlowScroll},
	{"DECSCNM", libghostty.ModeReverseColors}, {"DECOM", libghostty.ModeOrigin},
	{"DECAWM", libghostty.ModeWraparound}, {"DECARM", libghostty.ModeAutorepeat},
	{"X10Mouse", libghostty.ModeX10Mouse}, {"CursorBlink", libghostty.ModeCursorBlinking},
	{"DECTCEM", libghostty.ModeCursorVisible}, {"Mode3", libghostty.ModeEnableMode3},
	{"ReverseWrap", libghostty.ModeReverseWrap}, {"Alt47", libghostty.ModeAltScreenLegacy},
	{"DECNKM", libghostty.ModeKeypadKeys}, {"DECBKM", libghostty.ModeBackarrowKeyMode},
	{"DECLRMM", libghostty.ModeLeftRightMargin}, {"NormalMouse", libghostty.ModeNormalMouse},
	{"ButtonMouse", libghostty.ModeButtonMouse}, {"AnyMouse", libghostty.ModeAnyMouse},
	{"Focus", libghostty.ModeFocusEvent}, {"UTF8Mouse", libghostty.ModeUTF8Mouse},
	{"SGRMouse", libghostty.ModeSGRMouse}, {"AltScroll", libghostty.ModeAltScroll},
	{"URxvtMouse", libghostty.ModeURxvtMouse}, {"SGRPixels", libghostty.ModeSGRPixelsMouse},
	{"NumLock", libghostty.ModeNumlockKeypad}, {"AltEscPrefix", libghostty.ModeAltEscPrefix},
	{"AltSendsEsc", libghostty.ModeAltSendsEsc}, {"ReverseWrapExt", libghostty.ModeReverseWrapExt},
	{"Alt1047", libghostty.ModeAltScreen}, {"Alt1049", libghostty.ModeAltScreenSave},
	{"BracketedPaste", libghostty.ModeBracketedPaste}, {"SyncOutput", libghostty.ModeSyncOutput},
	{"Grapheme", libghostty.ModeGraphemeCluster}, {"ColorScheme", libghostty.ModeColorSchemeReport},
	{"Visibility", libghostty.ModeVisibilityReport}, {"InBandResize", libghostty.ModeInBandResize},
	{"PasteEvents", libghostty.ModePasteEvents},
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func styleString(s *libghostty.Style) string {
	return fmt.Sprintf("fg=%v bg=%v ul=%v/%v b=%v i=%v f=%v bl=%v inv=%v invis=%v s=%v o=%v",
		s.FgColor(), s.BgColor(), s.Underline(), s.UnderlineColor(), s.Bold(), s.Italic(), s.Faint(),
		s.Blink(), s.Inverse(), s.Invisible(), s.Strikethrough(), s.Overline())
}

func formatContent(term *libghostty.Terminal, format libghostty.FormatterFormat, trim bool) string {
	f := must(libghostty.NewFormatter(term,
		libghostty.WithFormatterFormat(format),
		libghostty.WithFormatterTrim(trim),
		libghostty.WithFormatterUnwrap(false)))
	defer f.Close()
	return must(f.FormatString())
}

// cellsOf renders the visible screen cell by cell: one line per row, each
// cell as its text and its resolved foreground, background and style.
func cellsOf(term *libghostty.Terminal) (cells, blanks, rowFlags string, shape libghostty.CursorVisualStyle, blinking bool) {
	rs := must(libghostty.NewRenderState())
	defer rs.Close()
	if err := rs.Update(term); err != nil {
		panic(err)
	}
	rows := must(libghostty.NewRenderStateRowIterator())
	defer rows.Close()
	if err := rs.RowIterator(rows); err != nil {
		panic(err)
	}
	rc := must(libghostty.NewRenderStateRowCells())
	defer rc.Close()
	var b, bl, rf strings.Builder
	for y := 0; rows.Next(); y++ {
		row := must(rows.Raw())
		if must(row.Wrap()) {
			fmt.Fprintf(&rf, "%dw ", y)
		}
		if must(row.Hyperlink()) {
			fmt.Fprintf(&rf, "%dh ", y)
		}
		if err := rows.Cells(rc); err != nil {
			panic(err)
		}
		for x := 0; rc.Next(); x++ {
			// Colors are compared resolved, so a cell erased with a
			// background color and a space with that background match.
			text := string(must(rc.AppendGraphemes(nil)))
			st := must(rc.Style())
			fg, bg := must(rc.FgColor()), must(rc.BgColor())
			attrs := fmt.Sprintf("%v %v ul=%v/%v b=%v i=%v f=%v bl=%v inv=%v invis=%v s=%v o=%v",
				fg, bg, st.Underline(), st.UnderlineColor(), st.Bold(), st.Italic(), st.Faint(),
				st.Blink(), st.Inverse(), st.Invisible(), st.Strikethrough(), st.Overline())
			if text == "" || text == " " {
				// An empty cell and a space look the same, and the
				// formatter writes spaces for gaps before later text.
				b.WriteString(" |")
				if bg != nil || st.Inverse() || st.Underline() != libghostty.UnderlineNone ||
					st.Strikethrough() || st.Overline() {
					fmt.Fprintf(&bl, "%d,%d %s\n", y, x, attrs)
				}
				continue
			}
			fmt.Fprintf(&b, "%q %s|", text, attrs)
		}
		b.WriteByte('\n')
	}
	return b.String(), bl.String(), rf.String(), must(rs.CursorVisualStyle()), must(rs.CursorBlinking())
}

func imageOf(term *libghostty.Terminal) terminalImage {
	var modes []string
	for _, m := range restoredModes {
		if must(term.Mode(m.mode)) {
			modes = append(modes, m.name)
		}
	}
	img := terminalImage{
		Cols:           must(term.Cols()),
		Rows:           must(term.Rows()),
		Screen:         must(term.ActiveScreen()),
		CursorX:        must(term.CursorX()),
		CursorY:        must(term.CursorY()),
		CursorVisible:  must(term.CursorVisible()),
		PendingWrap:    must(term.CursorPendingWrap()),
		CursorStyle:    styleString(must(term.CursorStyle())),
		Modes:          strings.Join(modes, ","),
		KittyFlags:     must(term.KittyKeyboardFlags()),
		ScrollbackRows: must(term.ScrollbackRows()),
		Palette:        *must(term.ColorPalette()),
		DynamicColors: fmt.Sprint(must(term.ColorForeground()), must(term.ColorBackground()),
			must(term.ColorCursor())),
		Text:   formatContent(term, libghostty.FormatterFormatPlain, true),
		Styled: formatContent(term, libghostty.FormatterFormatVT, false),
	}
	img.Cells, img.Blanks, img.RowFlags, img.CursorShape, img.CursorBlinking = cellsOf(term)
	return img
}

// diffImages describes how got differs from want, or returns "".
func diffImages(want, got terminalImage) string {
	var b strings.Builder
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := range wv.NumField() {
		w, g := wv.Field(i).Interface(), gv.Field(i).Interface()
		if reflect.DeepEqual(w, g) {
			continue
		}
		name := wv.Type().Field(i).Name
		if ws, ok := w.(string); ok {
			fmt.Fprintf(&b, "%s differs: %s\n", name, firstDifference(ws, g.(string)))
			continue
		}
		fmt.Fprintf(&b, "%s: want %v, got %v\n", name, w, g)
	}
	return b.String()
}

// firstDifference shows where two long strings first diverge.
func firstDifference(want, got string) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	from := max(i-60, 0)
	return fmt.Sprintf("at byte %d (lengths %d, %d)\n  want …%q\n  got  …%q",
		i, len(want), len(got), want[from:min(i+60, len(want))], got[from:min(i+60, len(got))])
}

// newClientTerminal is a terminal standing in for the user's.
func newClientTerminal(cols, rows uint16) (*libghostty.Terminal, error) {
	return libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackBytes(maxScrollbackBytes),
	)
}

// exitAltScreen is written to both terminals to compare their primary
// screens once the alternate screen has been checked.
const exitAltScreen = "\x1b[?1049l\x1b[?1047l\x1b[?47l"

// probe is written to both terminals after the comparison, to check state
// that only shows in how later output lands: tab stops, the scrolling
// region, origin mode, charsets, the pen and hyperlink, insert mode.
const probe = "\tP\x1b[Hq\x1b[99B\r\n\n\nend"

// checkRestore takes ts's reattach snapshot, restores it into a fresh
// terminal of the same size, and reports any difference. If the alternate
// screen is active it then leaves it in both and compares the primary
// screens too. It leaves ts on the primary screen.
func checkRestore(ts *terminalState) error {
	snap, err := ts.snapshot()
	if err != nil {
		return err
	}
	cols, rows := must(ts.term.Cols()), must(ts.term.Rows())
	client, err := newClientTerminal(cols, rows)
	if err != nil {
		return err
	}
	defer client.Close()
	// The CLI clears the screen and homes the cursor before the snapshot.
	client.VTWrite([]byte("\x1b[2J\x1b[H"))
	client.VTWrite(snap)

	want := imageOf(ts.term)
	if d := diffImages(want, imageOf(client)); d != "" {
		return fmt.Errorf("restored %s screen differs:\n%s", screenName(want.Screen), d)
	}
	if want.Screen == libghostty.ScreenAlternate {
		ts.term.VTWrite([]byte(exitAltScreen))
		client.VTWrite([]byte(exitAltScreen))
		if d := diffImages(imageOf(ts.term), imageOf(client)); d != "" {
			return fmt.Errorf("restored primary screen (under the alternate screen) differs:\n%s", d)
		}
	}
	ts.term.VTWrite([]byte(probe))
	client.VTWrite([]byte(probe))
	if d := diffImages(imageOf(ts.term), imageOf(client)); d != "" {
		return fmt.Errorf("later output lands differently:\n%s", d)
	}
	return nil
}

func screenName(s libghostty.TerminalScreen) string {
	if s == libghostty.ScreenAlternate {
		return "alternate"
	}
	return "primary"
}

func newTestTerminal(t testing.TB, cols, rows uint16) *terminalState {
	t.Helper()
	ts, err := newTerminalState(cols, rows, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.close)
	return ts
}

func TestSnapshotRestores(t *testing.T) {
	var scrollback strings.Builder
	for i := range 200 {
		fmt.Fprintf(&scrollback, "\x1b[3%dmline %d\x1b[0m of scrollback\r\n", i%8, i)
	}
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"empty", ""},
		{"primary screen", "hello\r\nworld\r\n$ "},
		{"cursor position", "a\r\nb\x1b[5;17H"},
		{"cursor at the right edge with a pending wrap", strings.Repeat("x", 80)},
		{"pending wrap on a wide character", strings.Repeat("x", 78) + "界"},
		{"hidden cursor", "text\x1b[?25l"},
		{"blinking bar cursor", "\x1b[5 q"},
		{"steady underline cursor", "\x1b[4 q"},
		{"blinking block cursor", "\x1b[1 q"},
		{"styles", "\x1b[1mbold\x1b[0m \x1b[3;4mitalic underline\x1b[0m \x1b[38;5;208;48;2;1;2;3mcolor\x1b[0m \x1b[4:3;58;5;1mcurly\x1b[0m \x1b[7;9;53minverse\x1b[0m"},
		{"pen style carries over", "\x1b[1;31mstill red when the next output arrives"},
		// The probe's text lands inside the open hyperlink.
		{"hyperlink in progress", "before \x1b]8;;https://example.com\x1b\\"},
		{"scrollback", scrollback.String() + "$ "},
		{"scrolling region", "\x1b[3;10rinside\x1b[5;1H"},
		{"left and right margins", "\x1b[?69h\x1b[10;30s\x1b[2;12Hy"},
		{"scrolling region under the alternate screen", "primary\x1b[2;30r\x1b[?1049h\x1b[2Jalt\x1b[5;5H"},
		{"modes", "\x1b[?1h\x1b[?2004h\x1b[?1000h\x1b[?1006h\x1b[?1004h\x1b[4h\x1b=\x1b[?7l"},
		{"kitty keyboard", "\x1b[>3u"},
		{"charset", "\x1b(0lqk\x1b(B"},
		{"tabstops", "\x1b[3g\x1b[5G\x1bH\x1b[1G\tx"},
		{"palette", "\x1b]4;1;rgb:12/34/56\x1b\\\x1b[31mred"},
		{"default colors", "\x1b]10;rgb:aa/bb/cc\x1b\\\x1b]11;rgb:01/02/03\x07\x1b]12;rgb:ff/00/00\x07"},
		{"cleared screen over scrollback", scrollback.String() + "\x1b[H\x1b[2J$ "},
		{"cursor below the last text", scrollback.String() + "done\r\n"},
		{"alternate screen (1049)", scrollback.String() + "$ vim\x1b[?1049h\x1b[H\x1b[2Jalt content\x1b[3;4H"},
		{"alternate screen (1047)", "primary\x1b[?1047h\x1b[2Jalt content"},
		{"alternate screen (47)", "primary\x1b[?47halt content"},
		{"alternate screen entered with two modes", "primary\x1b[?1047h\x1b[?47h alt"},
		{"cursor shape under the alternate screen", "\x1b[6 q\x1b[?47h"},
		{"cursor shapes of both screens", "\x1b[4 q\x1b[?1049h\x1b[2 q"},
		{"cursor shape set on the alternate screen", "\x1b[?47h\x1b[6 q0"},
		{"primary pen under the alternate screen (1049)", "\x1b[41;1mred\x1b[?1049h\x1b[0m\x1b[2J\x1b[1;5Hx\x1b[3;3H"},
		{"primary pen under the alternate screen (1047)", "\x1b[41;1mred\x1b[?1047h\x1b[0m\x1b[2J\x1b[1;5Hx\x1b[3;3H"},
		{"primary pen under the alternate screen (47)", "\x1b[41;1mred\x1b[?47h\x1b[0m\x1b[2J\x1b[1;5Hx\x1b[3;3H"},
		{"primary charset under the alternate screen", "\x1b(0lqk\x1b[?1049h\x1b(B\x1b[2J\x1b[1;5Hx"},
		{"primary hyperlink under the alternate screen", "before \x1b]8;;https://example.com\x1b\\\x1b[?1049h\x1b]8;;\x1b\\\x1b[2J\x1b[1;5Hx"},
		{"alternate screen with styles", "\x1b[32mgreen primary\x1b[0m\r\n\x1b[?1049h\x1b[2J\x1b[1;1H\x1b[44mblue alt\x1b[0m"},
		{"wide and combining characters", "界é👍🏽 ok"},
		{"synchronized output in progress", "\x1b[?2026hhalf a frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestTerminal(t, 80, 24)
			ts.feed([]byte(tc.stream))
			if err := checkRestore(ts); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// withoutKnownLimits leaves out the parts of img that a snapshot does not
// restore exactly (TestSnapshotKnownLimits shows each): the colors of blank
// cells, soft wraps and hyperlinks.
func withoutKnownLimits(img terminalImage) terminalImage {
	img.Blanks, img.RowFlags = "", ""
	return img
}

// TestSnapshotKnownLimits pins what the snapshot does not restore, so a
// libghostty update that fixes one shows up here (move the case into
// TestSnapshotRestores then) and a regression that widens one does not
// hide behind it. ARCHITECTURE.md explains each.
// TestSnapshotOriginModeUnderAlternateScreen checks that the copy the
// primary screen is formatted from, which leaves the alternate screen with
// 1049 and so restores the saved cursor, does not hand the saved cursor's
// origin mode to the client: origin mode belongs to the terminal, and the
// alternate screen's formatting would not clear it. Only the current screen
// is compared, since the saved cursor itself is a known limit.
func TestSnapshotOriginModeUnderAlternateScreen(t *testing.T) {
	for _, stream := range []string{
		"\x1b[5;20r\x1b[?6h\x1b7\x1b[?6l\x1b[?47hx",
		"\x1b[?6h\x1b7\x1b[?6l\x1b[?1047hx",
		"\x1b7\x1b[?6h\x1b[?47hx",
	} {
		ts := newTestTerminal(t, 80, 24)
		ts.feed([]byte(stream))
		snap, err := ts.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		client, err := newClientTerminal(80, 24)
		if err != nil {
			t.Fatal(err)
		}
		client.VTWrite([]byte("\x1b[2J\x1b[H"))
		client.VTWrite(snap)
		if d := diffRestored(ts.term, client); d != "" {
			t.Errorf("%q: restored terminal differs:\n%s\nsnapshot: %q", stream, d, snap)
		}
		client.Close()
	}
}

func TestSnapshotKnownLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		// probe is written to both terminals after the snapshot.
		probe string
		// differs is the image fields that differ after the probe.
		differs []string
	}{
		{
			// The formatter skips blank cells that carry only a
			// background (from an erase with a background color set),
			// and treats rows of them as blank lines.
			name: "background of erased cells", stream: "\x1b[44m\x1b[2J\x1b[Hblue\x1b[0m",
			differs: []string{"Blanks"},
		},
		{
			// The formatter writes the gap before later text on a row as
			// spaces in the pen of the text before the gap.
			name: "pen of the text before a gap", stream: "\x1b[41mred\x1b[0m\x1b[10Gplain",
			differs: []string{"Blanks"},
		},
		{
			// The cursor is restored with an absolute CUP after the
			// origin mode is on, so it lands relative to the margin.
			name: "cursor in origin mode", stream: "\x1b[5;20r\x1b[?6h\x1b[3;3Hx",
			differs: []string{"CursorY"},
		},
		{
			// Rows are written with a line break after each, so a
			// soft-wrapped line comes back as separate lines (which a
			// later resize does not reflow).
			name: "soft-wrapped line", stream: strings.Repeat("wrap", 30),
			differs: []string{"RowFlags"},
		},
		{
			// The formatter only emits the hyperlink the pen is in, not
			// the hyperlinks of text already on the screen.
			name: "hyperlinked text", stream: "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ after",
			differs: []string{"RowFlags"},
		},
		{
			// libghostty maps a wide character printed in the DEC
			// line-drawing charset but keeps it two columns wide; the
			// formatter writes the mapped character in one.
			name: "wide character in the line-drawing charset", stream: "\x1b(0危00\x1b(B",
			differs: []string{"Cells"},
		},
		{
			// The formatter restores a pending wrap by printing the cell
			// under the cursor again, which does nothing for a blank
			// cell (here the wrap comes back with a restored cursor).
			name: "pending wrap over a blank cell", stream: "\x1b[1;80Hx\x1b7\x1b[2K\x1b8",
			differs: []string{"PendingWrap"},
		},
		{
			// The cursor saved with DECSC (or 1048) is not formatted.
			name: "saved cursor", stream: "\x1b[5;5H\x1b7\x1b[H", probe: "\x1b8",
			differs: []string{"CursorX", "CursorY"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestTerminal(t, 80, 24)
			ts.feed([]byte(tc.stream))
			snap, err := ts.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			client, err := newClientTerminal(80, 24)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.VTWrite([]byte("\x1b[2J\x1b[H"))
			client.VTWrite(snap)
			ts.term.VTWrite([]byte(tc.probe))
			client.VTWrite([]byte(tc.probe))
			want, got := imageOf(ts.term), imageOf(client)
			var differs []string
			wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
			for i := range wv.NumField() {
				if !reflect.DeepEqual(wv.Field(i).Interface(), gv.Field(i).Interface()) {
					differs = append(differs, wv.Type().Field(i).Name)
				}
			}
			if !reflect.DeepEqual(differs, tc.differs) {
				t.Fatalf("fields that differ = %v, want %v:\n%s", differs, tc.differs, diffImages(want, got))
			}
		})
	}
}
