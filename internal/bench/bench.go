// Package bench implements `agentd bench sessions`: it starts a private
// daemon in a fresh runtime root, creates many sessions, and measures what
// they cost (memory, threads, files, CPU per process, attach and snapshot
// latency, PTY throughput and fan-out, state.db) through the protocol and
// the OS, the way a user's sessions would be measured.
//
// Ownership. Run owns everything it starts: the daemon (in its own process
// group, so a Ctrl-C reaches only the bench) and, through it, every worker
// and agent. Whatever happens, Run's cleanup stops all of them, found from
// the root's state.db rather than from the daemon, which may have died, and
// removes the root. Only a SIGKILL of the bench itself can leave them
// behind.
//
// Goroutines are bounded pools that end before the phase that started them
// returns: session creation and stats sampling (Options.Parallel at a
// time), and one reader per fan-out client plus the echo sender, which end
// when the phase closes their streams.
package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/robmorgan/agentd/internal/db"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/procstat"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

type processStats = protocol.ProcessStats

// Options configure a run.
type Options struct {
	// Exe is the agentd binary to run as the daemon.
	Exe string
	// Count is the number of idle sessions.
	Count int
	// OutputHeavy is the number of sessions that write output as fast as
	// the PTY takes it.
	OutputHeavy int
	// Attach is the number of clients attached to one output-heavy session
	// (one is started if OutputHeavy is 0).
	Attach int
	// Duration is how long idle CPU, throughput and fan-out are measured.
	Duration time.Duration
	// FillLines is how many lines the full-scrollback session prints; 0
	// skips it.
	FillLines int
	// Parallel bounds the requests in flight while creating sessions and
	// sampling them.
	Parallel int
	// Log receives progress lines; nil discards them.
	Log io.Writer
}

// The agents the bench's sessions run. They are shell commands, so the
// numbers are agentd's and not an agent's.
const (
	idleAgent = "exec cat"
	// floodLine is about one terminal line of mixed text.
	floodAgent = `yes "0123456789 the quick brown fox jumps over the lazy dog ABCDEFGHIJKLMNOPQRSTUVWXYZ" & exec cat`
	fillAgent  = `seq -f "%08g the quick brown fox jumps over the lazy dog 0123456789 abcdefghijklmnopqrstuvwxyz" 1 "$BENCH_FILL_LINES"; exec cat`
)

type bench struct {
	opts   Options
	root   string
	paths  *paths.AppPaths
	daemon *exec.Cmd
	// daemonExited is closed once the daemon has been reaped.
	daemonExited chan struct{}
	report       *Report
}

func (b *bench) logf(format string, args ...any) {
	if b.opts.Log != nil {
		fmt.Fprintf(b.opts.Log, format+"\n", args...)
	}
}

// Run runs the benchmark. It stops early, cleaning up, when ctx ends.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Count < 0 || opts.OutputHeavy < 0 || opts.Attach < 0 || opts.FillLines < 0 {
		return nil, errors.New("counts must not be negative")
	}
	if opts.Attach > 0 && opts.OutputHeavy == 0 {
		opts.OutputHeavy = 1
	}
	if opts.Duration <= 0 {
		opts.Duration = 5 * time.Second
	}
	if opts.Parallel <= 0 {
		opts.Parallel = 8
	}
	// Unix socket paths are short, so the root is too.
	root, err := os.MkdirTemp("/tmp", "agdb-")
	if err != nil {
		return nil, err
	}
	b := &bench{opts: opts, root: root, paths: paths.FromRoot(root), report: &Report{}}
	defer b.cleanup()
	b.report.Options.Count = opts.Count
	b.report.Options.OutputHeavy = opts.OutputHeavy
	b.report.Options.Attach = opts.Attach
	b.report.Options.DurationSec = opts.Duration.Seconds()
	b.report.Options.FillLines = opts.FillLines
	b.logf("runtime root %s", root)

	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"start the daemon", b.startDaemon},
		{"create idle sessions", b.createIdle},
		{"measure idle sessions", b.measureIdle},
		{"measure idle CPU", b.measureIdleCPU},
		{"measure snapshots", b.measureSnapshots},
		{"measure output", b.measureOutput},
		{"measure fan-out", b.measureFanOut},
		{"measure state.db", b.measureDatabase},
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.logf("%s...", step.name)
		if err := step.run(ctx); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return b.report, nil
}

