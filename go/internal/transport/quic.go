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
		// Each request or attachment uses one stream; this bounds how
		// many a single client can have open at once.
		MaxIncomingStreams:    256,
		MaxIncomingUniStreams: -1,
	}
}

// QUICOptions configures a QUIC listener.
type QUICOptions struct {
	// Authorized decides whether a client key may connect. It is called
	// during every handshake, so changes to the authorized set apply to new
	// connections without restarting the listener.
	Authorized func(fingerprint string) bool
	// OnConnect, if set, is told about each authenticated connection.
	OnConnect func(fingerprint string, remote net.Addr)
}

// QUICListener yields the streams clients open on their QUIC connections.
type QUICListener struct {
	l       *quic.Listener
	opts    QUICOptions
	streams chan Stream
	ctx     context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	conns map[*quic.Conn]struct{}
}

// ListenQUIC listens for QUIC connections on addr (a UDP host:port) with the
// given identity.
func ListenQUIC(addr string, id *Identity, opts QUICOptions) (*QUICListener, error) {
	if opts.Authorized == nil {
		return nil, errors.New("QUIC listener needs an Authorized check")
	}
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		NextProtos:   []string{ALPN},
		// Any certificate is requested, and then judged by its key's
		// fingerprint alone.
		ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := leafFingerprint(raw)
			if err != nil {
				return err
			}
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
		conns:   make(map[*quic.Conn]struct{}),
	}
	go q.acceptConns()
	return q, nil
}

// acceptConns runs until the listener closes, starting one goroutine per
// connection.
func (q *QUICListener) acceptConns() {
	for {
		conn, err := q.l.Accept(q.ctx)
		if err != nil {
			return
		}
		if !q.track(conn) {
			conn.CloseWithError(0, "agentd is shutting down")
			return
		}
		if q.opts.OnConnect != nil {
			if fp, err := leafFingerprintOf(conn); err == nil {
				q.opts.OnConnect(fp, conn.RemoteAddr())
			}
		}
		go q.acceptStreams(conn)
	}
}

// acceptStreams hands each stream a client opens to Accept. The channel is
// unbuffered: a client cannot get ahead of the daemon accepting its
// streams, and QUIC's stream limit bounds what it can have open. It ends
// when the connection or the listener closes.
func (q *QUICListener) acceptStreams(conn *quic.Conn) {
	defer q.untrack(conn)
	for {
		s, err := conn.AcceptStream(q.ctx)
		if err != nil {
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

func (q *QUICListener) track(conn *quic.Conn) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil {
		return false
	}
	q.conns[conn] = struct{}{}
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

// Close stops accepting and closes every client connection.
func (q *QUICListener) Close() error {
	q.mu.Lock()
	q.cancel()
	conns := q.conns
	q.conns = map[*quic.Conn]struct{}{}
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
