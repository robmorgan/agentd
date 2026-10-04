package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

func TestSessionListWithHosts(t *testing.T) {
	now := time.Now().UTC()
	summary := "waiting for input"
	sessions := []session.Record{
		{SessionID: "quiet", Status: session.StatusRunning, Attention: session.AttentionInfo, Cwd: "/w", CreatedAt: now, UpdatedAt: now},
		{SessionID: "needy", Status: session.StatusRunning, Attention: session.AttentionAction, AttentionSummary: &summary, AttentionAt: &now, Cwd: "/w", CreatedAt: now, UpdatedAt: now},
	}
	lines := renderSessionList(sessions, []string{"local", "devbox"}, 140, now)
	if !strings.Contains(lines[0], "HOST") {
		t.Fatalf("no HOST column: %q", lines[0])
	}
	// The session needing action sorts first and keeps its own host.
	if !strings.Contains(lines[1], "devbox") || !strings.Contains(lines[1], "needy") || !strings.Contains(lines[2], "local") || !strings.Contains(lines[2], "quiet") {
		t.Fatalf("rows:\n%s", strings.Join(lines, "\n"))
	}
	// Without hosts the list is unchanged.
	if plain := renderSessionList(sessions, nil, 140, now); strings.Contains(plain[0], "HOST") || plain[0] != renderSessionListLines(sessions, 140, now)[0] {
		t.Fatalf("plain list: %q", plain[0])
	}
}

func TestSessionsJSON(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	w := &protocol.Welcome{DaemonID: "SHA256:k"}
	var out bytes.Buffer
	err := writeSessionsJSON(&out, []hostSession{
		{host: "", welcome: w, rec: session.Record{SessionID: "a", UID: "u1", Agent: "sh", Status: session.StatusRunning, Attention: session.AttentionInfo, CreatedAt: now}},
		{host: "devbox", welcome: &protocol.Welcome{}, rec: session.Record{SessionID: "b", UID: "u2", Agent: "sh", Status: session.StatusExited, Attention: session.AttentionInfo, CreatedAt: now}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var first, second sessionJSON
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &second)
	if first.Host != "local" || first.Address != "a" || first.GlobalID != "SHA256:k/u1" {
		t.Fatalf("first = %+v", first)
	}
	// A daemon without an id has no global id to give.
	if second.Address != "devbox/b" || second.GlobalID != "" || second.Status != "exited" {
		t.Fatalf("second = %+v", second)
	}
}
