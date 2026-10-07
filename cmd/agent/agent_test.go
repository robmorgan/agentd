package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/transport"
	"github.com/robmorgan/agentd/internal/transport/transporttest"
)

// These tests drive the real agent and agentd binaries through real PTYs.

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir = dir
	for _, b := range []struct{ name, pkg, cgo string }{{"agentd", "../agentd", ""}, {"agent", ".", "0"}} {
		build := exec.Command("go", "build", "-o", filepath.Join(dir, b.name), b.pkg)
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if b.cgo != "" {
			build.Env = append(os.Environ(), "CGO_ENABLED="+b.cgo)
		}
		if err := build.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n", b.name, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// The CLI must build without cgo or Zig, and must never reach the daemon's
// state (state.db) itself: everything goes through the protocol.
func TestAgentStaysPureGoAndOffTheDaemonsState(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"internal/worker", "internal/daemon", "internal/db", "libghostty", "modernc.org/sqlite"} {
			if strings.Contains(dep, banned) {
				t.Errorf("agent depends on %s", dep)
			}
		}
	}
	build := exec.Command("go", "build", "-o", os.DevNull, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 build: %v\n%s", err, out)
	}
}

type env struct {
	t    *testing.T
	root string
	work string
}

func newEnv(t *testing.T, extraConfig string) *env {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir().
	root, err := os.MkdirTemp("/tmp", "agcli-")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	os.Mkdir(work, 0o755)
	config := "default_agent = \"sh\"\n\n[agents.sh]\ncommand = \"/bin/sh\"\nargs = []\n" + extraConfig
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, work: work}
	t.Cleanup(e.cleanup)
	return e
}

func (e *env) environ() []string {
	return append(os.Environ(), "AGENTD_DIR="+e.root, "AGENTD_BIN="+filepath.Join(binDir, "agentd"), "TERM=xterm-256color", "PS1=$ ")
}

// run runs a command without a terminal and returns its output.
func (e *env) run(bin string, args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(binDir, bin), args...)
	cmd.Env = e.environ()
	cmd.Dir = e.work
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustRun(bin string, args ...string) string {
	e.t.Helper()
	out, err := e.run(bin, args...)
	if err != nil {
		e.t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
	return out
}

// cleanup stops every session (their workers outlive the daemon) and then
// the daemon.
func (e *env) cleanup() {
	p := paths.FromRoot(e.root)
	if s, err := transport.DialUnix(p.Socket, time.Second); err == nil {
		s.SetDeadline(time.Now().Add(5 * time.Second))
		protocol.WriteRequest(s, &protocol.Request{ListSessions: protocol.Empty})
		if resp, _ := protocol.ReadResponse(s); resp != nil && resp.Sessions != nil {
			for _, rec := range *resp.Sessions {
				e.run("agent", "kill", "--rm", rec.SessionID)
			}
		}
		s.Close()
	}
	if s, err := transport.DialUnix(p.Socket, time.Second); err == nil {
		s.SetDeadline(time.Now().Add(5 * time.Second))
		protocol.WriteManagementRequest(s, &protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}})
		protocol.ReadManagementResponse(s)
		s.Close()
	}
	os.RemoveAll(e.root)
}

// term is agent running on a PTY, as it would in a terminal.
type term struct {
	t    *testing.T
	pty  *os.File
	tty  *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func (e *env) start(args ...string) *term {
	e.t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		e.t.Fatal(err)
	}
	pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 100})
	cmd := exec.Command(filepath.Join(binDir, "agent"), args...)
	cmd.Env = e.environ()
	cmd.Dir = e.work
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	tm := &term{t: e.t, pty: ptmx, tty: tty, cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(tm.done)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			tm.mu.Lock()
			tm.out.Write(buf[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	e.t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		ptmx.Close()
		tty.Close()
	})
	return tm
}

func (tm *term) output() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.out.String()
}

func (tm *term) write(s string) {
	if _, err := tm.pty.Write([]byte(s)); err != nil {
		tm.t.Fatal(err)
	}
}

// expect waits for want to appear in the output after what was already
// seen at mark.
func (tm *term) expect(mark int, want string) int {
	tm.t.Helper()
	return tm.expectWithin(mark, want, 10*time.Second)
}

