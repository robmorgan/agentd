package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robmorgan/agentd/internal/config"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

const (
	pickerQueryBG       = "\x1b[48;2;62;63;71m"
	pickerPlaceholderFG = "\x1b[38;2;151;152;153m"
	pickerTextFG        = "\x1b[38;2;255;255;255m"
	pickerErrorFG       = "\x1b[31m"
	pickerSelectedStyle = "\x1b[34m"
	pickerLegendStyle   = "\x1b[90m"
	pickerCursor        = "█"
	pickerEnterSequence = "\x1b[?25l"
	pickerExitSequence  = "\x1b[?25h"

	pickerRefreshInterval = 200 * time.Millisecond
	pickerBlinkInterval   = 500 * time.Millisecond
	pickerVisibleRows     = 8

	sessionListDefaultWidth    = 120
	sessionListSummaryWidth    = 160
	sessionListRunWidth        = 3
	sessionListAgeWidth        = 5
	sessionListActivityWidth   = 11
	sessionListSummaryMinWidth = 20
	sessionListNameMinWidth    = 10
	sessionListNameMaxWidth    = 26
	sessionListNameFloorWidth  = 8
	sessionListCwdMinWidth     = 16
	sessionListCwdMaxWidth     = 48
	sessionListCwdSummaryWidth = 32
	sessionListCwdFloorWidth   = 10
	sessionListStructuralWidth = 10
	pickerSessionPrefixWidth   = 4
	pickerSessionStructural    = 3

	// codexDefaultModel is the model sessions started from the picker or
	// overlay ask codex for.
	codexDefaultModel = "gpt-5.4"
)

// `agent list`

type sessionListLayout struct {
	visible, run, age, name, activity, cwd int
	// summary is the attention column's width; 0 hides it.
	summary int
}

// fitColumns grows cwd then name into spare width, or shrinks name then cwd
// down to their floors when there is too little.
func fitColumns(name, cwd *int, available, fixed int) {
	total := fixed + *name + *cwd
	if available >= total {
		remaining := available - total
		grow := func(w *int, maxWidth int) {
			add := min(remaining, max(maxWidth-*w, 0))
			*w += add
			remaining -= add
		}
		grow(cwd, sessionListCwdMaxWidth)
		grow(name, sessionListNameMaxWidth)
		return
	}
	deficit := total - available
	shrink := func(w *int, floor int) {
		remove := min(deficit, max(*w-floor, 0))
		*w -= remove
		deficit -= remove
	}
	shrink(name, sessionListNameFloorWidth)
	shrink(cwd, sessionListCwdFloorWidth)
}

// listLayout fits the columns of `agent list` into width. With summaries
// (some session has attention to explain) an ATTENTION column takes what
// the others leave, and the list may be wider.
func listLayout(width int, summaries bool) sessionListLayout {
	limit := sessionListDefaultWidth
	if summaries {
		limit = sessionListSummaryWidth
	}
	l := sessionListLayout{
		visible:  min(max(width, 1), limit),
		run:      sessionListRunWidth,
		age:      sessionListAgeWidth,
		name:     sessionListNameMinWidth,
		activity: sessionListActivityWidth,
		cwd:      sessionListCwdMinWidth,
	}
	fixed := sessionListStructuralWidth + l.run + l.age + l.activity
	if !summaries {
		fitColumns(&l.name, &l.cwd, l.visible, fixed)
		return l
	}
	// The attention column, with its separator, gets what is left once
	// name and cwd have their usual widths; without room for it, they keep
	// the room. `agent status` shows the summary in full.
	fitColumns(&l.name, &l.cwd, l.visible-sessionListSummaryMinWidth, fixed+2)
	if l.name < sessionListNameMinWidth || l.cwd < sessionListCwdMinWidth {
		l.name, l.cwd = sessionListNameMinWidth, sessionListCwdMinWidth
		fitColumns(&l.name, &l.cwd, l.visible, fixed)
		return l
	}
	// What needs attention matters more than the end of a long path.
	l.cwd = min(l.cwd, sessionListCwdSummaryWidth)
	l.summary = l.visible - fixed - 2 - l.name - l.cwd
	return l
}

