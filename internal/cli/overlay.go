package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/robmorgan/agtd/internal/config"
	"github.com/robmorgan/agtd/internal/protocol"
	"github.com/robmorgan/agtd/internal/session"
)

// The Ctrl-Y overlay: a box drawn over the attached session, with a command
// palette, a session switcher, a new-session form, session details and a
// stop confirmation.

type overlayMode int

const (
	overlayPalette overlayMode = iota
	overlaySwitcher
	overlayNewSession
	overlayDetails
	overlayStopConfirm
	// overlayTakeSize is a palette action rather than a mode: the
	// attachment takes the session's size and the overlay closes.
	overlayTakeSize
)

type overlayOutcomeKind int

const (
	overlayStay overlayOutcomeKind = iota
	overlayClose
	overlaySwitch
	// overlayTookSize closes the overlay after the attachment asked to
	// own the session's size.
	overlayTookSize
)

type overlayOutcome struct {
	kind    overlayOutcomeKind
	session string // overlaySwitch
}

type paletteItem struct {
	hint, title string
	mode        overlayMode
}

var paletteItems = []paletteItem{
	{"s", "Switch session", overlaySwitcher},
	{"t", "New session", overlayNewSession},
	{"i", "Session details", overlayDetails},
	{"x", "Stop session", overlayStopConfirm},
}

// takeSizeItem is offered while another attachment owns the size.
var takeSizeItem = paletteItem{"r", "Take over size", overlayTakeSize}

func (o *overlay) filteredPalette() []paletteItem {
	items := paletteItems
	if o.size != nil && !o.size.Owner {
		items = append(slices.Clip(items), takeSizeItem)
	}
	var out []paletteItem
	for _, item := range items {
		if matchesQuery(item.hint+" "+item.title, o.paletteQuery) {
			out = append(out, item)
		}
	}
	return out
}

type overlay struct {
	c       *client
	session string // the attached session
	keys    keyDecoder
	// size is the attachment's size ownership, nil when the session's
	// worker does not track it (protocol.CapAttachSizeOwner).
	size *protocol.SizeOwnership

	sessions         []session.Record
	mode             overlayMode
	paletteQuery     string
	paletteSelected  int
	switcherQuery    string
	switcherSelected int
	editAgent        bool
	nameInput        string
	agentInput       string
	detailText       string
	detailScroll     int
	toast            string
	theme            *overlayTheme
	now              func() time.Time
}

func newOverlay(c *client, id string, size *protocol.SizeOwnership) *overlay {
	return &overlay{c: c, session: id, size: size, theme: overlayThemeFor(os.Getenv), now: time.Now}
}

func (o *overlay) open() error {
	o.mode = overlayPalette
	o.paletteQuery, o.paletteSelected = "", 0
	o.switcherQuery, o.switcherSelected = "", 0
	o.toast = ""
	o.draw()
	return o.refresh()
}

func (o *overlay) refresh() error {
	sessions, err := o.c.listSessions(o.c.requestTimeout())
	if err != nil {
		return err
	}
	o.sessions = sessions
	if n := len(o.filteredSessions()); o.switcherSelected >= n {
		o.switcherSelected = max(n-1, 0)
	}
	return nil
}

func (o *overlay) filteredSessions() []*session.Record {
	var out []*session.Record
	for _, s := range orderedSessions(o.sessions) {
		if matchesQuery(sessionSearchText(s), o.switcherQuery) {
			out = append(out, s)
		}
	}
	return out
}

func popRune(s string) string {
	if r := []rune(s); len(r) > 0 {
		return string(r[:len(r)-1])
	}
	return s
}

func (o *overlay) handle(ev keyEvent) (overlayOutcome, error) {
	if ev.isPaste {
		switch o.mode {
		case overlayNewSession:
			if o.editAgent {
				o.agentInput += ev.paste
			} else {
				o.nameInput += ev.paste
			}
		case overlayPalette:
			o.paletteQuery += ev.paste
		case overlaySwitcher:
			o.switcherQuery += ev.paste
		}
		return overlayOutcome{}, nil
	}
	if ev.code == keyEsc && o.mode != overlayPalette && o.mode != overlayDetails {
		o.mode = overlayPalette
		return overlayOutcome{}, nil
	}
	switch o.mode {
	case overlaySwitcher:
		return o.handleSwitcher(ev), nil
	case overlayNewSession:
		return o.handleNewSession(ev)
	case overlayDetails:
		o.handleDetails(ev)
		return overlayOutcome{}, nil
	case overlayStopConfirm:
		return o.handleStop(ev)
	}
	return o.handlePalette(ev)
}

