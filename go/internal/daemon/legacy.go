package daemon

import (
	"fmt"
	"net"
	"strings"

	"github.com/robmorgan/agentd/go/internal/db"
	"github.com/robmorgan/agentd/go/internal/paths"
)

// checkLegacyRuntime guards the one-way migration of a state.db written by
// the agentd that preceded the Go daemon (schema v6/v7, protocol v32).
// Migrating drops columns that daemon and its session workers still query,
// so it must not happen while any of them is running.
func checkLegacyRuntime(p *paths.AppPaths) error {
	version, err := db.SchemaVersion(p.Database)
	if err != nil || version == 0 || version >= db.CurrentSchemaVersion {
		return err
	}
	if answers(p.Socket) {
		return fmt.Errorf("the previous agentd is still running on %s and its state.db (schema v%d) cannot be upgraded under it; "+
			"stop it first: `agent daemon restart` stops it and starts this one", p.Socket, version)
	}
	live, err := legacySessionsStillRunning(p)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("sessions started by the previous agentd are still running: %s; they cannot be carried over to this agentd. "+
			"Exit those agents (or kill their worker processes), then try again", strings.Join(live, ", "))
	}
	return nil
}

// legacySessionsStillRunning lists pre-v8 sessions whose worker still
// answers on its socket, reading the old database without migrating it.
func legacySessionsStillRunning(p *paths.AppPaths) ([]string, error) {
	sessions, err := db.LegacyRunningSessions(p.Database)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, ls := range sessions {
		if !answers(p.SessionSocketPath(ls.SessionID)) {
			continue
		}
		if ls.WorkerPID != nil {
			live = append(live, fmt.Sprintf("%s (worker pid %d)", ls.SessionID, *ls.WorkerPID))
		} else {
			live = append(live, ls.SessionID)
		}
	}
	return live, nil
}

func answers(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, workerDialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