func renderSessionListLines(sessions []session.Record, width int, now time.Time) []string {
	rows := make([]displayRow, 0, len(sessions))
	summaries := false
	for _, s := range orderedSessions(sessions) {
		row := buildDisplayRow(s, now)
		summaries = summaries || row.summary != ""
		rows = append(rows, row)
	}
	l := listLayout(width, summaries)
	headerRow := fmt.Sprintf("  %s  %s  %s  %s  %s", formatCell("RUN", l.run), formatCell("AGE", l.age), formatCell("NAME", l.name),
		formatCell("ACTIVITY", l.activity), formatCell("CWD", l.cwd))
	if l.summary > 0 {
		headerRow += "  " + formatCell("ATTENTION", l.summary)
	}
	lines := []string{pickerQueryBG + pickerTextFG + takeRunes(headerRow, l.visible) + ansiReset}
	if len(sessions) == 0 {
		return append(lines, truncateCell("  No sessions.", l.visible))
	}
	for _, row := range rows {
		icon, iconStyle := rowIcon(row)
		line := fmt.Sprintf("  %s  %s  %s  %s  %s",
			iconStyle+formatCell(icon, l.run)+ansiReset,
			styleAge(formatCell(row.age, l.age)),
			styleName(formatCell(row.name, l.name)),
			formatCell(row.activity, l.activity),
			styleCwd(formatPathCell(row.cwd, l.cwd)))
		if l.summary > 0 && row.summary != "" {
			line += "  " + styleSummary(truncateCell(row.summary, l.summary), row.attention)
		}
		lines = append(lines, line)
	}
	return lines
}

// The session picker (bare `agent`)

type pickerMode int

const (
	pickerBrowse pickerMode = iota
	pickerCreateAgent
	pickerSessionActions
	pickerDeleteConfirm
)

type sessionAction int

const (
	actionAttach sessionAction = iota
	actionDelete
)

func (a sessionAction) label() string {
	if a == actionAttach {
		return "attach"
	}
	return "delete"
}

type toast struct {
	isError bool
	message string
}

// picker is the session picker's state. Its key handling returns done with
// the chosen session ("" for none) once the user has decided.
type picker struct {
	c            *client
	sessions     []session.Record
	defaultAgent string
	createAgents []string

	query    string
	selected int // row in browse mode

	mode        pickerMode
	modeSession string
	modeIndex   int // selection in the other modes

	toast *toast
	now   func() time.Time
}

func newPicker(c *client) *picker {
	p := &picker{c: c, now: time.Now}
	p.loadAgents()
	return p
}

// loadAgents reads the configured agents, the default first.
func (p *picker) loadAgents() {
	names := []string{"claude"}
	p.defaultAgent = ""
	if cfg, err := config.Load(p.c.paths.Config); err == nil {
		if agents := cfg.AgentNames(); len(agents) > 0 {
			names = agents
		}
		p.defaultAgent = cfg.DefaultAgent
	}
	p.createAgents = orderedCreateAgents(names, p.defaultAgent)
}

func orderedCreateAgents(names []string, defaultAgent string) []string {
	for i, name := range names {
		if name == defaultAgent && i > 0 {
			out := append([]string{name}, names[:i]...)
			return append(out, names[i+1:]...)
		}
	}
	return names
}

func (p *picker) refresh() error {
	sessions, err := p.c.listSessions(p.c.requestTimeout())
	if err != nil {
		return err
	}
	p.sessions = sessions
	p.loadAgents()
	p.clampSelection()
	p.clampMode()
	return nil
}

func (p *picker) filteredSessions() []*session.Record {
	var out []*session.Record
	for _, s := range orderedSessions(p.sessions) {
		if matchesQuery(sessionSearchText(s), p.query) {
			out = append(out, s)
		}
	}
	return out
}

