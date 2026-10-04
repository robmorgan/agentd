package worker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mitchellh.com/libghostty"

	"github.com/robmorgan/agentd/internal/protocol"
)

// These tests check the snapshot a real worker sends on attach, restored
// into a terminal of the attaching client's size.

// restoreInto writes an attach's snapshot, and then output, into a fresh
// terminal of the client's size, the way the CLI writes them to the user's.
func restoreInto(t *testing.T, g protocol.Geometry, snapshot []byte, output ...[]byte) *libghostty.Terminal {
	t.Helper()
	term, err := newClientTerminal(g.Cols, g.Rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(term.Close)
	term.VTWrite([]byte("\x1b[2J\x1b[H"))
	term.VTWrite(snapshot)
	for _, o := range output {
		term.VTWrite(o)
	}
	return term
}

// rowText returns the text of one row of term's screen.
func rowText(term *libghostty.Terminal, y uint32) string {
	var b strings.Builder
	cols := must(term.Cols())
	for x := range cols {
		ref := must(term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: x, Y: y}))
		cps := must(ref.Graphemes())
		if len(cps) == 0 {
			b.WriteByte(' ')
		}
		for _, cp := range cps {
			b.WriteRune(rune(cp))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// TestReattachSnapshotFitsNewSize reattaches from a terminal of another
// size: the snapshot must be laid out for the new size, with the cursor
// under the last output line, not where it was in the old layout.
func TestReattachSnapshotFitsNewSize(t *testing.T) {
	h := startWorker(t)
	c := h.attach(protocol.Geometry{Cols: 80, Rows: 24})
	var lines strings.Builder
	for i := range 40 {
		fmt.Fprintf(&lines, "line%d\n", i)
	}
	c.input(lines.String())
	c.expectOutput("got:line39")
	c.conn.Close()
	h.eventually("attachment release", func() bool { return len(h.attachments()) == 0 })

	for _, g := range []protocol.Geometry{{Cols: 100, Rows: 30}, {Cols: 60, Rows: 12}} {
		c2 := h.attach(g)
		term := restoreInto(t, g, c2.snapshot)
		y := must(term.CursorY())
		if y == 0 || rowText(term, uint32(y-1)) != "got:line39" {
			t.Fatalf("at %dx%d the cursor is on row %d, below %q, want below got:line39", g.Cols, g.Rows, y, rowText(term, uint32(max(y, 1)-1)))
		}
		c2.conn.Close()
		h.eventually("attachment release", func() bool { return len(h.attachments()) == 0 })
	}
	h.sendInput("done\n")
	h.waitExit()
}

// splitSequenceAgent writes an escape sequence in two parts, on request.
const splitSequenceAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    head) printf 'before \033[3';;
    tail) printf '1mred\033[0m after\n';;
    done) exit 0;;
  esac
done`

// TestAttachWaitsForSequenceEnd attaches while the output so far ends in
// the middle of an escape sequence. The snapshot waits for the rest of the
// sequence; taken at once, the client would print the sequence's tail
// ("1m") as text.
func TestAttachWaitsForSequenceEnd(t *testing.T) {
	old := groundTimeout
	groundTimeout = time.Minute
	t.Cleanup(func() { groundTimeout = old })

	h := startWorkerWith(t, splitSequenceAgent)
	h.eventually("agent start", func() bool { return strings.Contains(h.history(), "ready") })
	a := h.attach(defaultGeometry)
	a.input("head\n")
	a.expectOutput("before \x1b[3")

	attached := make(chan *client, 1)
	go func() { attached <- h.attach(defaultGeometry) }()
	// Give the attach time to reach the worker and start waiting; if it
	// arrives after the tail, there is nothing to wait for anyway.
	time.Sleep(200 * time.Millisecond)
	a.input("tail\n")
	b := <-attached
	b.expectOutput("after")

	term := restoreInto(t, defaultGeometry, b.snapshot, b.output.Bytes())
	text := strings.TrimSpace(rowText(term, 1))
	if text != "before red after" {
		t.Fatalf("row reads %q, want %q (snapshot %q, then output %q)", text, "before red after", b.snapshot, b.output.String())
	}
	ref := must(term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: 7, Y: 1}))
	if st := must(ref.Style()); st.FgColor().Tag != libghostty.StyleColorPalette || st.FgColor().Palette != 1 {
		t.Fatalf("\"red\" is not red: %v", st.FgColor())
	}
	a.input("done\n")
	h.waitExit()
}

// TestAttachDoesNotWaitForeverForSequenceEnd attaches while a program has
// stopped in the middle of an escape sequence: the attach goes ahead after
// groundTimeout.
func TestAttachDoesNotWaitForeverForSequenceEnd(t *testing.T) {
	h := startWorkerWith(t, splitSequenceAgent)
	h.eventually("agent start", func() bool { return strings.Contains(h.history(), "ready") })
	a := h.attach(defaultGeometry)
	a.input("head\n")
	a.expectOutput("before \x1b[3")

	start := time.Now()
	b := h.attach(defaultGeometry)
	if waited := time.Since(start); waited < groundTimeout || waited > groundTimeout+5*time.Second {
		t.Fatalf("attach took %v, want about %v", waited, groundTimeout)
	}
	if !strings.Contains(string(b.snapshot), "before") {
		t.Fatalf("snapshot %q", b.snapshot)
	}
	a.input("done\n")
	h.waitExit()
}
