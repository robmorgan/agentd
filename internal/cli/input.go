package cli

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"unicode/utf8"
)

// stdinChunks returns the channel carrying everything read from stdin.
//
// One goroutine reads stdin for the whole process, blocking in read(2).
// The picker, the attach loop and the overlay all take their input from
// it, so switching between them (or between sessions) never has to stop a
// read in progress, and stdin is never made non-blocking, which would leak
// to the shell through the shared file description. The goroutine is never
// stopped: it ends with the process, or when stdin reaches EOF or fails, at
// which point the channel is closed.
//
// The channel holds at most stdinQueue chunks. When a consumer falls
// behind, the reader blocks, stops reading, and the terminal applies
// backpressure to the typist; nothing is buffered without bound.
func stdinChunks() <-chan []byte {
	stdinOnce.Do(func() {
		ch := make(chan []byte, stdinQueue)
		stdinCh = ch
		go func() {
			defer close(ch)
			for {
				buf := make([]byte, 4096)
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					ch <- buf[:n]
				}
				if err != nil {
					return
				}
			}
		}()
	})
	return stdinCh
}

const stdinQueue = 32

var (
	stdinOnce sync.Once
	stdinCh   chan []byte
)

// Attach hotkeys: legacy control bytes, and the kitty keyboard protocol's
// CSI-u form (attach pushes its "disambiguate" flag, so Ctrl-[ can be told
// apart from Esc).
const (
	attachDetachByte      = 0x1c // Ctrl-\
	attachOverlayByte     = 0x19 // Ctrl-Y
	attachNextSessionByte = 0x1d // Ctrl-]

	attachOverlayCodepoint         = 121 // y
	attachPreviousSessionCodepoint = 91  // [
	attachDetachCodepoint          = 92  // \
	attachNextSessionCodepoint     = 93  // ]
)

type attachActionKind int

const (
	actionData attachActionKind = iota
	actionDetach
	actionOpenOverlay
	actionPreviousSession
	actionNextSession
)

type attachAction struct {
	kind attachActionKind
	data []byte // for actionData
}

// attachParser splits attach input into bytes for the agent and hotkeys.
// A hotkey's CSI-u sequence may be split across reads; the incomplete part
// is held until the next chunk.
type attachParser struct {
	pending []byte
}

func (p *attachParser) push(chunk []byte) []attachAction {
	input := append(p.pending, chunk...)
	p.pending = nil
	var actions []attachAction
	var forwarded []byte
	flush := func() {
		if len(forwarded) > 0 {
			actions = append(actions, attachAction{kind: actionData, data: forwarded})
			forwarded = nil
		}
	}
	for i := 0; i < len(input); {
		if input[i] == 0x1b {
			kind, consumed, incomplete := parseAttachHotkeyCSIu(input[i:])
			if incomplete {
				p.pending = append([]byte(nil), input[i:]...)
				break
			}
			if consumed > 0 {
				flush()
				actions = append(actions, attachAction{kind: kind})
				i += consumed
				continue
			}
		}
		switch input[i] {
		case attachOverlayByte:
			flush()
			actions = append(actions, attachAction{kind: actionOpenOverlay})
		case attachDetachByte:
			flush()
			actions = append(actions, attachAction{kind: actionDetach})
		case attachNextSessionByte:
			flush()
			actions = append(actions, attachAction{kind: actionNextSession})
		default:
			forwarded = append(forwarded, input[i])
		}
		i++
	}
	flush()
	return actions
}

