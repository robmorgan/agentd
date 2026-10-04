package bench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

func isFlood(id string) bool { return strings.HasPrefix(id, "flood-") }

// measureOutput runs the output-heavy sessions with no client attached and
// measures the PTY output their workers take in: the cost of feeding
// libghostty, with nothing to fan out to.
func (b *bench) measureOutput(ctx context.Context) error {
	n := b.opts.OutputHeavy
	if n == 0 {
		return nil
	}
	if err := b.parallel(ctx, n, func(i int) error {
		_, err := b.create(floodName(i), "flood")
		return err
	}); err != nil {
		return err
	}
	if err := sleep(ctx, settle); err != nil {
		return err
	}
	p, err := b.sessionPIDs(isFlood)
	if err != nil {
		return err
	}
	sample := func() ([]*protocol.SessionStats, error) {
		out := make([]*protocol.SessionStats, n)
		err := b.parallel(ctx, n, func(i int) error {
			var err error
			out[i], err = b.sessionStats(floodName(i), false)
			return err
		})
		return out, err
	}
	start := time.Now()
	w0, a0 := cpuOf(p.workers), cpuOf(p.agents)
	before, err := sample()
	if err != nil {
		return err
	}
	if err := sleep(ctx, b.opts.Duration); err != nil {
		return err
	}
	after, err := sample()
	if err != nil {
		return err
	}
	w1, a1 := cpuOf(p.workers), cpuOf(p.agents)
	wall := time.Since(start)

	var total, chunks uint64
	rates := make([]float64, n)
	workers := make([]processStats, n)
	for i := range n {
		d := after[i].OutputBytes - before[i].OutputBytes
		total += d
		chunks += after[i].OutputChunks - before[i].OutputChunks
		rates[i] = float64(d) / wall.Seconds()
		workers[i] = after[i].Worker
	}
	o := &OutputReport{
		Sessions:              n,
		IntervalSec:           wall.Seconds(),
		BytesPerSec:           float64(total) / wall.Seconds(),
		PerSessionBytesPerSec: summarize(rates),
		WorkersCPUPercent:     percent(w1-w0, wall),
		AgentsCPUPercent:      percent(a1-a0, wall),
		Workers:               distOf(workers),
	}
	if total > 0 {
		o.CPUMsPerMiB = millis(w1-w0) / (float64(total) / (1 << 20))
		o.ChunkBytes = float64(total) / float64(chunks)
	}
	b.report.Output = o
	// Fan-out is measured on the first of them alone, so stop the rest.
	return b.parallel(ctx, n-1, func(i int) error {
		_, err := b.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: floodName(i + 1), Remove: true}})
		return err
	})
}

// echoInterval is how often the fan-out phase types a marker into the
// session. The PTY echoes it into the output every client receives.
const echoInterval = 100 * time.Millisecond

// fanOutClient is one client attached during the fan-out phase. Its reader
// goroutine is the only writer of everything but received, which the phase
// reads while it runs.
type fanOutClient struct {
	a        *attachment
	received atomic.Uint64
	// seen maps an echo marker to when this client first received it.
	seen  map[int]time.Time
	carry []byte
}

// markerPrefix and markerSuffix frame an echo marker's sequence number.
const (
	markerPrefix = "zq"
	markerSuffix = "qz"
	markerDigits = 6
	markerLen    = len(markerPrefix) + markerDigits + len(markerSuffix)
)

func marker(seq int) string {
	return fmt.Sprintf("%s%0*d%s", markerPrefix, markerDigits, seq, markerSuffix)
}

// scan records the markers in data, including one split across reads.
func (c *fanOutClient) scan(data []byte, now time.Time) {
	buf := append(c.carry, data...)
	for i := 0; ; {
		j := bytes.Index(buf[i:], []byte(markerPrefix))
		if j < 0 || i+j+markerLen > len(buf) {
			break
		}
		at := i + j
		if string(buf[at+markerLen-len(markerSuffix):at+markerLen]) == markerSuffix {
			if seq, err := strconv.Atoi(string(buf[at+len(markerPrefix) : at+len(markerPrefix)+markerDigits])); err == nil {
				if _, ok := c.seen[seq]; !ok {
					c.seen[seq] = now
				}
			}
		}
		i = at + 1
	}
	keep := min(len(buf), markerLen-1)
	c.carry = append(c.carry[:0], buf[len(buf)-keep:]...)
}

func (c *fanOutClient) read() {
	for {
		resp, err := protocol.ReadResponse(c.a.r)
		if err != nil || resp == nil || resp.PtyOutput == nil {
			return
		}
		c.received.Add(uint64(len(resp.PtyOutput.Data)))
		c.scan(resp.PtyOutput.Data, time.Now())
	}
}

