package protocol

// Resource usage (capability CapRuntimeStats).
//
// GetSessionStats asks a session's worker for its own numbers: the worker
// process (memory, CPU, threads, files, Go runtime), the agent's top
// process, and its terminal and output fan-out. The daemon forwards it to
// the worker, which measures itself precisely rather than being sampled
// from outside. GetDaemonStats is the daemon's own process. Both are
// one-shot requests, and a client sends them only when the daemon's
// Welcome lists CapRuntimeStats.

// GetSessionStats asks for a session's resource usage.
type GetSessionStats struct {
	SessionID string
	// Snapshot also formats a reattach snapshot and replays it into a fresh
	// terminal, timing both. That formats the whole scrollback, so it costs
	// what an attach costs and is only done when asked.
	Snapshot bool
}

// ProcessStats is one process's resource usage. Fields a process cannot
// report are zero: the Go runtime fields for the agent, everything but the
// pid when the platform has no reader.
type ProcessStats struct {
	PID uint32
	// RSSBytes counts shared pages too (the executable, libraries), so it
	// overstates what many identical workers cost together.
	RSSBytes uint64
	// PrivateBytes is the memory charged to this process alone (the
	// physical footprint on macOS, private pages on Linux).
	PrivateBytes   uint64
	CPUUserNanos   uint64
	CPUSystemNanos uint64
	Threads        uint32
	OpenFDs        uint32
	// UptimeNanos is how long the process has run, for CPU rates.
	UptimeNanos uint64
	Goroutines  uint32
	// GoHeapBytes is the memory in heap objects.
	GoHeapBytes uint64
	// GoRuntimeBytes is what the Go runtime holds from the OS. Private
	// memory beyond it is held outside Go: in a worker, mostly libghostty.
	GoRuntimeBytes uint64
}

// SnapshotStats times one reattach snapshot.
type SnapshotStats struct {
	Bytes uint64
	// FormatNanos is the time to format it from the terminal state.
	FormatNanos uint64
	// RestoreNanos is the time to replay it into a fresh terminal, which
	// is what a client's terminal does with it.
	RestoreNanos uint64
}

// SessionStats answers GetSessionStats.
type SessionStats struct {
	SessionID string
	Worker    ProcessStats
	// Agent is the agent's top process only, not its descendants.
	Agent ProcessStats
	Cols  uint16
	Rows  uint16
	// ScrollbackRows is the rows of history kept above the screen, up to
	// ScrollbackLimitBytes of terminal state.
	ScrollbackRows       uint64
	ScrollbackLimitBytes uint64
	Attachments          uint32
	// OutputBytes counts the PTY output read since the session started.
	OutputBytes uint64
	// DroppedOutputChunks counts output chunks dropped for attached
	// clients that fell behind, summed over all of them.
	DroppedOutputChunks uint64
	// Snapshot is set when the request asked for it.
	Snapshot *SnapshotStats
}

// DaemonStats answers GetDaemonStats.
type DaemonStats struct {
	Daemon ProcessStats
	// OpenStreams counts the client streams being served.
	OpenStreams uint32
}

func (e *encoder) processStats(p *ProcessStats) {
	e.u32(p.PID)
	e.u64(p.RSSBytes)
	e.u64(p.PrivateBytes)
	e.u64(p.CPUUserNanos)
	e.u64(p.CPUSystemNanos)
	e.u32(p.Threads)
	e.u32(p.OpenFDs)
	e.u64(p.UptimeNanos)
	e.u32(p.Goroutines)
	e.u64(p.GoHeapBytes)
	e.u64(p.GoRuntimeBytes)
}

func (d *decoder) processStats() ProcessStats {
	return ProcessStats{
		PID:            d.u32(),
		RSSBytes:       d.u64(),
		PrivateBytes:   d.u64(),
		CPUUserNanos:   d.u64(),
		CPUSystemNanos: d.u64(),
		Threads:        d.u32(),
		OpenFDs:        d.u32(),
		UptimeNanos:    d.u64(),
		Goroutines:     d.u32(),
		GoHeapBytes:    d.u64(),
		GoRuntimeBytes: d.u64(),
	}
}

func (e *encoder) sessionStats(s *SessionStats) {
	e.str(s.SessionID)
	e.processStats(&s.Worker)
	e.processStats(&s.Agent)
	e.u16(s.Cols)
	e.u16(s.Rows)
	e.u64(s.ScrollbackRows)
	e.u64(s.ScrollbackLimitBytes)
	e.u32(s.Attachments)
	e.u64(s.OutputBytes)
	e.u64(s.DroppedOutputChunks)
	if s.Snapshot == nil {
		e.u8(0)
		return
	}
	e.u8(1)
	e.u64(s.Snapshot.Bytes)
	e.u64(s.Snapshot.FormatNanos)
	e.u64(s.Snapshot.RestoreNanos)
}

func (d *decoder) sessionStats() *SessionStats {
	s := &SessionStats{
		SessionID:            d.str(),
		Worker:               d.processStats(),
		Agent:                d.processStats(),
		Cols:                 d.u16(),
		Rows:                 d.u16(),
		ScrollbackRows:       d.u64(),
		ScrollbackLimitBytes: d.u64(),
		Attachments:          d.u32(),
		OutputBytes:          d.u64(),
		DroppedOutputChunks:  d.u64(),
	}
	if d.bool() {
		s.Snapshot = &SnapshotStats{Bytes: d.u64(), FormatNanos: d.u64(), RestoreNanos: d.u64()}
	}
	return s
}
