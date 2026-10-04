package procstat

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestReadSelf(t *testing.T) {
	// Burn a little CPU so the CPU time is not zero.
	deadline := time.Now().Add(50 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ {
		_ = x * x
	}
	before := 0
	if p, err := Read(os.Getpid()); err == nil {
		before = p.FDs
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Read(os.Getpid())
	if err == ErrUnsupported {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", p)
	if p.RSS < 1<<20 || p.Threads < 1 || p.FDs < 3 || p.CPU() <= 0 {
		t.Fatalf("implausible: %+v", p)
	}
	if p.FDs != before+1 {
		t.Errorf("fds = %d after opening one more than %d", p.FDs, before)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if p.Private == 0 || p.Private > p.RSS*2 {
			t.Errorf("private = %d, rss %d", p.Private, p.RSS)
		}
	}
	// CPU time must be in nanoseconds, not Mach units: this process has
	// not run for longer than it has existed.
	if p.CPU() > time.Hour {
		t.Errorf("cpu = %s", p.CPU())
	}
}

func TestReadOtherProcess(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	// Until the child has exec'd, there is little to read; give it a moment.
	var p Process
	var err error
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		p, err = Read(cmd.Process.Pid)
		if err != nil || p.RSS > 0 || time.Now().After(deadline) {
			break
		}
	}
	if err == ErrUnsupported {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if p.RSS == 0 || p.Threads < 1 {
		t.Fatalf("implausible: %+v", p)
	}
	cmd.Process.Kill()
	cmd.Wait()
	if _, err := Read(cmd.Process.Pid); err == nil {
		t.Error("read a reaped process")
	}
}

func TestReadGoRuntime(t *testing.T) {
	g := ReadGoRuntime()
	t.Logf("%+v", g)
	if g.Goroutines < 1 || g.HeapBytes == 0 || g.RuntimeBytes < g.HeapBytes {
		t.Fatalf("implausible: %+v", g)
	}
}