func (o *overlay) handlePalette(ev keyEvent) (overlayOutcome, error) {
	items := o.filteredPalette()
	switch {
	case ev.code == keyEsc:
		return overlayOutcome{kind: overlayClose}, nil
	case ev.code == keyEnter:
		if o.paletteSelected < len(items) {
			if items[o.paletteSelected].mode == overlayTakeSize {
				return overlayOutcome{kind: overlayTookSize}, nil
			}
			return overlayOutcome{}, o.run(items[o.paletteSelected].mode)
		}
	case ev.code == keyUp:
		o.paletteSelected = max(o.paletteSelected-1, 0)
	case ev.code == keyDown:
		if o.paletteSelected+1 < len(items) {
			o.paletteSelected++
		}
	case ev.code == keyBackspace:
		o.paletteQuery = popRune(o.paletteQuery)
		o.paletteSelected = 0
	case ev.printable():
		o.paletteQuery += string(ev.r)
		o.paletteSelected = 0
	}
	return overlayOutcome{}, nil
}

func (o *overlay) run(mode overlayMode) error {
	switch mode {
	case overlaySwitcher:
		if err := o.refresh(); err != nil {
			return err
		}
		o.switcherQuery, o.switcherSelected = "", 0
	case overlayNewSession:
		o.editAgent = false
		o.nameInput = ""
		o.agentInput = "claude"
		if cfg, err := config.Load(o.c.paths.Config); err == nil {
			o.agentInput = cfg.DefaultAgent
		}
	case overlayDetails:
		resp, err := o.c.call(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: o.session}}, o.c.requestTimeout(),
			func(r *protocol.Response) bool { return r.Session != nil })
		if err != nil {
			return err
		}
		o.detailText = sessionDetails(resp.Session, o.now())
		if o.size != nil {
			o.detailText += "\n" + sizeDetails(o.size)
		}
		o.detailScroll = 0
	}
	o.mode = mode
	return nil
}

func (o *overlay) handleSwitcher(ev keyEvent) overlayOutcome {
	matches := o.filteredSessions()
	switch {
	case ev.code == keyUp:
		o.switcherSelected = max(o.switcherSelected-1, 0)
	case ev.code == keyDown:
		if o.switcherSelected+1 < len(matches) {
			o.switcherSelected++
		}
	case ev.code == keyBackspace:
		o.switcherQuery = popRune(o.switcherQuery)
		o.switcherSelected = 0
	case ev.code == keyEnter:
		if o.switcherSelected < len(matches) {
			return overlayOutcome{kind: overlaySwitch, session: matches[o.switcherSelected].SessionID}
		}
	case ev.printable():
		o.switcherQuery += string(ev.r)
		o.switcherSelected = 0
	}
	return overlayOutcome{}
}

func (o *overlay) handleNewSession(ev keyEvent) (overlayOutcome, error) {
	field := &o.nameInput
	if o.editAgent {
		field = &o.agentInput
	}
	switch {
	case ev.code == keyTab:
		o.editAgent = !o.editAgent
	case ev.code == keyBackspace:
		*field = popRune(*field)
	case ev.code == keyEnter:
		name, agent := strings.TrimSpace(o.nameInput), strings.TrimSpace(o.agentInput)
		switch {
		case agent == "":
			o.toast = "agent cannot be empty"
		case name != "" && !session.ValidName(name):
			o.toast = "invalid session name: " + session.NameRules
		case o.c.remoteName() != "":
			o.toast = remoteCreateHint(o.c.remoteName())
		default:
			id, message, err := o.c.createFromUI(name, agent)
			if err != nil {
				return overlayOutcome{}, err
			}
			if message != "" {
				o.toast = message
				break
			}
			return overlayOutcome{kind: overlaySwitch, session: id}, nil
		}
	case ev.printable():
		*field += string(ev.r)
	}
	return overlayOutcome{}, nil
}

func (o *overlay) handleDetails(ev keyEvent) {
	switch ev.code {
	case keyEsc:
		o.mode = overlayPalette
	case keyUp:
		o.detailScroll = max(o.detailScroll-1, 0)
	case keyDown:
		o.detailScroll++
	case keyPageUp:
		o.detailScroll = max(o.detailScroll-10, 0)
	case keyPageDown:
		o.detailScroll += 10
	}
}