func (b *bench) writeConfig() error {
	if err := os.MkdirAll(b.workDir(), 0o700); err != nil {
		return err
	}
	agent := func(name, script string) string {
		return fmt.Sprintf("[agents.%s]\ncommand = \"/bin/sh\"\nargs = [\"-c\", '''%s''']\n\n", name, script)
	}
	config := "default_agent = \"idle\"\n\n" + agent("idle", idleAgent) + agent("flood", floodAgent) +
		agent("fill", fmt.Sprintf("BENCH_FILL_LINES=%d; %s", b.opts.FillLines, fillAgent))
	return os.WriteFile(b.paths.Config, []byte(config), 0o600)
}

func (b *bench) workDir() string { return filepath.Join(b.root, "work") }

// startDaemon starts `agentd serve` on the private root and times how long
// it takes to answer a request.
func (b *bench) startDaemon(ctx context.Context) error {
	if err := b.paths.EnsureLayout(); err != nil {
		return err
	}
	if err := b.writeConfig(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(b.root, "bench-daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(b.opts.Exe, "serve")
	cmd.Env = append(os.Environ(), "AGENTD_DIR="+b.root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own process group, so a Ctrl-C meant for the bench does not stop
	// the daemon before the bench has cleaned up through it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	b.daemon = cmd
	exited := make(chan struct{})
	go func() {
		// Reaps the daemon. cleanup waits for it through b.daemonExited.
		cmd.Wait()
		close(exited)
	}()
	b.daemonExited = exited
	for {
		if _, err := b.request(&protocol.Request{GetDaemonInfo: protocol.Empty}); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return fmt.Errorf("agentd exited; see %s", logFile.Name())
		case <-time.After(2 * time.Millisecond):
		}
		if time.Since(start) > 30*time.Second {
			return errors.New("agentd did not start within 30s")
		}
	}
	b.report.DaemonStartupMs = millis(time.Since(start))
	b.report.Machine = b.machine()
	daemon, err := b.daemonStats()
	if err != nil {
		return err
	}
	b.report.DaemonIdle = sampleOf(daemon)
	return nil
}

// machine describes this machine and build.
func (b *bench) machine() Machine {
	m := Machine{OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), Commit: "unknown"}
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		m.OS = fmt.Sprintf("%s %s", unix.ByteSliceToString(u.Sysname[:]), unix.ByteSliceToString(u.Release[:]))
	}
	if s, err := transport.DialUnix(b.paths.Socket, requestTimeout); err == nil {
		s.SetDeadline(time.Now().Add(requestTimeout))
		if protocol.WriteRequest(s, &protocol.Request{Hello: protocol.NewHello("agentd bench")}) == nil {
			if resp, err := protocol.ReadResponse(s); err == nil && resp != nil && resp.Welcome != nil {
				h := resp.Welcome.Host
				m.Host, m.CPUs, m.Memory = h.Name, h.CPUs, h.MemoryBytes
			}
		}
		s.Close()
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		modified := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				m.Commit = s.Value[:min(12, len(s.Value))]
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if modified {
			m.Commit += "+dirty"
		}
	}
	return m
}

func idleName(i int) string  { return fmt.Sprintf("idle-%04d", i) }
func floodName(i int) string { return fmt.Sprintf("flood-%03d", i) }

const fillName = "fill"

// create creates a session and returns how long the daemon took.
func (b *bench) create(name, agent string) (time.Duration, error) {
	start := time.Now()
	_, err := b.request(&protocol.Request{CreateSession: &protocol.CreateSession{
		Cwd: b.workDir(), Name: &name, Agent: agent,
	}})
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", name, err)
	}
	return time.Since(start), nil
}

// parallel runs fn for 0..n-1, at most Options.Parallel at a time, and
// returns the first error. It stops starting new calls once ctx ends or a
// call fails, and returns only after every call it started has returned.
func (b *bench) parallel(ctx context.Context, n int, fn func(i int) error) error {
	return parallelN(ctx, n, b.opts.Parallel, fn)
}

