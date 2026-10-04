package worker

import (
	"time"

	"github.com/robmorgan/agentd/internal/procstat"
	"github.com/robmorgan/agentd/internal/protocol"
)

// stats measures the session: this worker process (from the OS and its own
// Go runtime), the agent's top process, and the terminal and output state.
//
// The terminal and fan-out counters are read on the owner goroutine, like
// everything else in ownerState. With snapshot, a reattach snapshot is also
// formatted there, which holds up PTY output exactly as an attach does, and
// is then replayed into a fresh terminal off the owner goroutine to time
// what a client's terminal does with it.
func (rt *runtime) stats(snapshot bool) (*protocol.SessionStats, error) {
	out := &protocol.SessionStats{
		SessionID:            rt.sessionID,
		ScrollbackLimitBytes: uint64(maxScrollbackBytes),
	}
	var snap []byte
	var formatTime time.Duration
	if err := rt.owner.do(func(s *ownerState) error {
		out.Cols, out.Rows = s.geometry.Cols, s.geometry.Rows
		out.ScrollbackRows = s.terminal.scrollbackRows()
		out.Attachments = uint32(len(s.attachments))
		out.OutputBytes = s.outputBytes
		out.OutputChunks = s.outputChunks
		out.DroppedOutputChunks = s.output.droppedChunks()
		if snapshot {
			start := time.Now()
			var err error
			if snap, err = s.snapshot(); err != nil {
				return err
			}
			formatTime = time.Since(start)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if snapshot {
		restoreTime, err := restoreTime(snap, out.Cols, out.Rows)
		if err != nil {
			return nil, err
		}
		out.Snapshot = &protocol.SnapshotStats{
			Bytes:        uint64(len(snap)),
			FormatNanos:  uint64(formatTime),
			RestoreNanos: uint64(restoreTime),
		}
	}
	// Read last, so the formatting above is included in the worker's own
	// numbers rather than skewing a later sample.
	out.Worker = procstat.DescribeSelf(rt.startedAt)
	out.Agent = procstat.Describe(rt.agentPID)
	return out, nil
}

// restoreTime replays a snapshot into a fresh terminal of the session's
// size and reports how long that took.
func restoreTime(snapshot []byte, cols, rows uint16) (time.Duration, error) {
	t, err := newTerminalState(cols, rows, maxScrollbackBytes)
	if err != nil {
		return 0, err
	}
	defer t.close()
	start := time.Now()
	t.feed(snapshot)
	return time.Since(start), nil
}