func (o *overlay) handleStop(ev keyEvent) (overlayOutcome, error) {
	if ev.code != keyEnter {
		return overlayOutcome{}, nil
	}
	_, err := o.c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: o.session}}, o.c.requestTimeout(),
		func(r *protocol.Response) bool { return r.KillSession != nil })
	if err != nil {
		return overlayOutcome{}, err
	}
	return overlayOutcome{kind: overlayClose}, nil
}

// sessionDetails is the overlay's Session Details text.
func sessionDetails(s *session.Record, now time.Time) string {
	text := fmt.Sprintf("name       %s\nagent      %s\ncwd        %s\nstatus     %s\nelapsed    %s\nactivity   %s",
		s.SessionID, s.Agent, escapeControls(s.Cwd), sessionStatusText(s), elapsedLabel(s, now), activityText(s, now))
	if s.Foreground != nil {
		text += "\nforeground " + escapeControls(*s.Foreground)
	}
	if s.Title != nil {
		text += "\ntitle      " + escapeControls(*s.Title)
	}
	if row := buildDisplayRow(s, now); row.summary != "" {
		text += fmt.Sprintf("\nattention  %s: %s", s.Attention, row.summary)
	}
	return text
}

// sizeDetails says who owns the session's size.
func sizeDetails(o *protocol.SizeOwnership) string {
	size := fmt.Sprintf("%dx%d", o.Geometry.Cols, o.Geometry.Rows)
	switch {
	case o.Owner:
		return "size       " + size + ", set by this terminal"
	case o.OwnerID == "":
		return "size       " + size
	}
	return fmt.Sprintf("size       %s, set by %s (Ctrl-Y, Take over size)", size, o.OwnerID)
}

func sessionStatusText(s *session.Record) string {
	if s.AttentionSummary != nil {
		return *s.AttentionSummary
	}
	switch sessionRunState(s) {
	case runStarting:
		return "starting"
	case runRunning:
		return "running"
	case runFailed:
		if s.Error != nil {
			return *s.Error
		}
		return "failed"
	case runRecovered:
		return "daemon lost the live process"
	}
	if s.ExitCode != nil {
		return fmt.Sprintf("exited (%d)", *s.ExitCode)
	}
	return "exited"
}

// Drawing
//
// The overlay follows the agtd design system's dark theme: a Graphite
// surface panel edged in a hairline, text and muted text, and amber
// (signal) for the one thing in focus: the selected row's bar, the active
// field, the Stop button. Sessions keep the glyphs agt prints everywhere
// (● running, ⚠ needs you, ○ exited).

// overlayTheme holds the overlay's SGR sequences: foregrounds (FG) and
// backgrounds (BG). Every one is "" when colour is off.
type overlayTheme struct {
	surfaceBG, groundBG, lineBG, signalBG                              string
	textFG, mutedFG, quietFG, lineFG, signalFG, onSignalFG, okFG, bold string
}

func rgbFG(r, g, b uint8) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b) }
func rgbBG(r, g, b uint8) string { return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b) }

// The design system's tokens (dark theme), and their nearest 256-colour
// entries for terminals without 24-bit colour.
var (
	overlayTrueColor = overlayTheme{
		surfaceBG:  rgbBG(0x18, 0x1a, 0x1d), // surface
		groundBG:   rgbBG(0x0f, 0x11, 0x13), // ground: inputs sit inset
		lineBG:     rgbBG(0x2a, 0x2d, 0x32), // line: the selected row
		signalBG:   rgbBG(0xfd, 0xa9, 0x34), // signal
		textFG:     rgbFG(0xeb, 0xed, 0xef), // text
		mutedFG:    rgbFG(0x8b, 0x90, 0x98), // text-muted
		quietFG:    rgbFG(0x83, 0x86, 0x8c), // quiet
		lineFG:     rgbFG(0x3a, 0x3e, 0x44), // line, a step up so the edge holds over any screen
		signalFG:   rgbFG(0xfd, 0xa9, 0x34), // signal / signal-text
		onSignalFG: rgbFG(0x0f, 0x11, 0x13), // on-signal
		okFG:       rgbFG(0x8c, 0xc2, 0x65), // ok
		bold:       "\x1b[1m",
	}
	overlay256 = overlayTheme{
		surfaceBG: "\x1b[48;5;234m", groundBG: "\x1b[48;5;233m", lineBG: "\x1b[48;5;236m", signalBG: "\x1b[48;5;214m",
		textFG: "\x1b[38;5;255m", mutedFG: "\x1b[38;5;246m", quietFG: "\x1b[38;5;245m", lineFG: "\x1b[38;5;238m",
		signalFG: "\x1b[38;5;214m", onSignalFG: "\x1b[38;5;233m", okFG: "\x1b[38;5;107m", bold: "\x1b[1m",
	}
)

