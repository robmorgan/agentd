package worker

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
	"go.mitchellh.com/libghostty"
)

// A snapshot with a scrollback cap restores the screen as a full one does,
// and only the last rows of scrollback: what the client ends up with
// formats as this terminal does under the same cap.
func TestSnapshotWithBoundedScrollback(t *testing.T) {
	var lines strings.Builder
	for i := range 100 {
		fmt.Fprintf(&lines, "\x1b[3%dmline %d\x1b[0m\r\n", i%8, i)
	}
	for _, tc := range []struct {
		name       string
		stream     string
		scrollback int
	}{
		{"no scrollback", "hello", 10},
		{"screen only", lines.String(), 0},
		{"some scrollback", lines.String(), 10},
		{"all of it, asked for exactly", lines.String(), 100 - 24 + 1},
		{"more than there is", lines.String(), 1000},
		{"a wrapped line across the cut", lines.String() + strings.Repeat("w", 200) + "\r\n" + strings.Repeat("x\r\n", 30), 2},
		{"alternate screen up", lines.String() + "\x1b[?1049h\x1b[2Jan editor", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestTerminal(t, 80, 24)
			ts.feed([]byte(tc.stream))
			snap, err := ts.snapshot(tc.scrollback)
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

			if d := diffBoundedRestore(ts.term, client, tc.scrollback); d != "" {
				t.Fatalf("restored terminal differs:\n%s", d)
			}
			// Leave the alternate screen in both, so the primary screen
			// and its scrollback are compared too.
			ts.term.VTWrite([]byte(exitAltScreen))
			client.VTWrite([]byte(exitAltScreen))
			if d := diffBoundedRestore(ts.term, client, tc.scrollback); d != "" {
				t.Fatalf("primary screen differs:\n%s", d)
			}
		})
	}
}

// An attach that caps its scrollback gets only that much in its snapshot,
// and a snapshot asked for later on the stream (an overlay closing) has
// none: the client's terminal already holds it.
func TestAttachWithBoundedScrollback(t *testing.T) {
	h := startWorker(t)
	h.sendInput("flood\n")
	h.eventually("flood to finish", func() bool { return strings.Contains(h.history(), "flood-done") })

	scrollbackOf := func(snapshot []byte) uint {
		t.Helper()
		client, err := newClientTerminal(defaultGeometry.Cols, defaultGeometry.Rows)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		client.VTWrite(snapshot)
		return must(client.ScrollbackRows())
	}

	c, attached := h.attachWith(protocol.AttachSession{
		Features:       []string{protocol.CapAttachScrollback},
		ScrollbackRows: 50,
	})
	if !attached.HasFeature(protocol.CapAttachScrollback) {
		t.Fatalf("attached features = %v", attached.Features)
	}
	if got := scrollbackOf(attached.Snapshot); got != 50 {
		t.Fatalf("snapshot scrollback rows = %d, want 50", got)
	}
	if !bytes.Contains(attached.Snapshot, []byte("flood-done")) || bytes.Contains(attached.Snapshot, []byte("line 100 ")) {
		t.Fatalf("snapshot is not the end of the output: %.200q", attached.Snapshot)
	}

	c.send(&protocol.Request{AttachSnapshot: protocol.Empty})
	for {
		resp := c.read()
		if resp.PtyOutput != nil {
			continue
		}
		if resp.AttachSnapshot == nil {
			t.Fatalf("unexpected response %#v", resp)
		}
		if got := scrollbackOf(resp.AttachSnapshot.Data); got != 0 {
			t.Fatalf("repaint snapshot scrollback rows = %d, want 0", got)
		}
		break
	}

	// Without the feature the snapshot has all of the scrollback.
	full := h.attach(defaultGeometry)
	if got := scrollbackOf(full.snapshot); got <= 50 {
		t.Fatalf("full snapshot scrollback rows = %d", got)
	}
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}

// diffBoundedRestore describes how restored, a terminal restored from a
// snapshot of orig with at most scrollback rows of scrollback, differs from
// what it should be: orig, but with only the last of those rows.
func diffBoundedRestore(orig, restored *libghostty.Terminal, scrollback int) string {
	want := withoutKnownLimits(imageOf(orig))
	sel, _ := must2(scrollbackSelection(orig, scrollback))
	want.ScrollbackRows = min(want.ScrollbackRows, uint(scrollback))
	want.Text = formatSelection(orig, libghostty.FormatterFormatPlain, true, sel)
	want.Styled = formatSelection(orig, libghostty.FormatterFormatVT, false, sel)
	return diffImages(want, withoutKnownLimits(imageOf(restored)))
}

// formatSelection is formatContent restricted to sel (nil for everything).
func formatSelection(term *libghostty.Terminal, format libghostty.FormatterFormat, trim bool, sel *libghostty.Selection) string {
	f := must(libghostty.NewFormatter(term,
		libghostty.WithFormatterFormat(format),
		libghostty.WithFormatterTrim(trim),
		libghostty.WithFormatterUnwrap(false),
		libghostty.WithFormatterSelection(sel)))
	defer f.Close()
	return must(f.FormatString())
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}

// FuzzSnapshotScrollback checks the scrollback cap on its own: a terminal
// restored from a capped snapshot is one restored from a full snapshot of
// the same terminal, less the older scrollback. Both go through the same
// formatter, so what a VT stream cannot restore (FuzzSnapshotRestore's
// known limits) affects them alike.
func FuzzSnapshotScrollback(f *testing.F) {
	for _, seed := range []string{
		"", "a\r\nb\r\nc\r\nd\r\ne\r\nf\r\ng\r\nh\r\ni\r\nj\r\nk\r\nl\r\n",
		"\x1b[31m" + strings.Repeat("wrapped ", 30) + "\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n",
		strings.Repeat("x\r\n", 20) + "\x1b[?1049h\x1b[2Jalt",
	} {
		f.Add([]byte(seed), uint8(3))
	}
	f.Fuzz(func(t *testing.T, data []byte, scrollback uint8) {
		ts, err := newTerminalState(fuzzCols, fuzzRows, maxScrollbackBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer ts.close()
		ts.feed(data)
		if !ts.atGround() {
			return
		}
		restore := func(n int) *libghostty.Terminal {
			snap, err := ts.snapshot(n)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			client, err := newClientTerminal(fuzzCols, fuzzRows)
			if err != nil {
				t.Fatal(err)
			}
			client.VTWrite([]byte("\x1b[2J\x1b[H"))
			client.VTWrite(snap)
			return client
		}
		full, capped := restore(allScrollback), restore(int(scrollback))
		defer full.Close()
		defer capped.Close()
		if d := diffBoundedRestore(full, capped, int(scrollback)); d != "" {
			t.Fatalf("restored with %d rows of scrollback:\n%s", scrollback, d)
		}
	})
}
