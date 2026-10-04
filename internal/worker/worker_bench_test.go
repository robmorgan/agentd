package worker

import (
	"bufio"
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// Benchmarks of a real worker (Run, a real PTY and /bin/sh) and of its
// output fan-out. BENCHMARKS.md records results.

// blastAgent writes 8 MiB of short lines on each "blast" line it reads.
const blastAgent = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    blast) yes 'line padding padding padding padding' | head -c 8388608; echo blast-done;;
    flood) i=0; while [ $i -lt 20000 ]; do echo "line $i padding padding padding padding"; i=$((i+1)); done; echo flood-done;;
    done) exit 0;;
  esac
done`

// BenchmarkPTYThroughput measures output from the agent's PTY, through the
// shadow terminal and the fan-out, to an attached client over the worker's
// socket.
func BenchmarkPTYThroughput(b *testing.B) {
	h := startWorkerWith(b, blastAgent)
	c := h.attach(protocol.Geometry{Cols: defaultPtyCols, Rows: defaultPtyRows})
	marker := []byte("blast-done")
	b.SetBytes(8 << 20)
	frames := 0
	cpuBefore := processCPU()
	for b.Loop() {
		c.input("blast\n")
		var tail []byte
		for {
			c.conn.SetReadDeadline(time.Now().Add(time.Minute))
			resp, err := protocol.ReadResponse(c.r)
			if err != nil || resp == nil || resp.PtyOutput == nil {
				b.Fatalf("read: %v %#v", err, resp)
			}
			frames++
			tail = append(tail, resp.PtyOutput.Data...)
			if bytes.Contains(tail, marker) {
				break
			}
			tail = tail[max(len(tail)-len(marker), 0):]
		}
	}
	b.StopTimer()
	// One frame per chunk the worker fanned out, and so per VTWrite.
	b.ReportMetric(float64(frames)/float64(b.N), "frames/op")
	// CPU time of this process (worker and client), not of the agent.
	b.ReportMetric(float64((processCPU()-cpuBefore).Milliseconds())/float64(b.N), "cpu-ms/op")
	c.input("done\n")
	h.waitExit()
}

// processCPU returns the user and system CPU time this process has used.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkFanOut measures publishing 8 KiB PTY reads to 1, 10 and 100
// subscribers, each drained by its own goroutine as attached clients are.
// A subscriber that falls behind drops chunks (see broadcaster); delivered%
// reports how many arrived.
func BenchmarkFanOut(b *testing.B) {
	chunk := floodChunk()
	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("%dsubscribers", n), func(b *testing.B) {
			bc := newBroadcaster()
			var delivered atomic.Int64
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for range n {
				sub := bc.subscribe()
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case data := <-sub.ch:
							sub.took(data)
							delivered.Add(int64(len(data)))
						case <-stop:
							return
						}
					}
				}()
			}
			b.SetBytes(int64(len(chunk)))
			b.ReportAllocs()
			for b.Loop() {
				bc.publish(chunk)
			}
			b.StopTimer()
			// Let the subscribers take what is still queued for them.
			deadline := time.Now().Add(5 * time.Second)
			for delivered.Load() < int64(b.N*n*len(chunk)) && time.Now().Before(deadline) {
				bc.mu.Lock()
				queued := 0
				for s := range bc.subs {
					queued += len(s.ch)
				}
				bc.mu.Unlock()
				if queued == 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			close(stop)
			wg.Wait()
			b.ReportMetric(100*float64(delivered.Load())/float64(b.N*n*len(chunk)), "delivered%")
		})
	}
}

// BenchmarkAttach measures an attach round trip to a worker: connect, send
// AttachSession, receive Attached with the snapshot, disconnect. "idle" has
// a few lines on screen; "scrollback" has ~1 MB of scrollback.
func BenchmarkAttach(b *testing.B) {
	g := protocol.Geometry{Cols: defaultPtyCols, Rows: defaultPtyRows}
	for _, tc := range []struct{ name, input, wait string }{
		{"idle", "", "ready"},
		{"scrollback", "flood\n", "flood-done"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			h := startWorkerWith(b, blastAgent)
			if tc.input != "" {
				h.sendInput(tc.input)
			}
			h.eventually(tc.wait, func() bool { return bytes.Contains([]byte(h.history()), []byte(tc.wait)) })
			var size int
			for b.Loop() {
				conn := h.dial()
				if err := protocol.WriteRequest(conn, &protocol.Request{AttachSession: &protocol.AttachSession{
					SessionID: h.sessionID, Kind: session.AttachmentAttach, Geometry: g,
				}}); err != nil {
					b.Fatal(err)
				}
				resp, err := protocol.ReadResponse(bufio.NewReader(conn))
				if err != nil || resp == nil || resp.Attached == nil {
					b.Fatalf("attach: %v %#v", err, resp)
				}
				size = len(resp.Attached.Snapshot)
				conn.Close()
			}
			b.StopTimer()
			b.ReportMetric(float64(size), "snapshot-bytes")
			h.sendInput("done\n")
			h.waitExit()
		})
	}
}