func (tm *term) expectWithin(mark int, want string, timeout time.Duration) int {
	tm.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out := tm.output()
		if i := strings.Index(out[mark:], want); i >= 0 {
			return mark + i + len(want)
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("no %q in output:\n%q", want, out[mark:])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (tm *term) mark() int { return len(tm.output()) }

// waitErr waits for agent to exit and returns how it exited.
func (tm *term) waitErr() error {
	tm.t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- tm.cmd.Wait() }()
	select {
	case err := <-exited:
		return err
	case <-time.After(10 * time.Second):
		tm.t.Fatalf("agent did not exit:\n%q", tm.output())
		return nil
	}
}

// wait waits for agent to exit and checks it exited cleanly, leaving the
// terminal as it found it.
func (tm *term) wait() {
	tm.t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- tm.cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			tm.t.Fatalf("agent exited with %v:\n%q", err, tm.output())
		}
	case <-time.After(10 * time.Second):
		tm.t.Fatalf("agent did not exit:\n%q", tm.output())
	}
	flags, err := unix.FcntlInt(tm.tty.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		tm.t.Fatalf("agent left the terminal non-blocking (flags %#x, %v)", flags, err)
	}
	// The master reports the terminal's modes; the slave may already be
	// revoked now that its session leader has exited.
	attrs, err := unix.IoctlGetTermios(int(tm.pty.Fd()), ioctlGetTermios)
	if err != nil || attrs.Lflag&unix.ICANON == 0 || attrs.Lflag&unix.ECHO == 0 {
		tm.t.Fatalf("agent left the terminal in raw mode (%+v, %v)", attrs, err)
	}
}

