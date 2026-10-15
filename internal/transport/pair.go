package transport

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"filippo.io/cpace"
	"github.com/quic-go/quic-go"

	"github.com/robmorgan/agtd/internal/protocol"
)

// Pairing replaces copying key fingerprints between machines with a short
// code: `agentd remote pair` opens a pairing window on the daemon and shows
// a code, and `agt host add ... --code` on the client proves it knows the
// code. Both then trust each other's key: the daemon authorizes the
// client's, and the client pins the daemon's.
//
// The code is short enough to type, so it must not be guessable offline.
// The two ends run CPace, a password-authenticated key exchange, over a
// QUIC connection that negotiated PairALPN: each learns whether the other
// used the same code and nothing that would let it test another code
// offline. Its context binds the exchange to both keys' fingerprints and to
// this TLS connection (an exported keying value), so a machine in the
// middle, which terminates TLS with each end under its own key, can only
// make one online guess per attempt. The daemon allows PairingAttempts wrong
// guesses per window, and a window lasts PairingTTL.
//
// The client proves the code first; the daemon only confirms it, and
// authorizes the client, once that proof checks out. Nothing is trusted on
// the pairing connection itself: it carries only this exchange and is then
// closed, and the client reconnects normally under its now authorized key.
const (
	// PairALPN selects the pairing protocol in the TLS handshake instead
	// of the agent protocol (ALPN).
	PairALPN = "agentd-pair"
	// PairingTTL is how long a pairing code is valid.
	PairingTTL = 5 * time.Minute
	// PairingAttempts is how many wrong codes close a pairing window.
	PairingAttempts = 3
	// pairTimeout bounds one pairing attempt, from connecting to the last
	// message.
	pairTimeout = 30 * time.Second
	// maxPairingConns bounds the pairing connections a listener serves at
	// once, whatever their clients do.
	maxPairingConns = 4
	// pairingCodeDigits is the length of a pairing code: 30 bits, so
	// PairingAttempts online guesses succeed with a probability of about
	// 3 in a billion.
	pairingCodeDigits = 9
	// maxPairName bounds the client name recorded with its key.
	maxPairName = 64
)

// ErrNotPairing means the daemon refused a pairing connection: no pairing
// window is open there (or it predates pairing).
var ErrNotPairing = errors.New("the daemon is not pairing")

// PairRefusedError is the daemon's own reason for ending a pairing attempt,
// such as a wrong code.
type PairRefusedError struct{ Message string }

func (e *PairRefusedError) Error() string { return e.Message }

// NewPairingCode returns a random pairing code, formatted as 123-456-789.
func NewPairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return "", err
	}
	digits := fmt.Sprintf("%0*d", pairingCodeDigits, n)
	return digits[0:3] + "-" + digits[3:6] + "-" + digits[6:9], nil
}

// NormalizePairingCode returns a code's digits, ignoring the dashes and
// spaces people type between them.
func NormalizePairingCode(code string) (string, error) {
	var b strings.Builder
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == ' ':
		default:
			return "", fmt.Errorf("`%s` is not a pairing code (9 digits, like 123-456-789)", code)
		}
	}
	if b.Len() != pairingCodeDigits {
		return "", fmt.Errorf("`%s` is not a pairing code (9 digits, like 123-456-789)", code)
	}
	return b.String(), nil
}

// Pairing is a daemon's pairing window: at most one is open at a time. The
// daemon owns it for its whole life, across rebinds of its QUIC listener,
// which reads it through QUICOptions.Pairing.
type Pairing struct {
	host string

	mu     sync.Mutex
	window *PairingWindow
}

// NewPairing returns a closed pairing window for a daemon on host (the
// machine's name, shown to clients that pair).
func NewPairing(host string) *Pairing {
	return &Pairing{host: host}
}

// PairingWindow is one open pairing window. It ends when a client pairs,
// PairingAttempts wrong codes have been tried, PairingTTL passes, or Close
// is called; Done is closed then, and Result says which.
type PairingWindow struct {
	// Code is the code to show, formatted.
	Code      string
	ExpiresAt time.Time

	p         *Pairing
	secret    string
	authorize func(fingerprint, name string) error
	timer     *time.Timer
	done      chan struct{}

	// Guarded by p.mu.
	ended    bool
	busy     bool
	failures int
	// result is written before done is closed.
	result PairResult
}

// PairResult is how a pairing window ended. Err is nil when a client paired.
// Remote is the paired client's address, or, when the window closed after
// too many wrong codes, the address of the last client that guessed.
type PairResult struct {
	Fingerprint string
	Name        string
	Remote      net.Addr
	Err         error
}

var (
	errPairingExpired = errors.New("the pairing code expired")
	errPairingClosed  = errors.New("pairing was cancelled")
	// ErrTooManyWrongCodes ends a pairing window after PairingAttempts
	// wrong codes.
	ErrTooManyWrongCodes = fmt.Errorf("closed after %d wrong codes", PairingAttempts)
)

