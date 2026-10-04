package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

func testPicker(t *testing.T, sessions ...session.Record) *picker {
	p := newPicker(testClient(t))
	p.sessions = sessions
	return p
}

func TestPickerRowsIncludeCreateAndMatchingSessions(t *testing.T) {
	p := testPicker(t, demo("alpha"), demo("beta"))
	if rows := p.rows(); len(rows) != 3 || rows[2] != "" {
		t.Fatalf("rows %q", rows)
	}
	p.query = "beta"
	if rows := p.rows(); strings.Join(rows, ",") != "beta," {
		t.Fatalf("filtered rows %q", rows)
	}
	p.query = ""
	p.selected = 8
	p.clampSelection()
	if p.selected != 2 {
		t.Fatalf("selected %d", p.selected)
	}
}

func TestPickerLettersAreQueryInput(t *testing.T) {
	p := testPicker(t, demo("alpha"), demo("beta"))
	for _, r := range "qr" {
		if _, done, err := p.handleKey(char(r)); done || err != nil {
			t.Fatalf("%c: %v %v", r, done, err)
		}
	}
	if p.query != "qr" {
		t.Fatalf("query %q", p.query)
	}
	p.query = ""
	p.handleKey(char('b'))
	if p.selected != 0 || strings.Join(p.rows(), ",") != "beta," {
		t.Fatalf("after b: %d %q", p.selected, p.rows())
	}
	p.query = ""
	p.handlePaste("beta")
	if p.selected != 0 || strings.Join(p.rows(), ",") != "beta," {
		t.Fatalf("after paste: %d %q", p.selected, p.rows())
	}
}

func TestPickerEscapeCloses(t *testing.T) {
	p := testPicker(t)
	if chosen, done, err := p.handleKey(key(keyEsc)); !done || chosen != "" || err != nil {
		t.Fatalf("%q %v %v", chosen, done, err)
	}
}

func TestPickerActionMenu(t *testing.T) {
	p := testPicker(t, demo("alpha"))
	if got := p.actions("alpha"); len(got) != 2 || got[0] != actionAttach {
		t.Fatalf("live: %v", got)
	}
	exited := testPicker(t, demoWith("alpha", session.StatusExited))
	if got := exited.actions("alpha"); len(got) != 1 || got[0] != actionDelete {
		t.Fatalf("exited: %v", got)
	}
	p.setMode(pickerSessionActions, "alpha", 9)
	if p.mode != pickerSessionActions || p.modeIndex != 1 {
		t.Fatalf("clamped to %v %d", p.mode, p.modeIndex)
	}
	// Enter on the first row opens its actions; Enter on attach picks it.
	p.mode = pickerBrowse
	p.handleKey(key(keyEnter))
	if p.mode != pickerSessionActions || p.modeSession != "alpha" {
		t.Fatalf("mode %v %q", p.mode, p.modeSession)
	}
	if chosen, done, _ := p.handleKey(key(keyEnter)); !done || chosen != "alpha" {
		t.Fatalf("attach: %q %v", chosen, done)
	}
	// delete asks first, defaulting to no.
	p.setMode(pickerSessionActions, "alpha", 0)
	p.handleKey(char('2'))
	if p.mode != pickerDeleteConfirm || p.modeIndex != 1 {
		t.Fatalf("delete: %v %d", p.mode, p.modeIndex)
	}
	p.handleKey(char('n'))
	if p.mode != pickerSessionActions || p.modeIndex != 1 {
		t.Fatalf("n: %v %d", p.mode, p.modeIndex)
	}
}

func TestPickerModeResetsWhenSessionDisappears(t *testing.T) {
	p := testPicker(t)
	p.setMode(pickerDeleteConfirm, "missing", 1)
	if p.mode != pickerBrowse {
		t.Fatalf("mode %v", p.mode)
	}
}

