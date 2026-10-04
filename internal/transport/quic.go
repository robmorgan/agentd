package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN identifies the agent protocol inside QUIC's TLS handshake.
const ALPN = "agentd"

// DeadPeerTimeout is how long a connection may go without hearing from its
// peer before it is closed. A client that vanishes without closing (a laptop
// lid shut, a network dropped) is noticed within this time, which ends its
// attachments. Both peers advertise it and QUIC uses the smaller of the two,
// so a daemon applies it to older clients too.
const DeadPeerTimeout = 15 * time.Second

// QUIC carries the agent protocol remotely: one QUIC connection per client,
// one bidirectional QUIC stream per request or attach session. Each QUIC
// stream is a Stream, so nothing above this package changes.
//
// Both peers authenticate with pinned keys inside TLS 1.3: the daemon only
// accepts client keys it has authorized, and a client only accepts the
// daemon key it has pinned. Certificates are self-signed and never checked
// against a CA.
func quicConfig(idleTimeout time.Duration) *quic.Config {
	return &quic.Config{
		// Keep idle-but-open connections (for example a long attach with
		// no output) alive, through NATs and firewalls too. Pinging three
		// times per idle timeout lets two pings be lost in a row.
		KeepAlivePeriod: idleTimeout / 3,
		MaxIdleTimeout:  idleTimeout,
		// Start at QUIC's minimum datagram size, not quic-go's 1280 bytes:
		// Tailscale's interface MTU is 1280 including the 28 bytes of IP and
		// UDP headers, so a 1280-byte datagram with Don't Fragment set never
		// leaves it and the handshake times out. Path MTU discovery still
		// raises the size where the path allows.
		InitialPacketSize: 1200,
		// Each request or attachment uses one stream; this bounds how
		// many a single client can have open at once.
		MaxIncomingStreams:    256,
		MaxIncomingUniStreams: -1,
		// Flow-control windows bound what a peer can make this end buffer
		// for data the application has not read yet: one stream at most
		// its stream window, the whole connection, however many of its
		// 256 streams are open, at most the connection window. A stream
		// starts at the initial window and grows towards the maximum only
		// while it is being read quickly; a stalled one stops growing.
		//
		// Measurements (BENCHMARKS.md): a 4 MiB stream window moves a
		// large transfer as fast as 16 MiB does at 20 ms round-trip time
		// and at about 23 MB/s at 100 ms, plenty for snapshots, history
		// and PTY output. A stalled stream takes only its own window of
		// the connection's, so the connection window starts at 8 fresh
		// stream windows (quic-go's 1.5 would let 2 stalled attachments
		// block every other stream) and is capped at 16 MiB, which is
		// therefore the most one client can make the daemon buffer.
		InitialStreamReceiveWindow:     512 << 10,
		MaxStreamReceiveWindow:         4 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     16 << 20,
		// Datagrams stay off (the default): nothing in the protocol is
		// replaceable enough to send unreliably; see BENCHMARKS.md.
	}
}

// QUICOptions configures a QUIC listener.
type QUICOptions struct {
	// Authorized decides whether a client key may connect. It is called
	// during every handshake, on every new stream, and periodically for
	// every open connection, so revoking a key also ends its live
	// connections, attachments included.
	Authorized func(fingerprint string) bool
	// OnConnect, if set, is told about each authenticated connection.
	OnConnect func(fingerprint string, remote net.Addr)
	// RecheckInterval is how often open connections are checked against
	// Authorized; zero means every 5 seconds.
	RecheckInterval time.Duration
	// IdleTimeout is how long a connection may go without hearing from
	// its client before it is closed; zero means DeadPeerTimeout.
	IdleTimeout time.Duration
}

// QUICListener yields the streams clients open on their QUIC connections.
//
// It owns one goroutine that accepts connections, one per connection that
// accepts its streams, and one that rechecks open connections against
// Authorized. All of them end when the listener is closed.
type QUICListener struct {
	l       *quic.Listener
	opts    QUICOptions
	streams chan Stream
	ctx     context.Context
	cancel  context.CancelFunc

	mu sync.Mutex
	// conns maps each open connection to what is known about it.
	conns map[*quic.Conn]*connEntry
}

