package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/go/internal/session"
)

func TestFormatElapsedUsesLargestUnit(t *testing.T) {
	for seconds, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m", 3599: "59m", 3600: "1h", 86399: "23h", 86400: "1d", 3 * 86400: "3d"} {
		if got := formatElapsed(seconds); got != want {
			t.Errorf("%d: got %q want %q", seconds, got, want)
		}
	}
}

func TestElapsedLabelUsesExitTimeAndClampsNegative(t *testing.T) {
	now := time.Now()
	s := demoWith("a", session.StatusExited)
	s.CreatedAt = now.Add(-5 * time.Hour)
	exited := s.CreatedAt.Add(90 * time.Minute)
	s.ExitedAt = &exited
	if got := elapsedLabel(&s, now); got != "1h" {
		t.Fatalf("exited: %q", got)
	}
	s.ExitedAt = nil
	s.CreatedAt = now.Add(time.Hour)
	if got := elapsedLabel(&s, now); got != "0s" {
		t.Fatalf("future start: %q", got)
	}
}

func TestDisplayRowMarksAttentionFromActionOrFailure(t *testing.T) {
	now := time.Now()
	s := demo("a")
	if buildDisplayRow(&s, now).needsAttention {
		t.Fatal("running session needs attention")
	}
	s.Attention = session.AttentionAction
	if !buildDisplayRow(&s, now).needsAttention {
		t.Fatal("action not marked")
	}
	f := demoWith("f", session.StatusFailed)
	if !buildDisplayRow(&f, now).needsAttention {
		t.Fatal("failure not marked")
	}
}

func TestRunStatesAndIcons(t *testing.T) {
	for status, want := range map[session.Status]struct {
		run  runState
		icon string
	}{
		session.StatusCreating:         {runStarting, "●"},
		session.StatusRunning:          {runRunning, "●"},
		session.StatusExited:           {runExited, "○"},
		session.StatusFailed:           {runFailed, "✖"},
		session.StatusUnknownRecovered: {runRecovered, "○"},
	} {
		s := demoWith("a", status)
		if got := sessionRunState(&s); got != want.run || runIcon(got) != want.icon {
			t.Errorf("%s: %v %q", status, got, runIcon(got))
		}
	}
	if !strings.HasPrefix(styleRun("x", runRunning), ansiRunning) || !strings.HasPrefix(styleRun("x", runFailed), ansiFailed) ||
		!strings.HasPrefix(styleRun("x", runExited), ansiInactive) {
		t.Fatal("run colours")
	}
}

func TestPathsWithControlCharactersAreEscaped(t *testing.T) {
	if got := escapeControls("/tmp/\x1b]0;evil\x07\tx\u0085"); got != `/tmp/\u{1b}]0;evil\u{7}\tx\u{85}` {
		t.Fatalf("got %q", got)
	}
	if got := escapeControls("/tmp/plain"); got != "/tmp/plain" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayCwdAbbreviatesHome(t *testing.T) {
	for _, tc := range []struct{ cwd, home, want string }{
		{"/home/t", "/home/t", "~"},
		{"/home/t/src", "/home/t/", "~/src"},
		{"/home/tom", "/home/t", "/home/tom"},
		{"/srv", "", "/srv"},
	} {
		if got := displayCwd(tc.cwd, tc.home); got != tc.want {
			t.Errorf("%q in %q: got %q want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}

func TestOrderedSessionsKeepActiveFirstThenAttention(t *testing.T) {
	now := time.Now()
	running := demo("running")
	running.UpdatedAt = now.Add(-2 * time.Minute)
	creating := demoWith("creating", session.StatusCreating)
	creating.UpdatedAt = now.Add(-time.Minute)
	failed := demoWith("failed", session.StatusFailed)
	exited := demoWith("exited", session.StatusExited)
	needsAction := demoWith("needs-action", session.StatusExited)
	needsAction.Attention = session.AttentionAction
	needsAction.UpdatedAt = now.Add(-5 * time.Minute)
	var got []string
	for _, s := range orderedSessions([]session.Record{exited, failed, running, needsAction, creating}) {
		got = append(got, s.SessionID)
	}
	if strings.Join(got, " ") != "creating running failed needs-action exited" {
		t.Fatalf("got %v", got)
	}
}

func TestCellFormatting(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{{"/a/b", 6, "/a/b  "}, {"/srv/repo/checkout", 10, "...heckout"}, {"/srv/repo", 3, "epo"}} {
		if got := formatPathCell(tc.in, tc.width); got != tc.want {
			t.Errorf("path %q/%d: %q", tc.in, tc.width, got)
		}
	}
	if got := formatCell("abcdefgh", 6); got != "abc..." {
		t.Fatalf("cell: %q", got)
	}
	if got := fitLine("abcdef", 4); got != "abc" {
		t.Fatalf("fitLine: %q", got)
	}
}

func TestHeader(t *testing.T) {
	// Tests do not run on a terminal, so the header is plain.
	if got := header(); got != headerText {
		t.Fatalf("got %q", got)
	}
	if got := stripANSI(colorHeader()); got != headerText {
		t.Fatalf("coloured header text: %q", got)
	}
	if !strings.Contains(colorHeader(), fgRGB(primaryBlue)+"agentd") {
		t.Fatal("agentd is not in the primary blue")
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for want, vars := range map[bool]map[string]string{
		true:  {"COLORTERM": "truecolor"},
		false: {"COLORTERM": "truecolor", "NO_COLOR": "1"},
	} {
		if got := supportsTrueColor(env(vars)); got != want {
			t.Errorf("%v: got %v", vars, got)
		}
	}
	if supportsTrueColor(env(map[string]string{"TERM": "xterm-256color"})) {
		t.Error("256 colours are not true colour")
	}
}