// rows are the matching sessions followed by the "create" row, which is
// the empty string.
func (p *picker) rows() []string {
	var rows []string
	for _, s := range p.filteredSessions() {
		rows = append(rows, s.SessionID)
	}
	return append(rows, "")
}

func (p *picker) sessionByID(id string) *session.Record {
	for i := range p.sessions {
		if p.sessions[i].SessionID == id {
			return &p.sessions[i]
		}
	}
	return nil
}

func (p *picker) clampSelection() {
	p.selected = min(p.selected, len(p.rows())-1)
}

func (p *picker) clampMode() {
	switch p.mode {
	case pickerCreateAgent:
		if len(p.createAgents) == 0 {
			p.mode = pickerBrowse
		} else {
			p.modeIndex = min(p.modeIndex, len(p.createAgents)-1)
		}
	case pickerSessionActions:
		actions := p.actions(p.modeSession)
		if len(actions) == 0 || p.sessionByID(p.modeSession) == nil {
			p.mode = pickerBrowse
		} else {
			p.modeIndex = min(p.modeIndex, len(actions)-1)
		}
	case pickerDeleteConfirm:
		if p.sessionByID(p.modeSession) == nil {
			p.mode = pickerBrowse
		} else {
			p.modeIndex = min(p.modeIndex, 1)
		}
	}
}

func (p *picker) actions(id string) []sessionAction {
	s := p.sessionByID(id)
	if s == nil {
		return nil
	}
	if s.Status == session.StatusRunning {
		return []sessionAction{actionAttach, actionDelete}
	}
	return []sessionAction{actionDelete}
}

func (p *picker) actionIndex(id string, action sessionAction) int {
	for i, a := range p.actions(id) {
		if a == action {
			return i
		}
	}
	return 0
}

// requestedName is the query as a session name, or "" when it is empty or
// not a valid name.
func (p *picker) requestedName() string {
	name := strings.TrimSpace(p.query)
	if !session.ValidName(name) {
		return ""
	}
	return name
}

func (p *picker) setMode(mode pickerMode, id string, index int) {
	p.mode, p.modeSession, p.modeIndex = mode, id, index
	p.clampMode()
}

func (p *picker) handlePaste(text string) {
	if p.mode != pickerBrowse {
		return
	}
	p.toast = nil
	p.query += text
	p.clampSelection()
	p.selected = 0
}

func (p *picker) handleKey(ev keyEvent) (chosen string, done bool, err error) {
	switch p.mode {
	case pickerCreateAgent:
		return p.handleCreateAgentKey(ev)
	case pickerSessionActions:
		return p.handleActionKey(ev)
	case pickerDeleteConfirm:
		return p.handleDeleteKey(ev)
	}
	return p.handleBrowseKey(ev)
}

func (p *picker) handleBrowseKey(ev keyEvent) (string, bool, error) {
	rows := p.rows()
	switch {
	case ev.code == keyEsc:
		return "", true, nil
	case ev.code == keyUp:
		p.selected = max(p.selected-1, 0)
	case ev.code == keyDown:
		if p.selected+1 < len(rows) {
			p.selected++
		}
	case ev.code == keyBackspace:
		p.toast = nil
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
		}
		p.clampSelection()
	case ev.code == keyEnter:
		if p.selected < len(rows) && rows[p.selected] != "" {
			p.toast = nil
			p.setMode(pickerSessionActions, rows[p.selected], 0)
			return "", false, nil
		}
		if strings.TrimSpace(p.query) != "" && p.requestedName() == "" {
			p.toast = &toast{isError: true, message: "invalid session name: " + session.NameRules}
			return "", false, nil
		}
		p.toast = nil
		index := 0
		for i, agent := range p.createAgents {
			if agent == p.defaultAgent {
				index = i
			}
		}
		p.setMode(pickerCreateAgent, "", index)
	case ev.printable():
		p.toast = nil
		p.query += string(ev.r)
		p.clampSelection()
		p.selected = 0
	}
	return "", false, nil
}

