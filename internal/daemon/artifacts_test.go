package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// gitIn runs git in dir for test setup, standing in for an agent at work.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// initRepo turns the harness's working directory into a repository with
// one commit on main.
func (h *harness) initRepo() string {
	h.t.Helper()
	needGit(h.t)
	gitIn(h.t, h.cwd, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(h.cwd, "README"), []byte("hello\n"), 0o644)
	gitIn(h.t, h.cwd, "add", ".")
	gitIn(h.t, h.cwd, "commit", "-q", "-m", "initial")
	return gitIn(h.t, h.cwd, "rev-parse", "HEAD")
}

func (h *harness) gitState(id string) *session.GitState {
	h.t.Helper()
	resp := h.request(&protocol.Request{GetGitState: &protocol.SessionRef{SessionID: id}})
	if resp.GitState == nil {
		h.t.Fatalf("git state %s: %#v", id, resp.Error)
	}
	return resp.GitState
}

func (h *harness) artifacts(id string) map[string]session.Artifact {
	h.t.Helper()
	resp := h.request(&protocol.Request{ListArtifacts: &protocol.SessionRef{SessionID: id}})
	if resp.Artifacts == nil {
		h.t.Fatalf("artifacts %s: %#v", id, resp.Error)
	}
	out := map[string]session.Artifact{}
	for _, a := range *resp.Artifacts {
		out[a.Name] = a
	}
	return out
}

// artifactTimeout bounds a whole download in these tests.
const artifactTimeout = 60 * time.Second

// artifact downloads an artifact on a stream of its own, checking every
// chunk's size, and returns its bytes, the number of chunks, and the error
// that ended it, if any.
func (h *harness) artifact(id, name string) ([]byte, int, string) {
	h.t.Helper()
	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(artifactTimeout))
	if err := protocol.WriteRequest(conn, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: name}}); err != nil {
		h.t.Fatal(err)
	}
	return readArtifact(h.t, bufio.NewReader(conn))
}

func readArtifact(t *testing.T, r *bufio.Reader) ([]byte, int, string) {
	t.Helper()
	var data bytes.Buffer
	chunks := 0
	for {
		resp, err := protocol.ReadResponse(r)
		switch {
		case err != nil:
			t.Fatalf("after %d bytes: %v", data.Len(), err)
		case resp == nil:
			t.Fatalf("stream closed after %d bytes without an end", data.Len())
		case resp.ArtifactChunk != nil:
			if n := len(resp.ArtifactChunk.Data); n == 0 || n > protocol.ArtifactChunkSize {
				t.Fatalf("chunk of %d bytes", n)
			}
			data.Write(resp.ArtifactChunk.Data)
			chunks++
		case resp.EndOfStream != nil:
			return data.Bytes(), chunks, ""
		case resp.Error != nil:
			return data.Bytes(), chunks, resp.Error.Message
		default:
			t.Fatalf("unexpected response %#v", resp)
		}
	}
}