// connEntry is the listener's record of one open client connection.
type connEntry struct {
	fingerprint string
	connectedAt time.Time
	// opened counts the streams the client has opened; open is how many
	// are open now. Each stream decrements open once, when it is closed.
	opened atomic.Uint64
	open   atomic.Int64
}

// ConnectionInfo describes one open client connection, for status output.
type ConnectionInfo struct {
	Fingerprint   string
	Remote        string
	ConnectedAt   time.Time
	StreamsOpened uint64
	StreamsOpen   int64
	// RTT is QUIC's smoothed round-trip time estimate.
	RTT                      time.Duration
	BytesSent, BytesReceived uint64
	PacketsSent, PacketsLost uint64
}

// Connections describes the open client connections, oldest first.
func (q *QUICListener) Connections() []ConnectionInfo {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]ConnectionInfo, 0, len(q.conns))
	for conn, e := range q.conns {
		stats := conn.ConnectionStats()
		out = append(out, ConnectionInfo{
			Fingerprint:   e.fingerprint,
			Remote:        conn.RemoteAddr().String(),
			ConnectedAt:   e.connectedAt,
			StreamsOpened: e.opened.Load(),
			StreamsOpen:   e.open.Load(),
			RTT:           stats.SmoothedRTT,
			BytesSent:     stats.BytesSent,
			BytesReceived: stats.BytesReceived,
			PacketsSent:   stats.PacketsSent,
			PacketsLost:   stats.PacketsLost,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

// ListenQUIC listens for QUIC connections on addr (a UDP host:port) with the
// given identity.
func ListenQUIC(addr string, id *Identity, opts QUICOptions) (*QUICListener, error) {
	if opts.Authorized == nil {
		return nil, errors.New("QUIC listener needs an Authorized check")
	}
	if opts.RecheckInterval <= 0 {
		opts.RecheckInterval = 5 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DeadPeerTimeout
	}
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		NextProtos:   []string{ALPN},
		// Any certificate is requested, and then judged by its key's
		// fingerprint alone.
		ClientAuth: tls.RequireAnyClientCert,
		// A resumed session skips certificate verification, so a revoked
		// client holding a ticket could come back without being checked.
		// A full handshake costs one round trip; always do one.
		SessionTicketsDisabled: true,
		// VerifyConnection runs on every handshake (VerifyPeerCertificate
		// would not run on a resumed one).
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client sent no certificate")
			}
			fp := Fingerprint(cs.PeerCertificates[0])
			if !opts.Authorized(fp) {
				return fmt.Errorf("client key %s is not authorized", fp)
			}
			return nil
		},
	}
	l, err := quic.ListenAddr(addr, tlsConf, quicConfig(opts.IdleTimeout))
	if err != nil {
		return nil, fmt.Errorf("failed to listen for QUIC on %s: %w", addr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &QUICListener{
		l:       l,
		opts:    opts,
		streams: make(chan Stream),
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[*quic.Conn]*connEntry),
	}
	go q.acceptConns()
	go q.recheck()
	return q, nil
}

// acceptConns runs until the listener closes, starting one goroutine per
// connection. If accepting fails for any other reason the listener is
// closed too, so Accept reports it rather than waiting forever.
func (q *QUICListener) acceptConns() {
	defer q.Close()
	for {
		conn, err := q.l.Accept(q.ctx)
		if err != nil {
			return
		}
		fp, err := leafFingerprintOf(conn)
		if err != nil {
			conn.CloseWithError(0, "no client certificate")
			continue
		}
		entry := &connEntry{fingerprint: fp, connectedAt: time.Now()}
		if !q.track(conn, entry) {
			conn.CloseWithError(0, "agentd is shutting down")
			return
		}
		if q.opts.OnConnect != nil {
			q.opts.OnConnect(fp, conn.RemoteAddr())
		}
		go q.acceptStreams(conn, entry)
	}
}

// acceptStreams hands each stream a client opens to Accept, after checking
// that the client's key is still authorized. The channel is unbuffered: a
// client cannot get ahead of the daemon accepting its streams, and QUIC's
// stream limit bounds what it can have open. It ends when the connection or
// the listener closes.
func (q *QUICListener) acceptStreams(conn *quic.Conn, entry *connEntry) {
	defer q.untrack(conn)
	for {
		s, err := conn.AcceptStream(q.ctx)
		if err != nil {
			return
		}
		if !q.opts.Authorized(entry.fingerprint) {
			s.CancelRead(0)
			s.CancelWrite(0)
			conn.CloseWithError(0, "client key revoked")
			return
		}
		entry.opened.Add(1)
		entry.open.Add(1)
		stream := &quicStream{Stream: s, onClose: func() { entry.open.Add(-1) }}
		select {
		case q.streams <- stream:
		case <-q.ctx.Done():
			// Never handed out, so never closed by its handler.
			entry.open.Add(-1)
			s.CancelRead(0)
			s.CancelWrite(0)
			return
		}
	}
}

// recheck closes connections whose key has been revoked since they
// connected, ending their attachments too. It runs until the listener
// closes.
func (q *QUICListener) recheck() {
	ticker := time.NewTicker(q.opts.RecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-ticker.C:
		}
		q.mu.Lock()
		open := make(map[*quic.Conn]string, len(q.conns))
		for conn, e := range q.conns {
			open[conn] = e.fingerprint
		}
		q.mu.Unlock()
		// Authorized reads a file, so it is called outside the lock, once
		// per key rather than per connection.
		verdict := map[string]bool{}
		for conn, fp := range open {
			ok, seen := verdict[fp]
			if !seen {
				ok = q.opts.Authorized(fp)
				verdict[fp] = ok
			}
			if !ok {
				conn.CloseWithError(0, "client key revoked")
			}
		}
	}
}