// Open opens a pairing window lasting ttl. authorize is called, once, with
// the key and name of the client that proves the code; the window reports
// success only if it returns nil. It fails if a window is already open.
func (p *Pairing) Open(ttl time.Duration, authorize func(fingerprint, name string) error) (*PairingWindow, error) {
	code, err := NewPairingCode()
	if err != nil {
		return nil, err
	}
	secret, _ := NormalizePairingCode(code)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window != nil {
		return nil, errors.New("another `agentd remote pair` is already waiting; finish or cancel it first")
	}
	w := &PairingWindow{
		Code:      code,
		ExpiresAt: time.Now().Add(ttl),
		p:         p,
		secret:    secret,
		authorize: authorize,
		done:      make(chan struct{}),
	}
	w.timer = time.AfterFunc(ttl, func() { w.end(PairResult{Err: errPairingExpired}) })
	p.window = w
	return w, nil
}

// Done is closed when the window has ended.
func (w *PairingWindow) Done() <-chan struct{} { return w.done }

// Result is how the window ended; it is valid once Done is closed.
func (w *PairingWindow) Result() PairResult { return w.result }

// Close ends the window if it is still open. A client already pairing
// fails.
func (w *PairingWindow) Close() { w.end(PairResult{Err: errPairingClosed}) }

// end ends the window with r, unless it has ended already.
func (w *PairingWindow) end(r PairResult) {
	if !w.markEnded() {
		return
	}
	w.result = r
	close(w.done)
}

// markEnded takes the window out of service; it reports false if it was
// already.
func (w *PairingWindow) markEnded() bool {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	if w.ended {
		return false
	}
	w.ended = true
	w.timer.Stop()
	if w.p.window == w {
		w.p.window = nil
	}
	return true
}

// accepting reports whether a pairing window is open, so the TLS handshake
// of a pairing connection may complete.
func (p *Pairing) accepting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.window != nil
}

// claim reserves the open window for one attempt. Attempts are serialized,
// so wrong guesses are counted exactly.
func (p *Pairing) claim() (*PairingWindow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.window
	switch {
	case w == nil:
		return nil, ErrNotPairing
	case w.busy:
		return nil, errors.New("another machine is pairing right now; try again in a moment")
	}
	w.busy = true
	return w, nil
}

// release ends an attempt that claim reserved.
func (w *PairingWindow) release() {
	w.p.mu.Lock()
	w.busy = false
	w.p.mu.Unlock()
}

// wrongCode counts a wrong guess and returns what to tell the client. The
// window closes at PairingAttempts, reporting remote as the last to guess.
func (w *PairingWindow) wrongCode(remote net.Addr) string {
	w.p.mu.Lock()
	w.failures++
	closing := w.failures >= PairingAttempts
	w.p.mu.Unlock()
	if closing {
		w.end(PairResult{Remote: remote, Err: ErrTooManyWrongCodes})
		return "wrong pairing code; pairing is now closed, so run `agentd remote pair` again for a new code"
	}
	return "wrong pairing code"
}

// succeed authorizes a client that proved the code and ends the window.
func (w *PairingWindow) succeed(fingerprint, name string, remote net.Addr) error {
	if !w.markEnded() {
		return errors.New("the pairing window closed")
	}
	err := w.authorize(fingerprint, name)
	if err != nil {
		w.result = PairResult{Err: fmt.Errorf("authorizing %s: %w", fingerprint, err)}
	} else {
		w.result = PairResult{Fingerprint: fingerprint, Name: name, Remote: remote}
	}
	close(w.done)
	return err
}

// pairContext binds a CPace exchange to both keys and to the TLS
// connection it runs on.
func pairContext(conn *quic.Conn, clientFP, daemonFP string) (*cpace.ContextInfo, error) {
	tlsState := conn.ConnectionState().TLS
	ekm, err := tlsState.ExportKeyingMaterial("EXPORTER-agentd-pair", nil, 32)
	if err != nil {
		return nil, err
	}
	ad := append([]byte(PairALPN+"\x00"), ekm...)
	return cpace.NewContextInfo(clientFP, daemonFP, ad), nil
}

// Each side confirms the shared key with its own label, so neither proof
// can be reflected back as the other.
func pairMAC(key []byte, side string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("agentd pairing " + side))
	return h.Sum(nil)
}

// pairName makes a client's claimed name safe to record: one line of
// printable text, bounded in length.
func pairName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if n == maxPairName {
			break
		}
		if !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// servePairing runs the daemon's side of one pairing connection, then
