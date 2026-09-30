package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/robmorgan/agentd/go/internal/config"
	"github.com/robmorgan/agentd/go/internal/protocol"
	"github.com/robmorgan/agentd/go/internal/session"
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
)

type overlayOutcomeKind int

const (
	overlayStay overlayOutcomeKind = iota
	overlayClose
	overlaySwitch
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
	{"s", "Switch Session", overlaySwitcher},
	{"t", "New Session", overlayNewSession},
	{"i", "Session Details", overlayDetails},
	{"x", "Stop Session", overlayStopConfirm},
}

func filteredPalette(query string) []paletteItem {
	var out []paletteItem
	for _, item := range paletteItems {
		if matchesQuery(item.hint+" "+item.title, query) {
			out = append(out, item)
		}
	}
	return out
}

type overlay struct {
	c       *client
	session string // the attached session
	keys    keyDecoder

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
	now              func() time.Time
}

func newOverlay(c *client, id string) *overlay {
	return &overlay{c: c, session: id, now: time.Now}
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
	items := filteredPalette(o.paletteQuery)
	switch {
	case ev.code == keyEsc:
		return overlayOutcome{kind: overlayClose}, nil
	case ev.code == keyEnter:
		if o.paletteSelected < len(items) {
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
		s := resp.Session
		o.detailText = fmt.Sprintf("name       %s\nagent      %s\ncwd        %s\nstatus     %s", s.SessionID, s.Agent, escapeControls(s.Cwd), sessionStatusText(s))
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

const (
	styleCyan     = "\x1b[36m"
	styleCyanBold = "\x1b[36;1m"
	styleBold     = "\x1b[1m"
	styleSubtle   = "\x1b[90m"
)

// span is text in one style; a line is a list of spans.
type span struct{ style, text string }

type line []span

func plain(text string) line { return line{{"", text}} }

// render cuts l to width cells and pads it with spaces, so it overwrites
// whatever was under it.
func (l line) render(width int) string {
	var b strings.Builder
	left := width
	for _, s := range l {
		if left <= 0 {
			break
		}
		text := takeRunes(s.text, left)
		left -= runeLen(text)
		if s.style != "" {
			b.WriteString(s.style + text + ansiReset)
		} else {
			b.WriteString(text)
		}
	}
	b.WriteString(strings.Repeat(" ", max(left, 0)))
	return b.String()
}

func runStyle(r runState) string {
	switch r {
	case runStarting, runRunning:
		return "\x1b[32m"
	case runFailed:
		return "\x1b[31m"
	}
	return styleSubtle
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

// overlayRect is the box: 80% of the screen each way, centred.
func overlayRect(cols, rows int) (x, y, w, h int) {
	w, h = cols*80/100, rows*80/100
	return (cols - w) / 2, (rows - h) / 2, w, h
}

func (o *overlay) title() string {
	switch o.mode {
	case overlaySwitcher:
		return "Switch Session"
	case overlayNewSession:
		return "New Session"
	case overlayDetails:
		return "Session Details"
	case overlayStopConfirm:
		return "Stop Session"
	}
	return "Overlay"
}

// body is the box's content: height lines of width cells.
func (o *overlay) body(width, height int) []line {
	lines := make([]line, height)
	put := func(row int, l line) {
		if row >= 0 && row < height {
			lines[row] = l
		}
	}
	prompt := func(query string) line { return line{{styleCyan, "> "}, {"", query}} }
	switch o.mode {
	case overlayPalette:
		put(0, prompt(o.paletteQuery))
		for i, item := range filteredPalette(o.paletteQuery) {
			style := ""
			if i == o.paletteSelected {
				style = styleCyanBold
			}
			if 2+i < height-1 {
				put(2+i, line{{styleSubtle, fmt.Sprintf("%2s", item.hint)}, {"", "  "}, {style, item.title}})
			}
		}
		put(height-1, plain(o.toast))
	case overlaySwitcher:
		put(0, prompt(o.switcherQuery))
		for i, s := range o.filteredSessions() {
			row := buildDisplayRow(s, o.now())
			style := styleBold
			if i == o.switcherSelected {
				style = styleCyanBold
			}
			put(2+i, line{{runStyle(row.run), runIcon(row.run)}, {"", "  "}, {styleSubtle, row.age}, {"", "  "},
				{style, s.SessionID}, {"", "  "}, {styleSubtle, row.cwd}})
		}
	case overlayNewSession:
		field := func(top int, label, value string, active bool) {
			style := ""
			if active {
				style = styleCyan
			}
			put(top, plain("┌"+strings.Repeat("─", max(width-2, 0))+"┐"))
			put(top+1, line{{"", "│"}, {styleSubtle, label}, {style, padRight(takeRunes(value, max(width-2-runeLen(label), 0)), max(width-2-runeLen(label), 0))}, {"", "│"}})
			put(top+2, plain("└"+strings.Repeat("─", max(width-2, 0))+"┘"))
		}
		field(0, "name   ", o.nameInput, !o.editAgent)
		field(3, "agent  ", o.agentInput, o.editAgent)
		for i, l := range wrapText("Blank auto-generates. Use lowercase letters, numbers, and hyphens.", width) {
			if i < 2 {
				put(6+i, plain(l))
			}
		}
		put(8, plain(o.toast))
	case overlayDetails:
		wrapped := wrapText(o.detailText, width)
		for i := range height {
			if j := o.detailScroll + i; j < len(wrapped) {
				put(i, plain(wrapped[j]))
			}
		}
	case overlayStopConfirm:
		for i, l := range wrapText("Press Enter to stop the current session, or Esc to cancel.", width) {
			put(i, plain(l))
		}
	}
	return lines
}

// draw paints the overlay over the session's screen, all of it every time.
// The cursor stays hidden while the overlay is open.
func (o *overlay) draw() {
	cols, rows, _, _ := terminalGeometry()
	x, y, w, h := overlayRect(int(cols), int(rows))
	if w < 4 || h < 3 {
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	at := func(row int) { fmt.Fprintf(&b, "\x1b[%d;%dH", y+row+1, x+1) }
	title := takeRunes(o.title(), w-2)
	at(0)
	b.WriteString("┌" + title + strings.Repeat("─", w-2-runeLen(title)) + "┐")
	for i, l := range o.body(w-2, h-2) {
		at(i + 1)
		b.WriteString("│" + l.render(w-2) + "│")
	}
	at(h - 1)
	b.WriteString("└" + strings.Repeat("─", w-2) + "┘")
	writeOut([]byte(b.String()))
}