func (q *QUICListener) track(conn *quic.Conn, e *connEntry) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil {
		return false
	}
	q.conns[conn] = e
	return true
}

func (q *QUICListener) untrack(conn *quic.Conn) {
	q.mu.Lock()
	delete(q.conns, conn)
	q.mu.Unlock()
}

func (q *QUICListener) Accept() (Stream, error) {
	select {
	case s := <-q.streams:
		return s, nil
	case <-q.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops accepting and closes every client connection. It is safe to
// call more than once.
func (q *QUICListener) Close() error {
	q.mu.Lock()
	q.cancel()
	conns := q.conns
	q.conns = map[*quic.Conn]*connEntry{}
	q.mu.Unlock()
	for conn := range conns {
		conn.CloseWithError(0, "agentd is shutting down")
	}
	return q.l.Close()
}

func (q *QUICListener) Addr() string { return q.l.Addr().String() }

// QUICClient is one client connection to a daemon.
type QUICClient struct {
	conn *quic.Conn
}

// DialTimeout bounds DialQUIC and ProbeFingerprint across all the addresses
// they try.
const DialTimeout = 10 * time.Second

// KeyChangedError means the daemon presented a key other than the pinned
// one: it was reinstalled, or something is impersonating it.
type KeyChangedError struct {
	Pinned, Presented string
}

func (e *KeyChangedError) Error() string {
	return fmt.Sprintf("daemon key %s does not match the pinned key %s", e.Presented, e.Pinned)
}

// DialQUIC connects to a daemon at addr, authenticating with id and
// refusing any daemon whose key does not match pinned, with a
// *KeyChangedError. A host name may resolve to several addresses
// (localhost is often ::1 and 127.0.0.1, only one of them listening), so
// each is tried in turn, each with an equal share of the time left.
func DialQUIC(ctx context.Context, addr string, id *Identity, pinned string) (*QUICClient, error) {
	conn, _, err := dialEach(ctx, addr, id, pinned)
	if err != nil {
		return nil, err
	}
	return &QUICClient{conn: conn}, nil
}

// ProbeFingerprint learns the key the daemon at addr presents, without
// pinning one, for `agent host add`. It works before this client is
// authorized: the daemon shows its certificate before judging the client's.
func ProbeFingerprint(ctx context.Context, addr string, id *Identity) (string, error) {
	conn, presented, err := dialEach(ctx, addr, id, "")
	if conn != nil {
		conn.CloseWithError(0, "")
	}
	if presented != "" {
		return presented, nil
	}
	return "", err
}

// IsKeyRefused reports whether err is the daemon refusing this client's
// key: a TLS alert, which QUIC carries as a crypto error. With TLS 1.3 the
// refusal may only surface on the first stream, after the dial succeeded.
func IsKeyRefused(err error) bool {
	var te *quic.TransportError
	return errors.As(err, &te) && te.Remote && te.ErrorCode.IsCryptoError()
}

// IsConnectionLost reports whether err means a QUIC connection, or an
// attempt to make one, failed in a way that a later attempt may not: the
// daemon went silent or unreachable, or closed the connection itself (to
// restart, say). A client may connect again after it. A refused or changed
// key, a protocol error, or this client closing the connection is not lost.
func IsConnectionLost(err error) bool {
	if err == nil || IsKeyRefused(err) {
		return false
	}
	var (
		idle      *quic.IdleTimeoutError
		handshake *quic.HandshakeTimeoutError
		reset     *quic.StatelessResetError
		app       *quic.ApplicationError
		netErr    net.Error
	)
	switch {
	case errors.As(err, &idle), errors.As(err, &handshake), errors.As(err, &reset):
		return true
	case errors.As(err, &app):
		return app.Remote
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr):
		return true
	}
	return false
}

// dialEach tries every address addr resolves to. With a pin it stops at the
// first connection, or at a daemon presenting another key; without one
// (pinned == "") it stops at the first daemon that presents a key, whether
// or not the handshake then completes. presented is the key the daemon
// showed on the last attempt.
func dialEach(ctx context.Context, addr string, id *Identity, pinned string) (conn *quic.Conn, presented string, err error) {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", err
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve %s: %w", host, err)
	}
	for i, ip := range ips {
		attemptCtx := ctx
		if deadline, ok := ctx.Deadline(); ok {
			var attemptCancel context.CancelFunc
			attemptCtx, attemptCancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(ips)-i))
			defer attemptCancel()
		}
		conn, presented, err = dialOne(attemptCtx, net.JoinHostPort(ip, port), id, pinned)
		switch {
		case err == nil:
			return conn, presented, nil
		case presented != "" && pinned == "":
			return nil, presented, err
		case presented != "" && presented != pinned:
			return nil, presented, &KeyChangedError{Pinned: pinned, Presented: presented}
		case ctx.Err() != nil:
			return nil, presented, err
		}
	}
	return nil, presented, err
}