// digitIndex is the 0-based index a digit key 1-9 picks, or -1.
func digitIndex(ev keyEvent) int {
	if ev.code == keyChar && ev.mods == 0 && ev.r >= '1' && ev.r <= '9' {
		return int(ev.r - '1')
	}
	return -1
}

func (p *picker) handleCreateAgentKey(ev keyEvent) (string, bool, error) {
	switch {
	case ev.code == keyEsc:
		p.mode = pickerBrowse
	case ev.code == keyUp:
		p.setMode(pickerCreateAgent, "", max(p.modeIndex-1, 0))
	case ev.code == keyDown:
		p.setMode(pickerCreateAgent, "", p.modeIndex+1)
	case ev.code == keyEnter:
		if p.modeIndex < len(p.createAgents) {
			return p.createSession(p.createAgents[p.modeIndex])
		}
	default:
		if i := digitIndex(ev); i >= 0 && i < len(p.createAgents) {
			return p.createSession(p.createAgents[i])
		}
	}
	return "", false, nil
}

func (p *picker) handleActionKey(ev keyEvent) (string, bool, error) {
	id := p.modeSession
	actions := p.actions(id)
	run := func(action sessionAction) (string, bool, error) {
		if action == actionAttach {
			return id, true, nil
		}
		p.toast = nil
		p.setMode(pickerDeleteConfirm, id, 1)
		return "", false, nil
	}
	switch {
	case ev.code == keyEsc:
		p.mode = pickerBrowse
	case ev.code == keyUp:
		p.setMode(pickerSessionActions, id, max(p.modeIndex-1, 0))
	case ev.code == keyDown:
		p.setMode(pickerSessionActions, id, p.modeIndex+1)
	case ev.code == keyEnter:
		if p.modeIndex < len(actions) {
			return run(actions[p.modeIndex])
		}
	default:
		if i := digitIndex(ev); i >= 0 && i < len(actions) {
			return run(actions[i])
		}
	}
	return "", false, nil
}

func (p *picker) handleDeleteKey(ev keyEvent) (string, bool, error) {
	id := p.modeSession
	back := func() { p.setMode(pickerSessionActions, id, p.actionIndex(id, actionDelete)) }
	remove := func() (string, bool, error) {
		if err := p.removeSession(id); err != nil {
			return "", false, err
		}
		p.mode = pickerBrowse
		p.toast = &toast{message: "removed session " + id}
		return "", false, nil
	}
	switch {
	case ev.code == keyEsc:
		back()
	case ev.code == keyUp:
		p.setMode(pickerDeleteConfirm, id, max(p.modeIndex-1, 0))
	case ev.code == keyDown:
		p.setMode(pickerDeleteConfirm, id, p.modeIndex+1)
	case ev.code == keyEnter:
		if p.modeIndex == 0 {
			return remove()
		}
		back()
	case ev.code == keyChar && ev.mods == 0 && ev.r == 'y':
		return remove()
	case ev.code == keyChar && ev.mods == 0 && ev.r == 'n':
		back()
	}
	return "", false, nil
}

func remoteCreateHint(host string) string {
	return fmt.Sprintf("start sessions on `%s` with `agent --host %s new --cwd DIR`", host, host)
}

func (p *picker) createSession(agent string) (string, bool, error) {
	name := p.requestedName()
	if strings.TrimSpace(p.query) != "" && name == "" {
		p.toast = &toast{isError: true, message: "invalid session name: " + session.NameRules}
		return "", false, nil
	}
	if host := p.c.remoteName(); host != "" {
		p.toast = &toast{isError: true, message: remoteCreateHint(host)}
		return "", false, nil
	}
	id, message, err := p.c.createFromUI(name, agent)
	if err != nil {
		return "", false, err
	}
	if message != "" {
		p.toast = &toast{isError: true, message: message}
		return "", false, nil
	}
	return id, true, nil
}