// checkGitArtifacts runs a session in a repository through what an agent
// produces: commits, staged and untracked files, then the session ending.
func checkGitArtifacts(t *testing.T, h *harness) {
	base := h.initRepo()
	id := h.mustCreate("producer")

	st := h.gitState(id)
	if !st.Repo || st.Base != base || st.BaseBranch != "main" || st.Branch != "main" || st.Head != base || st.CommitCount != 0 || len(st.Files) != 0 {
		t.Fatalf("state at start = %+v", st)
	}

	// The agent commits, then leaves work uncommitted.
	os.WriteFile(filepath.Join(h.cwd, "feature.go"), []byte("package feature\n"), 0o644)
	gitIn(t, h.cwd, "add", ".")
	gitIn(t, h.cwd, "commit", "-q", "-m", "add the feature")
	os.WriteFile(filepath.Join(h.cwd, "README"), []byte("hello\nworld\n"), 0o644)
	os.WriteFile(filepath.Join(h.cwd, "notes.txt"), []byte("todo\n"), 0o644)

	st = h.gitState(id)
	if st.CommitCount != 1 || len(st.Commits) != 1 || st.Commits[0].Subject != "add the feature" || st.Commits[0].Author != "Agent" {
		t.Fatalf("commits = %+v", st.Commits)
	}
	statuses := map[string]session.FileStatus{}
	for _, f := range st.Files {
		statuses[f.Path] = f.Status
	}
	if statuses["feature.go"] != session.FileAdded || statuses["README"] != session.FileModified || statuses["notes.txt"] != session.FileUntracked {
		t.Fatalf("files = %+v", st.Files)
	}

	arts := h.artifacts(id)
	for _, name := range []string{"diff", "patch", "history", "history.vt"} {
		if _, ok := arts[name]; !ok {
			t.Fatalf("artifacts = %+v, missing %s", arts, name)
		}
	}
	if arts["history"].Size != nil {
		t.Fatalf("a live session's history has a size: %+v", arts["history"])
	}

	diff, _, errMsg := h.artifact(id, "diff")
	for _, want := range []string{"+++ b/feature.go", "+world", "+++ b/notes.txt"} {
		if errMsg != "" || !bytes.Contains(diff, []byte(want)) {
			t.Fatalf("diff lacks %q (error %q):\n%s", want, errMsg, diff)
		}
	}
	patch, _, errMsg := h.artifact(id, "patch")
	if errMsg != "" || !bytes.Contains(patch, []byte("Subject: [PATCH] add the feature")) || bytes.Contains(patch, []byte("notes.txt")) {
		t.Fatalf("patch (error %q):\n%s", errMsg, patch)
	}
	if _, _, errMsg := h.artifact(id, "nope"); !strings.Contains(errMsg, "no artifact `nope`") {
		t.Fatalf("unknown artifact: %q", errMsg)
	}

	h.sendInput(id, "visible\n")
	h.eventually("output", func() bool { return strings.Contains(h.history(id), "got:visible") })
	live, _, errMsg := h.artifact(id, "history")
	if errMsg != "" || !bytes.Contains(live, []byte("got:visible")) {
		t.Fatalf("live history (error %q): %q", errMsg, live)
	}

	// Once the session has ended, its history is the saved log and its
	// git state is still there to read.
	h.sendInput(id, "done\n")
	h.eventually("session end", func() bool { return h.session(id).Status == session.StatusExited })
	arts = h.artifacts(id)
	if a := arts["history"]; a.Size == nil || *a.Size == 0 {
		t.Fatalf("saved history = %+v", a)
	}
	saved, _, errMsg := h.artifact(id, "history")
	if errMsg != "" || string(saved) != h.history(id) || uint64(len(saved)) != *arts["history"].Size {
		t.Fatalf("saved history artifact (error %q) differs from history", errMsg)
	}
	if st := h.gitState(id); st.CommitCount != 1 || len(st.Files) != 3 {
		t.Fatalf("state after exit = %+v", st)
	}
}

func TestGitStateAndArtifacts(t *testing.T) {
	checkGitArtifacts(t, newHarness(t))
}

func TestGitStateAndArtifactsOverQUIC(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	client := newClientIdentity(t)
	if _, err := transport.Authorize(h.paths.AuthorizedClientsPath(), client.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dialRemote(client); err != nil {
		t.Fatal(err)
	}
	checkGitArtifacts(t, h)
}

func TestSessionOutsideARepository(t *testing.T) {
	needGit(t)
	h := newHarness(t)
	id := h.mustCreate("plain")
	if st := h.gitState(id); st.Repo || st.Error != "" || st.Base != "" {
		t.Fatalf("state = %+v", st)
	}
	arts := h.artifacts(id)
	if _, ok := arts["diff"]; ok || len(arts) != 2 {
		t.Fatalf("artifacts = %+v", arts)
	}
	if _, _, errMsg := h.artifact(id, "diff"); !strings.Contains(errMsg, "does not run in a git repository") {
		t.Fatalf("diff: %q", errMsg)
	}
	// Turning the directory into a repository later still works: there is
	// no base, so the state is measured from HEAD.
	h.initRepo()
	os.WriteFile(filepath.Join(h.cwd, "README"), []byte("changed\n"), 0o644)
	if st := h.gitState(id); !st.Repo || st.Base != "" || st.CommitCount != 0 || len(st.Files) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if diff, _, errMsg := h.artifact(id, "diff"); errMsg != "" || !bytes.Contains(diff, []byte("+changed")) {
		t.Fatalf("diff (error %q): %s", errMsg, diff)
	}
	if _, _, errMsg := h.artifact(id, "patch"); !strings.Contains(errMsg, "no commits") {
		t.Fatalf("patch: %q", errMsg)
	}
	resp := h.request(&protocol.Request{GetGitState: &protocol.SessionRef{SessionID: "../etc"}})
	wantError(t, resp, "not found")
}

// On a control stream git state is an ordinary tagged request; an artifact
// is refused there, since it needs a stream of its own.
func TestGitStateOnTheControlStream(t *testing.T) {
	h := newHarness(t)
	h.initRepo()
	id := h.mustCreate("ctl")
	c := h.control()
	c.send(1, &protocol.Request{GetGitState: &protocol.SessionRef{SessionID: id}})
	c.send(2, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: "diff"}})
	c.send(3, &protocol.Request{ListArtifacts: &protocol.SessionRef{SessionID: id}})
	got := map[uint32]*protocol.Response{}
	for range 3 {
		reqID, resp := c.read()
		got[reqID] = resp
	}
	if got[1].GitState == nil || !got[1].GitState.Repo {
		t.Fatalf("git state = %#v", got[1])
	}
	wantError(t, got[2], "stream of its own")
	if got[3].Artifacts == nil {
		t.Fatalf("artifacts = %#v", got[3])
	}
}