const restoreSequence = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[?25h"

func TestAttachEndToEnd(t *testing.T) {
	e := newEnv(t, "")

	// agent new starts the daemon and attaches to the new session.
	tm := e.start("new", "demo")
	tm.expect(0, "\x1b]0;demo - agentd\x07")
	m := tm.expect(0, "attached to demo")
	tm.write("echo hi-$((1+2))\r")
	m = tm.expect(m, "hi-3")

	// A resize reaches the agent.
	pty.Setsize(tm.pty, &pty.Winsize{Rows: 40, Cols: 120})
	time.Sleep(200 * time.Millisecond)
	tm.write("stty size\r")
	m = tm.expect(m, "40 120")

	// Ctrl-\ detaches and puts the terminal back.
	tm.write("\x1c")
	tm.wait()
	if out := tm.output(); !strings.HasSuffix(out, restoreSequence+"\x1b]0;agentd\x07\x1b]2;agentd\x07") {
		t.Fatalf("detach did not restore the terminal:\n%q", out[max(len(out)-200, 0):])
	}

	// The session outlives the client.
	if out := e.mustRun("agent", "ls"); !strings.Contains(out, "demo") || !strings.Contains(out, "●") {
		t.Fatalf("ls after detach:\n%s", out)
	}

	// Reattaching replays the screen; the kitty form of Ctrl-\ detaches.
	tm = e.start("attach", "demo")
	m = tm.expect(0, "hi-3")
	tm.write("\x1b[92;5u")
	tm.wait()

	// The overlay opens over the session and closes back to it.
	tm = e.start("attach", "demo")
	m = tm.expect(0, "attached to demo")
	m = tm.expect(m, "hi-3")
	tm.write("\x19")
	m = tm.expect(m, "Switch Session")
	tm.write("\x1b[27u")
	m = tm.expect(m, "\x1b[2J\x1b[H")
	tm.write("echo after-overlay\r")
	m = tm.expect(m, "after-overlay")

	// Ctrl-] switches to the next running session.
	e.startAndDetach("two")
	tm.write("\x1d")
	m = tm.expect(m, "\x1b]0;two - agentd\x07")
	tm.write("exit\r")
	tm.expect(m, "session two finished (exit 0)")
	tm.wait()

	if out := e.mustRun("agent", "status", "two"); !strings.Contains(out, "status: exited") {
		t.Fatalf("status two:\n%s", out)
	}
	if out := e.mustRun("agent", "history", "demo"); !strings.Contains(out, "after-overlay") {
		t.Fatalf("history:\n%s", out)
	}
	e.mustRun("agent", "send-input", "demo", "echo", "from-send-input\r")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.mustRun("agent", "history", "demo"), "from-send-input") {
		if time.Now().After(deadline) {
			t.Fatal("send-input did not reach the session")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out := e.mustRun("agent", "kill", "--rm", "demo"); !strings.Contains(out, "terminated session demo") || !strings.Contains(out, "removed session demo") {
		t.Fatalf("kill --rm:\n%s", out)
	}
}

// startAndDetach creates a session and detaches from it at once.
func (e *env) startAndDetach(name string) {
	e.t.Helper()
	tm := e.start("new", name)
	tm.expect(0, "attached to "+name)
	tm.write("\x1c")
	tm.wait()
}

func TestPickerEndToEnd(t *testing.T) {
	e := newEnv(t, "")
	e.startAndDetach("demo")

	// Bare agent: filter, open the session's actions, attach.
	tm := e.start()
	m := tm.expect(0, "Type to filter")
	tm.write("de")
	m = tm.expect(m, "demo")
	tm.write("\r")
	m = tm.expect(m, "1. attach")
	tm.write("\r")
	m = tm.expect(m, "attached to demo")
	tm.write("\x1c")
	tm.wait()

	// Esc leaves without attaching.
	tm = e.start()
	tm.expect(0, "Type to filter")
	tm.write("\x1b")
	tm.wait()
	if strings.Contains(tm.output(), "attached to") {
		t.Fatal("Esc attached")
	}
}

func TestRemoteAttachEndToEnd(t *testing.T) {
	e := newEnv(t, "\n[remote]\nlisten = \"127.0.0.1:0\"\n")
	e.startAndDetach("demo")
	info := e.mustRun("agent", "daemon", "info")
	addr := ""
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(line, "remote: "); ok {
			addr = v
		}
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("remote not listening:\n%s", info)
	}
	daemonKey := strings.TrimSpace(e.mustRun("agentd", "remote", "id"))
	clientKey := strings.TrimSpace(e.mustRun("agent", "remote", "id"))

	// A wrong pin is refused; the right one is added without a prompt.
	if out, err := e.run("agent", "host", "add", "dev", addr, "--fingerprint", clientKey); err == nil || !strings.Contains(out, "not adding `dev`") {
		t.Fatalf("wrong fingerprint: %v\n%s", err, out)
	}
	e.mustRun("agent", "host", "add", "dev", addr, "--fingerprint", daemonKey)
	if out := e.mustRun("agent", "host", "ls", "--no-probe"); !strings.Contains(out, "dev\t"+addr+"\t"+daemonKey) {
		t.Fatalf("host ls --no-probe:\n%s", out)
	}
	if out := e.mustRun("agent", "host", "ls"); !regexp.MustCompile(`dev\s+` + regexp.QuoteMeta(addr) + `\s+unauthorized`).MatchString(out) {
		t.Fatalf("host ls before authorizing:\n%s", out)
	}

	// Until this machine is authorized the daemon refuses it, and the CLI
	// says how to fix that.
	if out, err := e.run("agent", "--host", "dev", "ls"); err == nil || !strings.Contains(out, "agentd remote authorize "+clientKey) {
		t.Fatalf("unauthorized: %v\n%s", err, out)
	}
	e.mustRun("agentd", "remote", "authorize", clientKey, "test")
	if out := e.mustRun("agent", "--host", "dev", "ls"); !strings.Contains(out, "demo") {
		t.Fatalf("remote ls:\n%s", out)
	}

	// Host health: this machine, the reachable host, and one that is not.
	// `agent host add` refuses a host it cannot reach, so write this one.
	if err := transport.AddHost(paths.FromRoot(e.root).HostsPath(), transport.Host{Name: "gone", Address: "127.0.0.1:9", Fingerprint: daemonKey}); err != nil {
		t.Fatal(err)
	}
	out := e.mustRun("agent", "hosts")
	for _, want := range []string{`local\s+\(this machine\)\s+online\s+\S+\s+1/1`, `dev\s+` + regexp.QuoteMeta(addr) + `\s+online\s+\S+\s+1/1\s+\d+\s+sh\s`, `gone\s+127\.0\.0\.1:9\s+offline`} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Fatalf("hosts: no %s in\n%s", want, out)
		}
	}
	info = e.mustRun("agent", "host", "info", "dev")
	for _, want := range []string{"fingerprint: " + daemonKey, "status: online", "agents: sh", "capabilities: control-stream"} {
		if !strings.Contains(info, want) {
			t.Fatalf("host info: no %q in\n%s", want, info)
		}
	}
	// Sessions across hosts: here "local" and "dev" are the same daemon,
	// so the same session shows twice, under one global id.
	if out := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(e.mustRun("agent", "ls", "--all"), ""); !regexp.MustCompile(`local\s+demo`).MatchString(out) || !regexp.MustCompile(`dev\s+demo`).MatchString(out) {
		t.Fatalf("ls --all:\n%s", out)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(e.mustRun("agent", "ls", "--all", "--json")), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue // stderr: the unreachable host
		}
		var v struct {
			Address  string `json:"address"`
			GlobalID string `json:"global_id"`
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("ls --all --json line %q: %v", line, err)
		}
		if !strings.HasPrefix(v.GlobalID, daemonKey+"/") {
			t.Fatalf("global id %q does not start with the daemon key", v.GlobalID)
		}
		ids = append(ids, v.GlobalID)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("global ids = %v", ids)
	}
	if out := e.mustRun("agent", "status", "dev/demo"); !strings.Contains(out, "host: dev") || !strings.Contains(out, "global_id: "+ids[0]) {
		t.Fatalf("status dev/demo:\n%s", out)
	}
	if out := e.mustRun("agent", "events", "--all"); !strings.Contains(out, "local/demo") || !strings.Contains(out, "dev/demo") {
		t.Fatalf("events --all:\n%s", out)
	}
	e.mustRun("agent", "host", "rm", "gone")

	// Placement picks the lighter host: both are this daemon here, so the
	// tie goes to this machine.
	tm := e.start("--host", "auto", "new", "--cwd", e.work, "placed")
	tm.expect(0, "placing the session on local")
	tm.expect(0, "attached to placed")
	tm.write("\x1c")
	tm.wait()
	// Events come over QUIC too, named by host.
	if out := e.mustRun("agent", "events", "dev/demo"); !strings.Contains(out, "dev/demo  info    created") {
		t.Fatalf("remote events:\n%s", out)
	}
	follow := e.start("--host", "dev", "events", "--follow", "--lines", "0")
	e.mustRun("agent", "--host", "dev", "send-input", "demo", `printf '\a'`+"\r")
	follow.expect(0, "dev/demo  action  bell")

	// Attach over QUIC with a host/session address.
	tm = e.start("attach", "dev/demo")
	m := tm.expect(0, "attached to demo")
	tm.write("echo over-quic\r")
	tm.expect(m, "over-quic")
	tm.write("\x1c")
	tm.wait()
}

