package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/robmorgan/agtd/internal/paths"
	"github.com/robmorgan/agtd/internal/protocol"
	"github.com/robmorgan/agtd/internal/session"
	"github.com/robmorgan/agtd/internal/transport"
)

// servePair serves `agtd remote pair`: it opens the pairing window, sends
// its code, and waits until a client pairs (and is authorized), the window
// closes, or the command goes away, which cancels the window. The window
// belongs to this stream, so an interrupted command never leaves one open.
func (s *Server) servePair(conn transport.Stream) error {
	fail := func(msg string) error {
		return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Error: &protocol.ErrorResponse{Message: msg}})
	}
	// An authorized remote client could already do anything a new one
	// could, but pairing is about trusting a machine, which is decided on
	// this one.
	if _, local := conn.(*net.UnixConn); !local {
		return fail("pairing can only be started on agtd's own machine")
	}
	remote := s.RemoteAddr()
	if remote == "" {
		if s.config.Remote.Listen == "" {
			return fail("remote access is off; turn it on with `agtd remote enable`")
		}
		return fail(fmt.Sprintf("remote access is not listening on %s: %s", s.config.Remote.Listen, s.remoteError()))
	}
	authorized := s.paths.AuthorizedClientsPath()
	w, err := s.pairing.Open(transport.PairingTTL, func(fp, name string) error {
		_, err := transport.Authorize(authorized, fp, name)
		return err
	})
	if err != nil {
		return fail(err.Error())
	}
	defer w.Close()
	if err := protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Pairing: &protocol.ManagementPairing{
		Code: w.Code, ExpiresAt: w.ExpiresAt, Remote: remote, Fingerprint: s.daemonID,
	}}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "agtd: pairing window open until %s\n", w.ExpiresAt.Format("15:04:05"))

	// The command sends nothing more; its stream ending (Ctrl-C) cancels
	// the window. This goroutine ends when the stream is closed, at the
	// latest by untrack once servePair returns.
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(gone)
	}()
	select {
	case <-w.Done():
	case <-gone:
		fmt.Fprintf(os.Stderr, "agtd: pairing cancelled\n")
		return nil
	case <-s.shutdown:
		return fail("agtd is shutting down")
	}
	r := w.Result()
	if r.Err != nil {
		fmt.Fprintf(os.Stderr, "agtd: pairing ended: %v\n", r.Err)
		// Someone guessing codes is worth a record; a window that expired
		// or was cancelled here is not.
		if errors.Is(r.Err, transport.ErrTooManyWrongCodes) {
			s.recordHostEvent(session.EventPairFailed, fmt.Sprintf("pairing %v, the last from %s", r.Err, hostOf(r.Remote)))
		}
		return fail(r.Err.Error())
	}
	fmt.Fprintf(os.Stderr, "agtd: paired with %s (%s) from %s; it is now authorized\n", r.Fingerprint, r.Name, r.Remote)
	s.recordHostEvent(session.EventPaired,
		fmt.Sprintf("%s paired from %s", transport.DescribeClient(r.Fingerprint, r.Name), hostOf(r.Remote)))
	return protocol.WriteManagementResponse(conn, &protocol.ManagementResponse{Paired: &protocol.ManagementPaired{
		Fingerprint: r.Fingerprint, Name: r.Name, Remote: r.Remote.String(),
	}})
}

// Pair asks the running daemon to open a pairing window, tells opened its
// code, and waits until a client pairs, the window closes, or ctx ends
// (which cancels the window).
func Pair(ctx context.Context, p *paths.AppPaths, opened func(*protocol.ManagementPairing)) (*protocol.ManagementPaired, error) {
	running, err := daemonRunning(p.LockPath())
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, errors.New("agtd is not running; start it with remote access on (`agtd remote enable`)")
	}
	conn, err := transport.DialUnix(p.Socket, workerDialTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Closing the stream is how the window is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(workerDialTimeout))
	if err := protocol.WriteManagementRequest(conn, &protocol.ManagementRequest{Pair: protocol.Empty}); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	resp, err := readPairResponse(ctx, r)
	if err != nil {
		return nil, err
	}
	if resp.Pairing == nil {
		return nil, errors.New("unexpected management response")
	}
	opened(resp.Pairing)
	conn.SetDeadline(time.Time{})
	resp, err = readPairResponse(ctx, r)
	if err != nil {
		return nil, err
	}
	if resp.Paired == nil {
		return nil, errors.New("unexpected management response")
	}
	return resp.Paired, nil
}

func readPairResponse(ctx context.Context, r io.Reader) (*protocol.ManagementResponse, error) {
	resp, err := protocol.ReadManagementResponse(r)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, err
	case resp == nil:
		return nil, errors.New("agtd closed the connection")
	case resp.Error != nil && strings.Contains(resp.Error.Message, "unknown daemon management request kind"):
		return nil, errors.New("the running agtd predates pairing; replace it with this version first (`agtd upgrade`)")
	case resp.Error != nil:
		return nil, errors.New(resp.Error.Message)
	}
	return resp, nil
}
