package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// Sessions across hosts. Every host's daemon owns its own sessions; the CLI
// only gathers what each one reports, so nothing here is a directory that
// could disagree with a daemon. A host that cannot be reached is reported
// and skipped, never waited on past probeTimeout.

// hostSession is a session together with the host that owns it.
type hostSession struct {
	host    string // "" for this machine
	welcome *protocol.Welcome
	rec     session.Record
}

// address is how commands name the session: `host/name`, or the name on
// this machine.
func (s hostSession) address() string {
	if s.host == "" {
		return s.rec.SessionID
	}
	return s.host + "/" + s.rec.SessionID
}

// globalID is the session's global identifier, if its daemon reports an id.
func (s hostSession) globalID() string {
	if s.welcome == nil {
		return ""
	}
	return protocol.GlobalSessionID(s.welcome.DaemonID, s.rec.UID)
}

// listAllSessions is `agent list --all`: this machine's sessions and every
// reachable host's, in one list ordered by what needs attention first.
func (a *app) listAllSessions(asJSON bool) error {
	// This machine's daemon is started as for any command; remote ones
	// are only asked.
	local := &client{paths: a.paths}
	err := local.ensureDaemon()
	local.close()
	if err != nil {
		return err
	}
	probes, err := a.probeHosts(false)
	if err != nil {
		return err
	}
	var listed []hostSession
	for _, p := range probes {
		if !p.online() {
			fmt.Fprintf(os.Stderr, "agent list: %s is %s", p.name, p.status)
			if p.err != nil && p.status != hostOffline {
				fmt.Fprintf(os.Stderr, ": %v", p.err)
			}
			fmt.Fprintln(os.Stderr)
			continue
		}
		host := p.name
		if p.host == nil {
			host = ""
		}
		for _, rec := range p.sessions {
			listed = append(listed, hostSession{host: host, welcome: p.welcome, rec: rec})
		}
	}
	if asJSON {
		return writeSessionsJSON(os.Stdout, listed)
	}
	sessions := make([]session.Record, len(listed))
	hosts := make([]string, len(listed))
	for i, s := range listed {
		sessions[i] = s.rec
		hosts[i] = s.host
		if hosts[i] == "" {
			hosts[i] = localHostName
		}
	}
	width := stdoutWidth()
	if width == 0 {
		width = sessionListDefaultWidth
	}
	for _, line := range renderSessionList(sessions, hosts, width, time.Now()) {
		fmt.Println(line)
	}
	return nil
}

// sessionJSON is one line of `agent list --json`.
type sessionJSON struct {
	Host      string     `json:"host"`
	Address   string     `json:"address"`
	GlobalID  string     `json:"global_id,omitempty"`
	Name      string     `json:"name"`
	UID       string     `json:"uid,omitempty"`
	Agent     string     `json:"agent"`
	Status    string     `json:"status"`
	Activity  string     `json:"activity,omitempty"`
	Attention string     `json:"attention"`
	Summary   string     `json:"attention_summary,omitempty"`
	Cwd       string     `json:"cwd"`
	Workspace string     `json:"workspace,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExitedAt  *time.Time `json:"exited_at,omitempty"`
}

// writeSessionsJSON writes one JSON object per session, for scripts.
func writeSessionsJSON(w io.Writer, sessions []hostSession) error {
	enc := json.NewEncoder(w)
	for _, s := range sessions {
		host := s.host
		if host == "" {
			host = localHostName
		}
		v := sessionJSON{
			Host: host, Address: s.address(), GlobalID: s.globalID(),
			Name: s.rec.SessionID, UID: s.rec.UID, Agent: s.rec.Agent,
			Status: string(s.rec.Status), Activity: string(s.rec.Activity),
			Attention: string(s.rec.Attention), Cwd: s.rec.Cwd,
			CreatedAt: s.rec.CreatedAt, ExitedAt: s.rec.ExitedAt,
		}
		if s.rec.AttentionSummary != nil && s.rec.Attention.Rank() > 0 {
			v.Summary = *s.rec.AttentionSummary
		}
		if s.rec.Workspace != nil {
			v.Workspace = *s.rec.Workspace
		}
		if err := enc.Encode(v); err != nil {
			return err
		}
	}
	return nil
}
