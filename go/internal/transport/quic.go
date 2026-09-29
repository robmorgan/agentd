package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN identifies the agent protocol inside QUIC's TLS handshake.
const ALPN = "agentd"

// QUIC carries the agent protocol remotely: one QUIC connection per client,
// one bidirectional QUIC stream per request or attach session. Each QUIC
// stream is a Stream, so nothing above this package changes.
//
// Both peers authenticate with pinned keys inside TLS 1.3: the daemon only
// accepts client keys it has authorized, and a client only accepts the
// daemon key it has pinned. Certificates are self-signed and never checked
// against a CA.
func quicConfig() *quic.Config {
	return &quic.Config{
		// Keep idle-but-open client connections (for example a long
		// attach with no output) alive through NATs and firewalls.
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
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
	// conns maps each open connection to its client key's fingerprint.
	conns map[*quic.Conn]string
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
	l, err := quic.ListenAddr(addr, tlsConf, quicConfig())
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
		conns:   make(map[*quic.Conn]string),
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
		if !q.track(conn, fp) {
			conn.CloseWithError(0, "agentd is shutting down")
			return
		}
		if q.opts.OnConnect != nil {
			q.opts.OnConnect(fp, conn.RemoteAddr())
		}
		go q.acceptStreams(conn, fp)
	}
}

// acceptStreams hands each stream a client opens to Accept, after checking
// that the client's key is still authorized. The channel is unbuffered: a
// client cannot get ahead of the daemon accepting its streams, and QUIC's
// stream limit bounds what it can have open. It ends when the connection or
// the listener closes.
func (q *QUICListener) acceptStreams(conn *quic.Conn, fp string) {
	defer q.untrack(conn)
	for {
		s, err := conn.AcceptStream(q.ctx)
		if err != nil {
			return
		}
		if !q.opts.Authorized(fp) {
			s.CancelRead(0)
			s.CancelWrite(0)
			conn.CloseWithError(0, "client key revoked")
			return
		}
		select {
		case q.streams <- &quicStream{s}:
		case <-q.ctx.Done():
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
		for conn, fp := range q.conns {
			open[conn] = fp
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

func (q *QUICListener) track(conn *quic.Conn, fp string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil {
		return false
	}
	q.conns[conn] = fp
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
	q.conns = map[*quic.Conn]string{}
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

// DialQUIC connects to a daemon at addr, authenticating with id and
// refusing any daemon whose key does not match serverFingerprint.
func DialQUIC(ctx context.Context, addr string, id *Identity, serverFingerprint string) (*QUICClient, error) {
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		NextProtos:   []string{ALPN},
		ServerName:   "agentd",
		// The daemon's certificate is self-signed; it is trusted only by
		// its pinned key fingerprint, checked below.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := leafFingerprint(raw)
			if err != nil {
				return err
			}
			if fp != serverFingerprint {
				return fmt.Errorf("daemon key %s does not match the pinned key %s", fp, serverFingerprint)
			}
			return nil
		},
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, quicConfig())
	if err != nil {
		return nil, err
	}
	return &QUICClient{conn: conn}, nil
}

// OpenStream opens a stream for one request or attach session.
func (c *QUICClient) OpenStream(ctx context.Context) (Stream, error) {
	s, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStream{s}, nil
}

func (c *QUICClient) Close() error { return c.conn.CloseWithError(0, "") }

// quicStream adapts a QUIC stream to Stream. Closing a QUIC stream only ends
// its send side, which is exactly CloseWrite; Close also stops receiving.
type quicStream struct{ *quic.Stream }

func (s *quicStream) CloseWrite() error { return s.Stream.Close() }

func (s *quicStream) Close() error {
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
