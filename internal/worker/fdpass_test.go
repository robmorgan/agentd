package worker

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The prototype of the handoff design that was not chosen: passing the PTY
// master to a separately started worker with SCM_RIGHTS (see handoff.go for
// why exec in place won). It is kept here, as a tested helper, because it is
// the building block for handing descriptors to an unrelated process, for
// example a future supervisor that outlives workers, and because it shows
// the kernel side works: the received descriptor is the same PTY, and the
// agent keeps running through the transfer.

// sendFiles sends msg with files' descriptors attached (SCM_RIGHTS). The
// receiver gets its own descriptors for the same open files; the sender's
// stay open until it closes them.
func sendFiles(conn *net.UnixConn, msg []byte, files ...*os.File) error {
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = rawFD(f)
	}
	n, oobn, err := conn.WriteMsgUnix(msg, syscall.UnixRights(fds...), nil)
	if err == nil && (n != len(msg) || oobn == 0) {
		err = errors.New("short write")
	}
	return err
}

// receiveFiles receives one message sent by sendFiles, with up to max
// descriptors, which it marks close-on-exec.
func receiveFiles(conn *net.UnixConn, max int) ([]byte, []*os.File, error) {
	buf := make([]byte, 64<<10)
	oob := make([]byte, syscall.CmsgSpace(max*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return nil, nil, errors.New("descriptors were truncated")
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, nil, err
	}
	var files []*os.File
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			return nil, nil, err
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "received"))
		}
	}
	return buf[:n], files, nil
}

func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
		t.Cleanup(func() { c.Close() })
	}
	return conns[0], conns[1]
}

// A PTY master passed over SCM_RIGHTS still reaches the running agent: the
// receiver writes input and reads the agent's output after the sender has
// closed its own descriptor.
func TestSCMRightsCarriesALivePTY(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `stty -echo; echo ready; while IFS= read -r l; do echo "got:$l"; done`)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	readUntil(t, ptmx, "ready")

	sender, receiver := socketPair(t)
	if err := sendFiles(sender, []byte("pty"), ptmx); err != nil {
		t.Fatal(err)
	}
	ptmx.Close()
	msg, files, err := receiveFiles(receiver, 4)
	if err != nil || string(msg) != "pty" || len(files) != 1 {
		t.Fatalf("received %q, %d files, %v", msg, len(files), err)
	}
	received := files[0]
	defer received.Close()

	if _, err := received.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, received, "got:hello")
	// The agent is still the sender's child: only this process (the
	// sender) can collect its exit status, which is why handoffs exec in
	// place instead.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("agent died in the transfer: %v", err)
	}
}

func readUntil(t *testing.T, f *os.File, want string) {
	t.Helper()
	var got bytes.Buffer
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for !strings.Contains(got.String(), want) {
			n, err := f.Read(buf)
			got.Write(buf[:n])
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading for %q: %v (got %q)", want, err, got.String())
		}
	case <-time.After(testTimeout):
		t.Fatalf("no %q from the PTY", want)
	}
}
