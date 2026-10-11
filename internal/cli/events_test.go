package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

func demoEvent() session.Event {
	return session.Event{ID: 42, SessionID: "fix", At: time.Date(2026, 10, 4, 8, 30, 5, 0, time.Local),
		Kind: session.EventBell, Attention: session.AttentionAction, Summary: "bell: \x1b[31m✳ Fix"}
}

func TestFormatEvent(t *testing.T) {
	ev := demoEvent()
	today := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	if got := formatEvent(ev, "", today); got != `08:30:05  #42  fix  action  bell          bell: \u{1b}[31m✳ Fix` {
		t.Fatalf("got %q", got)
	}
	got := formatEvent(ev, "devbox", today.AddDate(0, 0, 1))
	if !strings.HasPrefix(got, "2026-10-04 08:30:05  #42  devbox/fix  action") {
		t.Fatalf("other day, remote: %q", got)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(eventJSON(ev, "devbox")), &v); err != nil {
		t.Fatal(err)
	}
	if v["id"] != float64(42) || v["host"] != "devbox" || v["session"] != "fix" || v["kind"] != "bell" || v["attention"] != "action" {
		t.Fatalf("json = %v", v)
	}
	if _, ok := mapFromJSON(t, eventJSON(ev, ""))["host"]; ok {
		t.Fatal("local events name a host")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mapFromJSON(t *testing.T, s string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEventHandlerFiltersAndRuns(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	h := &eventHandler{host: "dev", minLevel: session.AttentionNotice, out: &out,
		exec: `printf '%s|%s|%s|%s|%s\n' "$AGENTD_EVENT_ID" "$AGENTD_EVENT_HOST" "$AGENTD_EVENT_SESSION" "$AGENTD_EVENT_KIND" "$AGENTD_EVENT_SUMMARY" >> ` + dir + `/log`}
	info := demoEvent()
	info.Attention, info.Kind = session.AttentionInfo, session.EventWorking
	if err := h.handle(info); err != nil || out.Len() != 0 {
		t.Fatalf("below --level: %q %v", out.String(), err)
	}
	if err := h.handle(demoEvent()); err != nil || !strings.Contains(out.String(), "#42  dev/fix") {
		t.Fatalf("printed %q, %v", out.String(), err)
	}
	if got := readFile(t, dir+"/log"); got != "42|dev|fix|bell|bell: \x1b[31m✳ Fix\n" {
		t.Fatalf("--exec saw %q", got)
	}
}

func TestAlertBytes(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ev := demoEvent()
	ev.Summary = "Approve;\x07 now"
	if got := string(alertBytes(ev, "", env(nil))); got != "\a\x1b]9;agentd: fix: Approve;  now\x1b\\" {
		t.Fatalf("osc 9: %q", got)
	}
	if got := string(alertBytes(ev, "dev", env(map[string]string{"VTE_VERSION": "7600"}))); got != "\a\x1b]777;notify;agentd: dev/fix;Approve;  now\x1b\\" {
		t.Fatalf("osc 777: %q", got)
	}
	got := string(alertBytes(ev, "", env(map[string]string{"TMUX": "/tmp/tmux"})))
	if !strings.HasPrefix(got, "\a\x1bPtmux;\x1b\x1b]9;") || !strings.HasSuffix(got, "\x1b\x1b\\\x1b\\") {
		t.Fatalf("tmux: %q", got)
	}
	// Value: protects=invisible format characters (unicode.Cf) becoming
	// spaces in OSC notification text, so a summary cannot reorder or
	// hide what the desktop notification appears to say; fails_when=
	// oscText drops its unicode.Cf arm; why_new=the cases above cover
	// only C0/C1 controls; seam=none
	if got := oscText("a‮b​c"); got != "a b c" {
		t.Fatalf("oscText kept format characters: %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]session.AttentionLevel{"": session.AttentionInfo, "notice": session.AttentionNotice, "action": session.AttentionAction} {
		if got, err := parseLevel(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseLevel("urgent"); err == nil {
		t.Error("unknown level accepted")
	}
}
