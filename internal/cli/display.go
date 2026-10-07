package cli

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robmorgan/agentd/internal/session"
)

const (
	ansiReset    = "\x1b[0m"
	ansiRunning  = "\x1b[32m"
	ansiFailed   = "\x1b[31m"
	ansiInactive = "\x1b[90m"
	ansiEmphasis = "\x1b[1m"
	ansiDimText  = "\x1b[2m\x1b[90m"
	ansiAction   = "\x1b[33m"
)

// runState is how a session is shown: its status, with creating shown as
// starting and unknown_recovered as recovered.
type runState int

const (
	runStarting runState = iota
	runRunning
	runExited
	runFailed
	runRecovered
)

func sessionRunState(s *session.Record) runState {
	switch s.Status {
	case session.StatusCreating:
		return runStarting
	case session.StatusRunning:
		return runRunning
	case session.StatusFailed:
		return runFailed
	case session.StatusUnknownRecovered:
		return runRecovered
	}
	return runExited
}

type displayRow struct {
	run            runState
	age, name, cwd string
	needsAttention bool
	// attention is the session's pending attention, and summary its
	// text when it is above info (else "").
	attention session.AttentionLevel
	summary   string
	activity  string
}

func buildDisplayRow(s *session.Record, now time.Time) displayRow {
	run := sessionRunState(s)
	row := displayRow{
		run:            run,
		age:            elapsedLabel(s, now),
		name:           s.SessionID,
		cwd:            displayCwd(s.Cwd, os.Getenv("HOME")),
		needsAttention: run == runFailed || s.Attention == session.AttentionAction,
		attention:      s.Attention,
		activity:       activityText(s, now),
	}
	if s.Attention.Rank() > 0 && s.AttentionSummary != nil {
		row.summary = escapeControls(strings.Join(strings.Fields(*s.AttentionSummary), " "))
	}
	return row
}

// activityText is what a session is doing, for lists: "working" (with the
// progress the program reported, if any), "idle 5m" (since its last
// output), "waiting" or "blocked" (since they asked), or for one that is
// not running its state.
func activityText(s *session.Record, now time.Time) string {
	switch sessionRunState(s) {
	case runStarting:
		return "starting"
	case runExited, runFailed, runRecovered:
		return "exited"
	}
	switch s.Activity {
	case session.ActivityUnknown:
		return "running"
	case session.ActivityWorking:
		if p := s.StatusProgress; p != nil && *p > 0 && *p < 100 {
			return fmt.Sprintf("working %d%%", *p)
		}
	case session.ActivityIdle:
		if s.LastOutputAt != nil {
			return "idle " + formatElapsed(max(int64(now.Sub(*s.LastOutputAt)/time.Second), 0))
		}
	case session.ActivityWaiting, session.ActivityBlocked:
		if s.AttentionAt != nil {
			return string(s.Activity) + " " + formatElapsed(max(int64(now.Sub(*s.AttentionAt)/time.Second), 0))
		}
	}
	return escapeControls(string(s.Activity))
}

// rowIcon is the icon a list shows for a session: its run state, or a
// warning sign for a live session that needs action.
func rowIcon(row displayRow) (icon, style string) {
	if row.attention == session.AttentionAction && (row.run == runRunning || row.run == runStarting) {
		return "⚠", ansiAction
	}
	return runIcon(row.run), runStyle(row.run)
}

// styleSummary styles an attention summary: action stands out.
func styleSummary(text string, a session.AttentionLevel) string {
	if a == session.AttentionAction {
		return ansiAction + text + ansiReset
	}
	return text
}

// displayCwd shortens a session cwd for list views by replacing the home
// directory with ~. The full path is still shown by `agent status`.
func displayCwd(cwd, home string) string {
	cwd = escapeControls(cwd)
	home = strings.TrimRight(home, "/")
	if home == "" {
		return cwd
	}
	rest, ok := strings.CutPrefix(cwd, home)
	switch {
	case !ok:
		return cwd
	case rest == "":
		return "~"
	case strings.HasPrefix(rest, "/"):
		return "~" + rest
	}
	return cwd
}

// escapeControls makes control characters in a path visible (\t, \u{1b})
// rather than letting a terminal act on them. Anything shown from a path
// goes through it first.
func escapeControls(text string) string {
	if !strings.ContainsFunc(text, unicode.IsControl) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func runIcon(r runState) string {
	switch r {
	case runStarting, runRunning:
		return "●"
	case runFailed:
		return "✖"
	}
	return "○"
}

func styleRun(text string, r runState) string {
	switch r {
	case runStarting, runRunning:
		return ansiRunning + text + ansiReset
	case runFailed:
		return ansiFailed + text + ansiReset
	}
	return ansiInactive + text + ansiReset
}

func styleAge(text string) string  { return ansiDimText + text + ansiReset }
func styleName(text string) string { return ansiEmphasis + text + ansiReset }
func styleCwd(text string) string  { return ansiDimText + text + ansiReset }

// elapsedLabel is how long a session ran (or has been running): 42s, 5m,
// 3h, 2d.
func elapsedLabel(s *session.Record, now time.Time) string {
	end := now
	if s.ExitedAt != nil {
		end = *s.ExitedAt
	}
	return formatElapsed(max(int64(end.Sub(s.CreatedAt)/time.Second), 0))
}

func formatElapsed(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 60*60:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 60*60*24:
		return fmt.Sprintf("%dh", seconds/(60*60))
	}
	return fmt.Sprintf("%dd", seconds/(60*60*24))
}

