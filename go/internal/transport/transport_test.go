package transport

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func shortSocketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "agdt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir + "/s.sock"
}

func TestUnixListenerIsPrivateAndHalfCloses(t *testing.T) {
	path := shortSocketPath(t)
	l, err := ListenUnix(path, UnixOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info.Mode(), err)
	}

	accepted := make(chan Stream, 1)
	go func() {
		s, err := l.Accept()
		if err == nil {
			accepted <- s
		}
	}()
	client, err := DialUnix(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	// The client half-closes after its request; the server reads it to
	// EOF and can still answer.
	client.Write([]byte("request"))
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "request" {
		t.Fatalf("server read %q, %v", got, err)
	}
	server.Write([]byte("response"))
	server.CloseWrite()
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "response" {
		t.Fatalf("client read %q, %v", got, err)
	}
}

func TestUnixListenerKeepSocketOnClose(t *testing.T) {
	path := shortSocketPath(t)
	l, err := ListenUnix(path, UnixOptions{KeepSocketOnClose: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket removed despite KeepSocketOnClose: %v", err)
	}
	l2, err := ListenUnix(shortSocketPath(t), UnixOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()
	if _, err := os.Stat(l2.Addr()); !os.IsNotExist(err) {
		t.Fatalf("socket kept without KeepSocketOnClose: %v", err)
	}
}

// fakeListener returns scripted Accept results, then reports closed.
type fakeListener struct {
	mu      sync.Mutex
	results []error // nil means "yield a stream"
}

func (f *fakeListener) Accept() (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) == 0 {
		return nil, net.ErrClosed
	}
	err := f.results[0]
	f.results = f.results[1:]
	if err != nil {
		return nil, err
	}
	a, b := net.Pipe()
	b.Close()
	return pipeStream{a}, nil
}
func (f *fakeListener) Close() error { return nil }
func (f *fakeListener) Addr() string { return "fake" }

type pipeStream struct{ net.Conn }

func (p pipeStream) CloseWrite() error { return p.Close() }

// A temporary Accept failure (e.g. EMFILE) must not end the accept loop, and
// Serve must return once the listener reports it is closed.
func TestServeRetriesTemporaryErrorsAndReturnsOnClose(t *testing.T) {
	l := &fakeListener{results: []error{errors.New("too many open files"), nil, nil}}
	var handlers sync.WaitGroup
	var count sync.Mutex
	handled := 0
	done := make(chan struct{})
	go func() {
		Serve(l, "test", &handlers, func(s Stream) {
			s.Close()
			count.Lock()
			handled++
			count.Unlock()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the listener closed")
	}
	handlers.Wait()
	if handled != 2 {
		t.Fatalf("handled %d streams, want 2", handled)
	}
}
