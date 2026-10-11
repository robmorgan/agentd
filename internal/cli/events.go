package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// `agent events`: a daemon's recorded session events, once or followed.
//
// Following is resumable by design: events are persisted and numbered, so
// the CLI keeps the id of the last event it handled and, when the stream
// ends (the daemon restarted, the network went away), subscribes again
// after it. Nothing is missed or repeated across a reconnect, except events
// the daemon pruned meanwhile. Task-level notifications build on this:
// --notify alerts the user's terminal on action-level events and --exec
// runs a command for each event.

type eventsOptions struct {
	follow, json, notify, all bool
	exec                      string
	lines                     int
	after                     uint64
	afterSet                  bool
	level                     string
}

// eventsFollowRetry bounds the backoff between reconnects while following.
const (
	eventsFirstRetry = 500 * time.Millisecond
	eventsMaxRetry   = 5 * time.Second
)

func (a *app) events(sessionArg string, opts eventsOptions) error {
	minLevel, err := parseLevel(opts.level)
	if err != nil {
		return usageError{err}
	}
	if opts.lines < 0 || opts.lines > 1000 {
		return usageError{errors.New("--lines must be between 0 and 1000")}
	}
	h := &eventHandler{json: opts.json, exec: opts.exec, minLevel: minLevel, out: os.Stdout, mu: &sync.Mutex{}}
	if opts.notify {
		h.alerts = openAlertTerminal()
		if h.alerts == nil {
			fmt.Fprintln(os.Stderr, "agent events: no terminal to notify on; --notify does nothing")
		} else {
			defer h.alerts.Close()
		}
	}
	if opts.all {
		if sessionArg != "" || a.host != "" {
			return usageError{errors.New("--all lists every host's events; it takes no session or --host")}
		}
		return a.eventsAllHosts(opts, h)
	}
	id := sessionArg
	c, err := a.connect([]*string{&id}, "", nil)
	if err != nil {
		return err
	}
	hh := *h
	hh.host = c.remoteName()
	return c.runEvents(id, opts, &hh)
}

// runEvents prints this client's daemon's events for session id (all
// sessions when ""), then follows them if asked.
func (c *client) runEvents(id string, opts eventsOptions, h *eventHandler) error {
	w, err := c.welcome()
	if err != nil {
		return err
	}
	if !protocol.HasCapability(w.Capabilities, protocol.CapEvents) {
		return errors.New("this agentd does not record events; upgrade it (`agent daemon upgrade`)")
	}
	var sessionID *string
	if id != "" {
		sessionID = &id
	}

	var cursor uint64
	if opts.afterSet {
		cursor = opts.after
		if !opts.follow {
			return c.listEventsAfter(cursor, sessionID, opts.lines, h)
		}
	} else {
		// The newest events, which also tell a follower where "now" is:
		// it subscribes after the last one, so nothing recorded between
		// the two requests is missed.
		limit := uint32(max(opts.lines, 1))
		resp, err := c.call(&protocol.Request{ListEvents: &protocol.ListEvents{SessionID: sessionID, Limit: limit}}, 0,
			func(r *protocol.Response) bool { return r.Events != nil })
		if err != nil {
			return err
		}
		events := *resp.Events
		if len(events) > 0 {
			cursor = events[len(events)-1].ID
		}
		if opts.lines == 0 {
			events = nil
		}
		for _, ev := range events {
			if err := h.handle(ev); err != nil {
				return err
			}
		}
	}
	if !opts.follow {
		return nil
	}
	return c.followEvents(cursor, sessionID, h)
}

// eventsAllHosts runs `agent events --all`: one reader per host (this
// machine's daemon and every host in hosts.toml), each on its own
// goroutine with its own connection, sharing one handler whose lock keeps
// their lines whole. Event ids are per host, so --after is refused. Without
// --follow it waits for every host; with it, a host that cannot be reached
// yet is retried with backoff, like a lost stream, until interrupted.
func (a *app) eventsAllHosts(opts eventsOptions, h *eventHandler) error {
	if opts.afterSet {
		return usageError{errors.New("--after names an event of one host; it cannot be used with --all")}
	}
	local := &client{paths: a.paths}
	if err := local.ensureDaemon(); err != nil {
		return err
	}
	local.close()
	hosts, err := transport.ReadHosts(a.paths.HostsPath())
	if err != nil {
		return err
	}
	clients := []*client{{paths: a.paths}}
	for i := range hosts {
		clients = append(clients, &client{paths: a.paths, host: &hosts[i]})
	}
	var wg sync.WaitGroup
	for _, c := range clients {
		hh := *h
		hh.host = c.remoteName()
		if hh.host == "" {
			hh.host = localHostName
		}
		wg.Go(func() {
			defer c.close()
			delay := time.Duration(0)
			for {
				err := c.runEvents("", opts, &hh)
				if err == nil || !opts.follow || errors.Is(err, errHandler) || !(transport.IsConnectionLost(err) || errors.Is(err, context.DeadlineExceeded)) {
					if err != nil {
						fmt.Fprintf(os.Stderr, "agent events: %s: %v\n", hh.host, err)
					}
					return
				}
				delay = min(max(2*delay, eventsFirstRetry), eventsMaxRetry)
				c.close()
				time.Sleep(delay)
			}
		})
	}
	wg.Wait()
	return nil
}

