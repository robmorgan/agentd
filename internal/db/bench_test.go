package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/robmorgan/agentd/internal/session"
)

// Every call opens and closes its own SQLite connection (see connect), so
// these include that cost; it is what each `agent ls` and every worker
// state change pays.

func openWithSessions(b *testing.B, n int) *Database {
	b.Helper()
	d, err := Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	for i := range n {
		if _, err := d.InsertSession(NewSession{
			SessionID: fmt.Sprintf("session-%05d", i), Agent: "claude", Mode: session.ModeExecute, Cwd: "/srv/repo",
		}); err != nil {
			b.Fatal(err)
		}
	}
	return d
}

func BenchmarkListSessions(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			d := openWithSessions(b, n)
			b.ReportAllocs()
			for b.Loop() {
				recs, err := d.ListSessions()
				if err != nil || len(recs) != n {
					b.Fatal(len(recs), err)
				}
			}
		})
	}
}

func BenchmarkGetSession(b *testing.B) {
	d := openWithSessions(b, 1000)
	b.ReportAllocs()
	for b.Loop() {
		if rec, err := d.GetSession("session-00500"); err != nil || rec == nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertSession(b *testing.B) {
	d := openWithSessions(b, 1000)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := d.InsertSession(NewSession{
			SessionID: fmt.Sprintf("new-%07d", i), Agent: "claude", Mode: session.ModeExecute, Cwd: "/srv/repo",
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMarkRunning is a worker's first write, which every session start
// waits for.
func BenchmarkMarkRunning(b *testing.B) {
	d := openWithSessions(b, 1000)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		id := fmt.Sprintf("new-%07d", i)
		createdAt, err := d.InsertSession(NewSession{SessionID: id, Agent: "claude", Mode: session.ModeExecute, Cwd: "/srv/repo"})
		if err != nil {
			b.Fatal(err)
		}
		if err := d.MarkRunning(id, createdAt, 1, 2); err != nil {
			b.Fatal(err)
		}
	}
}