// closes it. The client sends one stream; anything else it does is
// ignored.
func (q *QUICListener) servePairing(conn *quic.Conn, clientFP string) {
	defer q.untrack(conn)
	defer conn.CloseWithError(0, "")
	ctx, cancel := context.WithTimeout(q.ctx, pairTimeout)
	defer cancel()
	s, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	defer s.CancelRead(0)
	s.SetDeadline(time.Now().Add(pairTimeout))
	reply := func(m *protocol.PairMessage) bool { return protocol.WritePairMessage(s, m) == nil }
	// last sends the exchange's final message. Closing the connection
	// straight after could discard it, so the client closes it once it
	// has read it.
	last := func(m *protocol.PairMessage) {
		if reply(m) {
			s.Close()
			select {
			case <-conn.Context().Done():
			case <-ctx.Done():
			}
		}
	}
	refuse := func(msg string) { last(&protocol.PairMessage{Error: &protocol.ErrorResponse{Message: msg}}) }

	m, err := protocol.ReadPairMessage(s)
	if err != nil || m.Hello == nil {
		return
	}
	hello := m.Hello
	w, err := q.opts.Pairing.claim()
	if err != nil {
		refuse(err.Error())
		return
	}
	defer w.release()
	ci, err := pairContext(conn, clientFP, q.fingerprint)
	if err != nil {
		refuse("pairing is not available on this connection")
		return
	}
	msg, key, err := cpace.Exchange(w.secret, ci, hello.Message)
	if err != nil {
		refuse("malformed pairing message")
		return
	}
	if !reply(&protocol.PairMessage{Challenge: &protocol.PairChallenge{Message: msg, Host: q.opts.Pairing.host}}) {
		return
	}
	m, err = protocol.ReadPairMessage(s)
	if err != nil || m.Proof == nil {
		return
	}
	if !hmac.Equal(m.Proof.MAC, pairMAC(key, "client")) {
		refuse(w.wrongCode(conn.RemoteAddr()))
		return
	}
	if err := w.succeed(clientFP, pairName(hello.Name), conn.RemoteAddr()); err != nil {
		refuse("paired, but this machine could not be authorized: " + err.Error())
		return
	}
	last(&protocol.PairMessage{Accepted: &protocol.PairAccepted{MAC: pairMAC(key, "daemon")}})
}

// PairedHost is the daemon a client paired with.
type PairedHost struct {
	// Fingerprint is the daemon's key, to pin.
	Fingerprint string
	// Host is the daemon machine's name.
	Host string
}

// Pair proves to the daemon at addr that this client knows its pairing
// code. On success the daemon has authorized id under name, and the
// returned key is the daemon's, proven to know the code too. It returns
// ErrNotPairing if the daemon has no pairing window open, and a
// *PairRefusedError if the daemon refused the attempt (a wrong code).
func Pair(ctx context.Context, addr string, id *Identity, code, name string) (*PairedHost, error) {
	secret, err := NormalizePairingCode(code)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, pairTimeout)
	defer cancel()
	conn, presented, err := dialEach(ctx, addr, id, "", PairALPN)
	if err != nil {
		return nil, pairErr(err)
	}
	defer conn.CloseWithError(0, "")
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, pairErr(err)
	}
	defer s.CancelRead(0)
	if deadline, ok := ctx.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	read := func() (*protocol.PairMessage, error) {
		m, err := protocol.ReadPairMessage(s)
		if err != nil {
			return nil, pairErr(err)
		}
		if m.Error != nil {
			return nil, &PairRefusedError{Message: m.Error.Message}
		}
		return m, nil
	}

	ci, err := pairContext(conn, id.Fingerprint, presented)
	if err != nil {
		return nil, err
	}
	msg, state, err := cpace.Start(secret, ci)
	if err != nil {
		return nil, err
	}
	if err := protocol.WritePairMessage(s, &protocol.PairMessage{Hello: &protocol.PairHello{Name: name, Message: msg}}); err != nil {
		return nil, pairErr(err)
	}
	m, err := read()
	if err != nil {
		return nil, err
	}
	challenge := m.Challenge
	if challenge == nil {
		return nil, errors.New("unexpected pairing message from the daemon")
	}
	key, err := state.Finish(challenge.Message)
	if err != nil {
		return nil, fmt.Errorf("malformed pairing message from the daemon: %w", err)
	}
	if err := protocol.WritePairMessage(s, &protocol.PairMessage{Proof: &protocol.PairProof{MAC: pairMAC(key, "client")}}); err != nil {
		return nil, pairErr(err)
	}
	m, err = read()
	if err != nil {
		return nil, err
	}
	if m.Accepted == nil || !hmac.Equal(m.Accepted.MAC, pairMAC(key, "daemon")) {
		return nil, errors.New("the daemon could not prove it knows the pairing code; not trusting it")
	}
	return &PairedHost{Fingerprint: presented, Host: challenge.Host}, nil
}

// pairErr turns the daemon refusing the pairing handshake into
// ErrNotPairing. With TLS 1.3 the refusal can surface on the stream, after
// the dial succeeded.
func pairErr(err error) error {
	if IsKeyRefused(err) {
		return ErrNotPairing
	}
	return err
}
