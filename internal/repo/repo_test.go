package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/robmorgan/agentd/internal/session"
)

// These tests run the real git against real temporary repositories.

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Println("skipping: git is not installed")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// run runs git in dir for test setup, with a fixed identity and no user or
// system configuration.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repository on main with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "keep.txt", "one\ntwo\nthree\n")
	write(t, dir, "edit.txt", "a\nb\nc\n")
	write(t, dir, "old-name.txt", "moving\nalong\n")
	write(t, dir, "gone.txt", "bye\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// snapshot fingerprints everything under .git, so a test can show that
// reading state changed nothing there.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(filepath.Join(dir, ".git"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, _ := d.Info()
		data, _ := os.ReadFile(path)
		fmt.Fprintf(&b, "%s %v %x\n", strings.TrimPrefix(path, dir), info.ModTime().UnixNano(), sha256.Sum256(data))
		return nil
	})
	return b.String()
}

func sameSnapshot(t *testing.T, before, after string) {
	t.Helper()
	if before == after {
		return
	}
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	for _, line := range b {
		if !slices.Contains(a, line) {
			t.Errorf("changed or new: %s", line)
		}
	}
	for _, line := range a {
		if !slices.Contains(b, line) {
			t.Errorf("changed or gone: %s", line)
		}
	}
	t.Fatal("reading git state changed the repository")
}

func fileByPath(st session.GitState, path string) *session.GitFile {
	for i := range st.Files {
		if st.Files[i].Path == path {
			return &st.Files[i]
		}
	}
	return nil
}

func TestProducedWorkSinceBase(t *testing.T) {
	dir := newRepo(t)
	base, ok := ProbeBase(ctx(t), dir)
	if !ok || base.Branch != "main" || !commitHash.MatchString(base.Commit) {
		t.Fatalf("ProbeBase = %+v, %v", base, ok)
	}

	// Two commits on top of the base...
	write(t, dir, "feature.go", "package x\n")
	run(t, dir, "add", "feature.go")
	run(t, dir, "commit", "-q", "-m", "add feature")
	write(t, dir, "feature.go", "package x\n\nfunc F() {}\n")
	run(t, dir, "commit", "-q", "-am", "implement feature")
	// ...then work not yet committed: staged, unstaged, untracked, a
	// rename made with plain mv, a binary file and a deletion.
	write(t, dir, "staged.txt", "s1\ns2\n")
	run(t, dir, "add", "staged.txt")
	write(t, dir, "edit.txt", "a\nB\nc\nd\n")
	write(t, dir, "notes/untracked.md", "# notes\nline\nline\n")
	os.Rename(filepath.Join(dir, "old-name.txt"), filepath.Join(dir, "new-name.txt"))
	os.WriteFile(filepath.Join(dir, "image.bin"), []byte{0x89, 'P', 'N', 'G', 0, 0, 1, 2}, 0o644)
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, dir, ".gitignore", "*.log\n")
	write(t, dir, "debug.log", "ignored\n")

	before := snapshot(t, dir)
	st := State(ctx(t), dir, base)
	sameSnapshot(t, before, snapshot(t, dir))

	if !st.Repo || st.Error != "" || st.Branch != "main" || st.Detached() || st.Base != base.Commit || st.BaseBranch != "main" || st.BaseMissing {
		t.Fatalf("state = %+v", st)
	}
	if real, _ := filepath.EvalSymlinks(dir); st.Root != real {
		t.Fatalf("root = %q, want %q", st.Root, real)
	}
	if st.Head != run(t, dir, "rev-parse", "HEAD") || st.MeasuredFrom() != base.Commit {
		t.Fatalf("head %q, measured from %q", st.Head, st.MeasuredFrom())
	}
	if st.CommitCount != 2 || len(st.Commits) != 2 || st.Commits[0].Subject != "implement feature" || st.Commits[1].Subject != "add feature" ||
		st.Commits[0].Author != "Ada" || time.Since(st.Commits[0].Time) > time.Hour {
		t.Fatalf("commits = %d %+v", st.CommitCount, st.Commits)
	}

	want := map[string]struct {
		status   session.FileStatus
		old      string
		add, del uint32
		binary   bool
	}{
		"feature.go":         {session.FileAdded, "", 3, 0, false},
		"staged.txt":         {session.FileAdded, "", 2, 0, false},
		"edit.txt":           {session.FileModified, "", 2, 1, false},
		"notes/untracked.md": {session.FileUntracked, "", 3, 0, false},
		"new-name.txt":       {session.FileRenamed, "old-name.txt", 0, 0, false},
		"image.bin":          {session.FileUntracked, "", 0, 0, true},
		"gone.txt":           {session.FileDeleted, "", 0, 1, false},
		".gitignore":         {session.FileUntracked, "", 1, 0, false},
	}
	if len(st.Files) != len(want) || st.FilesTruncated {
		t.Fatalf("files = %+v (truncated %v)", st.Files, st.FilesTruncated)
	}
	for path, w := range want {
		f := fileByPath(st, path)
		if f == nil || f.Status != w.status || f.OldPath != w.old || f.Additions != w.add || f.Deletions != w.del || f.Binary != w.binary {
			t.Errorf("%s = %+v, want %+v", path, f, w)
		}
	}
}