// sessionSortBucket orders sessions for every list: live ones needing
// action first, then live ones with something to notice, the other live
// ones, failed or lost ones, ended ones whose end is unseen (action, then
// notice), and finished ones last. Ended sessions stay below live ones
// even when they need action: only removing them clears it, and they
// would otherwise crowd out the sessions the user can still act in.
func sessionSortBucket(s *session.Record) int {
	live := s.Status == session.StatusCreating || s.Status == session.StatusRunning
	switch {
	case live:
		return 2 - s.Attention.Rank()
	case s.Status == session.StatusFailed || s.Status == session.StatusUnknownRecovered:
		return 3
	case s.Attention == session.AttentionAction:
		return 4
	case s.Attention == session.AttentionNotice:
		return 5
	}
	return 6
}

// sortTime orders sessions within a bucket, newest first: when their
// attention was raised, or else when they last changed.
func sortTime(s *session.Record) time.Time {
	if s.Attention.Rank() > 0 && s.AttentionAt != nil {
		return *s.AttentionAt
	}
	return s.UpdatedAt
}

// orderedSessions sorts by bucket, most recent first within one.
func orderedSessions(sessions []session.Record) []*session.Record {
	out := make([]*session.Record, len(sessions))
	for i := range sessions {
		out[i] = &sessions[i]
	}
	slices.SortStableFunc(out, func(a, b *session.Record) int {
		if d := sessionSortBucket(a) - sessionSortBucket(b); d != 0 {
			return d
		}
		return sortTime(b).Compare(sortTime(a))
	})
	return out
}

func sessionSearchText(s *session.Record) string {
	return fmt.Sprintf("%s %s %s %s %s %s", s.SessionID, s.Agent, s.Cwd, s.Status, s.Attention, s.Activity)
}

func matchesQuery(haystack, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(query))
}

// Widths below count runes, as a terminal counts cells for the ASCII and
// most of the text shown here.

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func takeRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if n >= len(r) {
		return s
	}
	return string(r[len(r)-n:])
}

func padRight(s string, width int) string {
	if n := runeLen(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// truncateCell cuts s to width runes, ending in "..." when there is room.
func truncateCell(s string, width int) string {
	if runeLen(s) <= width {
		return s
	}
	if width <= 3 {
		return takeRunes(s, width)
	}
	return takeRunes(s, width-3) + "..."
}

func formatCell(s string, width int) string { return padRight(truncateCell(s, width), width) }

// formatPathCell is formatCell, but truncates from the left so the end of a
// path (usually the most specific part) stays visible.
func formatPathCell(s string, width int) string {
	n := runeLen(s)
	switch {
	case n <= width:
	case width <= 3:
		s = lastRunes(s, width)
	default:
		s = "..." + lastRunes(s, width-3)
	}
	return padRight(s, width)
}

const (
	headerText        = "agentd - agent multiplexer"
	headerSuffix      = " - agent multiplexer"
	headerSuffixGamma = 1.35
)

type rgb struct{ r, g, b uint8 }

var (
	primaryBlue   = rgb{95, 251, 255}
	defaultViolet = rgb{185, 131, 255}
)

func fgRGB(c rgb) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b) }

// header is the "agentd - agent multiplexer" banner: a fixed truecolor
// gradient on a truecolor terminal, plain text anywhere else.
func header() string {
	if !stdoutIsTerminal() || !supportsTrueColor(os.Getenv) {
		return headerText
	}
	return colorHeader()
}

func colorHeader() string {
	var b strings.Builder
	b.WriteString("\x1b[1m" + fgRGB(primaryBlue) + "agentd\x1b[39m")
	suffix := []rune(headerSuffix)
	denom := math.Max(float64(len(suffix)-1), 1)
	for i, r := range suffix {
		t := math.Pow(float64(i)/denom, headerSuffixGamma)
		lerp := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t)) }
		b.WriteString(fgRGB(rgb{lerp(primaryBlue.r, defaultViolet.r), lerp(primaryBlue.g, defaultViolet.g), lerp(primaryBlue.b, defaultViolet.b)}))
		b.WriteRune(r)
	}
	b.WriteString("\x1b[39m\x1b[22m")
	return b.String()
}

// supportsTrueColor follows the usual conventions: NO_COLOR and TERM=dumb
// turn colour off, and COLORTERM (or a TERM naming it) says the terminal
// takes 24-bit colour.
func supportsTrueColor(getenv func(string) string) bool {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return false
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}
	term := getenv("TERM")
	return strings.Contains(term, "truecolor") || strings.Contains(term, "24bit") || strings.HasSuffix(term, "-direct")
}