// bigUntracked writes an untracked text file of about size bytes, so the
// diff artifact is at least that large.
func (h *harness) bigUntracked(size int) {
	h.t.Helper()
	line := "a line of agent output that makes the diff artifact large\n"
	data := bytes.Repeat([]byte(line), size/len(line)+1)
	if err := os.WriteFile(filepath.Join(h.cwd, "big.txt"), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func TestLargeArtifactIsChunked(t *testing.T) {
	h := newHarness(t)
	h.initRepo()
	h.bigUntracked(20 << 20)
	id := h.mustCreate("big")
	data, chunks, errMsg := h.artifact(id, "diff")
	if errMsg != "" || len(data) < 20<<20 || chunks < len(data)/protocol.ArtifactChunkSize {
		t.Fatalf("%d bytes in %d chunks, error %q", len(data), chunks, errMsg)
	}
	if !bytes.HasSuffix(data, []byte("+a line of agent output that makes the diff artifact large\n")) {
		t.Fatalf("diff ends with %q", data[len(data)-100:])
	}
}

// gitChildren lists the git processes this test process started (through
// the in-process daemon).
func gitChildren(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != strconv.Itoa(os.Getpid()) || filepath.Base(f[2]) != "git" {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		pids = append(pids, pid)
	}
	return pids
}

// A client that stops reading holds the transfer back: git stays blocked
// on its pipe instead of the daemon buffering the artifact, and the
// transfer completes intact once the client reads again. A client that
// goes away mid-transfer takes git down with it.
func TestStalledArtifactClientHoldsGitBack(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // where the daemon copies the index
	h := newHarness(t)
	h.initRepo()
	h.bigUntracked(32 << 20)
	id := h.mustCreate("stall")
	if pids := gitChildren(t); len(pids) != 0 {
		t.Fatalf("git already running: %v", pids)
	}

	conn := h.dial()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(artifactTimeout))
	protocol.WriteRequest(conn, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: "diff"}})
	// Not reading. If the daemon buffered, git would finish and exit.
	h.eventually("git started", func() bool { return len(gitChildren(t)) == 1 })
	time.Sleep(time.Second)
	if pids := gitChildren(t); len(pids) != 1 {
		t.Fatalf("git finished while its output was not being read (%v): the daemon buffered it", pids)
	}
	data, _, errMsg := readArtifact(t, bufio.NewReader(conn))
	if errMsg != "" || len(data) < 32<<20 {
		t.Fatalf("after the stall: %d bytes, error %q", len(data), errMsg)
	}
	h.eventually("git exit", func() bool { return len(gitChildren(t)) == 0 })

	// Disconnect mid-transfer.
	conn2 := h.dial()
	protocol.WriteRequest(conn2, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: "diff"}})
	r := bufio.NewReader(conn2)
	if resp, err := protocol.ReadResponse(r); err != nil || resp.ArtifactChunk == nil {
		t.Fatalf("first chunk: %#v %v", resp, err)
	}
	h.eventually("git started", func() bool { return len(gitChildren(t)) == 1 })
	conn2.Close()
	h.eventually("git killed after the client left", func() bool { return len(gitChildren(t)) == 0 })
	h.eventually("index copy removed", func() bool {
		left, _ := os.ReadDir(tmp)
		return len(left) == 0
	})
}