func TestNotARepository(t *testing.T) {
	dir := t.TempDir()
	st := State(ctx(t), dir, Base{})
	if st.Repo || st.Error != "" {
		t.Fatalf("state = %+v", st)
	}
	if _, ok := ProbeBase(ctx(t), dir); ok {
		t.Fatal("ProbeBase found a base outside a repository")
	}
	if _, err := Diff(ctx(t), dir, Base{}); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Diff = %v", err)
	}
	// Inside .git there is no work tree either.
	repo := newRepo(t)
	if st := State(ctx(t), filepath.Join(repo, ".git"), Base{}); st.Repo || st.Error != "" {
		t.Fatalf("state in .git = %+v", st)
	}
	// A recorded base is reported even when the directory is gone.
	gone := filepath.Join(t.TempDir(), "gone")
	st = State(ctx(t), gone, Base{Commit: strings.Repeat("a", 40), Branch: "main"})
	if st.Repo || !strings.Contains(st.Error, "no longer exists") || st.Base == "" || st.BaseBranch != "main" {
		t.Fatalf("state for a removed directory = %+v", st)
	}
}

func TestGitNotInstalled(t *testing.T) {
	old := gitPath
	gitPath = filepath.Join(t.TempDir(), "no-git-here")
	t.Cleanup(func() { gitPath = old })
	st := State(ctx(t), t.TempDir(), Base{})
	if st.Repo || !strings.Contains(st.Error, "not installed") {
		t.Fatalf("state = %+v", st)
	}
}

// A repository with no commits yet: everything is new, measured from the
// empty tree.
func TestUnbornBranch(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "trunk")
	write(t, dir, "staged.txt", "x\n")
	run(t, dir, "add", "staged.txt")
	write(t, dir, "loose.txt", "y\nz\n")
	if _, ok := ProbeBase(ctx(t), dir); ok {
		t.Fatal("ProbeBase recorded a base on an unborn branch")
	}
	st := State(ctx(t), dir, Base{})
	if !st.Repo || st.Error != "" || st.Branch != "trunk" || st.Head != "" || st.Detached() || st.CommitCount != 0 {
		t.Fatalf("state = %+v", st)
	}
	if f := fileByPath(st, "staged.txt"); f == nil || f.Status != session.FileAdded || f.Additions != 1 {
		t.Fatalf("staged.txt = %+v", f)
	}
	if f := fileByPath(st, "loose.txt"); f == nil || f.Status != session.FileUntracked || f.Additions != 2 {
		t.Fatalf("loose.txt = %+v", f)
	}
	s := readAll(t, mustDiff(t, dir, Base{}))
	if !strings.Contains(s, "+++ b/loose.txt") || !strings.Contains(s, "+++ b/staged.txt") {
		t.Fatalf("diff:\n%s", s)
	}
}

func TestDetachedHead(t *testing.T) {
	dir := newRepo(t)
	run(t, dir, "checkout", "-q", "--detach")
	base, ok := ProbeBase(ctx(t), dir)
	if !ok || base.Branch != "" {
		t.Fatalf("ProbeBase = %+v, %v", base, ok)
	}
	st := State(ctx(t), dir, base)
	if !st.Repo || st.Branch != "" || !st.Detached() || st.Upstream != "" || len(st.Files) != 0 {
		t.Fatalf("state = %+v", st)
	}
}

// Without a recorded base (a session from before bases were recorded) the
// state is measured from HEAD; a base that has vanished is reported.
func TestWithoutBase(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "edit.txt", "changed\n")
	for _, base := range []Base{{}, {Commit: strings.Repeat("0", 40)}} {
		st := State(ctx(t), dir, base)
		if st.CommitCount != 0 || len(st.Files) != 1 || st.Files[0].Path != "edit.txt" || st.BaseMissing != (base.Commit != "") || st.MeasuredFrom() != st.Head {
			t.Fatalf("base %q: state = %+v", base.Commit, st)
		}
	}
}

