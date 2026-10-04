package main

import (
	"strings"
	"testing"
)

func TestStatsEndToEnd(t *testing.T) {
	e := newEnv(t, "")
	e.startAndDetach("busy")

	out := e.mustRun("agent", "status", "--stats", "busy")
	for _, want := range []string{"status: running", "worker: pid ", "goroutines ", "agent: pid ", "terminal: 100x30"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status --stats lacks %q:\n%s", want, out)
		}
	}
	// The agent is not a Go program.
	if agent := lineWith(out, "agent: "); strings.Contains(agent, "goroutines") {
		t.Errorf("agent line: %s", agent)
	}

	out = e.mustRun("agent", "daemon", "stats")
	for _, want := range []string{"daemon: pid ", "SESSION", "busy", "total: 1 running sessions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("daemon stats lacks %q:\n%s", want, out)
		}
	}
}

func lineWith(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