// gitIn runs git in dir for test setup, standing in for an agent at work.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// What a session produced, locally and over QUIC: its diff, a diffstat,
// the git section of status, and downloadable artifacts.
func TestDiffAndArtifactsEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	e := newEnv(t, "\n[remote]\nlisten = \"127.0.0.1:0\"\n")
	gitIn(t, e.work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(e.work, "README"), []byte("hello\n"), 0o644)
	gitIn(t, e.work, "add", ".")
	gitIn(t, e.work, "commit", "-q", "-m", "initial")
	e.startAndDetach("demo")

	// The agent commits something, then leaves an edit and a new file.
	os.WriteFile(filepath.Join(e.work, "feature.go"), []byte("package feature\n"), 0o644)
	gitIn(t, e.work, "add", ".")
	gitIn(t, e.work, "commit", "-q", "-m", "add the feature")
	os.WriteFile(filepath.Join(e.work, "README"), []byte("hello\nworld\n"), 0o644)
	os.WriteFile(filepath.Join(e.work, "notes.txt"), []byte("todo\n"), 0o644)

	diff := e.mustRun("agent", "diff", "demo")
	for _, want := range []string{"+++ b/feature.go", "+world", "+++ b/notes.txt"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "\x1b[") {
		t.Fatalf("diff to a pipe is coloured:\n%q", diff)
	}
	if out := e.mustRun("agent", "diff", "--name-only", "demo"); out != "README\nfeature.go\nnotes.txt\n" {
		t.Fatalf("--name-only:\n%s", out)
	}
	if out := e.mustRun("agent", "diff", "--stat", "demo"); !strings.Contains(out, " feature.go | 1 +") || !strings.Contains(out, "3 files changed, 3 insertions(+)") {
		t.Fatalf("--stat:\n%s", out)
	}
	status := e.mustRun("agent", "status", "demo")
	for _, want := range []string{"git_branch: main", "git_commits: 1 since base", "add the feature (Agent,", "git_changed_files: 3 (1 added, 1 modified, 1 untracked), +3 -0"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status lacks %q:\n%s", want, status)
		}
	}
	if out := e.mustRun("agent", "artifacts", "demo"); !strings.Contains(out, "diff\tdiff\t-\t") || !strings.Contains(out, "patch\tpatch\t-\t1 commit since") || !strings.Contains(out, "history.vt\t") {
		t.Fatalf("artifacts:\n%s", out)
	}
	mbox := filepath.Join(e.root, "out.mbox")
	e.mustRun("agent", "artifact", "demo", "patch", "-o", mbox)
	if data, _ := os.ReadFile(mbox); !bytes.Contains(data, []byte("Subject: [PATCH] add the feature")) {
		t.Fatalf("patch artifact:\n%s", data)
	}
	if out, err := e.run("agent", "artifact", "demo", "nope", "-o", filepath.Join(e.root, "nope")); err == nil || !strings.Contains(out, "no artifact `nope`") {
		t.Fatalf("unknown artifact: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(e.root, "nope")); err == nil {
		t.Fatal("a failed download left a file behind")
	}

	// The same over QUIC, with --host and with a host/session address.
	info := e.mustRun("agent", "daemon", "info")
	addr := ""
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(line, "remote: "); ok {
			addr = v
		}
	}
	e.mustRun("agent", "host", "add", "dev", addr, "--fingerprint", strings.TrimSpace(e.mustRun("agentd", "remote", "id")))
	e.mustRun("agentd", "remote", "authorize", strings.TrimSpace(e.mustRun("agent", "remote", "id")), "test")
	if remote := e.mustRun("agent", "--host", "dev", "diff", "demo"); remote != diff {
		t.Fatalf("remote diff differs:\n%s", remote)
	}
	if out := e.mustRun("agent", "diff", "--name-only", "dev/demo"); out != "README\nfeature.go\nnotes.txt\n" {
		t.Fatalf("remote --name-only:\n%s", out)
	}
	if out := e.mustRun("agent", "artifact", "dev/demo", "patch"); !strings.Contains(out, "Subject: [PATCH] add the feature") {
		t.Fatalf("remote patch:\n%s", out)
	}
}

