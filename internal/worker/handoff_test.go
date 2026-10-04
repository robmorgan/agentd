package worker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

// The parts of a handoff that run in one process: the terminal state that
// crosses the exec, the pump that must stop without consuming output, and
// the probe of the new executable. The exec itself is covered by the daemon
// package's tests, which run real worker processes.

// terminalFacts is what a client or the agent could observe of a terminal.
type terminalFacts struct {
	vt, plain       string
	cursorX, cursor uint16
	screen          libghostty.TerminalScreen
	bracketedPaste  bool
	title           string
}

func facts(t *testing.T, s *terminalState) terminalFacts {
	t.Helper()
	vt, err := s.format(true)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.format(false)
	if err != nil {
		t.Fatal(err)
	}
	var f terminalFacts
	f.vt, f.plain = string(vt), string(plain)
	f.cursorX, _ = s.term.CursorX()
	f.cursor, _ = s.term.CursorY()
	f.screen, _ = s.term.ActiveScreen()
	f.bracketedPaste, _ = s.term.Mode(libghostty.ModeBracketedPaste)
	f.title, _ = s.term.Title()
	return f
}

// A handoff restores the shadow terminal exactly: both screens and the
// scrollback, the cursor, modes, the title, and an escape sequence that was
// cut off mid-way, whose remainder (arriving after the handoff) must still
// be parsed as the same sequence.
func TestTerminalHandoffIsExact(t *testing.T) {
	old, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer old.close()
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "\x1b[3%dmscrollback line %d\x1b[0m\r\n", i%8, i)
	}
	b.WriteString("\x1b]2;handoff title\x07")
	b.WriteString("\x1b[?2004h")              // bracketed paste
	b.WriteString("\x1b[5;20rprimary\r\n")    // a scrolling region
	b.WriteString("\x1b[?1049h\x1b[2J\x1b[H") // the alternate screen
	b.WriteString("\x1b[1;4mfull-screen app\x1b[0m\x1b[10;7H")
	b.WriteString("\x1b[3") // cut off mid-sequence
	old.feed([]byte(b.String()))

	h, err := old.handoff()
	if err != nil {
		t.Fatal(err)
	}
	restored, inexact, err := restoreTerminal(h)
	if err != nil || inexact != nil {
		t.Fatalf("restore: inexact %v, err %v", inexact, err)
	}
	defer restored.close()

	before, after := facts(t, old), facts(t, restored)
	if before != after {
		t.Fatalf("restored terminal differs:\nbefore %+v\nafter  %+v", before, after)
	}
	if after.screen != libghostty.ScreenAlternate || !after.bracketedPaste || after.title != "handoff title" {
		t.Fatalf("restored state is not what was written: %+v", after)
	}

	// The rest of the cut-off sequence, and everything after it, lands the
	// same way in both.
	rest := []byte("1mred\x1b[0m after\x1b[?1049l back on primary")
	old.feed(rest)
	restored.feed(rest)
	if before, after := facts(t, old), facts(t, restored); before != after {
		t.Fatalf("terminals diverged after more output:\nbefore %+v\nafter  %+v", before, after)
	}
	// Back on the primary screen: its scrollback came through too.
	if got := facts(t, restored).plain; !strings.Contains(got, "scrollback line 0\n") || !strings.Contains(got, "back on primary") {
		t.Fatalf("primary screen after restore: %q", got[:min(len(got), 300)])
	}

	// A restored terminal hands off again just as exactly.
	again, err := restored.handoff()
	if err != nil {
		t.Fatal(err)
	}
	third, inexact, err := restoreTerminal(again)
	if err != nil || inexact != nil {
		t.Fatalf("second restore: inexact %v, err %v", inexact, err)
	}
	defer third.close()
	if facts(t, restored) != facts(t, third) {
		t.Fatal("second handoff changed the terminal")
	}
}

