package worker

import (
	"bytes"
	"fmt"
	"testing"
)

// Benchmarks of the shadow terminal: the cost of feeding it PTY output,
// which bounds how fast the worker drains the PTY, and of the reattach
// snapshot. BENCHMARKS.md records results.

// floodChunk is 8 KiB of line-oriented output, the largest chunk pumpPty
// hands the owner.
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

// BenchmarkVTWriteChunkSize feeds the same 64 KiB of colored compiler output
// in chunks of different sizes, one VTWrite per chunk as the worker makes
// one per PTY read. The 1-byte case is all call overhead; the difference
// between the sizes is the cost of crossing into libghostty per call.
func BenchmarkVTWriteChunkSize(b *testing.B) {
	var data []byte
	for _, s := range loadStreams(b) {
		if s.name == "clang" {
			data = s.data[:64<<10]
		}
	}
	for _, size := range []int{1, 64, 1024, 8 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			ts, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
			if err != nil {
				b.Fatal(err)
			}
			defer ts.close()
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				for i := 0; i < len(data); i += size {
					ts.term.VTWrite(data[i:min(i+size, len(data))])
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64((len(data)+size-1)/size), "ns/call")
		})
	}
}

// BenchmarkTerminalSnapshot measures the reattach snapshot cost with ~1 MB of
// scrollback, on the primary screen and with the alternate screen up (which
// also copies the terminal to format the primary screen under it).
func BenchmarkTerminalSnapshot(b *testing.B) {
	for _, tc := range []struct {
		name  string
		after string
	}{
		{"primary", ""},
		{"alternate", "\x1b[?1049h\x1b[2Jan editor\x1b[H"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ts, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
			if err != nil {
				b.Fatal(err)
			}
			defer ts.close()
			chunk := floodChunk()
			for range 128 {
				ts.feed(chunk)
			}
			ts.feed([]byte(tc.after))
			b.ReportAllocs()
			var size int
			for b.Loop() {
				snap, err := ts.snapshot()
				if err != nil {
					b.Fatal(err)
				}
				size = len(snap)
			}
			b.ReportMetric(float64(size), "snapshot-bytes")
		})
	}
}

// BenchmarkRecordedStreamSnapshot measures the snapshot of each recorded
// program's final screen, at the size it was recorded at.
func BenchmarkRecordedStreamSnapshot(b *testing.B) {
	for _, s := range loadStreams(b) {
		b.Run(s.name, func(b *testing.B) {
			ts, err := newTerminalState(streamCols, streamRows, maxScrollbackBytes)
			if err != nil {
				b.Fatal(err)
			}
			defer ts.close()
			// Stop before the program exits, while its screen is up: at
			// the last time the alternate screen was entered, if it was.
			data := s.data
			if i := bytes.LastIndex(data, []byte("\x1b[?1049l")); i > 0 {
				data = data[:i]
			}
			ts.feed(data)
			b.ReportAllocs()
			var size int
			for b.Loop() {
				snap, err := ts.snapshot()
				if err != nil {
					b.Fatal(err)
				}
				size = len(snap)
			}
			b.ReportMetric(float64(size), "snapshot-bytes")
			b.ReportMetric(float64(len(data)), "stream-bytes")
		})
	}
}

// BenchmarkNewTerminal measures creating and freeing a session's shadow
// terminal.
func BenchmarkNewTerminal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		ts, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
		if err != nil {
			b.Fatal(err)
		}
		ts.close()
	}
}