// A remote attachment survives the daemon going away: the CLI shows that it
// is reconnecting, attaches to the same session again when the daemon is
// back, and the detach key still works while it waits.
func TestRemoteAttachReconnects(t *testing.T) {
	// A fixed port, so the restarted daemon listens where the host points.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()
	e := newEnv(t, "\n[remote]\nlisten = \""+addr+"\"\n")
	e.startAndDetach("demo")
	e.mustRun("agent", "host", "add", "dev", addr, "--fingerprint", strings.TrimSpace(e.mustRun("agentd", "remote", "id")))
	e.mustRun("agentd", "remote", "authorize", strings.TrimSpace(e.mustRun("agent", "remote", "id")), "test")

	tm := e.start("attach", "dev/demo")
	m := tm.expect(0, "attached to demo")
	tm.write("export N=before; echo $N-1\r")
	m = tm.expect(m, "before-1")

	e.mustRun("agent", "daemon", "restart")
	m = tm.expect(m, "connection to dev lost, reconnecting")
	// The session's screen is repainted, and input reaches it again.
	m = tm.expect(m, "before-1")
	tm.write("echo $N-2\r")
	m = tm.expect(m, "before-2")

	// With the daemon gone for good, the CLI keeps retrying until the user
	// detaches.
	s, err := transport.DialUnix(paths.FromRoot(e.root).Socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(5 * time.Second))
	protocol.WriteManagementRequest(s, &protocol.ManagementRequest{Shutdown: &protocol.ManagementShutdown{Force: true}})
	protocol.ReadManagementResponse(s)
	s.Close()
	m = tm.expect(m, "connection to dev lost, reconnecting")
	tm.expect(m, "(attempt 2)")
	tm.write("\x1c")
	tm.wait()
	// Start the daemon again so cleanup can stop the session.
	e.mustRun("agent", "ls")
}

