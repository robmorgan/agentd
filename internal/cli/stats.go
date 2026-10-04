package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// requireStats fails unless the daemon reports resource usage.
func (c *client) requireStats() error {
	w, err := c.welcome()
	if err != nil {
		return err
	}
	if !protocol.HasCapability(w.Capabilities, protocol.CapRuntimeStats) {
		return fmt.Errorf("this agentd (%s) does not report resource usage; %s", w.DaemonVersion, upgradeHint)
	}
	return nil
}

func (c *client) sessionStats(id string) (*protocol.SessionStats, error) {
	resp, err := c.call(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: id}}, 0, func(r *protocol.Response) bool { return r.SessionStats != nil })
	if err != nil {
		return nil, err
	}
	return resp.SessionStats, nil
}

// printSessionStats is the --stats part of `agent status`.
func printSessionStats(w io.Writer, s *protocol.SessionStats) {
	fmt.Fprintf(w, "worker: %s\n", describeProcess(s.Worker))
	fmt.Fprintf(w, "agent: %s\n", describeProcess(s.Agent))
	fmt.Fprintf(w, "terminal: %dx%d, scrollback %d rows (limit %s), output %s, attachments %d, dropped output chunks %d\n",
		s.Cols, s.Rows, s.ScrollbackRows, formatBytes(s.ScrollbackLimitBytes), formatBytes(s.OutputBytes),
		s.Attachments, s.DroppedOutputChunks)
}

// describeProcess is one line of a process's usage. The Go runtime fields
// appear only for Go processes (the daemon and workers).
func describeProcess(p protocol.ProcessStats) string {
	parts := []string{
		fmt.Sprintf("pid %d", p.PID),
		"private " + formatBytes(p.PrivateBytes),
		"rss " + formatBytes(p.RSSBytes),
		"cpu " + formatCPU(p),
		fmt.Sprintf("threads %d", p.Threads),
		fmt.Sprintf("fds %d", p.OpenFDs),
	}
	if p.Goroutines > 0 {
		parts = append(parts,
			fmt.Sprintf("goroutines %d", p.Goroutines),
			"go heap "+formatBytes(p.GoHeapBytes),
			"go runtime "+formatBytes(p.GoRuntimeBytes))
	}
	return strings.Join(parts, ", ")
}

func formatCPU(p protocol.ProcessStats) string {
	return time.Duration(p.CPUUserNanos + p.CPUSystemNanos).Round(time.Millisecond).String()
}

// statsParallelism bounds the session stats requests `agent daemon stats`
// has in flight; the daemon serves up to 16 per control stream.
const statsParallelism = 8

// printDaemonStats is `agent daemon stats`: the daemon process, then one
// row per running session, then totals.
func (c *client) printDaemonStats(w io.Writer) error {
	resp, err := c.call(&protocol.Request{GetDaemonStats: protocol.Empty}, 0, func(r *protocol.Response) bool { return r.DaemonStats != nil })
	if err != nil {
		return err
	}
	d := resp.DaemonStats
	fmt.Fprintf(w, "daemon: %s, streams %d\n", describeProcess(d.Daemon), d.OpenStreams)

	sessions, err := c.listSessions(0)
	if err != nil {
		return err
	}
	var running []string
	for _, s := range sessions {
		if s.Status == session.StatusRunning {
			running = append(running, s.SessionID)
		}
	}
	sort.Strings(running)
	stats := make([]*protocol.SessionStats, len(running))
	errs := make([]error, len(running))
	var wg sync.WaitGroup
	slots := make(chan struct{}, statsParallelism)
	for i, id := range running {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			stats[i], errs[i] = c.sessionStats(id)
		}()
	}
	wg.Wait()

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tPRIVATE\tRSS\tCPU\tTHREADS\tFDS\tGOROUTINES\tGO HEAP\tSCROLLBACK\tAGENT PRIVATE")
	var workers, agents uint64
	for i, id := range running {
		s := stats[i]
		if s == nil {
			fmt.Fprintf(tw, "%s\t%s\n", id, errs[i])
			continue
		}
		workers += s.Worker.PrivateBytes
		agents += s.Agent.PrivateBytes
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%d\t%s\n", id,
			formatBytes(s.Worker.PrivateBytes), formatBytes(s.Worker.RSSBytes), formatCPU(s.Worker),
			s.Worker.Threads, s.Worker.OpenFDs, s.Worker.Goroutines, formatBytes(s.Worker.GoHeapBytes),
			s.ScrollbackRows, formatBytes(s.Agent.PrivateBytes))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "total: %d running sessions; private memory: workers %s, agents %s (top processes only), daemon %s\n",
		len(running), formatBytes(workers), formatBytes(agents), formatBytes(d.Daemon.PrivateBytes))
	return nil
}
