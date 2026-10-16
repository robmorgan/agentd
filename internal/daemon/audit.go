package daemon

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/robmorgan/agtd/internal/db"
	"github.com/robmorgan/agtd/internal/paths"
	"github.com/robmorgan/agtd/internal/session"
	"github.com/robmorgan/agtd/internal/transport"
)

// Host events (db.RecordHostEvent) are the audit record of who can reach
// this daemon remotely: pairings, revocations noticed on live connections,
// and remote connections. `agtd remote authorize` and `revoke` run
// outside the daemon and record their own.

// connectAuditInterval is how often a connection from the same key and
// address is recorded. Every remote command connects anew, so recording
// each one would bury the rest of the feed under a polling script.
const connectAuditInterval = 10 * time.Minute

// recordHostEvent records a host event and wakes event subscribers. A
// failure is logged, not returned: the audit record must not get in the
// way of what it records.
func (s *Server) recordHostEvent(kind session.EventKind, summary string) {
	if _, err := s.db.RecordHostEvent(db.NewEvent{Kind: kind, Summary: summary}); err != nil {
		fmt.Fprintf(os.Stderr, "agtd: recording %s event: %v\n", kind, err)
		return
	}
	s.events.notify()
}

// RecordHostEvent records a host event from outside the daemon, as
// `agtd remote authorize` and `revoke` do. A running daemon's
// subscribers see it on their next poll of the events table.
func RecordHostEvent(p *paths.AppPaths, kind session.EventKind, summary string) error {
	store, err := db.Open(p.Database)
	if err != nil {
		return err
	}
	_, err = store.RecordHostEvent(db.NewEvent{Kind: kind, Summary: summary})
	return err
}

// clientName is the name a client key is authorized under, if any.
func (s *Server) clientName(fp string) string {
	return transport.AuthorizedName(s.paths.AuthorizedClientsPath(), fp)
}

// remoteConnected records a remote client's connection, at most once per
// key and address every connectAuditInterval.
func (s *Server) remoteConnected(fp string, remote net.Addr) {
	if !s.connects.due(fp, remote) {
		return
	}
	s.recordHostEvent(session.EventConnected,
		fmt.Sprintf("%s connected from %s", transport.DescribeClient(fp, s.clientName(fp)), hostOf(remote)))
}

// remoteRevoked records the daemon dropping a connection whose key was
// revoked. The key is gone from the authorized file by now, so the event
// names it by fingerprint.
func (s *Server) remoteRevoked(fp string, remote net.Addr) {
	s.recordHostEvent(session.EventDisconnected,
		fmt.Sprintf("%s at %s disconnected: key revoked", fp, hostOf(remote)))
}

// hostOf is remote's address without the port, which changes with every
// connection and says nothing about who connected.
func hostOf(remote net.Addr) string {
	if remote == nil {
		return "an unknown address"
	}
	if host, _, err := net.SplitHostPort(remote.String()); err == nil {
		return host
	}
	return remote.String()
}

// connectAudit remembers when each key and address was last recorded as
// connecting.
type connectAudit struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
}

func newConnectAudit() *connectAudit {
	return &connectAudit{now: time.Now, last: map[string]time.Time{}}
}

// due reports whether a connection from fp at remote should be recorded,
// and if so notes that it was.
func (a *connectAudit) due(fp string, remote net.Addr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	// Forget what has aged out, so the map stays as small as the set of
	// recently active clients.
	for k, at := range a.last {
		if now.Sub(at) >= connectAuditInterval {
			delete(a.last, k)
		}
	}
	key := fp + " " + hostOf(remote)
	if _, seen := a.last[key]; seen {
		return false
	}
	a.last[key] = now
	return true
}