// A remote attachment that loses its connection reattaches only to the
// same incarnation of its session: if the session was removed and another
// started under its name while the client was away, the client says so
// and stops rather than silently attaching to the new one.
func TestRemoteReattachRefusesARecreatedSession(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()
	e := newEnv(t, "\n[remote]\nlisten = \""+addr+"\"\n")
	e.startAndDetach("demo")
	// The client reaches the daemon through a relay that stands in for
	// the network.
	relay := transporttest.NewRelay(t, addr)
	e.mustRun("agent", "host", "add", "dev", relay.Addr(), "--fingerprint", strings.TrimSpace(e.mustRun("agentd", "remote", "id")))
	e.mustRun("agentd", "remote", "authorize", strings.TrimSpace(e.mustRun("agent", "remote", "id")), "test")
	if out := e.mustRun("agent", "status", "demo"); !strings.Contains(out, "uid: ") {
		t.Fatalf("status shows no uid:\n%s", out)
	}

	tm := e.start("attach", "dev/demo")
	m := tm.expect(0, "attached to demo")
	tm.write("echo first-$((1+1))\r")
	m = tm.expect(m, "first-2")

	// The connection is lost, and while the client cannot get back in the
	// session is replaced by another of the same name.
	relay.BlockNew(true)
	e.mustRun("agent", "daemon", "restart")
	m = tm.expect(m, "connection to dev lost, reconnecting")
	e.mustRun("agent", "kill", "--rm", "demo")
	e.startAndDetach("demo")
	relay.BlockNew(false)

	tm.expectWithin(m, "is not the session you were attached to", 30*time.Second)
	if err := tm.waitErr(); err == nil {
		t.Fatal("agent exited cleanly after refusing to reattach")
	}
	if out := e.mustRun("agent", "attachments", "demo"); strings.Contains(out, "attach-") {
		t.Fatalf("the new session got an attachment:\n%s", out)
	}
}

// A bell in a session reaches `agent events --follow --notify` (and the
// user's terminal as a notification), shows in `agent ls` and `status`,
// and attaching acknowledges it. Following survives a daemon restart.
func TestEventsEndToEnd(t *testing.T) {
	e := newEnv(t, "")
	e.startAndDetach("demo")

	follow := e.start("events", "demo", "--follow", "--notify")
	m := follow.expect(0, "created")
	m = follow.expect(m, "started")

	// The shell echoes the command as typed; only running it rings.
	e.mustRun("agent", "send-input", "demo", `printf '\a'`+"\r")
	m = follow.expect(m, "action  bell          bell")
	m = follow.expect(m, "\a\x1b]9;agentd: demo: bell")

	ls := e.mustRun("agent", "ls")
	if !strings.Contains(ls, "⚠") || !strings.Contains(ls, "waiting") || !strings.Contains(ls, "ATTENTION") {
		t.Fatalf("ls:\n%s", ls)
	}
	status := e.mustRun("agent", "status", "demo")
	for _, want := range []string{"attention: action", "attention_summary: bell", "activity: waiting", "elapsed: "} {
		if !strings.Contains(status, want) {
			t.Fatalf("status lacks %q:\n%s", want, status)
		}
	}
	if out := e.mustRun("agent", "events", "--json", "--level", "action"); !strings.Contains(out, `"kind":"bell"`) || strings.Contains(out, `"kind":"created"`) {
		t.Fatalf("events --json:\n%s", out)
	}

	// Looking at the session acknowledges it.
	tm := e.start("attach", "demo")
	tm.expect(0, "attached to demo")
	tm.write("\x1c")
	tm.wait()
	m = follow.expect(m, "acknowledged")
	if ls := e.mustRun("agent", "ls"); strings.Contains(ls, "⚠") {
		t.Fatalf("ls after attach:\n%s", ls)
	}

	// The follower resumes after a restart without repeating anything.
	e.mustRun("agent", "daemon", "restart")
	m = follow.expect(m, "reconnecting")
	m = follow.expect(m, "recovered")
	if n := strings.Count(follow.output(), "  bell   "); n != 1 {
		t.Fatalf("bell shown %d times:\n%s", n, follow.output())
	}
}

