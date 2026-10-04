package bench

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Dist summarises a set of samples.
type Dist struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
}

func summarize(samples []float64) Dist {
	if len(samples) == 0 {
		return Dist{}
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	at := func(q float64) float64 { return s[min(len(s)-1, int(math.Ceil(q*float64(len(s))))-1)] }
	return Dist{N: len(s), Min: s[0], P50: at(0.5), P99: at(0.99), Mean: sum / float64(len(s)), Max: s[len(s)-1]}
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Report is the result of a run. Sizes are bytes, latencies milliseconds,
// rates per second, and CPU in percent of one core.
type Report struct {
	Machine Machine `json:"machine"`
	Options struct {
		Count       int     `json:"count"`
		OutputHeavy int     `json:"output_heavy"`
		Attach      int     `json:"attach"`
		DurationSec float64 `json:"duration_sec"`
		FillLines   int     `json:"fill_lines"`
	} `json:"options"`

	// DaemonStartupMs is from starting `agentd serve` to its first answer.
	DaemonStartupMs float64 `json:"daemon_startup_ms"`
	// DaemonIdle is the daemon with no sessions; DaemonLoaded with all the
	// idle sessions running.
	DaemonIdle   ProcessSample `json:"daemon_idle"`
	DaemonLoaded ProcessSample `json:"daemon_loaded"`
	// DaemonPerSession is (loaded - idle) / count.
	DaemonPerSession PerSession `json:"daemon_per_session"`

	CreateMs       Dist    `json:"create_ms"`
	CreateTotalSec float64 `json:"create_total_sec"`

	// IdleWorkers and IdleAgents are the idle sessions' processes.
	IdleWorkers ProcessDist `json:"idle_workers"`
	IdleAgents  ProcessDist `json:"idle_agents"`

	IdleCPU IdleCPU `json:"idle_cpu"`

	// IdleSnapshot is attach and snapshot cost for idle sessions (a mostly
	// empty screen); FullSnapshot for one session with full scrollback.
	IdleSnapshot SnapshotReport  `json:"idle_snapshot"`
	FullSnapshot *SnapshotReport `json:"full_snapshot,omitempty"`

	Output *OutputReport `json:"output,omitempty"`
	FanOut *FanOutReport `json:"fan_out,omitempty"`

	Database DatabaseReport `json:"database"`
}

// Machine describes where the run happened.
type Machine struct {
	Host      string `json:"host"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      uint32 `json:"cpus"`
	Memory    uint64 `json:"memory"`
	GoVersion string `json:"go_version"`
	Commit    string `json:"commit"`
}

// ProcessSample is one process's usage at one moment.
type ProcessSample struct {
	Private    uint64 `json:"private"`
	RSS        uint64 `json:"rss"`
	Goroutines uint32 `json:"goroutines"`
	Threads    uint32 `json:"threads"`
	FDs        uint32 `json:"fds"`
	GoHeap     uint64 `json:"go_heap"`
}

// PerSession is a fixed cost divided over the sessions.
type PerSession struct {
	Private    float64 `json:"private"`
	Goroutines float64 `json:"goroutines"`
	Threads    float64 `json:"threads"`
	FDs        float64 `json:"fds"`
}

// ProcessDist is a set of processes' usage.
type ProcessDist struct {
	Private    Dist `json:"private"`
	RSS        Dist `json:"rss"`
	Threads    Dist `json:"threads"`
	FDs        Dist `json:"fds"`
	Goroutines Dist `json:"goroutines,omitzero"`
	GoHeap     Dist `json:"go_heap,omitzero"`
	GoRuntime  Dist `json:"go_runtime,omitzero"`
}

// IdleCPU is the CPU the idle sessions used over IntervalSec, measured from
// outside so that measuring does not add to it.
type IdleCPU struct {
	IntervalSec    float64 `json:"interval_sec"`
	DaemonPercent  float64 `json:"daemon_percent"`
	WorkersPercent float64 `json:"workers_percent"`
	AgentsPercent  float64 `json:"agents_percent"`
	// PerWorkerMicros is CPU microseconds per worker per second.
	PerWorkerMicros float64 `json:"per_worker_micros_per_sec"`
}

// SnapshotReport is the cost of reattaching.
type SnapshotReport struct {
	Sessions       int     `json:"sessions"`
	ScrollbackRows uint64  `json:"scrollback_rows"`
	Bytes          Dist    `json:"bytes"`
	FormatMs       Dist    `json:"format_ms"`
	RestoreMs      Dist    `json:"restore_ms"`
	AttachMs       Dist    `json:"attach_ms"`
	WorkerPrivate  float64 `json:"worker_private,omitempty"`
}

// OutputReport is PTY throughput with no client attached.
type OutputReport struct {
	Sessions    int     `json:"sessions"`
	IntervalSec float64 `json:"interval_sec"`
	// BytesPerSec is the PTY output all of them read, per second.
	BytesPerSec           float64 `json:"bytes_per_sec"`
	PerSessionBytesPerSec Dist    `json:"per_session_bytes_per_sec"`
	WorkersCPUPercent     float64 `json:"workers_cpu_percent"`
	AgentsCPUPercent      float64 `json:"agents_cpu_percent"`
	CPUMsPerMiB           float64 `json:"worker_cpu_ms_per_mib"`
	// ChunkBytes is the mean size of a PTY read.
	ChunkBytes float64     `json:"chunk_bytes"`
	Workers    ProcessDist `json:"workers"`
}

// FanOutReport is one output-heavy session with Clients attached.
type FanOutReport struct {
	Clients     int     `json:"clients"`
	IntervalSec float64 `json:"interval_sec"`
	// PTYBytesPerSec is the session's output while they were attached.
	PTYBytesPerSec    float64 `json:"pty_bytes_per_sec"`
	ClientBytesPerSec Dist    `json:"client_bytes_per_sec"`
	// Delivered is the fraction of the output each client received; the
	// rest was dropped for falling behind.
	Delivered     Dist   `json:"delivered"`
	DroppedChunks uint64 `json:"dropped_chunks"`
	// ChunkBytes is the mean size of a PTY read, each a frame per client.
	ChunkBytes float64 `json:"chunk_bytes"`
	// EchoMs is how long typed input took to come back, as echo, to every
	// client: input latency behind a stream of output.
	EchoMs     Dist    `json:"echo_ms"`
	EchoesLost int     `json:"echoes_lost"`
	WorkerCPU  float64 `json:"worker_cpu_percent"`
	DaemonCPU  float64 `json:"daemon_cpu_percent"`
}

// DatabaseReport is the cost of state.db with Rows sessions in it.
type DatabaseReport struct {
	Rows      int    `json:"rows"`
	FileBytes uint64 `json:"file_bytes"`
	// ListSessionsMs and GetSessionMs go through the daemon, which also
	// probes each running worker's socket; the DB fields are the database
	// alone.
	ListSessionsMs   Dist `json:"list_sessions_ms"`
	GetSessionMs     Dist `json:"get_session_ms"`
	ListSessionsDBMs Dist `json:"list_sessions_db_ms"`
	GetSessionDBMs   Dist `json:"get_session_db_ms"`
	InsertDBMs       Dist `json:"insert_db_ms"`
}

func sampleOf(p processStats) ProcessSample {
	return ProcessSample{
		Private: p.PrivateBytes, RSS: p.RSSBytes, Goroutines: p.Goroutines,
		Threads: p.Threads, FDs: p.OpenFDs, GoHeap: p.GoHeapBytes,
	}
}

func distOf(ps []processStats) ProcessDist {
	col := func(f func(processStats) float64) Dist {
		v := make([]float64, len(ps))
		for i, p := range ps {
			v[i] = f(p)
		}
		return summarize(v)
	}
	d := ProcessDist{
		Private: col(func(p processStats) float64 { return float64(p.PrivateBytes) }),
		RSS:     col(func(p processStats) float64 { return float64(p.RSSBytes) }),
		Threads: col(func(p processStats) float64 { return float64(p.Threads) }),
		FDs:     col(func(p processStats) float64 { return float64(p.OpenFDs) }),
	}
	if len(ps) > 0 && ps[0].Goroutines > 0 {
		d.Goroutines = col(func(p processStats) float64 { return float64(p.Goroutines) })
		d.GoHeap = col(func(p processStats) float64 { return float64(p.GoHeapBytes) })
		d.GoRuntime = col(func(p processStats) float64 { return float64(p.GoRuntimeBytes) })
	}
	return d
}

func mib(v float64) string {
	switch {
	case math.Abs(v) >= 1<<30:
		return fmt.Sprintf("%.2f GiB", v/(1<<30))
	case math.Abs(v) >= 1<<20:
		return fmt.Sprintf("%.1f MiB", v/(1<<20))
	case math.Abs(v) >= 1<<10:
		return fmt.Sprintf("%.1f KiB", v/(1<<10))
	}
	return fmt.Sprintf("%.0f B", v)
}

func ms(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.2f s", v/1000)
	case v >= 10:
		return fmt.Sprintf("%.0f ms", v)
	case v >= 0.1:
		return fmt.Sprintf("%.2f ms", v)
	}
	return fmt.Sprintf("%.0f µs", v*1000)
}

func count(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// Print writes the report as tables.
func (r *Report) Print(w io.Writer) {
	m := r.Machine
	fmt.Fprintf(w, "agentd bench sessions: %d idle", r.Options.Count)
	if r.Options.OutputHeavy > 0 {
		fmt.Fprintf(w, ", %d output-heavy (%d attached)", r.Options.OutputHeavy, r.Options.Attach)
	}
	fmt.Fprintf(w, "\nmachine: %s, %s/%s, %d CPUs, %s RAM, %s, commit %s\n\n",
		m.Host, m.OS, m.Arch, m.CPUs, mib(float64(m.Memory)), m.GoVersion, m.Commit)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(cells ...string) { fmt.Fprintln(tw, strings.Join(cells, "\t")) }
	distRow := func(name string, d Dist, f func(float64) string) {
		if d.N == 0 {
			return
		}
		row(name, f(d.P50), f(d.Mean), f(d.P99), f(d.Max))
	}
	section := func(title string) {
		tw.Flush()
		fmt.Fprintf(w, "\n%s\n", title)
	}

	fmt.Fprintf(w, "daemon startup: %s\n", ms(r.DaemonStartupMs))
	fmt.Fprintf(w, "create: %d sessions in %.2f s\n", r.CreateMs.N, r.CreateTotalSec)
	section("Daemon")
	row("", "PRIVATE", "RSS", "GOROUTINES", "THREADS", "FDS", "GO HEAP")
	ps := func(name string, p ProcessSample) {
		row(name, mib(float64(p.Private)), mib(float64(p.RSS)), fmt.Sprint(p.Goroutines), fmt.Sprint(p.Threads), fmt.Sprint(p.FDs), mib(float64(p.GoHeap)))
	}
	ps("idle", r.DaemonIdle)
	ps(fmt.Sprintf("%d sessions", r.Options.Count), r.DaemonLoaded)
	d := r.DaemonPerSession
	row("per session", mib(d.Private), "", count(d.Goroutines), count(d.Threads), count(d.FDs), "")

	procRows := func(p ProcessDist) {
		row("", "P50", "MEAN", "P99", "MAX")
		distRow("private", p.Private, mib)
		distRow("rss", p.RSS, mib)
		distRow("threads", p.Threads, count)
		distRow("fds", p.FDs, count)
		distRow("goroutines", p.Goroutines, count)
		distRow("go heap", p.GoHeap, mib)
		distRow("go runtime", p.GoRuntime, mib)
	}
	section("Per idle session: worker")
	procRows(r.IdleWorkers)
	section("Per idle session: agent (top process)")
	procRows(r.IdleAgents)
	section("Latency")
	row("", "P50", "MEAN", "P99", "MAX")
	distRow("create", r.CreateMs, ms)
	snap := func(name string, s SnapshotReport) {
		distRow(name+" attach", s.AttachMs, ms)
		distRow(name+" snapshot format", s.FormatMs, ms)
		distRow(name+" snapshot restore", s.RestoreMs, ms)
		distRow(name+" snapshot size", s.Bytes, mib)
	}
	snap("idle", r.IdleSnapshot)
	if r.FullSnapshot != nil {
		snap(fmt.Sprintf("full (%d rows)", r.FullSnapshot.ScrollbackRows), *r.FullSnapshot)
	}
	db := r.Database
	distRow(fmt.Sprintf("ListSessions (%d rows)", db.Rows), db.ListSessionsMs, ms)
	distRow("  of which state.db", db.ListSessionsDBMs, ms)
	distRow("GetSession", db.GetSessionMs, ms)
	distRow("  of which state.db", db.GetSessionDBMs, ms)
	distRow("state.db insert", db.InsertDBMs, ms)
	tw.Flush()
	fmt.Fprintf(w, "state.db: %s with %d rows\n", mib(float64(db.FileBytes)), db.Rows)

	c := r.IdleCPU
	fmt.Fprintf(w, "\nidle CPU over %.1f s: daemon %.2f%%, workers %.2f%% (%.0f µs/s each), agents %.2f%%\n",
		c.IntervalSec, c.DaemonPercent, c.WorkersPercent, c.PerWorkerMicros, c.AgentsPercent)
	if r.FullSnapshot != nil && r.FullSnapshot.WorkerPrivate > 0 {
		fmt.Fprintf(w, "worker with full scrollback: %s private\n", mib(r.FullSnapshot.WorkerPrivate))
	}

	if o := r.Output; o != nil {
		fmt.Fprintf(w, "\nOutput: %d output-heavy sessions, no clients, over %.1f s\n", o.Sessions, o.IntervalSec)
		fmt.Fprintf(w, "PTY throughput: %s/s total, %s/s per session (p50), %s per read; workers %.0f%% CPU, agents %.0f%%; %.1f ms worker CPU per MiB\n",
			mib(o.BytesPerSec), mib(o.PerSessionBytesPerSec.P50), mib(o.ChunkBytes), o.WorkersCPUPercent, o.AgentsCPUPercent, o.CPUMsPerMiB)
		section("Output-heavy worker")
		procRows(o.Workers)
		tw.Flush()
	}
	if f := r.FanOut; f != nil {
		fmt.Fprintf(w, "\nFan-out: %d clients on one output-heavy session over %.1f s\n", f.Clients, f.IntervalSec)
		fmt.Fprintf(w, "PTY output %s/s in %s reads; per client %s/s (p50), delivered %.1f%% (min %.1f%%); %d chunks dropped; worker %.0f%% CPU, daemon %.0f%%\n",
			mib(f.PTYBytesPerSec), mib(f.ChunkBytes), mib(f.ClientBytesPerSec.P50), 100*f.Delivered.P50, 100*f.Delivered.Min, f.DroppedChunks, f.WorkerCPU, f.DaemonCPU)
		fmt.Fprintf(w, "echo to every client: p50 %s, p99 %s, max %s, %d lost\n", ms(f.EchoMs.P50), ms(f.EchoMs.P99), ms(f.EchoMs.Max), f.EchoesLost)
	}
}