// overlayThemeFor picks 24-bit colour where the terminal says it takes
// it, 256 colours otherwise, and none under NO_COLOR or TERM=dumb.
func overlayThemeFor(getenv func(string) string) *overlayTheme {
	switch {
	case getenv("NO_COLOR") != "" || getenv("TERM") == "dumb":
		return &overlayTheme{}
	case supportsTrueColor(getenv):
		return &overlayTrueColor
	}
	return &overlay256
}

// span is text in one style; a line is a list of spans over a background
// ("" for the panel's surface).
type span struct{ style, text string }

type line struct {
	bg    string
	spans []span
}

func plain(text string) line { return line{spans: []span{{"", text}}} }

// render cuts l to width cells and pads it with spaces, so it overwrites
// whatever was under it. Each span starts from the line's base colours, so
// a span's own style only adds to them.
func (l line) render(width int, t *overlayTheme) string {
	bg := l.bg
	if bg == "" {
		bg = t.surfaceBG
	}
	base := bg + t.textFG
	var b strings.Builder
	left := width
	for _, s := range l.spans {
		if left <= 0 {
			break
		}
		text := takeRunes(s.text, left)
		left -= runeLen(text)
		if style := base + s.style; style != "" {
			b.WriteString(style + text + ansiReset)
		} else {
			b.WriteString(text)
		}
	}
	if left > 0 {
		if base != "" {
			b.WriteString(base + strings.Repeat(" ", left) + ansiReset)
		} else {
			b.WriteString(strings.Repeat(" ", left))
		}
	}
	return b.String()
}

func runStyle(r runState) string {
	switch r {
	case runStarting, runRunning:
		return "\x1b[32m"
	case runFailed:
		return "\x1b[31m"
	}
	return ansiInactive
}

// wrapText breaks text into lines of at most width runes, at spaces where
// it can.
func wrapText(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		cur := ""
		for _, w := range words {
			switch {
			case cur == "":
				cur = w
			case runeLen(cur)+1+runeLen(w) <= width:
				cur += " " + w
			default:
				out = append(out, cur)
				cur = w
			}
			for width > 0 && runeLen(cur) > width {
				out = append(out, takeRunes(cur, width))
				cur = string([]rune(cur)[width:])
			}
		}
		out = append(out, cur)
	}
	return out
}

// overlayRect is the box: 80% of the screen each way, at most 100 by 26,
// centred. It depends only on the screen's size, so changing mode never
// leaves part of a bigger box behind.
func overlayRect(cols, rows int) (x, y, w, h int) {
	w, h = min(cols*80/100, 100), min(rows*80/100, 26)
	return (cols - w) / 2, (rows - h) / 2, w, h
}

func (o *overlay) title() string {
	switch o.mode {
	case overlaySwitcher:
		return "Switch session"
	case overlayNewSession:
		return "New session"
	case overlayDetails:
		return "Session details"
	case overlayStopConfirm:
		return "Stop session"
	}
	return "Commands"
}

// hints are the keys the current mode takes, shown at the foot of the box.
func (o *overlay) hints() [][2]string {
	switch o.mode {
	case overlaySwitcher:
		return [][2]string{{"↑↓", "choose"}, {"enter", "switch"}, {"esc", "back"}}
	case overlayNewSession:
		return [][2]string{{"tab", "next field"}, {"enter", "create"}, {"esc", "back"}}
	case overlayDetails:
		return [][2]string{{"↑↓", "scroll"}, {"pgup/pgdn", "page"}, {"esc", "back"}}
	case overlayStopConfirm:
		return [][2]string{{"enter", "stop"}, {"esc", "cancel"}}
	}
	return [][2]string{{"↑↓", "choose"}, {"enter", "run"}, {"esc", "close"}}
}

// listRow is a row of a list: a bar marking the selected row, then the
// row's own spans, then right-aligned text. The selected row is raised on
// line with an amber bar.
func (o *overlay) listRow(width int, selected bool, spans []span, right span) line {
	t := o.theme
	l := line{spans: []span{{"", " "}}}
	if selected {
		l = line{bg: t.lineBG, spans: []span{{t.signalFG, "▌"}}}
	}
	l.spans = append(l.spans, span{"", " "})
	l.spans = append(l.spans, spans...)
	used := 0
	for _, s := range l.spans {
		used += runeLen(s.text)
	}
	if room := width - used - runeLen(right.text) - 1; room > 0 && right.text != "" {
		l.spans = append(l.spans, span{"", strings.Repeat(" ", room)}, right)
	}
	return l
}

