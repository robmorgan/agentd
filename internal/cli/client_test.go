package cli

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
)

// fakeDaemon listens on a runtime root's socket and answers every
// connection with reply (after reading a request header), or hangs up when
// reply is nil.
func fakeDaemon(t *testing.T, reply []byte) *client {
	t.Helper()
	// Socket paths are short-lived and limited in length; /tmp keeps them
	// short on macOS.
	dir, err := os.MkdirTemp("/tmp", "agcli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := paths.FromRoot(dir)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", p.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var header [16]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					return
				}
				io.CopyN(io.Discard, conn, int64(binary.LittleEndian.Uint32(header[12:16])))
				if reply != nil {
					conn.Write(reply)
				}
			}()
		}
	}()
	return &client{paths: p}
}

func TestRefusesDaemonThatHangsUp(t *testing.T) {
	c := fakeDaemon(t, nil)
	err := c.ensureCompatible()
	if err == nil || !strings.Contains(err.Error(), "does not speak agent protocol 1") || !strings.Contains(err.Error(), "agent daemon upgrade") {
		t.Fatalf("got %v", err)
	}
}

// A DaemonInfo response framed at another protocol version: the frame
// version alone is rejected.
func TestRefusesDaemonSpeakingAnotherProtocol(t *testing.T) {
	other := protocol.ProtocolVersion + 1
	payload := []byte{5, 0, 0, 0, '0', '.', '1', '.', '0'}
	payload = binary.LittleEndian.AppendUint16(payload, other)
	frame := binary.LittleEndian.AppendUint32(nil, 0x41474450)
	frame = binary.LittleEndian.AppendUint16(frame, other)
	frame = binary.LittleEndian.AppendUint16(frame, 101)
	frame = append(frame, 0, 0, 0, 0)
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(payload)))
	c := fakeDaemon(t, append(frame, payload...))
	err := c.ensureCompatible()
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version `2`") || !strings.Contains(err.Error(), "does not speak agent protocol 1") {
		t.Fatalf("got %v", err)
	}
}

// Session commands against an incompatible daemon fail with the upgrade
// hint, and the CLI never touches state.db.
func TestSessionCommandsRefuseIncompatibleDaemon(t *testing.T) {
	c := fakeDaemon(t, nil)
	for range 4 {
		if err := c.prepareDaemon(""); err == nil || !strings.Contains(err.Error(), "agent daemon upgrade") {
			t.Fatalf("got %v", err)
		}
	}
	// `agent daemon info` still gets something to talk to.
	if err := c.prepareDaemon("info"); err != nil {
		t.Fatalf("daemon info: %v", err)
	}
	if _, err := os.Stat(c.paths.Database); !os.IsNotExist(err) {
		t.Fatalf("state.db: %v", err)
	}
}

// The daemon owns stale-socket cleanup (under agentd.lock), so a start
// that never answers leaves the socket and pid file alone, even when the
// pid file names a live process, and points at the daemon log.
func TestStartDaemonTimesOutWithoutTouchingSocketOrPIDFile(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "agcli-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	c := &client{paths: paths.FromRoot(dir)}
	c.paths.EnsureLayout()
	os.WriteFile(c.paths.PIDFile, []byte("1\n"), 0o600)
	os.WriteFile(c.paths.Socket, nil, 0o600)
	start := time.Now()
	err := c.startDaemon("/usr/bin/true", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for agentd to start") || !strings.Contains(err.Error(), filepath.Join(dir, "agentd.log")) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("took too long")
	}
	for _, path := range []string{c.paths.PIDFile, c.paths.Socket} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func TestRemoteDaemonIsNeverManagedFromHere(t *testing.T) {
	c := testClient(t)
	c.host = &transport.Host{Name: "dev", Address: "127.0.0.1:1", Fingerprint: "SHA256:x"}
	for _, verb := range []string{"restart", "upgrade"} {
		if err := c.prepareDaemon(verb); err == nil || !strings.Contains(err.Error(), "only manages the local daemon") {
			t.Errorf("%s: %v", verb, err)
		}
	}
	if err := c.prepareDaemon("info"); err != nil {
		t.Errorf("info: %v", err)
	}
}