// createFromUI starts a session in the current directory for the picker or
// overlay. A daemon's refusal comes back as message, for a toast.
func (c *client) createFromUI(name, agent string) (id, message string, err error) {
	cwd, err := resolveCwd("")
	if err != nil {
		return "", "", err
	}
	req := &protocol.CreateSession{Cwd: cwd, Agent: agent}
	if name != "" {
		req.Name = &name
	}
	if agent == "codex" {
		model := codexDefaultModel
		req.Model = &model
	}
	resp, err := c.request(&protocol.Request{CreateSession: req}, c.requestTimeout())
	switch {
	case err != nil:
		return "", "", err
	case resp.Error != nil:
		return "", resp.Error.Message, nil
	case resp.CreateSession == nil:
		return "", "", fmt.Errorf("unexpected response: %s", describeResponse(resp))
	}
	return resp.CreateSession.SessionID, "", nil
}

func (p *picker) removeSession(id string) error {
	_, err := p.c.call(&protocol.Request{KillSession: &protocol.KillSession{SessionID: id, Remove: true}}, p.c.requestTimeout(),
		func(r *protocol.Response) bool { return r.KillSession != nil })
	if err != nil {
		return err
	}
	return p.refresh()
}

// Rendering. Lines are cut to one less than the width, so the terminal
// never wraps them.

func lineWidth(width int) int { return max(width-1, 1) }

func fitLine(line string, width int) string {
	if width == 0 {
		return ""
	}
	return takeRunes(line, lineWidth(width))
}

func backgroundRow(width int) string {
	return pickerQueryBG + strings.Repeat(" ", lineWidth(width)) + ansiReset
}

func menuLine(content string, width int, selected bool) string {
	maxChars := lineWidth(width)
	prefix, style := "  ", ""
	if selected {
		prefix, style = "› ", pickerSelectedStyle
	}
	visible := takeRunes(prefix+content, maxChars)
	return pickerQueryBG + style + visible + strings.Repeat(" ", max(maxChars-runeLen(visible), 0)) + ansiReset
}

func queryLine(query string, width int, cursorVisible bool) string {
	maxChars := lineWidth(width)
	const marker = "› "
	cursor := " "
	if cursorVisible {
		cursor = pickerCursor
	}
	available := max(maxChars-(runeLen(marker)+1), 0)
	var content, color string
	var budget int
	placeholder := query == ""
	if placeholder {
		content, color, budget = "Type to filter sessions or name a new one.", pickerPlaceholderFG, available
	} else {
		content, color, budget = query, pickerTextFG, max(available-1, 0)
	}
	visible := takeRunes(content, budget)
	padding := max(maxChars-(runeLen(marker)+runeLen(visible)+1), 0)
	withCursor := color + visible + pickerTextFG + cursor
	if placeholder {
		withCursor = pickerTextFG + cursor + color + visible
	}
	return pickerQueryBG + pickerTextFG + marker + withCursor + strings.Repeat(" ", padding) + ansiReset
}

func toastLine(t *toast, width int) string {
	prefix := "notice"
	if t.isError {
		prefix = "error"
	}
	line := fitLine(prefix+": "+strings.Join(strings.Fields(t.message), " "), width)
	if t.isError {
		return pickerErrorFG + line + ansiReset
	}
	return line
}

func createRow(label string, width int, selected bool) string {
	leader, style := "  ", ""
	if selected {
		leader, style = "› ", pickerSelectedStyle
	}
	prefix := leader + "+ "
	return style + prefix + truncateCell(label, max(lineWidth(width)-runeLen(prefix), 0)) + ansiReset
}

// pickerSessionRow is one session in the picker: icon, age, name,
// activity and cwd, then what needs attention, if anything, in what width
// is left.
func pickerSessionRow(s *session.Record, width int, selected bool, now time.Time) string {
	row := buildDisplayRow(s, now)
	leader, style := "  ", ""
	if selected {
		leader, style = "› ", pickerSelectedStyle
	}
	available := max(lineWidth(width)-pickerSessionPrefixWidth, 0)
	age, name, activity, cwd := sessionListAgeWidth, sessionListNameMinWidth, sessionListActivityWidth, sessionListCwdMinWidth
	fixed := pickerSessionStructural + age + activity
	if row.summary != "" {
		available -= sessionListSummaryMinWidth
	}
	fitColumns(&name, &cwd, available, fixed)
	icon, iconStyle := rowIcon(row)
	line := fmt.Sprintf("%s%s%s%s %s%s %s %s %s%s", style, leader, ansiReset, iconStyle+icon+ansiReset, style,
		styleAge(formatCell(row.age, age)), styleName(formatCell(row.name, name)), formatCell(row.activity, activity),
		styleCwd(formatPathCell(row.cwd, cwd)), ansiReset)
	if rest := lineWidth(width) - pickerSessionPrefixWidth - fixed - name - cwd - 1; row.summary != "" && rest >= 8 {
		line += " " + styleSummary(truncateCell(row.summary, rest), row.attention)
	}
	return line
}