// input is a query or form field: an inset strip with a prompt glyph and
// a block cursor. An active field's glyph is amber.
func (o *overlay) input(glyph, value, placeholder string, active bool) line {
	t := o.theme
	glyphStyle := t.quietFG
	if active {
		glyphStyle = t.signalFG
	}
	l := line{bg: t.groundBG, spans: []span{{"", " "}, {glyphStyle, glyph + " "}, {"", value}}}
	if active {
		l.spans = append(l.spans, span{"\x1b[7m", " "})
	}
	if value == "" && placeholder != "" {
		l.spans = append(l.spans, span{t.quietFG, placeholder})
	}
	return l
}

// body is the box's content: height lines of width cells. Its frame is a
// title row with the attached session's name, the mode's content, a toast
// and the key hints.
func (o *overlay) body(width, height int) []line {
	t := o.theme
	lines := make([]line, height)
	put := func(row int, l line) {
		if row >= 0 && row < height {
			lines[row] = l
		}
	}

	// The title row: the mode on the left; the attached session as
	// host/session and how long it has been running on the right.
	name := escapeControls(o.session)
	right := []span{{"", o.hostName() + "/" + name}}
	if up := o.uptime(); up != "" {
		right = append(right, span{t.mutedFG, "  " + up})
	}
	title := line{spans: []span{{"", " "}, {t.bold, o.title()}}}
	used := 1 + runeLen(o.title())
	for _, s := range right {
		used += runeLen(s.text)
	}
	if gap := width - used; gap > 0 {
		title.spans = append(title.spans, span{"", strings.Repeat(" ", gap)})
		title.spans = append(title.spans, right...)
	}
	put(1, title)

	var hint []span
	hint = append(hint, span{"", " "})
	for i, h := range o.hints() {
		if i > 0 {
			hint = append(hint, span{t.quietFG, " · "})
		}
		hint = append(hint, span{"", h[0]}, span{t.mutedFG, " " + h[1]})
	}
	put(height-2, line{spans: hint})
	if o.toast != "" {
		put(height-4, line{spans: []span{{"", " "}, {t.signalFG, o.toast}}})
	}

	top, bottom := 3, height-4 // content rows: [top, bottom)
	switch o.mode {
	case overlayPalette:
		put(top, o.input("❯", o.paletteQuery, "Type to filter", true))
		for i, item := range o.filteredPalette() {
			if top+2+i >= bottom {
				break
			}
			selected := i == o.paletteSelected
			style := ""
			if selected {
				style = t.bold
			}
			put(top+2+i, o.listRow(width, selected, []span{{style, item.title}}, span{t.mutedFG, item.hint}))
		}
	case overlaySwitcher:
		put(top, o.input("❯", o.switcherQuery, "Filter sessions", true))
		matches := o.filteredSessions()
		room := max(bottom-top-2, 1)
		first := max(o.switcherSelected-room+1, 0)
		for i := first; i < len(matches) && i-first < room; i++ {
			s := matches[i]
			row := buildDisplayRow(s, o.now())
			icon, iconStyle := o.sessionIcon(row)
			spans := []span{{iconStyle, icon}, {"", " "}, {t.bold, s.SessionID}, {"", "  "}, {t.mutedFG, row.activity}}
			if row.summary != "" && row.attention == session.AttentionAction {
				spans = append(spans, span{"", "  "}, span{t.signalFG, row.summary})
			} else if row.summary != "" {
				spans = append(spans, span{"", "  "}, span{"", row.summary})
			}
			spans = append(spans, span{"", "  "}, span{t.quietFG, row.cwd})
			put(top+2+i-first, o.listRow(width, i == o.switcherSelected, spans, span{t.quietFG, row.age}))
		}
		if len(matches) == 0 {
			put(top+2, line{spans: []span{{"", "  "}, {t.mutedFG, "No sessions match."}}})
		}
	case overlayNewSession:
		label := func(row int, text string) { put(row, line{spans: []span{{"", " "}, {t.mutedFG, text}}}) }
		label(top, "Name")
		put(top+1, o.input("▌", o.nameInput, "Blank picks a name", !o.editAgent))
		label(top+3, "Agent")
		put(top+4, o.input("▌", o.agentInput, "", o.editAgent))
		for i, l := range wrapText("Lowercase letters, numbers and hyphens.", width-2) {
			if top+6+i < bottom {
				put(top+6+i, line{spans: []span{{"", " "}, {t.quietFG, l}}})
			}
		}
	case overlayDetails:
		// Each detail is "key   value": the key column is muted.
		var rows []line
		for _, para := range strings.Split(o.detailText, "\n") {
			key, value := "", para
			if i := strings.IndexByte(para, ' '); i > 0 {
				j := i
				for j < len(para) && para[j] == ' ' {
					j++
				}
				key, value = para[:j], para[j:]
			}
			for k, l := range wrapText(value, max(width-2-runeLen(key), 1)) {
				lead := key
				if k > 0 {
					lead = strings.Repeat(" ", runeLen(key))
				}
				rows = append(rows, line{spans: []span{{"", " "}, {t.mutedFG, lead}, {"", l}}})
			}
		}
		for i := top; i < bottom; i++ {
			if j := o.detailScroll + i - top; j < len(rows) {
				put(i, rows[j])
			}
		}
	case overlayStopConfirm:
		msg := "Stop " + name + "? Its agent and terminal end."
		wrapped := wrapText(msg, width-2)
		for i, l := range wrapped {
			put(top+i, line{spans: []span{{"", " "}, {"", l}}})
		}
		put(top+len(wrapped)+1, line{spans: []span{{"", " "},
			{t.signalBG + t.onSignalFG + t.bold, "  Stop  "}, {"", "  "}, {t.lineBG, "  Cancel  "}}})
	}
	return lines
}

