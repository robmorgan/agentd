// Package procstat reads the resource usage of a process (memory, CPU time,
// threads, open files) from the operating system, and the Go runtime's own
// numbers for the calling process. Session workers report their usage with
// it and `agentd bench` measures the daemon and the agents with it.
//
// The OS-specific readers live in build-tagged files: macOS asks libproc
// (cgo), Linux reads /proc. Elsewhere Read returns ErrUnsupported.
//
// Only agentd uses this package; the agent CLI gets the numbers over the
// protocol.
package procstat

import (
	"errors"
	"os"
	"runtime/metrics"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
)

// ErrUnsupported is returned by Read on platforms it has no reader for.
var ErrUnsupported = errors.New("process statistics are not supported on this platform")

// Process is one process's resource usage, as the OS sees it.
type Process struct {
	// RSS is the resident set: every page of the process in RAM, including
	// pages shared with other processes (the executable's text, shared
	// libraries), so summing it over many workers overcounts.
	RSS uint64
	// Private is the memory charged to this process alone: the physical
	// footprint on macOS (what Activity Monitor shows), and Private_Clean +
	// Private_Dirty from smaps_rollup on Linux. It is the better measure of
	// what one more session costs. Zero when the OS does not say.
	Private uint64
	// CPUUser and CPUSystem are the CPU time used since the process started.
	CPUUser, CPUSystem time.Duration
	Threads            int
	// FDs counts open file descriptors.
	FDs int
}

// CPU is the total CPU time used.
func (p Process) CPU() time.Duration { return p.CPUUser + p.CPUSystem }

// Read returns the resource usage of process pid, which must belong to the
// calling user.
func Read(pid int) (Process, error) { return read(pid) }

// GoRuntime is the Go runtime's view of the calling process.
type GoRuntime struct {
	Goroutines int
	// HeapBytes is the memory occupied by heap objects, live or not yet
	// swept.
	HeapBytes uint64
	// RuntimeBytes is the memory the Go runtime has mapped and not
	// returned to the OS: heap, stacks, and runtime metadata. RSS minus
	// this is roughly what the process holds outside Go: the executable
	// and C libraries, and in a session worker libghostty's terminal state.
	RuntimeBytes uint64
}

var goMetrics = []string{
	"/sched/goroutines:goroutines",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/total:bytes",
	"/memory/classes/heap/released:bytes",
}

// ReadGoRuntime samples the Go runtime of the calling process. It does not
// stop the world.
func ReadGoRuntime() GoRuntime {
	samples := make([]metrics.Sample, len(goMetrics))
	for i, name := range goMetrics {
		samples[i].Name = name
	}
	metrics.Read(samples)
	u := func(i int) uint64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[i].Value.Uint64()
	}
	total, released := u(2), u(3)
	return GoRuntime{
		Goroutines:   int(u(0)),
		HeapBytes:    u(1),
		RuntimeBytes: total - min(released, total),
	}
}

// Describe returns pid's usage in the protocol's form. Read errors leave
// the usage fields zero: a process that just exited has none to report.
func Describe(pid int) protocol.ProcessStats {
	s := protocol.ProcessStats{PID: uint32(pid)}
	if p, err := Read(pid); err == nil {
		s.RSSBytes = p.RSS
		s.PrivateBytes = p.Private
		s.CPUUserNanos = uint64(p.CPUUser)
		s.CPUSystemNanos = uint64(p.CPUSystem)
		s.Threads = uint32(p.Threads)
		s.OpenFDs = uint32(p.FDs)
	}
	return s
}

// DescribeSelf is Describe for the calling process, with its Go runtime and
// the time since started.
func DescribeSelf(started time.Time) protocol.ProcessStats {
	s := Describe(os.Getpid())
	g := ReadGoRuntime()
	s.UptimeNanos = uint64(time.Since(started))
	s.Goroutines = uint32(g.Goroutines)
	s.GoHeapBytes = g.HeapBytes
	s.GoRuntimeBytes = g.RuntimeBytes
	return s
}