// listEventsAfter prints events after cursor, page by page, up to limit
// (0: all of them).
func (c *client) listEventsAfter(cursor uint64, sessionID *string, limit int, h *eventHandler) error {
	printed := 0
	for {
		page := uint32(1000)
		if limit > 0 {
			page = uint32(min(limit-printed, 1000))
		}
		after := cursor
		resp, err := c.call(&protocol.Request{ListEvents: &protocol.ListEvents{AfterID: &after, SessionID: sessionID, Limit: page}}, 0,
			func(r *protocol.Response) bool { return r.Events != nil })
		if err != nil {
			return err
		}
		for _, ev := range *resp.Events {
			if err := h.handle(ev); err != nil {
				return err
			}
			cursor = ev.ID
			printed++
		}
		if len(*resp.Events) < int(page) || (limit > 0 && printed >= limit) {
			return nil
		}
	}
}

// followEvents follows events after cursor until interrupted or the
// daemon refuses, reconnecting whenever the stream is lost.
func (c *client) followEvents(cursor uint64, sessionID *string, h *eventHandler) error {
	delay := time.Duration(0)
	for {
		err := c.streamEvents(cursor, sessionID, func(ev session.Event) error {
			cursor = ev.ID
			delay = 0
			return h.handle(ev)
		})
		var refused *daemonRefusal
		switch {
		case errors.As(err, &refused), errors.Is(err, errHandler), transport.IsKeyRefused(err):
			return err
		case c.host != nil && err != nil && !transport.IsConnectionLost(err) && !isStreamEnd(err):
			return err
		}
		reason := "the stream ended"
		if err != nil {
			reason = err.Error()
		}
		delay = min(max(2*delay, eventsFirstRetry), eventsMaxRetry)
		fmt.Fprintf(os.Stderr, "agent events: %s; reconnecting in %s\n", reason, delay)
		// A lost remote connection is dead; the next stream dials anew.
		c.close()
		time.Sleep(delay)
	}
}

// daemonRefusal is an Error frame from the daemon: retrying will not help.
type daemonRefusal struct{ msg string }

func (e *daemonRefusal) Error() string { return e.msg }

// errHandler wraps a failure handling an event (writing output), which
// ends following.
var errHandler = errors.New("handling event")

func isStreamEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// streamEvents subscribes after cursor and hands each event to fn until
// the stream ends. A clean end returns nil.
func (c *client) streamEvents(cursor uint64, sessionID *string, fn func(session.Event) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.controlTimeout()+c.requestTimeout())
	s, err := c.open(ctx)
	cancel()
	if err != nil {
		return err
	}
	defer s.Close()
	after := cursor
	if err := protocol.WriteRequest(s, &protocol.Request{SubscribeEvents: &protocol.SubscribeEvents{AfterID: &after, SessionID: sessionID}}); err != nil {
		return err
	}
	r := bufio.NewReader(s)
	for {
		resp, err := protocol.ReadResponse(r)
		switch {
		case err != nil:
			return err
		case resp == nil:
			return nil
		case resp.Error != nil:
			return &daemonRefusal{resp.Error.Message}
		case resp.Event == nil:
			return &daemonRefusal{fmt.Sprintf("unexpected response on an events stream: %s", describeResponse(resp))}
		}
		if err := fn(*resp.Event); err != nil {
			return fmt.Errorf("%w: %w", errHandler, err)
		}
	}
}

// eventHandler prints, notifies and runs commands for events.
type eventHandler struct {
	host     string
	json     bool
	exec     string
	minLevel session.AttentionLevel
	out      io.Writer
	// alerts is the terminal --notify writes to, or nil.
	alerts *os.File
	// mu, shared by the handlers of every host with --all, keeps one
	// event's output and command from interleaving with another's.
	mu *sync.Mutex
}