// hostName is the machine the attached session runs on: the remote host's
// name, or this machine's short hostname.
func (o *overlay) hostName() string {
	if name := o.c.remoteName(); name != "" {
		return escapeControls(name)
	}
	name, _, _ := strings.Cut(localHostname(), ".")
	return escapeControls(name)
}

// uptime is how long the attached session has run ("2d3h44m"), or ""
// before the session list has loaded.
func (o *overlay) uptime() string {
	for i := range o.sessions {
		if s := &o.sessions[i]; s.SessionID == o.session {
			end := o.now()
			if s.ExitedAt != nil {
				end = *s.ExitedAt
			}
			return formatUptime(end.Sub(s.CreatedAt))
		}
	}
	return ""
}

// formatUptime spells a duration in days, hours and minutes, leaving out
// the units that are zero: "2d3h44m", "3h5m", "12m", "40s".
func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", max(int64(d/time.Second), 0))
	}
	m := int64(d / time.Minute)
	var parts []string
	for _, u := range []struct {
		n      int64
		suffix string
	}{{m / (60 * 24), "d"}, {m / 60 % 24, "h"}, {m % 60, "m"}} {
		if u.n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", u.n, u.suffix))
		}
	}
	return strings.Join(parts, "")
}

// sessionIcon is a session's glyph and colour: ok for the ● of a running
// session, amber for one that needs you, quiet for the rest.
func (o *overlay) sessionIcon(row displayRow) (string, string) {
	icon, _ := rowIcon(row)
	switch {
	case icon == "⚠":
		return icon, o.theme.signalFG
	case row.run == runRunning || row.run == runStarting:
		return icon, o.theme.okFG
	}
	return icon, o.theme.quietFG
}

// draw paints the overlay over the session's screen, all of it every time.
// The cursor stays hidden while the overlay is open.
func (o *overlay) draw() {
	cols, rows, _, _ := terminalGeometry()
	x, y, w, h := overlayRect(int(cols), int(rows))
	if w < 8 || h < 8 {
		return
	}
	t := o.theme
	edge := func(s string) string {
		if t.surfaceBG+t.lineFG == "" {
			return s
		}
		return t.surfaceBG + t.lineFG + s + ansiReset
	}
	pad := line{}.render(1, t)
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	at := func(row int) { fmt.Fprintf(&b, "\x1b[%d;%dH", y+row+1, x+1) }
	at(0)
	b.WriteString(edge("╭" + strings.Repeat("─", w-2) + "╮"))
	for i, l := range o.body(w-4, h-2) {
		at(i + 1)
		b.WriteString(edge("│") + pad + l.render(w-4, t) + pad + edge("│"))
	}
	at(h - 1)
	b.WriteString(edge("╰" + strings.Repeat("─", w-2) + "╯"))
	writeOut([]byte(b.String()))
}