// parallelN is parallel with at most limit calls at a time.
func parallelN(ctx context.Context, n, limit int, fn func(i int) error) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	slots := make(chan struct{}, max(1, limit))
	for i := range n {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed || ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (b *bench) createIdle(ctx context.Context) error {
	latencies := make([]float64, b.opts.Count)
	start := time.Now()
	var created int
	var mu sync.Mutex
	err := b.parallel(ctx, b.opts.Count, func(i int) error {
		d, err := b.create(idleName(i), "idle")
		if err != nil {
			return err
		}
		latencies[i] = millis(d)
		mu.Lock()
		created++
		if created%100 == 0 {
			b.logf("  %d sessions", created)
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	b.report.CreateTotalSec = time.Since(start).Seconds()
	b.report.CreateMs = summarize(latencies)
	b.logf("created %d sessions", b.opts.Count)
	return nil
}

// settle gives new workers time to finish starting (and the agents to
// print their first output) before they are measured.
const settle = time.Second

func (b *bench) measureIdle(ctx context.Context) error {
	if err := sleep(ctx, settle); err != nil {
		return err
	}
	workers := make([]processStats, b.opts.Count)
	agents := make([]processStats, b.opts.Count)
	err := b.parallel(ctx, b.opts.Count, func(i int) error {
		s, err := b.sessionStats(idleName(i), false)
		if err != nil {
			return err
		}
		workers[i], agents[i] = s.Worker, s.Agent
		return nil
	})
	if err != nil {
		return err
	}
	b.report.IdleWorkers = distOf(workers)
	b.report.IdleAgents = distOf(agents)

	daemon, err := b.daemonStats()
	if err != nil {
		return err
	}
	b.report.DaemonLoaded = sampleOf(daemon)
	if n := float64(b.opts.Count); n > 0 {
		idle, loaded := b.report.DaemonIdle, b.report.DaemonLoaded
		b.report.DaemonPerSession = PerSession{
			Private:    (float64(loaded.Private) - float64(idle.Private)) / n,
			Goroutines: (float64(loaded.Goroutines) - float64(idle.Goroutines)) / n,
			Threads:    (float64(loaded.Threads) - float64(idle.Threads)) / n,
			FDs:        (float64(loaded.FDs) - float64(idle.FDs)) / n,
		}
	}
	return nil
}

// pids are the processes of some sessions, read from state.db.
type pids struct{ workers, agents []int }

func (b *bench) sessionPIDs(match func(string) bool) (pids, error) {
	store, err := db.Open(b.paths.Database)
	if err != nil {
		return pids{}, err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return pids{}, err
	}
	var p pids
	for _, rec := range recs {
		if rec.Status != session.StatusRunning || !match(rec.SessionID) {
			continue
		}
		if rec.WorkerPID != nil {
			p.workers = append(p.workers, int(*rec.WorkerPID))
		}
		if rec.AgentPID != nil {
			p.agents = append(p.agents, int(*rec.AgentPID))
		}
	}
	return p, nil
}

// cpuOf sums the CPU time of processes, read from outside so that
// measuring adds nothing to it. A process that has gone counts as zero.
func cpuOf(pids []int) time.Duration {
	var total time.Duration
	for _, pid := range pids {
		if p, err := procstat.Read(pid); err == nil {
			total += p.CPU()
		}
	}
	return total
}

func percent(cpu, wall time.Duration) float64 { return 100 * float64(cpu) / float64(wall) }

func isIdle(id string) bool { return len(id) > 5 && id[:5] == "idle-" }

func (b *bench) measureIdleCPU(ctx context.Context) error {
	p, err := b.sessionPIDs(isIdle)
	if err != nil {
		return err
	}
	daemon := []int{b.daemon.Process.Pid}
	start := time.Now()
	d0, w0, a0 := cpuOf(daemon), cpuOf(p.workers), cpuOf(p.agents)
	if err := sleep(ctx, b.opts.Duration); err != nil {
		return err
	}
	d1, w1, a1 := cpuOf(daemon), cpuOf(p.workers), cpuOf(p.agents)
	wall := time.Since(start)
	c := IdleCPU{
		IntervalSec:    wall.Seconds(),
		DaemonPercent:  percent(d1-d0, wall),
		WorkersPercent: percent(w1-w0, wall),
		AgentsPercent:  percent(a1-a0, wall),
	}
	if n := len(p.workers); n > 0 {
		c.PerWorkerMicros = float64((w1-w0)/time.Microsecond) / float64(n) / wall.Seconds()
	}
	b.report.IdleCPU = c
	return nil
}

// snapshotSamples bounds how many idle sessions are attached to and
// snapshotted; they are alike.
const snapshotSamples = 100

// snapshotRepeats is how often the full-scrollback session is measured.
const snapshotRepeats = 5

// measureSnapshot attaches to each session and asks its worker to time a
// snapshot, repeats times. Sessions are measured in parallel, but each
// one's measurements one at a time, so they do not queue behind each other
// on its worker.
func (b *bench) measureSnapshot(ctx context.Context, ids []string, repeats int) (SnapshotReport, error) {
	var mu sync.Mutex
	var attach, format, restore, size []float64
	var rows uint64
	n := len(ids) * repeats
	err := parallelN(ctx, n, min(b.opts.Parallel, len(ids)), func(i int) error {
		id := ids[i%len(ids)]
		a, elapsed, err := b.attach(id)
		if err != nil {
			return err
		}
		a.s.Close()
		s, err := b.sessionStats(id, true)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		attach = append(attach, millis(elapsed))
		format = append(format, float64(s.Snapshot.FormatNanos)/1e6)
		restore = append(restore, float64(s.Snapshot.RestoreNanos)/1e6)
		size = append(size, float64(s.Snapshot.Bytes))
		rows = max(rows, s.ScrollbackRows)
		return nil
	})
	return SnapshotReport{
		Sessions: len(ids), ScrollbackRows: rows, AttachMs: summarize(attach),
		FormatMs: summarize(format), RestoreMs: summarize(restore), Bytes: summarize(size),
	}, err
}

func (b *bench) measureSnapshots(ctx context.Context) error {
	var ids []string
	for i := 0; i < min(b.opts.Count, snapshotSamples); i++ {
		ids = append(ids, idleName(i))
	}
	if len(ids) > 0 {
		r, err := b.measureSnapshot(ctx, ids, 1)
		if err != nil {
			return err
		}
		b.report.IdleSnapshot = r
	}
	if b.opts.FillLines == 0 {
		return nil
	}
	if _, err := b.create(fillName, "fill"); err != nil {
		return err
	}
	// Wait for the session to finish printing: its output stops growing.
	var last uint64
	for stable := 0; stable < 3; {
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
		s, err := b.sessionStats(fillName, false)
		if err != nil {
			return err
		}
		if s.OutputBytes == last && last > 0 {
			stable++
		} else {
			stable = 0
		}
		last = s.OutputBytes
	}
	r, err := b.measureSnapshot(ctx, []string{fillName}, snapshotRepeats)
	if err != nil {
		return err
	}
	s, err := b.sessionStats(fillName, false)
	if err != nil {
		return err
	}
	r.WorkerPrivate = float64(s.Worker.PrivateBytes)
	b.report.FullSnapshot = &r
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// cleanup stops every process the run started and removes the root. It
// asks each worker to stop with SIGTERM, as the daemon does, so agents are
// stopped through their worker; whatever is left after a grace period is
// killed, process groups included.
func (b *bench) cleanup() {
	var all pids
	if _, err := os.Stat(b.paths.Database); err == nil {
		all, _ = b.sessionPIDs(func(string) bool { return true })
	}
	for _, pid := range all.workers {
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range all.workers {
		for pid > 1 && syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, pid := range all.agents {
		if pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	for _, pid := range all.workers {
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if b.daemon != nil {
		_ = b.daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-b.daemonExited:
		case <-time.After(5 * time.Second):
			_ = b.daemon.Process.Kill()
			<-b.daemonExited
		}
	}
	os.RemoveAll(b.root)
}