// A program that reports its own status (OSC 7501) drives the activity,
// events, listings and status end to end: "Deploy?" and "Deployed" ride
// the reports in base64.
func TestProgramStatusEndToEnd(t *testing.T) {
	e := newEnv(t, "")
	e.startAndDetach("demo")

	follow := e.start("events", "demo", "--follow")
	m := follow.expect(0, "started")

	// The shell echoes the command as typed, which is harmless text; the
	// report reaches the terminal when printf runs.
	e.mustRun("agent", "send-input", "demo",
		`printf '\033]7501;state=blocked:kind=permission:app=claude-code:progress=40:msg=RGVwbG95Pw==\033\\'`+"\r")
	m = follow.expect(m, "action  blocked")
	m = follow.expect(m, "permission: Deploy?")

	ls := e.mustRun("agent", "ls")
	if !strings.Contains(ls, "⚠") || !strings.Contains(ls, "blocked") {
		t.Fatalf("ls:\n%s", ls)
	}
	status := e.mustRun("agent", "status", "demo")
	for _, want := range []string{"activity: blocked", "status_app: claude-code", "status_kind: permission",
		"status_progress: 40%", "status_msg: Deploy?", "attention_summary: permission: Deploy?"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status lacks %q:\n%s", want, status)
		}
	}
	if out := e.mustRun("agent", "ls", "--json"); !strings.Contains(out, `"status_kind":"permission"`) ||
		!strings.Contains(out, `"status_progress":40`) {
		t.Fatalf("ls --json:\n%s", out)
	}

	// The program finishes; the record replaces the blocked one.
	e.mustRun("agent", "send-input", "demo", `printf '\033]7501;state=done:msg=RGVwbG95ZWQ=\033\\'`+"\r")
	m = follow.expect(m, "notice  done")
	follow.expect(m, "Deployed")
	if status := e.mustRun("agent", "status", "demo"); !strings.Contains(status, "activity: done") {
		t.Fatalf("status after done:\n%s", status)
	}
}

// A local attachment survives `agent daemon upgrade`, which replaces the
// daemon and hands the session's worker over to the new binary while the
// shell keeps running, and `agent daemon restart`: both times the CLI
// attaches again by itself and the same shell answers.
func TestLocalAttachSurvivesUpgradeAndRestart(t *testing.T) {
	e := newEnv(t, "")
	tm := e.start("new", "demo")
	m := tm.expect(0, "attached to demo")
	tm.write("export N=before; echo $N-1\r")
	m = tm.expect(m, "before-1")

	out := e.mustRun("agent", "daemon", "upgrade")
	if !strings.Contains(out, "Session demo now runs") {
		t.Fatalf("upgrade did not hand the session off:\n%s", out)
	}
	// The daemon restarts first and the worker hands off right after, so
	// the CLI may reattach once or twice (the second time on the worker's
	// SessionRestarting), depending on timing; either way it repaints.
	m = tm.expect(m, "reconnecting")
	m = tm.expect(m, "before-1")
	tm.write("echo $N-2\r")
	m = tm.expect(m, "before-2")

	e.mustRun("agent", "daemon", "restart")
	m = tm.expect(m, "connection to agentd lost, reconnecting")
	m = tm.expect(m, "before-2")
	tm.write("echo $N-3\r")
	m = tm.expect(m, "before-3")
	tm.write("\x1c")
	tm.wait()
}
