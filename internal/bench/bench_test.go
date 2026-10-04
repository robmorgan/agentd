package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/db"
)

// The bench runs a freshly built agentd as its daemon and workers.
var agentdBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentd-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agentdBin = filepath.Join(dir, "agentd")
	build := exec.Command("go", "build", "-o", agentdBin, "../../cmd/agentd")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building agentd for bench tests:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// logWriter collects the progress log and calls on for each line.
type logWriter struct {
	mu    sync.Mutex
	lines []string
	on    func(line string)
}

func (w *logWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	w.mu.Lock()
	w.lines = append(w.lines, line)
	w.mu.Unlock()
	if w.on != nil {
		w.on(line)
	}
	return len(p), nil
}

func (w *logWriter) root() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.lines {
		if root, ok := strings.CutPrefix(l, "runtime root "); ok {
			return root
		}
	}
	return ""
}

// processesUnder lists the processes whose command line mentions root:
// the daemon and the workers (whose --cwd is inside it).
func processesUnder(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, root) && !strings.Contains(line, "ps -axo") {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

func TestRunSmall(t *testing.T) {
	log := &logWriter{}
	r, err := Run(context.Background(), Options{
		Exe: agentdBin, Count: 3, OutputHeavy: 2, Attach: 2, Duration: 300 * time.Millisecond,
		FillLines: 2000, Parallel: 2, Log: log,
	})
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, strings.Join(log.lines, "\n"))
	}
	var out strings.Builder
	r.Print(&out)
	t.Logf("\n%s", out.String())

	root := log.root()
	if root == "" {
		t.Fatal("no runtime root logged")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("runtime root %s was not removed: %v", root, err)
	}
	if left := processesUnder(t, root); len(left) > 0 {
		t.Errorf("processes left behind:\n%s", strings.Join(left, "\n"))
	}

	if r.DaemonStartupMs <= 0 || r.CreateMs.N != 3 || r.IdleWorkers.Private.N != 3 || r.IdleAgents.FDs.N != 3 {
		t.Errorf("report = %+v", r)
	}
	if r.IdleWorkers.Goroutines.Min < 1 || r.IdleWorkers.GoHeap.Min <= 0 {
		t.Errorf("worker Go runtime = %+v", r.IdleWorkers)
	}
	if r.IdleSnapshot.Bytes.Min <= 0 || r.IdleSnapshot.AttachMs.N != 3 {
		t.Errorf("idle snapshot = %+v", r.IdleSnapshot)
	}
	if r.FullSnapshot == nil || r.FullSnapshot.ScrollbackRows == 0 || r.FullSnapshot.Bytes.Min <= r.IdleSnapshot.Bytes.Max {
		t.Errorf("full snapshot = %+v", r.FullSnapshot)
	}
	if r.Output == nil || r.Output.BytesPerSec <= 0 || r.Output.Sessions != 2 {
		t.Errorf("output = %+v", r.Output)
	}
	if f := r.FanOut; f == nil || f.Clients != 2 || f.PTYBytesPerSec <= 0 || f.ClientBytesPerSec.Min <= 0 || f.EchoMs.N == 0 {
		t.Errorf("fan-out = %+v", r.FanOut)
	}
	if d := r.Database; d.Rows != 3+1+1 || d.ListSessionsMs.N == 0 || d.InsertDBMs.N == 0 || d.FileBytes == 0 {
		t.Errorf("database = %+v", d)
	}
}

// TestInterruptedRunCleansUp stops a run once its sessions exist, as Ctrl-C
// would, and checks that every process it started is gone.
func TestInterruptedRunCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers, agents []int
	log := &logWriter{}
	log.on = func(line string) {
		if line != "measure idle sessions..." {
			return
		}
		store, err := db.Open(filepath.Join(log.root(), "state.db"))
		if err != nil {
			t.Error(err)
		} else if recs, err := store.ListSessions(); err != nil {
			t.Error(err)
		} else {
			for _, rec := range recs {
				workers = append(workers, int(*rec.WorkerPID))
				agents = append(agents, int(*rec.AgentPID))
			}
		}
		cancel()
	}
	_, err := Run(ctx, Options{Exe: agentdBin, Count: 4, Parallel: 4, FillLines: 0, Log: log})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want it cancelled", err)
	}
	if len(workers) != 4 {
		t.Fatalf("saw %d workers", len(workers))
	}
	root := log.root()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("runtime root %s was not removed: %v", root, err)
	}
	// Orphans are reaped by init, which may take a moment.
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range append(workers, agents...) {
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if left := processesUnder(t, root); len(left) > 0 {
		t.Errorf("processes left behind:\n%s", strings.Join(left, "\n"))
	}
}