// parseAttachHotkeyCSIu recognises Ctrl+y, Ctrl+[, Ctrl+\ and Ctrl+] as
// kitty keyboard protocol key events: ESC [ code[:alt...] ; mods[:event]
// [;text] u. Lock modifiers are ignored and key releases are not hotkeys.
// It returns consumed == 0 for anything else, which is forwarded as it is.
func parseAttachHotkeyCSIu(b []byte) (kind attachActionKind, consumed int, incomplete bool) {
	if len(b) < 2 || b[0] != 0x1b || b[1] != '[' {
		return 0, 0, false
	}
	i := 2
	code, ok := csiDecimal(b, &i)
	if !ok {
		return 0, 0, false
	}
	switch code {
	case attachOverlayCodepoint:
		kind = actionOpenOverlay
	case attachPreviousSessionCodepoint:
		kind = actionPreviousSession
	case attachDetachCodepoint:
		kind = actionDetach
	case attachNextSessionCodepoint:
		kind = actionNextSession
	default:
		return 0, 0, false
	}
	for i < len(b) && b[i] == ':' {
		i++
		start := i
		if _, ok := csiDecimal(b, &i); !ok && i != start {
			return 0, 0, false
		}
		if i >= len(b) {
			return 0, 0, true
		}
	}
	if i >= len(b) {
		return 0, 0, true
	}
	if b[i] != ';' {
		return 0, 0, false
	}
	i++
	if i >= len(b) {
		return 0, 0, true
	}
	mods, ok := csiDecimal(b, &i)
	if !ok || mods == 0 {
		return 0, 0, false
	}
	if (mods-1)&0b111111 != 0b100 {
		return 0, 0, false
	}
	if i < len(b) && b[i] == ':' {
		i++
		if i >= len(b) {
			return 0, 0, true
		}
		event, ok := csiDecimal(b, &i)
		if !ok || event == 3 {
			return 0, 0, false
		}
	}
	if i < len(b) && b[i] == ';' {
		i++
		if i >= len(b) {
			return 0, 0, true
		}
		for i < len(b) && (isDigit(b[i]) || b[i] == ':') {
			i++
		}
	}
	if i >= len(b) {
		return 0, 0, true
	}
	if b[i] != 'u' {
		return 0, 0, false
	}
	return kind, i + 1, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// csiDecimal reads a decimal number at b[*i:], advancing *i. It fails when
// there are no digits or the value overflows.
func csiDecimal(b []byte, i *int) (uint32, bool) {
	start := *i
	var v uint64
	for *i < len(b) && isDigit(b[*i]) {
		v = v*10 + uint64(b[*i]-'0')
		if v > 1<<32-1 {
			return 0, false
		}
		*i++
	}
	return uint32(v), *i > start
}

// Keys for the picker and the overlay.

type keyCode int

const (
	keyNone keyCode = iota
	keyChar
	keyEnter
	keyEsc
	keyBackspace
	keyTab
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyDelete
)

type keyMods uint8

const (
	modShift keyMods = 1 << iota
	modAlt
	modCtrl
)

// keyEvent is one key press, or (with paste set) a bracketed paste.
type keyEvent struct {
	code    keyCode
	r       rune // for keyChar
	mods    keyMods
	paste   string
	isPaste bool
}

// keyDecoder turns terminal input into key events. It reads the legacy
// encodings a plain raw-mode terminal sends and the kitty keyboard
// protocol's CSI-u form, which the overlay receives because attach enables
// it. As with most terminal libraries, an ESC that ends a read is the Esc
// key; any other incomplete sequence waits for the next read.
type keyDecoder struct {
	pending []byte
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

func (d *keyDecoder) push(chunk []byte) []keyEvent {
	b := append(d.pending, chunk...)
	d.pending = nil
	var events []keyEvent
	for len(b) > 0 {
		if bytes.HasPrefix(b, pasteStart) {
			end := bytes.Index(b, pasteEnd)
			if end < 0 {
				d.pending = append([]byte(nil), b...)
				break
			}
			events = append(events, keyEvent{isPaste: true, paste: string(b[len(pasteStart):end])})
			b = b[end+len(pasteEnd):]
			continue
		}
		ev, n := decodeKey(b)
		if n == 0 {
			d.pending = append([]byte(nil), b...)
			break
		}
		if ev.code != keyNone {
			events = append(events, ev)
		}
		b = b[n:]
	}
	return events
}

// decodeKey decodes the key at the start of b, returning how many bytes it
// used; 0 means b holds only the start of a sequence.
func decodeKey(b []byte) (keyEvent, int) {
	c := b[0]
	switch {
	case c == 0x1b:
		if len(b) == 1 {
			return keyEvent{code: keyEsc}, 1
		}
		switch b[1] {
		case '[':
			return decodeCSI(b)
		case 'O':
			if len(b) < 3 {
				return keyEvent{}, 0
			}
			return keyEvent{code: ss3Key(b[2])}, 3
		case 0x1b:
			return keyEvent{code: keyEsc}, 1
		}
		ev, n := decodeKey(b[1:])
		if n == 0 {
			return keyEvent{}, 0
		}
		ev.mods |= modAlt
		return ev, n + 1
	case c == '\r' || c == '\n':
		return keyEvent{code: keyEnter}, 1
	case c == 0x7f || c == 0x08:
		return keyEvent{code: keyBackspace}, 1
	case c == '\t':
		return keyEvent{code: keyTab}, 1
	case c == 0:
		return keyEvent{code: keyChar, r: ' ', mods: modCtrl}, 1
	case c < 0x20:
		return keyEvent{code: keyChar, r: rune('a' + c - 1), mods: modCtrl}, 1
	}
	if !utf8.FullRune(b) {
		return keyEvent{}, 0
	}
	r, n := utf8.DecodeRune(b)
	return keyEvent{code: keyChar, r: r}, n
}

func ss3Key(c byte) keyCode {
	switch c {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	}
	return keyNone
}

func decodeCSI(b []byte) (keyEvent, int) {
	i := 2
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	if i >= len(b) {
		return keyEvent{}, 0
	}
	final := b[i]
	n := i + 1
	if final < 0x40 || final > 0x7e {
		// Not a CSI sequence after all: drop the ESC [ and go on.
		return keyEvent{}, 2
	}
	params := splitParams(string(b[2:i]))
	param := func(idx, sub int) int {
		if idx >= len(params) || sub >= len(params[idx]) {
			return 0
		}
		v, _ := strconv.Atoi(params[idx][sub])
		return v
	}
	mods := keyMods(0)
	if m := param(1, 0); m > 1 {
		m--
		if m&1 != 0 {
			mods |= modShift
		}
		if m&2 != 0 {
			mods |= modAlt
		}
		if m&4 != 0 {
			mods |= modCtrl
		}
	}
	if param(1, 1) == 3 {
		return keyEvent{}, n // a key release
	}
	var code keyCode
	switch final {
	case 'A', 'B', 'C', 'D', 'H', 'F':
		code = ss3Key(final)
	case 'Z':
		return keyEvent{code: keyTab, mods: modShift}, n
	case '~':
		switch param(0, 0) {
		case 1, 7:
			code = keyHome
		case 3:
			code = keyDelete
		case 4, 8:
			code = keyEnd
		case 5:
			code = keyPageUp
		case 6:
			code = keyPageDown
		}
	case 'u':
		switch cp := param(0, 0); cp {
		case 27:
			code = keyEsc
		case 13:
			code = keyEnter
		case 9:
			code = keyTab
		case 127, 8:
			code = keyBackspace
		default:
			if cp < 0x20 || cp > utf8.MaxRune {
				return keyEvent{}, n
			}
			r := rune(cp)
			if mods&modShift != 0 {
				if shifted := param(0, 1); shifted > 0 {
					r = rune(shifted)
				}
			}
			return keyEvent{code: keyChar, r: r, mods: mods}, n
		}
	}
	return keyEvent{code: code, mods: mods}, n
}

func splitParams(s string) [][]string {
	if s == "" {
		return nil
	}
	var out [][]string
	for _, field := range bytes.Split([]byte(s), []byte(";")) {
		var subs []string
		for _, sub := range bytes.Split(field, []byte(":")) {
			subs = append(subs, string(sub))
		}
		out = append(out, subs)
	}
	return out
}

// printable reports whether the key types text: a character with no
// modifier but Shift.
func (e keyEvent) printable() bool {
	return e.code == keyChar && e.mods&^modShift == 0
}
