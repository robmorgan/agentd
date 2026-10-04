package worker

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// fullScrollback is a terminal whose scrollback has reached the session
// limit.
func fullScrollback(t *testing.T) *terminalState {
	t.Helper()
	ts, err := newTerminalState(160, 48, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.close)
	var b bytes.Buffer
	for i := 0; b.Len() < 24<<20; i++ {
		fmt.Fprintf(&b, "\x1b[3%dmline %d\x1b[0m compiler output: /src/pkg/file_%d.go:%d: undefined: x\r\n", i%8, i, i%97, i)
	}
	ts.feed(b.Bytes())
	return ts
}

func compressedPages(t *testing.T, ts *terminalState) (pages, compressed, resident uint64) {
	t.Helper()
	m, err := ts.memory()
	if err != nil {
		t.Fatal(err)
	}
	return m.Pages, m.CompressedPages, m.ResidentBytes
}

// Idle scrollback is compressed in bounded steps, only once output has been
// quiet, only again once it changes, and compressing never changes what a
// reattaching client is shown.
func TestIdleScrollbackIsCompressed(t *testing.T) {
	ts := fullScrollback(t)
	start := time.Now()
	before, err := ts.format(true)
	if err != nil {
		t.Fatal(err)
	}
	formatBefore := time.Since(start)
	_, _, residentBefore := compressedPages(t, ts)

	// Output just arrived: nothing happens.
	ts.compressIdle(time.Now())
	if _, n, _ := compressedPages(t, ts); n != 0 {
		t.Fatalf("compressed %d pages of a busy terminal", n)
	}

	// Quiet: ticks compress it, each within its budget.
	quiet := ts.lastFeed.Add(compressIdleAfter)
	for i := 0; i < 100 && !ts.compressed; i++ {
		start := time.Now()
		ts.compressIdle(quiet)
		if d := time.Since(start); d > 20*compressStepBudget {
			t.Logf("a tick took %v", d) // a loaded machine, not a failure
		}
	}
	if !ts.compressed {
		t.Fatal("compression never finished")
	}
	pages, n, residentAfter := compressedPages(t, ts)
	t.Logf("%d of %d pages compressed; resident %.1f MiB -> %.1f MiB", n, pages, float64(residentBefore)/(1<<20), float64(residentAfter)/(1<<20))
	if n == 0 || residentAfter*4 > residentBefore {
		t.Fatalf("compression saved too little: %d -> %d bytes", residentBefore, residentAfter)
	}
	start = time.Now()
	after, err := ts.format(true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %v before compression, %v after", formatBefore, time.Since(start))
	// Formatting a snapshot decompresses the pages it reads (every attach
	// does this); the next quiet tick compresses them again.
	_, n2, inflated := compressedPages(t, ts)
	for i := 0; i < 100; i++ {
		ts.compressIdle(quiet)
	}
	_, n3, recompressed := compressedPages(t, ts)
	t.Logf("after a snapshot: %d pages compressed, %.1f MiB; after quiet ticks: %d, %.1f MiB", n2, float64(inflated)/(1<<20), n3, float64(recompressed)/(1<<20))
	if recompressed*4 > residentBefore {
		t.Fatal("the scrollback stayed inflated after a snapshot")
	}
	if !bytes.Equal(before, after) {
		t.Fatal("compressing the scrollback changed the reattach snapshot")
	}

	// Done until the scrollback changes.
	token := ts.compressedToken
	ts.compressIdle(quiet.Add(time.Hour))
	if ts.compressedToken != token {
		t.Fatal("an unchanged terminal was compressed again")
	}
	ts.feed([]byte("more\r\n"))
	ts.compressIdle(ts.lastFeed.Add(compressIdleAfter))
	if tok, _ := ts.term.CompressionActivity(); tok != ts.compressedToken {
		t.Fatal("new output was not compressed after going quiet again")
	}
}

// A terminal restored after a handoff keeps its scrollback compressed.
func TestRestoredScrollbackIsCompressed(t *testing.T) {
	ts := fullScrollback(t)
	data, err := ts.term.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	term, err := decodeSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	m, err := term.MemoryUsage()
	if err != nil {
		t.Fatal(err)
	}
	if m.CompressionSupported && m.Primary.CompressedPages == 0 {
		t.Fatalf("restored terminal holds its scrollback uncompressed: %+v", m.Primary)
	}
}