// legendRow explains the icons of the sessions shown, in as many entries
// as fit.
func legendRow(width int, sessions []*session.Record) string {
	entries := []struct {
		run   runState
		label string
	}{{runRunning, "running"}, {-1, "needs you"}, {runExited, "exited"}, {runRecovered, "lost"}, {runFailed, "failed"}}
	maxChars := lineWidth(width)
	line := "  "
	visible, shown := 0, 0
	for _, e := range entries {
		present := false
		icon, style := runIcon(e.run), runStyle(e.run)
		for _, s := range sessions {
			row := displayRow{run: sessionRunState(s), attention: s.Attention}
			rowIcon, rowStyle := rowIcon(row)
			if e.run < 0 && rowIcon == "⚠" {
				present, icon, style = true, rowIcon, rowStyle
			} else if e.run >= 0 && row.run == e.run && rowIcon != "⚠" {
				present = true
			}
		}
		if !present {
			continue
		}
		needed := runeLen(e.label)
		if shown > 0 {
			needed += 3
		}
		if visible > 0 && visible+needed > maxChars {
			break
		}
		if visible == 0 && runeLen(e.label) > maxChars {
			return ""
		}
		if shown > 0 {
			line += pickerLegendStyle + " • " + ansiReset
			visible += 3
		}
		line += style + icon + ansiReset + pickerLegendStyle + " " + e.label + ansiReset
		visible += runeLen(e.label)
		shown++
	}
	if shown == 0 {
		return ""
	}
	return line
}

func (p *picker) renderLines(width int, cursorVisible bool) []string {
	lines := []string{header(), ""}
	switch p.mode {
	case pickerBrowse:
		lines = append(lines, backgroundRow(width), queryLine(p.query, width, cursorVisible), backgroundRow(width), "")
		rows := p.rows()
		var visible []*session.Record
		for i, id := range rows[:min(len(rows), pickerVisibleRows)] {
			if id == "" {
				label := "Create new session"
				if q := strings.TrimSpace(p.query); q != "" {
					label += ": " + q
				}
				lines = append(lines, createRow(label, width, i == p.selected))
				continue
			}
			if s := p.sessionByID(id); s != nil {
				lines = append(lines, pickerSessionRow(s, width, i == p.selected, p.now()))
				visible = append(visible, s)
			}
		}
		if legend := legendRow(width, visible); legend != "" {
			lines = append(lines, "", legend)
		}
	case pickerCreateAgent:
		lines = append(lines, p.createAgentLines(width)...)
	case pickerSessionActions:
		lines = append(lines, p.actionLines(width)...)
	case pickerDeleteConfirm:
		lines = append(lines, p.deleteLines(width)...)
	}
	if p.toast != nil {
		lines = append(lines, "", toastLine(p.toast, width))
	}
	return lines
}

func (p *picker) actionLines(width int) []string {
	id := p.modeSession
	lines := []string{backgroundRow(width)}
	heading := id
	s := p.sessionByID(id)
	if s != nil {
		heading = s.SessionID + "  " + s.Agent
	}
	lines = append(lines, menuLine(fitLine(heading, width), width, false))
	if s != nil {
		lines = append(lines, menuLine(fitLine(escapeControls(s.Cwd), width), width, false))
	}
	for i, action := range p.actions(id) {
		lines = append(lines, menuLine(fmt.Sprintf("%d. %s", i+1, action.label()), width, i == p.modeIndex))
	}
	return append(lines, backgroundRow(width), fitLine("Enter selects. Esc goes back.", width), "")
}

