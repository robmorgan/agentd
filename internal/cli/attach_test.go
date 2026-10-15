package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

func TestAttachSequences(t *testing.T) {
	if attachRestoreSequence != "\x1b[?1049l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[>4m\x1b[?25h" {
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
	o := newOverlay(testClient(t), "alpha", nil)
	if out, err := o.handle(keyEvent{code: keyChar, r: 'y', mods: modCtrl}); err != nil || out.kind != overlayStay {
		t.Fatalf("ctrl-y: %+v %v", out, err)
	}
	if out, _ := o.handle(key(keyEsc)); out.kind != overlayClose {
		t.Fatalf("esc: %+v", out)
	}
	o.handle(char('n'))
	o.handle(char('e'))
	if items := o.filteredPalette(); len(items) != 1 || items[0].title != "New session" {
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
	o := newOverlay(testClient(t), "alpha", nil)
	o.sessions = []session.Record{demo("alpha"), demo("beta")}
	body := o.body(60, 20)
	if got := stripANSI(body[1].render(60, o.theme)); !strings.HasPrefix(got, " Commands ") || !strings.Contains(got, "/alpha  ") || strings.Contains(got, "╱") {
		t.Fatalf("title %q", got)
	}
	if got := stripANSI(body[3].render(60, o.theme)); !strings.HasPrefix(got, " ❯ ") || runeLen(got) != 60 {
		t.Fatalf("prompt %q", got)
	}
	if got := stripANSI(body[5].render(60, o.theme)); !strings.HasPrefix(got, "▌ Switch session") || !strings.HasSuffix(got, "s ") {
		t.Fatalf("first item %q", got)
	}
	if got := stripANSI(body[6].render(60, o.theme)); !strings.HasPrefix(got, "  New session") {
		t.Fatalf("second item %q", got)
	}
	if got := stripANSI(body[18].render(60, o.theme)); !strings.Contains(got, "enter run") {
		t.Fatalf("hints %q", got)
	}
	o.mode = overlaySwitcher
	if got := stripANSI(o.body(60, 20)[5].render(60, o.theme)); !strings.Contains(got, "beta") || !strings.Contains(got, "/tmp/beta") {
		t.Fatalf("switcher row %q", got)
	}
	for d, want := range map[time.Duration]string{
		40 * time.Second:                              "40s",
		12 * time.Minute:                              "12m",
		3*time.Hour + 5*time.Minute:                   "3h5m",
		51*time.Hour + 44*time.Minute + 9*time.Second: "2d3h44m",
		48 * time.Hour:                                "2d",
	} {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
	o.mode = overlayStopConfirm
	if got := stripANSI(o.body(60, 20)[5].render(60, o.theme)); !strings.Contains(got, "Stop") || !strings.Contains(got, "Cancel") {
		t.Fatalf("stop buttons %q", got)
	}
	if x, y, w, h := overlayRect(100, 30); x != 10 || y != 3 || w != 80 || h != 24 {
		t.Fatalf("rect %d %d %d %d", x, y, w, h)
	}
	if got := wrapText("Press Enter to stop the current session, or Esc to cancel.", 20); len(got) != 3 || got[0] != "Press Enter to stop" {
		t.Fatalf("wrap %q", got)
	}
}

// The overlay offers to take the size only while another attachment owns
// it, and choosing that closes it with overlayTookSize.
func TestOverlayTakeSize(t *testing.T) {
	titles := func(o *overlay) []string {
		var out []string
		for _, item := range o.filteredPalette() {
			out = append(out, item.title)
		}
		return out
	}
	if got := titles(newOverlay(testClient(t), "alpha", nil)); slices.Contains(got, takeSizeItem.title) {
		t.Fatalf("take-size offered without size ownership: %v", got)
	}
	owner := &protocol.SizeOwnership{Owner: true, OwnerID: "attach-1"}
	if got := titles(newOverlay(testClient(t), "alpha", owner)); slices.Contains(got, takeSizeItem.title) {
		t.Fatalf("take-size offered to the owner: %v", got)
	}
	pending := &protocol.SizeOwnership{OwnerID: "attach-1", Geometry: protocol.Geometry{Cols: 100, Rows: 30}}
	o := newOverlay(testClient(t), "alpha", pending)
	for _, r := range "take" {
		o.handle(char(r))
	}
	if got := titles(o); len(got) != 1 || got[0] != takeSizeItem.title {
		t.Fatalf("filtered %v", got)
	}
	if out, err := o.handle(key(keyEnter)); err != nil || out.kind != overlayTookSize {
		t.Fatalf("enter: %+v %v", out, err)
	}
	if got := len(paletteItems); got != 4 {
		t.Fatalf("the take-size item leaked into the shared palette: %d items", got)
	}

	if got := sizeDetails(pending); !strings.Contains(got, "100x30, set by attach-1") {
		t.Fatalf("pending details %q", got)
	}
	if got := sizeDetails(&protocol.SizeOwnership{Owner: true, Geometry: protocol.Geometry{Cols: 80, Rows: 24}}); !strings.Contains(got, "80x24, set by this terminal") {
		t.Fatalf("owner details %q", got)
	}
}

// Size notes update the attachment's ownership without ending it.
func TestSizeOwnerFrames(t *testing.T) {
	size := &protocol.SizeOwnership{OwnerID: "attach-1"}
	note := protocol.SizeOwnership{Owner: true, OwnerID: "attach-2", Geometry: protocol.Geometry{Cols: 90, Rows: 20}}
	for _, overlay := range []bool{false, true} {
		*size = protocol.SizeOwnership{}
		if _, done, err := handleFrame(frame{resp: &protocol.Response{AttachSizeOwner: &note}}, overlay, size); done || err != nil || *size != note {
			t.Fatalf("size note (overlay %v): done %v err %v size %+v", overlay, done, err, size)
		}
	}
	if _, done, err := handleFrame(frame{resp: &protocol.Response{AttachSizeOwner: &note}}, false, nil); done || err != nil {
		t.Fatalf("size note without tracking: done %v err %v", done, err)
	}
}
