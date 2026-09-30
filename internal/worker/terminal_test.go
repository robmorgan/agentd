package worker

import (
	"fmt"
	"testing"
)

// floodChunk is 8 KiB of line-oriented output, the size pumpPty reads.
func floodChunk() []byte {
	var chunk []byte
	for i := 0; len(chunk) < 8192; i++ {
		chunk = append(chunk, fmt.Sprintf("line %d padding padding padding padding\r\n", i)...)
	}
	return chunk[:8192]
}

// BenchmarkTerminalFeed measures shadow-terminal throughput, which bounds how
// fast the worker can drain the PTY. A Debug build of libghostty-vt is ~50 KB/s.
func BenchmarkTerminalFeed(b *testing.B) {
	ts, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
	if err != nil {
		b.Fatal(err)
	}
	defer ts.close()
	chunk := floodChunk()
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for b.Loop() {
		ts.feed(chunk)
	}
}

// BenchmarkTerminalSnapshot measures the reattach snapshot cost with ~1 MB of
// scrollback.
func BenchmarkTerminalSnapshot(b *testing.B) {
	ts, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
	if err != nil {
		b.Fatal(err)
	}
	defer ts.close()
	chunk := floodChunk()
	for range 128 {
		ts.feed(chunk)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ts.format(true); err != nil {
			b.Fatal(err)
		}
	}
}