func (p *picker) createAgentLines(width int) []string {
	query := strings.TrimSpace(p.query)
	heading := "Choose coding agent"
	switch {
	case query != "" && p.requestedName() != "":
		heading = "Create: " + query
	case query != "":
		heading = "Invalid session name"
	}
	lines := []string{backgroundRow(width), menuLine(fitLine(heading, width), width, false)}
	for i, agent := range p.createAgents {
		label := agent
		if agent == p.defaultAgent {
			label += " (Default)"
		}
		lines = append(lines, menuLine(fmt.Sprintf("%d. %s", i+1, label), width, i == p.modeIndex))
	}
	help := "Enter selects. Esc goes back."
	if query != "" && p.requestedName() == "" {
		help = "Use lowercase letters, numbers, and hyphens. " + session.NameRules
	}
	return append(lines, backgroundRow(width), fitLine(help, width), "")
}

func (p *picker) deleteLines(width int) []string {
	id := p.modeSession
	lines := []string{backgroundRow(width)}
	if s := p.sessionByID(id); s != nil {
		lines = append(lines, menuLine(fitLine(escapeControls(s.Cwd), width), width, false))
	}
	lines = append(lines, menuLine(fmt.Sprintf("Delete %s? Its working directory is kept.", id), width, false))
	for i, label := range []string{"yes", "no"} {
		lines = append(lines, menuLine(fmt.Sprintf("%d. %s", i+1, label), width, i == p.modeIndex))
	}
	return append(lines, backgroundRow(width), fitLine("Enter selects. Esc returns to actions.", width), "")
}

// pickSession runs the picker below the prompt until the user picks or
// creates a session (returned), or cancels ("").
func (c *client) pickSession() (string, error) {
	raw, err := enterRaw(false)
	if err != nil {
		return "", fmt.Errorf("failed to enable raw terminal mode: %w", err)
	}
	defer raw.restore()
	writeOut([]byte(pickerEnterSequence))
	defer writeOut([]byte(pickerExitSequence))

	p := newPicker(c)
	if err := p.refresh(); err != nil {
		return "", err
	}
	var keys keyDecoder
	input := stdinChunks()
	start := time.Now()
	drawn := 0
	var last []string
	ticker := time.NewTicker(pickerRefreshInterval)
	defer ticker.Stop()
	for {
		width := stdoutWidth()
		if width == 0 {
			width = 80
		}
		cursorVisible := (time.Since(start)/pickerBlinkInterval)%2 == 0
		if lines := p.renderLines(width, cursorVisible); !equalLines(lines, last) {
			drawn = drawPicker(lines, drawn)
			last = lines
		}
		select {
		case <-ticker.C:
			if err := p.refresh(); err != nil {
				return "", err
			}
		case chunk, ok := <-input:
			if !ok {
				clearPicker(drawn)
				return "", errors.New("stdin closed")
			}
			for _, ev := range keys.push(chunk) {
				if ev.isPaste {
					p.handlePaste(ev.paste)
					continue
				}
				chosen, done, err := p.handleKey(ev)
				if err != nil {
					return "", err
				}
				if done {
					clearPicker(drawn)
					return chosen, nil
				}
			}
		}
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// drawPicker redraws the picker in place of the previous drawLines lines.
func drawPicker(lines []string, previous int) int {
	clearPicker(previous)
	var b strings.Builder
	if previous == 0 {
		b.WriteString("\r\n")
	}
	b.WriteString(strings.Join(lines, "\r\n"))
	writeOut([]byte(b.String()))
	return len(lines)
}

func clearPicker(previous int) {
	if previous == 0 {
		return
	}
	seq := "\r"
	if previous > 1 {
		seq += fmt.Sprintf("\x1b[%dA", previous-1)
	}
	writeOut([]byte(seq + "\x1b[J"))
}