// Terminal queries keep being answered by a restored terminal (its
// callbacks are wired up again).
func TestRestoredTerminalAnswersQueries(t *testing.T) {
	old, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer old.close()
	h, err := old.handoff()
	if err != nil {
		t.Fatal(err)
	}
	restored, _, err := restoreTerminal(h)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	replies, _ := restored.feed([]byte("\x1b[18t\x1b[6n"))
	if got := string(bytes.Join(replies, nil)); !strings.Contains(got, "\x1b[8;24;80t") || !strings.Contains(got, "R") {
		t.Fatalf("replies = %q", got)
	}
}

// A snapshot the new build cannot decode (the format is not yet stable
// across libghostty versions) still restores the screen's text.
func TestTerminalHandoffFallsBackToText(t *testing.T) {
	old, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer old.close()
	old.feed([]byte("earlier output\r\n\x1b[32mgreen\x1b[0m\r\n"))
	h, err := old.handoff()
	if err != nil {
		t.Fatal(err)
	}
	h.Snapshot = append([]byte(nil), h.Snapshot[:len(h.Snapshot)/2]...)
	restored, inexact, err := restoreTerminal(h)
	if err != nil || inexact == nil {
		t.Fatalf("restore: inexact %v, err %v", inexact, err)
	}
	defer restored.close()
	if got := facts(t, restored).plain; !strings.Contains(got, "earlier output") || !strings.Contains(got, "green") {
		t.Fatalf("fallback lost the screen: %q", got)
	}
}

// Stopping the pump leaves output that arrives afterwards in the PTY for the
// next reader, and everything read before the stop reaches the owner.
func TestPumpStopLeavesOutputInThePTY(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	o := newOwner()
	term, err := newTerminalState(80, 24, maxScrollbackBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()
	state := newOwnerState("pump", ptmx, term)
	go o.run(state)
	defer o.stop()

	eof := make(chan struct{})
	p, err := startPump(ptmx, o, eof)
	if err != nil {
		t.Fatal(err)
	}
	tty.Write([]byte("before-stop\r\n"))
	deadline := time.Now().Add(testTimeout)
	for !strings.Contains(historyOf(t, o), "before-stop") {
		if time.Now().After(deadline) {
			t.Fatal("output before the stop never reached the terminal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.stop()

	tty.Write([]byte("after-stop\r\n"))
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(historyOf(t, o), "after-stop") {
		t.Fatal("a stopped pump consumed output")
	}
	readUntil(t, ptmx, "after-stop")
	select {
	case <-eof:
		t.Fatal("stopping the pump reported EOF")
	default:
	}
}

func historyOf(t *testing.T, o *owner) string {
	t.Helper()
	var h string
	if err := o.do(func(s *ownerState) error {
		var err error
		h, err = s.history(false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// The probe catches an executable that would fail after the exec, when it
// is too late to go back.
func TestProbeHandoff(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := script("good", `echo "`+handoffProbeOutput+`"`)
	if err := probeHandoff(good); err != nil {
		t.Fatalf("good executable: %v", err)
	}
	for name, exe := range map[string]string{
		"missing":   filepath.Join(dir, "missing"),
		"failing":   script("failing", "exit 1"),
		"too old":   script("old", `echo "agentd session-worker handoff 0"`),
		"no answer": script("silent", "exit 0"),
		"empty":     "",
	} {
		if err := probeHandoff(exe); err == nil {
			t.Errorf("%s executable passed the probe", name)
		}
	}
}

func TestTrackerWaitsForHandlers(t *testing.T) {
	var tr tracker
	if !tr.waitIdle(0) {
		t.Fatal("an unused tracker is not idle")
	}
	tr.Add(1)
	tr.Add(1)
	if tr.waitIdle(10 * time.Millisecond) {
		t.Fatal("idle with handlers running")
	}
	tr.Done()
	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.Done()
	}()
	if !tr.waitIdle(testTimeout) {
		t.Fatal("never idle")
	}
	// It can be used again after a wait, as a failed handoff does.
	tr.Add(1)
	if tr.waitIdle(10 * time.Millisecond) {
		t.Fatal("idle with a handler running")
	}
	tr.Done()
	if !tr.waitIdle(0) {
		t.Fatal("not idle")
	}
}
