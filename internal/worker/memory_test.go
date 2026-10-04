package worker

import (
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"testing"

	"github.com/robmorgan/agentd/internal/procstat"
)

// fillText is enough 80-column lines to fill a session's scrollback to its
// byte limit at the worker's default size.
func fillText() []byte {
	var out []byte
	for i := 1; i <= 60_000; i++ {
		out = fmt.Appendf(out, "%08d the quick brown fox jumps over the lazy dog 0123456789 abcdefghijklmnopqrstuvwxyz\r\n", i)
	}
	return out
}

// privateMemory is this process's private memory after a full GC, so only
// what is still referenced (and libghostty's memory, which Go does not see)
// is counted.
func privateMemory(tb testing.TB) uint64 {
	goruntime.GC()
	debug.FreeOSMemory()
	p, err := procstat.Read(os.Getpid())
	if err != nil {
		tb.Skip(err)
	}
	return p.Private
}

// BenchmarkTerminalMemory measures what one shadow terminal costs, which is
// libghostty memory outside the Go heap: the per-session terminal cost a
// worker pays. "empty" is a new session's terminal (10,000 of them, the
// roadmap's terminal-state workload); "full" is one whose scrollback is at
// its byte limit, the most a session's terminal holds. Run with
//
//	go test ./internal/worker -run '^$' -bench TerminalMemory -benchtime 1x
func BenchmarkTerminalMemory(b *testing.B) {
	fill := fillText()
	for _, tc := range []struct {
		name  string
		count int
		fill  bool
	}{
		{"empty", 10_000, false},
		{"full", 50, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				before := privateMemory(b)
				heapBefore := procstat.ReadGoRuntime().HeapBytes
				terms := make([]*terminalState, 0, tc.count)
				var rows uint64
				for range tc.count {
					t, err := newTerminalState(defaultPtyCols, defaultPtyRows, maxScrollbackBytes)
					if err != nil {
						b.Fatal(err)
					}
					if tc.fill {
						t.feed(fill)
						rows = t.scrollbackRows()
					}
					terms = append(terms, t)
				}
				after := privateMemory(b)
				heapAfter := procstat.ReadGoRuntime().HeapBytes
				for _, t := range terms {
					t.close()
				}
				released := privateMemory(b)
				n := float64(tc.count)
				b.ReportMetric((float64(after)-float64(before))/n, "B/terminal")
				b.ReportMetric((float64(heapAfter)-float64(heapBefore))/n, "goheap-B/terminal")
				b.ReportMetric((float64(after)-float64(released))/n, "freed-B/terminal")
				if tc.fill {
					b.ReportMetric(float64(rows), "scrollback-rows")
				}
			}
		})
	}
}