// percentiles returns the 50th and 99th percentile and the maximum.
func percentiles(d []time.Duration) (p50, p99, max time.Duration) {
	s := slices.Clone(d)
	slices.Sort(s)
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return at(0.50), at(0.99), s[len(s)-1]
}

// Over one QUIC connection, a large artifact transfer on its own stream
// does not hold up an interactive attachment: input keeps echoing promptly
// while the client stalls the transfer and while it pulls it at full
// speed. The numbers are logged (go test -v -run InteractiveDuring).
func TestInteractiveDuringLargeArtifactOverQUIC(t *testing.T) {
	h := newHarnessWithConfig(t, remoteConfig)
	client := newClientIdentity(t)
	if _, err := transport.Authorize(h.paths.AuthorizedClientsPath(), client.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	qc, err := h.dialRemote(client)
	if err != nil {
		t.Fatal(err)
	}
	h.initRepo()
	const size = 48 << 20
	h.bigUntracked(size)
	id := h.mustCreate("interactive")
	att := h.attach(id)

	n := 0
	echo := func() time.Duration {
		n++
		marker := fmt.Sprintf("ping-%d", n)
		start := time.Now()
		att.input(marker + "\n")
		att.expectOutput("got:" + marker)
		return time.Since(start)
	}
	measure := func(rounds int) []time.Duration {
		var out []time.Duration
		for range rounds {
			out = append(out, echo())
			time.Sleep(10 * time.Millisecond)
		}
		return out
	}
	report := func(phase string, d []time.Duration) time.Duration {
		p50, p99, max := percentiles(d)
		t.Logf("%-28s echo round trips: n=%d p50=%v p99=%v max=%v", phase, len(d), p50.Round(10*time.Microsecond), p99.Round(10*time.Microsecond), max.Round(10*time.Microsecond))
		return max
	}
	// Generous: the machine is shared with other test suites.
	const bound = 2 * time.Second

	report("idle", measure(30))

	// A transfer whose reader has stalled holds its own stream's flow
	// control window, nothing else.
	stalled, err := qc.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	protocol.WriteRequest(stalled, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: "diff"}})
	time.Sleep(500 * time.Millisecond)
	if max := report("stalled transfer", measure(30)); max > bound {
		t.Errorf("echo took %v while a transfer was stalled", max)
	}
	stalled.Close()

	// A transfer pulled at full speed on the same connection.
	type result struct {
		bytes   int
		elapsed time.Duration
		err     string
	}
	done := make(chan result, 1)
	go func() {
		s, err := qc.OpenStream(t.Context())
		if err != nil {
			done <- result{err: err.Error()}
			return
		}
		defer s.Close()
		s.SetDeadline(time.Now().Add(artifactTimeout))
		start := time.Now()
		protocol.WriteRequest(s, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: "diff"}})
		r := bufio.NewReader(s)
		total := 0
		for {
			resp, err := protocol.ReadResponse(r)
			switch {
			case err != nil:
				done <- result{bytes: total, err: err.Error()}
				return
			case resp.ArtifactChunk != nil:
				total += len(resp.ArtifactChunk.Data)
			case resp.EndOfStream != nil:
				done <- result{bytes: total, elapsed: time.Since(start)}
				return
			default:
				done <- result{bytes: total, err: fmt.Sprintf("%#v", resp)}
				return
			}
		}
	}()
	// Echo round trips that started while the transfer was running.
	var during []time.Duration
	var res result
echoing:
	for {
		select {
		case res = <-done:
			break echoing
		default:
		}
		during = append(during, echo())
		time.Sleep(5 * time.Millisecond)
	}
	if res.err != "" || res.bytes < size {
		t.Fatalf("transfer: %+v", res)
	}
	if len(during) < 3 {
		t.Fatalf("only %d echoes during the transfer", len(during))
	}
	t.Logf("transfer: %.1f MiB in %v (%.0f MiB/s)", float64(res.bytes)/(1<<20), res.elapsed.Round(time.Millisecond), float64(res.bytes)/(1<<20)/res.elapsed.Seconds())
	if max := report("full-speed transfer", during); max > bound {
		t.Errorf("echo took %v during a transfer", max)
	}
}