func TestUpstreamAheadBehind(t *testing.T) {
	origin := newRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	run(t, origin, "clone", "-q", origin, clone)
	write(t, origin, "upstream.txt", "u\n")
	run(t, origin, "add", ".")
	run(t, origin, "commit", "-q", "-m", "upstream work")
	run(t, clone, "fetch", "-q")
	for i := range 2 {
		write(t, clone, fmt.Sprintf("local%d.txt", i), "l\n")
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-q", "-m", "local work")
	}
	st := State(ctx(t), clone, Base{})
	if st.Upstream != "origin/main" || st.Ahead != 2 || st.Behind != 1 {
		t.Fatalf("upstream %q ahead %d behind %d", st.Upstream, st.Ahead, st.Behind)
	}
}

// Huge repositories cost a bounded read: lists are capped and say so.
func TestListsAreCapped(t *testing.T) {
	oldFiles, oldCommits := MaxFiles, MaxCommits
	MaxFiles, MaxCommits = 3, 2
	t.Cleanup(func() { MaxFiles, MaxCommits = oldFiles, oldCommits })

	dir := newRepo(t)
	base, _ := ProbeBase(ctx(t), dir)
	for i := range 4 {
		run(t, dir, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
	}
	for i := range 6 {
		write(t, dir, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	st := State(ctx(t), dir, base)
	if st.CommitCount != 4 || len(st.Commits) != 2 || st.Commits[0].Subject != "c3" {
		t.Fatalf("commits = %d %+v", st.CommitCount, st.Commits)
	}
	if len(st.Files) != 3 || !st.FilesTruncated {
		t.Fatalf("files = %+v truncated %v", st.Files, st.FilesTruncated)
	}
	for _, f := range st.Files {
		if f.Additions != 1 {
			t.Fatalf("line counts missing for the listed files: %+v", st.Files)
		}
	}

	old := maxUntracked
	maxUntracked = 2
	t.Cleanup(func() { maxUntracked = old })
	MaxFiles = 100
	if st := State(ctx(t), dir, base); len(st.Files) != 2 || !st.FilesTruncated {
		t.Fatalf("with capped untracked files: %+v truncated %v", st.Files, st.FilesTruncated)
	}
}

func mustDiff(t *testing.T, dir string, base Base) *Stream {
	t.Helper()
	s, err := Diff(ctx(t), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readAll(t *testing.T, s *Stream) string {
	t.Helper()
	data, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The diff artifact holds everything since the base, untracked files
// included, and applies cleanly to a checkout of the base.
func TestDiffAppliesToTheBase(t *testing.T) {
	dir := newRepo(t)
	base, _ := ProbeBase(ctx(t), dir)
	write(t, dir, "committed.txt", "c\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "commit")
	write(t, dir, "edit.txt", "a\nb\nc\nmore\n")
	write(t, dir, "brand new.txt", "hello\n")
	os.Rename(filepath.Join(dir, "old-name.txt"), filepath.Join(dir, "renamed.txt"))

	before := snapshot(t, dir)
	diff := readAll(t, mustDiff(t, dir, base))
	sameSnapshot(t, before, snapshot(t, dir))
	for _, want := range []string{"+++ b/committed.txt", "+more", "new file mode", "rename from old-name.txt", "rename to renamed.txt"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff lacks %q:\n%s", want, diff)
		}
	}

	check := filepath.Join(t.TempDir(), "check")
	run(t, dir, "worktree", "add", "-q", "--detach", check, base.Commit)
	patch := filepath.Join(t.TempDir(), "d.patch")
	os.WriteFile(patch, []byte(diff), 0o644)
	run(t, check, "apply", patch)
	for _, name := range []string{"committed.txt", "edit.txt", "brand new.txt", "renamed.txt"} {
		got, _ := os.ReadFile(filepath.Join(check, name))
		want, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s after apply = %q, want %q", name, got, want)
		}
	}
}

// The patch artifact is an mbox of the produced commits that `git am`
// replays onto the base.
func TestPatchReplaysCommits(t *testing.T) {
	dir := newRepo(t)
	base, _ := ProbeBase(ctx(t), dir)
	if _, err := Patch(ctx(t), dir, base); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("Patch with nothing committed = %v", err)
	}
	if _, err := Patch(ctx(t), dir, Base{}); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("Patch without a base = %v", err)
	}
	for i := range 3 {
		write(t, dir, fmt.Sprintf("p%d.txt", i), "patch\n")
		run(t, dir, "add", ".")
		run(t, dir, "commit", "-q", "-m", fmt.Sprintf("patch %d", i))
	}
	s, err := Patch(ctx(t), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	mbox := readAll(t, s)
	if n := strings.Count(mbox, "\nSubject: [PATCH "); n != 3 {
		t.Fatalf("%d patches in:\n%s", n, mbox)
	}
	check := filepath.Join(t.TempDir(), "check")
	run(t, dir, "worktree", "add", "-q", "--detach", check, base.Commit)
	path := filepath.Join(t.TempDir(), "p.mbox")
	os.WriteFile(path, []byte(mbox), 0o644)
	run(t, check, "am", "-q", path)
	if got, want := run(t, check, "rev-parse", "HEAD^{tree}"), run(t, dir, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("tree after am = %s, want %s", got, want)
	}
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// A reader that stops early (a client that went away) stops git, and the
// private index copy is removed.
func TestClosingADiffEarlyStopsGit(t *testing.T) {
	dir := newRepo(t)
	big := strings.Repeat("a fairly long line of text for the diff\n", 400_000) // ~16 MiB
	write(t, dir, "big.txt", big)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // where the index copy goes
	s := mustDiff(t, dir, Base{})
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatal(err)
	}
	// git cannot finish: nothing reads the rest and the pipe is full.
	time.Sleep(100 * time.Millisecond)
	pid := s.cmd.Process.Pid
	if processGone(pid) {
		t.Fatal("git finished without its output being read")
	}
	if copies, _ := os.ReadDir(tmp); len(copies) != 1 {
		t.Fatalf("index copies while streaming: %v", copies)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after an early stop = %v", err)
	}
	if !processGone(pid) {
		t.Fatal("git still running after Close")
	}
	if copies, _ := os.ReadDir(tmp); len(copies) != 0 {
		t.Fatalf("index copies left behind: %v", copies)
	}

	// Cancelling the context does the same.
	c, cancel := context.WithCancel(context.Background())
	s, err := Diff(c, dir, Base{})
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(s, buf)
	cancel()
	io.Copy(io.Discard, s)
	s.Close()
	if !processGone(s.cmd.Process.Pid) {
		t.Fatal("git still running after its context ended")
	}
}

func TestSummarize(t *testing.T) {
	dir := newRepo(t)
	base, _ := ProbeBase(ctx(t), dir)
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	s, err := Summarize(ctx(t), dir, base)
	if err != nil || s.Base != base.Commit || s.CommitCount != 1 || s.Root == "" {
		t.Fatalf("Summarize = %+v, %v", s, err)
	}
	if s, err := Summarize(ctx(t), dir, Base{}); err != nil || s.Base != "" || s.CommitCount != 0 {
		t.Fatalf("Summarize without base = %+v, %v", s, err)
	}
	if _, err := Summarize(ctx(t), t.TempDir(), Base{}); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Summarize outside a repository = %v", err)
	}
}

// git's bytes need not be UTF-8, but what goes on the wire must be.
func TestInvalidUTF8IsReplaced(t *testing.T) {
	dir := newRepo(t)
	base, _ := ProbeBase(ctx(t), dir)
	msg := filepath.Join(t.TempDir(), "msg")
	os.WriteFile(msg, []byte("caf\xe9 \xff\n"), 0o644)
	run(t, dir, "-c", "i18n.commitEncoding=", "commit", "-q", "--allow-empty", "-F", msg)
	st := State(ctx(t), dir, base)
	if len(st.Commits) != 1 || !utf8.ValidString(st.Commits[0].Subject) || !strings.HasPrefix(st.Commits[0].Subject, "caf") {
		t.Fatalf("commits = %+v", st.Commits)
	}
}

// The daemon's own GIT_* environment never leaks into the commands.
func TestIgnoresDaemonGitEnvironment(t *testing.T) {
	other := newRepo(t)
	dir := newRepo(t)
	write(t, dir, "edit.txt", "mine\n")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	st := State(ctx(t), dir, Base{})
	if real, _ := filepath.EvalSymlinks(dir); st.Root != real || len(st.Files) != 1 {
		t.Fatalf("state = %+v", st)
	}
}