func TestPickerCreateAgentMenu(t *testing.T) {
	p := testPicker(t, demo("alpha"))
	p.defaultAgent, p.createAgents = "codex", []string{"codex", "claude"}
	p.setMode(pickerCreateAgent, "", 9)
	if p.modeIndex != 1 {
		t.Fatalf("clamped to %d", p.modeIndex)
	}
	rendered := strings.Join(p.renderLines(120, true), "\n")
	for _, want := range []string{"1. codex (Default)", "› 2. claude"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(rendered, "/tmp/alpha") || strings.Contains(rendered, " • ") {
		t.Error("session list shown in a submenu")
	}
	// Enter on the create row opens the menu at the default agent.
	p.mode = pickerBrowse
	p.selected = 1
	p.handleKey(key(keyEnter))
	if p.mode != pickerCreateAgent || p.modeIndex != 0 {
		t.Fatalf("open: %v %d", p.mode, p.modeIndex)
	}
	// An invalid name is refused before the menu opens.
	p.mode, p.query, p.selected = pickerBrowse, "Bad Name", 0
	p.handleKey(key(keyEnter))
	if p.mode != pickerBrowse || p.toast == nil || !p.toast.isError {
		t.Fatalf("invalid name: %v %+v", p.mode, p.toast)
	}
}

func TestPickerDefaultAgents(t *testing.T) {
	p := testPicker(t)
	if strings.Join(p.createAgents, ",") != "claude,codex" || p.defaultAgent != "claude" {
		t.Fatalf("defaults: %v %q", p.createAgents, p.defaultAgent)
	}
	if got := orderedCreateAgents([]string{"codex", "claude", "zed"}, "claude"); strings.Join(got, ",") != "claude,codex,zed" {
		t.Fatalf("got %v", got)
	}
	if got := orderedCreateAgents([]string{"codex", "claude", "zed"}, ""); strings.Join(got, ",") != "codex,claude,zed" {
		t.Fatalf("got %v", got)
	}
}

func TestPickerRendering(t *testing.T) {
	p := testPicker(t, demo("alpha"), demo("beta"))
	p.setMode(pickerSessionActions, "alpha", 0)
	rendered := strings.Join(p.renderLines(120, true), "\n")
	if strings.Contains(rendered, "/tmp/beta") || !strings.Contains(rendered, "› 1. attach") ||
		!strings.Contains(rendered, "  2. delete") || !strings.Contains(rendered, "/tmp/alpha") {
		t.Fatalf("actions:\n%s", stripANSI(rendered))
	}
	p.setMode(pickerDeleteConfirm, "alpha", 1)
	rendered = strings.Join(p.renderLines(120, true), "\n")
	if !strings.Contains(rendered, "  1. yes") || !strings.Contains(rendered, "› 2. no") || !strings.Contains(rendered, "Its working directory is kept.") {
		t.Fatalf("delete:\n%s", stripANSI(rendered))
	}
	p.mode = pickerBrowse
	rendered = strings.Join(p.renderLines(120, true), "\n")
	if !strings.Contains(rendered, ansiRunning) || !strings.Contains(rendered, "running") || !strings.HasPrefix(rendered, "agentd -") {
		t.Fatalf("browse:\n%s", stripANSI(rendered))
	}
	p.toast = &toast{isError: true, message: "merge would conflict\nrun:\n  git status"}
	rendered = strings.Join(p.renderLines(200, true), "\n")
	if !strings.Contains(rendered, "error: merge would conflict run: git status") {
		t.Fatalf("toast:\n%s", stripANSI(rendered))
	}
	if line := toastLine(&toast{isError: true, message: "merge blocked"}, 120); !strings.HasPrefix(line, pickerErrorFG) || !strings.HasSuffix(line, ansiReset) {
		t.Fatalf("error toast %q", line)
	}
}

func TestPickerSessionRow(t *testing.T) {
	now := time.Now()
	s := demo("alpha")
	s.CreatedAt = now.Add(-23 * time.Minute)
	running := pickerSessionRow(&s, 120, true, now)
	f := demoWith("alpha", session.StatusFailed)
	failed := pickerSessionRow(&f, 120, false, now)
	if !strings.Contains(running, ansiRunning) || !strings.Contains(running, pickerSelectedStyle) || !strings.Contains(running, "› ") || !strings.Contains(failed, ansiFailed) {
		t.Fatal("styles")
	}
	if plain := stripANSI(running); !strings.Contains(plain, "23m") || !strings.Contains(plain, "alpha") || !strings.Contains(plain, "/tmp/alpha") {
		t.Fatalf("row %q", plain)
	}
	// The create row lines up with the session rows' age column.
	create := stripANSI(createRow("Create new session", 120, false))
	if a, b := runeLen(strings.Split(stripANSI(pickerSessionRow(&s, 120, false, now)), "23m")[0]), runeLen(strings.Split(create, "Create new session")[0]); a != b {
		t.Fatalf("columns %d and %d", a, b)
	}
	if got := stripANSI(createRow("Create new session: beta", 20, false)); !strings.HasSuffix(strings.TrimRight(got, " "), "...") {
		t.Fatalf("create row %q", got)
	}
	s.SessionID = "a-very-long-session-name-that-should-be-truncated-in-the-fixed-width-column"
	s.Cwd = "/srv/checkouts/a-very-long-directory-name/that-should-be-truncated"
	wide := stripANSI(pickerSessionRow(&s, 120, false, now))
	narrow := stripANSI(pickerSessionRow(&s, 60, false, now))
	if !strings.Contains(wide, "a-very") || !strings.Contains(wide, "directory-name/that-should-be-truncated") {
		t.Fatalf("wide %q", wide)
	}
	if !strings.Contains(narrow, "...") || !strings.HasSuffix(strings.TrimRight(narrow, " "), "truncated") {
		t.Fatalf("narrow %q", narrow)
	}
}

func TestPickerLegend(t *testing.T) {
	a, d, g := demo("alpha"), demoWith("delta", session.StatusExited), demoWith("gamma", session.StatusFailed)
	got := legendRow(200, []*session.Record{&a, &d, &g})
	for _, want := range []string{ansiFailed, ansiRunning, pickerLegendStyle, "●", "○", "✖", "running", "exited", "failed", " • "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.HasPrefix(got, "  ") || strings.Contains(got, "lost") {
		t.Fatalf("legend %q", got)
	}
}

func TestPickerQueryLine(t *testing.T) {
	empty := queryLine("", 60, true)
	if !strings.HasPrefix(empty, pickerQueryBG) || !strings.HasSuffix(empty, ansiReset) || !strings.Contains(empty, pickerPlaceholderFG) {
		t.Fatalf("empty %q", empty)
	}
	if strings.Index(empty, pickerCursor) > strings.Index(empty, "Type to filter sessions or name a new one.") {
		t.Fatal("cursor after placeholder")
	}
	typed := queryLine("abc", 30, false)
	if !strings.Contains(typed, "› ") || !strings.Contains(typed, "abc") || strings.Contains(typed, "Type to filter") {
		t.Fatalf("typed %q", typed)
	}
	if got := runeLen(stripANSI(queryLine("abc", 30, true))); got != 29 {
		t.Fatalf("query line is %d wide", got)
	}
	if row := backgroundRow(20); !strings.HasPrefix(row, pickerQueryBG) || !strings.HasSuffix(row, ansiReset) {
		t.Fatalf("background %q", row)
	}
	for _, seq := range []string{pickerEnterSequence, pickerExitSequence} {
		if strings.Contains(seq, "\x1b[?1049") {
			t.Fatal("picker enters the alternate screen")
		}
	}
}

func TestSessionList(t *testing.T) {
	now := time.Now()
	rendered := strings.Join(renderSessionListLines([]session.Record{demo("alpha")}, 120, now), "\n")
	for _, want := range []string{"RUN", "NAME", "CWD", "/tmp/alpha", "●"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(rendered, "agentd - ") || strings.Contains(rendered, "› ") {
		t.Fatal("list has a header or selector")
	}
	empty := strings.Join(renderSessionListLines(nil, 120, now), "\n")
	if !strings.Contains(empty, "No sessions.") || !strings.Contains(empty, "RUN") {
		t.Fatalf("empty %q", empty)
	}
	s := demo("alpha")
	s.SessionID = "a-very-long-session-name-that-should-be-truncated-in-the-fixed-width-column"
	s.Cwd = "/srv/checkouts/a-very-long-directory-name/that-should-be-truncated"
	summary := "needs manual review because a long follow-up summary is present"
	s.AttentionSummary = &summary
	wide := renderSessionListLines([]session.Record{s}, 120, now)
	narrow := strings.Join(renderSessionListLines([]session.Record{s}, 80, now), "\n")
	if len(wide) != 2 || strings.Contains(wide[1], summary) || !strings.Contains(wide[1], "a-very") || !strings.Contains(wide[1], "that-should-be-truncated") {
		t.Fatalf("wide %q", wide)
	}
	if !strings.Contains(narrow, "...") || !strings.Contains(narrow, "that-should-be-truncated") {
		t.Fatalf("narrow %q", narrow)
	}
	r := demoWith("beta", session.StatusUnknownRecovered)
	if got := strings.Join(renderSessionListLines([]session.Record{demo("alpha"), r}, 120, now), "\n"); strings.Contains(got, " • ") {
		t.Fatal("list has a legend")
	}
	for _, width := range []int{200, 80} {
		l := listLayout(width, false)
		header := stripANSI(renderSessionListLines(nil, width, now)[0])
		if got, want := runeLen(header), 10+l.run+l.age+l.name+l.activity+l.cwd; got != want {
			t.Errorf("width %d: header is %d wide, want %d", width, got, want)
		}
	}
}

func TestSessionListShowsAttention(t *testing.T) {
	ask := needing("asks", session.StatusRunning, session.AttentionAction, "Claude needs your permission to use Bash", time.Minute)
	ask.Activity = session.ActivityWaiting
	now := ask.AttentionAt.Add(time.Minute)
	quiet := demo("quiet")
	quiet.Activity = session.ActivityIdle
	quiet.LastOutputAt = &now
	lines := renderSessionListLines([]session.Record{quiet, ask}, 160, now)
	header := stripANSI(lines[0])
	if !strings.Contains(header, "ACTIVITY") || !strings.Contains(header, "ATTENTION") {
		t.Fatalf("header %q", header)
	}
	first := stripANSI(lines[1])
	if !strings.Contains(first, "⚠") || !strings.Contains(first, "asks") || !strings.Contains(first, "waiting 1m") ||
		!strings.Contains(first, "Claude needs your permission to use Bash") || !strings.Contains(lines[1], ansiAction) {
		t.Fatalf("first row %q", first)
	}
	if second := stripANSI(lines[2]); !strings.Contains(second, "quiet") || !strings.Contains(second, "idle 0s") || strings.Contains(second, "⚠") {
		t.Fatalf("second row %q", second)
	}
	l := listLayout(160, true)
	if got, want := runeLen(header), 12+l.run+l.age+l.name+l.activity+l.cwd+l.summary; got != want || l.summary < sessionListSummaryMinWidth {
		t.Fatalf("header is %d wide, want %d (summary %d)", got, want, l.summary)
	}
	// Narrow terminals drop the summary before anything else.
	if narrow := listLayout(60, true); narrow.summary != 0 {
		t.Fatalf("narrow summary %d", narrow.summary)
	}
}

func TestPickerShowsAttention(t *testing.T) {
	ask := needing("asks", session.StatusRunning, session.AttentionAction, "Approve the command?", time.Minute)
	ask.Activity = session.ActivityWaiting
	now := ask.AttentionAt.Add(time.Minute)
	row := pickerSessionRow(&ask, 120, false, now)
	if plain := stripANSI(row); !strings.Contains(plain, "⚠") || !strings.Contains(plain, "waiting 1m") || !strings.Contains(plain, "Approve the command?") {
		t.Fatalf("row %q", plain)
	}
	if runeLen(stripANSI(row)) > lineWidth(120) {
		t.Fatalf("row is %d wide", runeLen(stripANSI(row)))
	}
	// Sessions needing action come first, and the legend explains the sign.
	p := testPicker(t, demo("alpha"), ask)
	if rows := p.rows(); rows[0] != "asks" {
		t.Fatalf("rows %v", rows)
	}
	rendered := stripANSI(strings.Join(p.renderLines(120, true), "\n"))
	if !strings.Contains(rendered, "⚠ needs you") || !strings.Contains(rendered, "● running") {
		t.Fatalf("legend:\n%s", rendered)
	}
}