// measureFanOut attaches clients to one output-heavy session and measures
// what each receives, what the worker drops for them, and how long typed
// input takes to come back to all of them.
func (b *bench) measureFanOut(ctx context.Context) error {
	k := b.opts.Attach
	if k == 0 {
		return nil
	}
	id := floodName(0)
	p, err := b.sessionPIDs(func(s string) bool { return s == id })
	if err != nil {
		return err
	}
	clients := make([]*fanOutClient, 0, k)
	var readers sync.WaitGroup
	defer func() {
		for _, c := range clients {
			c.a.s.Close()
		}
		readers.Wait()
	}()
	for range k {
		a, _, err := b.attach(id)
		if err != nil {
			return err
		}
		c := &fanOutClient{a: a, seen: map[int]time.Time{}}
		clients = append(clients, c)
		readers.Add(1)
		go func() {
			defer readers.Done()
			c.read()
		}()
	}
	if err := sleep(ctx, settle); err != nil {
		return err
	}

	before, err := b.sessionStats(id, false)
	if err != nil {
		return err
	}
	received0 := make([]uint64, k)
	for i, c := range clients {
		received0[i] = c.received.Load()
	}
	daemon := []int{b.daemon.Process.Pid}
	start := time.Now()
	wc0, dc0 := cpuOf(p.workers), cpuOf(daemon)

	// Type markers on the first client's stream until the interval ends.
	var sentMu sync.Mutex
	sent := map[int]time.Time{}
	stopSending := make(chan struct{})
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		ticker := time.NewTicker(echoInterval)
		defer ticker.Stop()
		for seq := 0; ; seq++ {
			select {
			case <-stopSending:
				return
			case <-ticker.C:
			}
			sentMu.Lock()
			sent[seq] = time.Now()
			sentMu.Unlock()
			in := &protocol.Request{AttachInput: &protocol.Bytes{Data: []byte(marker(seq) + "\n")}}
			if protocol.WriteRequest(clients[0].a.s, in) != nil {
				return
			}
		}
	}()
	err = sleep(ctx, b.opts.Duration)
	close(stopSending)
	<-senderDone
	if err != nil {
		return err
	}
	after, err := b.sessionStats(id, false)
	if err != nil {
		return err
	}
	wall := time.Since(start)
	wc1, dc1 := cpuOf(p.workers), cpuOf(daemon)
	received := make([]uint64, k)
	for i, c := range clients {
		received[i] = c.received.Load() - received0[i]
	}
	// Let the last markers arrive, then stop the readers so their maps can
	// be read.
	if err := sleep(ctx, 500*time.Millisecond); err != nil {
		return err
	}
	for _, c := range clients {
		c.a.s.Close()
	}
	readers.Wait()

	produced := after.OutputBytes - before.OutputBytes
	rates := make([]float64, k)
	delivered := make([]float64, k)
	for i, r := range received {
		rates[i] = float64(r) / wall.Seconds()
		if produced > 0 {
			delivered[i] = min(1, float64(r)/float64(produced))
		}
	}
	var echoes []float64
	lost := 0
	for seq, at := range sent {
		for _, c := range clients {
			if got, ok := c.seen[seq]; ok {
				echoes = append(echoes, millis(got.Sub(at)))
			} else {
				lost++
			}
		}
	}
	b.report.FanOut = &FanOutReport{
		Clients:           k,
		IntervalSec:       wall.Seconds(),
		PTYBytesPerSec:    float64(produced) / wall.Seconds(),
		ClientBytesPerSec: summarize(rates),
		Delivered:         summarize(delivered),
		DroppedChunks:     after.DroppedOutputChunks - before.DroppedOutputChunks,
		ChunkBytes:        float64(produced) / float64(max(1, after.OutputChunks-before.OutputChunks)),
		EchoMs:            summarize(echoes),
		EchoesLost:        lost,
		WorkerCPU:         percent(wc1-wc0, wall),
		DaemonCPU:         percent(dc1-dc0, wall),
	}
	return nil
}

// Repetitions of the database measurements.
const (
	listRepeats = 10
	getRepeats  = 50
)

func timeIt(n int, fn func() error) (Dist, error) {
	samples := make([]float64, 0, n)
	for range n {
		start := time.Now()
		if err := fn(); err != nil {
			return Dist{}, err
		}
		samples = append(samples, millis(time.Since(start)))
	}
	return summarize(samples), nil
}

// measureDatabase times session lookups through the daemon and against
// state.db directly, and inserts into a copy-sized scratch database.
func (b *bench) measureDatabase(ctx context.Context) error {
	store, err := db.Open(b.paths.Database)
	if err != nil {
		return err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return err
	}
	r := DatabaseReport{Rows: len(recs)}
	if info, err := os.Stat(b.paths.Database); err == nil {
		r.FileBytes = uint64(info.Size())
	}
	id := ""
	if len(recs) > 0 {
		id = recs[len(recs)/2].SessionID
	}
	if r.ListSessionsMs, err = timeIt(listRepeats, func() error {
		_, err := b.request(&protocol.Request{ListSessions: protocol.Empty})
		return err
	}); err != nil {
		return err
	}
	if r.ListSessionsDBMs, err = timeIt(listRepeats, func() error {
		_, err := store.ListSessions()
		return err
	}); err != nil {
		return err
	}
	if id != "" {
		if r.GetSessionMs, err = timeIt(getRepeats, func() error {
			_, err := b.request(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}})
			return err
		}); err != nil {
			return err
		}
		if r.GetSessionDBMs, err = timeIt(getRepeats, func() error {
			_, err := store.GetSession(id)
			return err
		}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Inserts go to a scratch database with as many rows as state.db ends
	// up with, so the live one is not disturbed.
	scratch, err := db.Open(filepath.Join(b.root, "bench-insert.db"))
	if err != nil {
		return err
	}
	if r.InsertDBMs, err = func() (Dist, error) {
		i := 0
		return timeIt(max(len(recs), 1), func() error {
			i++
			_, err := scratch.InsertSession(db.NewSession{
				SessionID: fmt.Sprintf("s-%05d", i), Agent: "idle", Mode: session.ModeExecute, Cwd: b.workDir(),
			})
			return err
		})
	}(); err != nil {
		return err
	}
	b.report.Database = r
	return nil
}