func dialOne(ctx context.Context, addr string, id *Identity, pinned string) (*quic.Conn, string, error) {
	var presented string
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		NextProtos:   []string{ALPN},
		ServerName:   "agentd",
		// The daemon's certificate is self-signed; it is trusted only by
		// its pinned key fingerprint, checked below. TLS still checks the
		// handshake signature, so the daemon proves it holds that key.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := leafFingerprint(raw)
			if err != nil {
				return err
			}
			presented = fp
			if pinned != "" && fp != pinned {
				return &KeyChangedError{Pinned: pinned, Presented: fp}
			}
			return nil
		},
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, quicConfig(DeadPeerTimeout))
	return conn, presented, err
}

// OpenStream opens a stream for one request or attach session.
func (c *QUICClient) OpenStream(ctx context.Context) (Stream, error) {
	s, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStream{Stream: s}, nil
}

func (c *QUICClient) Close() error { return c.conn.CloseWithError(0, "") }

// quicStream adapts a QUIC stream to Stream. Closing a QUIC stream only ends
// its send side, which is exactly CloseWrite; Close also stops receiving.
// onClose, if set, runs once, at the first Close.
type quicStream struct {
	*quic.Stream
	onClose   func()
	closeOnce sync.Once
}

func (s *quicStream) CloseWrite() error { return s.Stream.Close() }

func (s *quicStream) Close() error {
	if s.onClose != nil {
		s.closeOnce.Do(s.onClose)
	}
	s.Stream.CancelRead(0)
	return s.Stream.Close()
}

func leafFingerprint(raw [][]byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("peer sent no certificate")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return "", fmt.Errorf("unreadable peer certificate: %w", err)
	}
	return Fingerprint(leaf), nil
}

func leafFingerprintOf(conn *quic.Conn) (string, error) {
	certs := conn.ConnectionState().TLS.PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("peer sent no certificate")
	}
	return Fingerprint(certs[0]), nil
}