func (h *eventHandler) handle(ev session.Event) error {
	if h.mu != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	if ev.Attention.Rank() < h.minLevel.Rank() {
		return nil
	}
	line := formatEvent(ev, h.host, time.Now())
	if h.json {
		line = eventJSON(ev, h.host)
	}
	if _, err := fmt.Fprintln(h.out, line); err != nil {
		return err
	}
	if h.alerts != nil && ev.Attention == session.AttentionAction {
		_, _ = h.alerts.Write(alertBytes(ev, h.host, os.Getenv))
	}
	if h.exec != "" {
		cmd := exec.Command("/bin/sh", "-c", h.exec)
		cmd.Env = append(os.Environ(), eventEnv(ev, h.host)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "agent events: --exec for event %d: %v\n", ev.ID, err)
		}
	}
	return nil
}

func parseLevel(v string) (session.AttentionLevel, error) {
	if v == "" {
		return session.AttentionInfo, nil
	}
	level, err := session.ParseAttention(v)
	if err != nil {
		return "", fmt.Errorf("--level must be info, notice or action, not `%s`", v)
	}
	return level, nil
}

func eventSession(ev session.Event, host string) string {
	if host != "" {
		return host + "/" + ev.SessionID
	}
	return ev.SessionID
}

// formatEvent is one line of `agent events`: time (with the date when it is
// not today), id, session, attention, kind and summary.
func formatEvent(ev session.Event, host string, now time.Time) string {
	at := ev.At.Local()
	stamp := at.Format("15:04:05")
	if y, m, d := at.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		stamp = at.Format("2006-01-02 15:04:05")
	}
	return fmt.Sprintf("%s  #%d  %s  %s  %s  %s", stamp, ev.ID, eventSession(ev, host),
		attentionLabel(ev.Attention), padRight(escapeControls(string(ev.Kind)), 12), escapeControls(ev.Summary))
}

func attentionLabel(a session.AttentionLevel) string {
	switch a {
	case session.AttentionAction:
		return "action"
	case session.AttentionNotice:
		return "notice"
	}
	return "info  "
}

func eventJSON(ev session.Event, host string) string {
	v := struct {
		ID        uint64 `json:"id"`
		Host      string `json:"host,omitempty"`
		Session   string `json:"session"`
		At        string `json:"at"`
		Kind      string `json:"kind"`
		Attention string `json:"attention"`
		Summary   string `json:"summary"`
	}{ev.ID, host, ev.SessionID, ev.At.UTC().Format(time.RFC3339Nano), string(ev.Kind), string(ev.Attention), ev.Summary}
	data, _ := json.Marshal(v)
	return string(data)
}

// eventEnv describes an event to an --exec command.
func eventEnv(ev session.Event, host string) []string {
	return []string{
		"AGENTD_EVENT_ID=" + strconv.FormatUint(ev.ID, 10),
		"AGENTD_EVENT_HOST=" + host,
		"AGENTD_EVENT_SESSION=" + ev.SessionID,
		"AGENTD_EVENT_KIND=" + string(ev.Kind),
		"AGENTD_EVENT_ATTENTION=" + string(ev.Attention),
		"AGENTD_EVENT_SUMMARY=" + ev.Summary,
		"AGENTD_EVENT_TIME=" + ev.At.UTC().Format(time.RFC3339),
	}
}

// openAlertTerminal opens the terminal --notify writes to: the controlling
// terminal, so alerts reach the user even when stdout is piped.
func openAlertTerminal() *os.File {
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		return f
	}
	return nil
}

// alertBytes rings the bell and asks the terminal for a desktop
// notification. OSC 9 is the most widely understood (iTerm2, Ghostty,
// WezTerm, kitty, Windows Terminal); VTE-based terminals, foot and urxvt
// take OSC 777 instead. Inside tmux the sequence is passed through (tmux
// needs `set -g allow-passthrough on`).
func alertBytes(ev session.Event, host string, getenv func(string) string) []byte {
	title := "agentd: " + eventSession(ev, host)
	body := oscText(ev.Summary)
	if body == "" {
		body = string(ev.Kind)
	}
	var seq string
	term := getenv("TERM")
	if getenv("VTE_VERSION") != "" || strings.HasPrefix(term, "foot") || strings.HasPrefix(term, "rxvt") {
		seq = "\x1b]777;notify;" + strings.ReplaceAll(oscText(title), ";", ",") + ";" + body + "\x1b\\"
	} else {
		seq = "\x1b]9;" + oscText(title) + ": " + body + "\x1b\\"
	}
	if getenv("TMUX") != "" {
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	return []byte("\a" + seq)
}

// oscText strips what could end or confuse an OSC string, and the
// invisible format characters (unicode.Cf) that could reorder or hide
// what a desktop notification appears to say.
func oscText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}
