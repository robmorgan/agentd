package cli

import (
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/session"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;?<>=]*[ -/]*[@-~]")

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

func demo(id string) session.Record { return demoWith(id, session.StatusRunning) }

func demoWith(id string, status session.Status) session.Record {
	now := time.Now()
	model := "gpt-5.4"
	worker, agent := uint32(123), uint32(456)
	return session.Record{
		SessionID: id, Agent: "codex", Model: &model, Mode: session.ModeExecute,
		Cwd: "/tmp/" + id, Status: status, WorkerPID: &worker, AgentPID: &agent,
		Attention: session.AttentionInfo, CreatedAt: now, UpdatedAt: now,
	}
}

// testClient is a client for a runtime root with no daemon and no config.
func testClient(t *testing.T) *client {
	t.Helper()
	return &client{paths: paths.FromRoot(filepath.Join(t.TempDir(), "root"))}
}

func key(code keyCode) keyEvent { return keyEvent{code: code} }
func char(r rune) keyEvent      { return keyEvent{code: keyChar, r: r} }
