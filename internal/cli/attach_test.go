package cli

import (
	"strings"
	"testing"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

func TestAttachSequences(t *testing.T) {
	if attachRestoreSequence != "\x1b[?1049l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h" {
		t.Fatal("restore sequence changed")
	}
	got := string(attachStartupBytes([]byte("snapshot")))
	if got != "\x1b[>1u\x1b[2J\x1b[Hsnapshot" {
		t.Fatalf("startup %q", got)
	}
	if strings.Contains(got, "\x1b[?1049") {
		t.Fatal("attach enters the alternate screen")
	}
	if got := string(terminalTitleBytes("demo - " + attachExitTitle)); got != "\x1b]0;demo - agentd\x07\x1b]2;demo - agentd\x07" {
		t.Fatalf("title %q", got)
	}
}

func TestAdjacentSession(t *testing.T) {
	a, b, c := demo("alpha"), demo("beta"), demo("gamma")
	ordered := []*session.Record{&a, &b, &c}
	for _, tc := range []struct {
		current  string
		previous bool
		want     string
	}{{"alpha", false, "beta"}, {"gamma", false, "alpha"}, {"alpha", true, "gamma"}, {"beta", true, "alpha"}, {"missing", false, ""}} {
		if got := adjacentSessionIn(ordered, tc.current, tc.previous); got != tc.want {
			t.Errorf("%s previous=%v: got %q want %q", tc.current, tc.previous, got, tc.want)
		}
	}
	if got := adjacentSessionIn(ordered[:1], "alpha", false); got != "" {
		t.Fatalf("single session: %q", got)
	}
}

func TestFormatSessionEnd(t *testing.T) {
	code := int32(3)
	msg := "spawn failed"
	for _, tc := range []struct {
		e    protocol.SessionEnded
		want string
	}{
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusExited, ExitCode: &code}, "session a finished (exit 3)"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusExited}, "session a finished"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusFailed, Error: &msg}, "session a failed: spawn failed"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusFailed}, "session a failed"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusRunning}, "session a ended"},
	} {
		if got := formatSessionEnd(&tc.e); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestOverlayKeys(t *testing.T) {
	o := newOverlay(testClient(t), "alpha")
	if out, err := o.handle(keyEvent{code: keyChar, r: 'y', mods: modCtrl}); err != nil || out.kind != overlayStay {
		t.Fatalf("ctrl-y: %+v %v", out, err)
	}
	if out, _ := o.handle(key(keyEsc)); out.kind != overlayClose {
		t.Fatalf("esc: %+v", out)
	}
	o.handle(char('n'))
	o.handle(char('e'))
	if items := filteredPalette(o.paletteQuery); len(items) != 1 || items[0].title != "New Session" {
		t.Fatalf("filtered %+v", items)
	}
	o.handle(key(keyEnter))
	if o.mode != overlayNewSession || o.agentInput != "claude" {
		t.Fatalf("new session: %v %q", o.mode, o.agentInput)
	}
	o.handle(char('B'))
	o.handle(key(keyTab))
	o.handle(key(keyBackspace))
	if o.nameInput != "B" || o.agentInput != "claud" {
		t.Fatalf("fields %q %q", o.nameInput, o.agentInput)
	}
	o.handle(key(keyEnter))
	if !strings.Contains(o.toast, "invalid session name") {
		t.Fatalf("toast %q", o.toast)
	}
	o.handle(key(keyEsc))
	if o.mode != overlayPalette {
		t.Fatalf("esc from form: %v", o.mode)
	}
}

func TestOverlayDraws(t *testing.T) {
	o := newOverlay(testClient(t), "alpha")
	o.sessions = []session.Record{demo("alpha"), demo("beta")}
	body := o.body(60, 20)
	if got := stripANSI(body[0].render(60)); !strings.HasPrefix(got, "> ") || runeLen(got) != 60 {
		t.Fatalf("prompt %q", got)
	}
	if got := stripANSI(body[2].render(60)); !strings.HasPrefix(got, " s  Switch Session") {
		t.Fatalf("first item %q", got)
	}
	o.mode = overlaySwitcher
	if got := stripANSI(o.body(60, 20)[2].render(60)); !strings.Contains(got, "beta") || !strings.Contains(got, "/tmp/beta") {
		t.Fatalf("switcher row %q", got)
	}
	if x, y, w, h := overlayRect(100, 30); x != 10 || y != 3 || w != 80 || h != 24 {
		t.Fatalf("rect %d %d %d %d", x, y, w, h)
	}
	if got := wrapText("Press Enter to stop the current session, or Esc to cancel.", 20); len(got) != 3 || got[0] != "Press Enter to stop" {
		t.Fatalf("wrap %q", got)
	}
}
