// Package repo reads the state of the git repository a session works in, for
// the daemon. agentd does not manage git: a session's directory may or may
// not be a repository, and agentd never changes one. This package only
// reads, by running the system git in the session's directory:
//
//   - with GIT_OPTIONAL_LOCKS=0 and core.fsmonitor off, so reading never
//     refreshes the index or starts a file system monitor in the agent's
//     repository;
//   - without a pager, colour, external diff drivers or textconv filters,
//     and with explicit flags for everything whose default a user's config
//     could change (rename detection, diff prefixes, relative paths), so
//     the output is the same on every machine;
//   - with every GIT_* variable of the daemon's own environment removed,
//     so a daemon started from inside a repository never reads another;
//   - in its own process group, killed as a group when its context ends,
//     so a client that goes away never leaves git (or anything git started)
//     running.
//
// Untracked files are part of what an agent produced, but `git diff` does
// not show them. To include them without touching the repository's index,
// the package copies the index to a private temporary file, marks the
// untracked files intent-to-add there, and points git at the copy
// (GIT_INDEX_FILE), with a private object directory for the one object that
// writes (the empty blob). Nothing in the repository is written. Files
// moved without `git mv` then show up as renames, and binary files and
// gitattributes are handled by git itself.
//
// One-shot answers (State) are bounded: at most MaxCommits commits and
// MaxFiles files are listed, and git is stopped once that many have been
// read, so a huge repository costs no more than a bounded read. Streams
// (Diff, Patch) hand git's stdout to the caller unbuffered; the caller's
// pace is git's pace.
package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

var (
	// gitPath is the git binary. Tests point it elsewhere.
	gitPath = "git"
	// MaxCommits bounds the commits State lists (CommitCount still counts
	// them all).
	MaxCommits = 200
	// MaxFiles bounds the changed files State lists.
	MaxFiles = 1000
	// maxUntracked bounds the untracked files included in State and Diff.
	// Beyond it, State reports its files as truncated and Diff leaves the
	// rest out.
	maxUntracked = 20000
)

const (
	// maxOutput bounds the output of the commands whose whole answer is
	// read into memory (a hash, a branch name, a count).
	maxOutput = 64 << 10
	// maxRecord bounds one NUL-terminated record (a path, a commit line).
	maxRecord = 1 << 20
	// maxStderr bounds the error output kept to explain a failure.
	maxStderr = 4 << 10
	// waitDelay bounds how long a killed git may hold its pipes open.
	waitDelay = 2 * time.Second
)

// ErrNotRepository means the directory is not inside a git work tree.
var ErrNotRepository = errors.New("not a git repository")

// Base is what a session recorded about its repository when it started.
type Base struct {
	// Commit is the commit HEAD pointed at, or "" when nothing was
	// recorded. Branch is the branch checked out then, or "" if detached.
	Commit, Branch string
}

var commitHash = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ProbeBase returns the commit and branch of the repository dir is in, for
// a session about to start there. ok is false unless dir is in a work tree
// whose HEAD has a commit.
func ProbeBase(ctx context.Context, dir string) (base Base, ok bool) {
	head, err := output(ctx, dir, nil, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if err != nil || !commitHash.MatchString(head) {
		return Base{}, false
	}
	branch, _ := output(ctx, dir, nil, nil, "symbolic-ref", "-q", "--short", "HEAD")
	return Base{Commit: head, Branch: branch}, true
}

// repo is a work tree as read at one moment.
type repo struct {
	root string
	// index is the path of the work tree's index file, objects its object
	// directory.
	index, objects string
	// head is the commit HEAD points at, "" on an unborn branch. Commands
	// use the hash rather than HEAD, so they agree even if the agent
	// commits meanwhile.
	head string
	// branch is the checked-out branch, "" when detached.
	branch string
}

func open(ctx context.Context, dir string) (*repo, error) {
	if info, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("working directory %s no longer exists", dir)
		}
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("working directory %s is not a directory", dir)
	}
	root, err := output(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		var gerr *commandError
		// Outside a repository, inside a .git directory and in a bare
		// repository there is no work tree to describe.
		if errors.As(err, &gerr) && gerr.code == 128 &&
			(strings.Contains(gerr.stderr, "not a git repository") || strings.Contains(gerr.stderr, "work tree")) {
			return nil, ErrNotRepository
		}
		return nil, err
	}
	r := &repo{root: root}
	if r.head, err = output(ctx, root, nil, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err != nil {
		if exitCode(err) != 1 {
			return nil, err
		}
		r.head = "" // an unborn branch
	}
	if r.branch, err = output(ctx, root, nil, nil, "symbolic-ref", "-q", "--short", "HEAD"); err != nil {
		if exitCode(err) != 1 {
			return nil, err
		}
		r.branch = "" // detached
	}
	paths, err := output(ctx, root, nil, nil, "rev-parse", "--git-path", "index", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	index, objects, ok := strings.Cut(paths, "\n")
	if !ok {
		return nil, fmt.Errorf("git rev-parse: unexpected output %q", paths)
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	r.index, r.objects = abs(index), abs(objects)
	return r, nil
}

// from resolves what changes are measured from: the base when it is still
// in the repository, else HEAD, else (on an unborn branch) the empty tree.
func (r *repo) from(ctx context.Context, base Base) (rev string, fromBase, baseMissing bool, err error) {
	if base.Commit != "" {
		if commitHash.MatchString(base.Commit) {
			if _, err := output(ctx, r.root, nil, nil, "rev-parse", "-q", "--verify", base.Commit+"^{commit}"); err == nil {
				return base.Commit, true, false, nil
			}
		}
		baseMissing = true
	}
	if r.head != "" {
		return r.head, false, baseMissing, nil
	}
	tree, err := output(ctx, r.root, nil, strings.NewReader(""), "hash-object", "-t", "tree", "--stdin")
	return tree, false, baseMissing, err
}

// State reads the git state of the repository dir is in. Failures are
// reported in the state (Error), since a session's git state is extra
// information that must never make a status request fail.
func State(ctx context.Context, dir string, base Base) session.GitState {
	st := session.GitState{Base: base.Commit, BaseBranch: base.Branch}
	if err := state(ctx, dir, base, &st); err != nil {
		st = session.GitState{Base: base.Commit, BaseBranch: base.Branch}
		if !errors.Is(err, ErrNotRepository) {
			st.Error = err.Error()
		}
	}
	return st
}

func state(ctx context.Context, dir string, base Base, st *session.GitState) error {
	r, err := open(ctx, dir)
	if err != nil {
		return err
	}
	st.Repo, st.Root, st.Head, st.Branch = true, text(r.root), r.head, text(r.branch)

	if r.branch != "" && r.head != "" {
		// No upstream is the common case, not an error.
		if up, err := output(ctx, r.root, nil, nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil && up != "" {
			st.Upstream = text(up)
			if counts, err := output(ctx, r.root, nil, nil, "rev-list", "--left-right", "--count", "@{upstream}..."+r.head); err == nil {
				behind, ahead, _ := strings.Cut(counts, "\t")
				st.Behind, st.Ahead = parseCount(behind), parseCount(ahead)
			}
		}
	}

	from, fromBase, baseMissing, err := r.from(ctx, base)
	if err != nil {
		return err
	}
	st.BaseMissing = baseMissing
	if fromBase && r.head != "" {
		if err := r.commits(ctx, from, st); err != nil {
			return err
		}
	}

	idx, err := r.tempIndex(ctx)
	if err != nil {
		return err
	}
	defer idx.remove()
	st.FilesTruncated = idx.truncated
	return r.files(ctx, from, idx, st)
}

// text makes git's bytes (paths, names, subjects) valid UTF-8 for the wire,
// replacing what is not; they are for people to read, not to address files.
func text(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

func parseCount(s string) uint32 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(n)
}

func (r *repo) commits(ctx context.Context, from string, st *session.GitState) error {
	span := from + ".." + r.head
	count, err := output(ctx, r.root, nil, nil, "rev-list", "--count", span)
	if err != nil {
		return err
	}
	st.CommitCount = parseCount(count)
	if st.CommitCount == 0 {
		return nil
	}
	recs, err := startRecords(ctx, r.root, nil, nil, "log", "-z", "--no-color", "--no-show-signature",
		"--format=%H%x1f%an%x1f%at%x1f%s", "-n", strconv.Itoa(MaxCommits), span)
	if err != nil {
		return err
	}
	for {
		rec, ok := recs.next()
		if !ok {
			break
		}
		fields := strings.SplitN(string(rec), "\x1f", 4)
		if len(fields) != 4 {
			recs.stop()
			return fmt.Errorf("git log: unexpected output %q", rec)
		}
		secs, _ := strconv.ParseInt(fields[2], 10, 64)
		st.Commits = append(st.Commits, session.GitCommit{
			Hash: fields[0], Author: text(fields[1]), Time: time.Unix(secs, 0).UTC(), Subject: text(fields[3]),
		})
	}
	return recs.wait()
}

// diffFlags pin everything about `git diff` that configuration could
// change.
var diffFlags = []string{
	"--no-color", "--no-ext-diff", "--no-textconv", "--find-renames", "--no-relative",
	"--src-prefix=a/", "--dst-prefix=b/", "--submodule=short",
}

func (r *repo) files(ctx context.Context, from string, idx *tempIndex, st *session.GitState) error {
	env := idx.env()
	names, err := startRecords(ctx, r.root, env, nil, append(append([]string{"diff", "-z", "--name-status"}, diffFlags...), from)...)
	if err != nil {
		return err
	}
	byPath := map[string]int{}
	for {
		rec, ok := names.next()
		if !ok {
			break
		}
		if len(st.Files) == MaxFiles {
			st.FilesTruncated = true
			names.stop()
			break
		}
		code := string(rec)
		f := session.GitFile{}
		if f.Status, err = parseStatus(code); err != nil {
			names.stop()
			return err
		}
		path, ok := names.next()
		if ok && (f.Status == session.FileRenamed || f.Status == session.FileCopied) {
			f.OldPath = text(string(path))
			path, ok = names.next()
		}
		if !ok {
			names.stop()
			return fmt.Errorf("git diff: truncated --name-status output")
		}
		raw := string(path)
		f.Path = text(raw)
		if f.Status == session.FileAdded && idx.untracked[raw] {
			f.Status = session.FileUntracked
		}
		byPath[raw] = len(st.Files)
		st.Files = append(st.Files, f)
	}
	if err := names.wait(); err != nil {
		return err
	}
	if len(st.Files) == 0 {
		return nil
	}

	// Line counts, in a second pass in the same order. git stops after
	// the files listed above, so a huge diff is never computed whole.
	counts, err := startRecords(ctx, r.root, env, nil, append(append([]string{"diff", "-z", "--numstat"}, diffFlags...), from)...)
	if err != nil {
		return err
	}
	for seen := 0; seen < len(st.Files); seen++ {
		rec, ok := counts.next()
		if !ok {
			break
		}
		add, rest, _ := strings.Cut(string(rec), "\t")
		del, path, _ := strings.Cut(rest, "\t")
		if path == "" {
			// A rename: the old and new paths follow as records.
			counts.next()
			p, _ := counts.next()
			path = string(p)
		}
		i, ok := byPath[path]
		if !ok {
			continue
		}
		f := &st.Files[i]
		if add == "-" {
			f.Binary = true
			continue
		}
		f.Additions, f.Deletions = parseCount(add), parseCount(del)
	}
	if st.FilesTruncated {
		// The rest are files that were not listed.
		counts.stop()
	}
	return counts.wait()
}

func parseStatus(code string) (session.FileStatus, error) {
	if code == "" {
		return "", errors.New("git diff: empty status")
	}
	switch code[0] {
	case 'A':
		return session.FileAdded, nil
	case 'M':
		return session.FileModified, nil
	case 'D':
		return session.FileDeleted, nil
	case 'R':
		return session.FileRenamed, nil
	case 'C':
		return session.FileCopied, nil
	case 'T':
		return session.FileTypeChanged, nil
	case 'U':
		return session.FileUnmerged, nil
	}
	return "", fmt.Errorf("git diff: unknown status %q", code)
}

// tempIndex is a private copy of a work tree's index with its untracked
// files marked intent-to-add, so `git diff` against it includes them.
//
// Marking a file intent-to-add stores the empty blob, so commands using the
// copy also get a private object directory, with the repository's own as an
// alternate to read from: nothing is ever written to the repository.
type tempIndex struct {
	dir       string
	repoObjs  string
	untracked map[string]bool
	// truncated reports that there were more untracked files than
	// maxUntracked; the rest are not in the index.
	truncated bool
}

func (t *tempIndex) env() []string {
	return []string{
		"GIT_INDEX_FILE=" + filepath.Join(t.dir, "index"),
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(t.dir, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + quoteAlternate(t.repoObjs),
	}
}

func (t *tempIndex) remove() { os.RemoveAll(t.dir) }

// quoteAlternate quotes a path for GIT_ALTERNATE_OBJECT_DIRECTORIES, which
// separates paths with ':' and takes C-style quoting for the rest.
func quoteAlternate(path string) string {
	if !strings.ContainsAny(path, ":\"\\") {
		return path
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

func (r *repo) tempIndex(ctx context.Context) (*tempIndex, error) {
	dir, err := os.MkdirTemp("", "agentd-git-")
	if err != nil {
		return nil, err
	}
	t := &tempIndex{dir: dir, repoObjs: r.objects, untracked: map[string]bool{}}
	if err := os.Mkdir(filepath.Join(dir, "objects"), 0o700); err != nil {
		t.remove()
		return nil, err
	}
	// git replaces the index by renaming a new file over it, so the open
	// file is always a complete index even while the agent changes it. A
	// repository without one yet has an empty index, which is what a
	// missing GIT_INDEX_FILE means too.
	if err := copyFile(r.index, filepath.Join(dir, "index")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.remove()
		return nil, err
	}
	// A file that disappears between listing and adding makes `git add`
	// fail; list again once in that case.
	for attempt := 0; ; attempt++ {
		err = r.addUntracked(ctx, t)
		if err == nil || attempt == 1 {
			break
		}
	}
	if err != nil {
		t.remove()
		return nil, err
	}
	return t, nil
}

func (r *repo) addUntracked(ctx context.Context, t *tempIndex) error {
	recs, err := startRecords(ctx, r.root, nil, nil, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	clear(t.untracked)
	t.truncated = false
	var list bytes.Buffer
	for {
		rec, ok := recs.next()
		if !ok {
			break
		}
		// A nested repository is listed as its directory; it is not a file
		// of this one.
		if bytes.HasSuffix(rec, []byte("/")) {
			continue
		}
		if len(t.untracked) == maxUntracked {
			t.truncated = true
			recs.stop()
			break
		}
		t.untracked[string(rec)] = true
		list.Write(rec)
		list.WriteByte(0)
	}
	if err := recs.wait(); err != nil {
		return err
	}
	if list.Len() == 0 {
		return nil
	}
	env := append(t.env(), "GIT_LITERAL_PATHSPECS=1")
	_, err = output(ctx, r.root, env, &list, "-c", "core.splitIndex=false",
		"add", "--intent-to-add", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Summary is the cheap part of a repository's state: enough to tell which
// artifacts a session has.
type Summary struct {
	// Root is the work tree's top-level directory.
	Root string
	// Base is the recorded base, when it is still in the repository.
	Base string
	// CommitCount is the number of commits since Base.
	CommitCount uint32
}

// Summarize reads a Summary. It returns ErrNotRepository outside a work
// tree.
func Summarize(ctx context.Context, dir string, base Base) (Summary, error) {
	r, err := open(ctx, dir)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Root: r.root}
	from, fromBase, _, err := r.from(ctx, base)
	if err != nil || !fromBase || r.head == "" {
		return s, err
	}
	s.Base = from
	count, err := output(ctx, r.root, nil, nil, "rev-list", "--count", from+".."+r.head)
	s.CommitCount = parseCount(count)
	return s, err
}

// Stream is the stdout of a running git command. Read it to the end, then
// Close it to learn whether git succeeded. Closing it early stops git.
type Stream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *limitedBuffer
	cancel context.CancelFunc
	// done reports that stdout reached EOF.
	done bool
	// cleanup runs once git has exited.
	cleanup func()
	args    []string
}

func (s *Stream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	if err == io.EOF {
		s.done = true
	}
	return n, err
}

// Close stops git if it is still running and waits for it. It returns
// git's failure only if the output was read to the end: output cut short
// by its reader is not git's fault.
func (s *Stream) Close() error {
	if !s.done {
		s.cancel()
	}
	err := s.cmd.Wait()
	s.cancel()
	if s.cleanup != nil {
		s.cleanup()
	}
	if !s.done || err == nil {
		return nil
	}
	return describe(s.args, err, s.stderr.String())
}

// Diff starts `git diff` of the work tree dir is in against the base,
// untracked files included. Without a base (or with one no longer in the
// repository) it diffs against HEAD, and on an unborn branch against the
// empty tree. Ending ctx stops git.
func Diff(ctx context.Context, dir string, base Base) (*Stream, error) {
	r, err := open(ctx, dir)
	if err != nil {
		return nil, err
	}
	from, _, _, err := r.from(ctx, base)
	if err != nil {
		return nil, err
	}
	idx, err := r.tempIndex(ctx)
	if err != nil {
		return nil, err
	}
	s, err := startStream(ctx, r.root, idx.env(), nil, append(append([]string{"diff"}, diffFlags...), from)...)
	if err != nil {
		idx.remove()
		return nil, err
	}
	s.cleanup = idx.remove
	return s, nil
}

// ErrNoCommits means a session has no produced commits to export: it
// recorded no base, or nothing was committed since.
var ErrNoCommits = errors.New("no commits since the session's base")

// Patch starts `git format-patch --stdout` for the commits since the base:
// an mbox that `git am` applies elsewhere.
func Patch(ctx context.Context, dir string, base Base) (*Stream, error) {
	r, err := open(ctx, dir)
	if err != nil {
		return nil, err
	}
	from, fromBase, _, err := r.from(ctx, base)
	if err != nil {
		return nil, err
	}
	if !fromBase || r.head == "" {
		return nil, ErrNoCommits
	}
	if count, err := output(ctx, r.root, nil, nil, "rev-list", "--count", from+".."+r.head); err != nil {
		return nil, err
	} else if parseCount(count) == 0 {
		return nil, ErrNoCommits
	}
	return startStream(ctx, r.root, nil, nil, "format-patch", "--stdout", "--no-color", "--no-ext-diff",
		"--no-textconv", "--find-renames", "--src-prefix=a/", "--dst-prefix=b/", "--no-signature", from+".."+r.head)
}

// ---------------------------------------------------------------------------
// Running git

// command prepares git with args in dir. extraEnv is added to a cleaned
// copy of the daemon's environment.
func command(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	full := append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "color.ui=false"}, args...)
	cmd := exec.CommandContext(ctx, gitPath, full...)
	cmd.Dir = dir
	env := []string{}
	for _, kv := range os.Environ() {
		// The daemon's own GIT_* settings (GIT_DIR from a hook, say) must
		// not point git at another repository.
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), extraEnv...)
	// Its own process group, killed whole, so nothing git started outlives
	// a cancelled request.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// commandError is a git command that ran and failed.
type commandError struct {
	args   []string
	code   int
	stderr string
}

func (e *commandError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if line, _, ok := strings.Cut(msg, "\n"); ok {
		msg = line
	}
	msg = strings.TrimPrefix(msg, "fatal: ")
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.code)
	}
	return fmt.Sprintf("git %s: %s", subcommand(e.args), msg)
}

// subcommand is the git command args run, skipping `-c name=value` options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

func exitCode(err error) int {
	var gerr *commandError
	if errors.As(err, &gerr) {
		return gerr.code
	}
	return -1
}

// describe turns a failed run into an error that says what went wrong in
// terms a user can act on.
func describe(args []string, err error, stderr string) error {
	var exit *exec.ExitError
	var missing *fs.PathError
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.As(err, &missing) && missing.Path == gitPath:
		return errors.New("git is not installed (or not on agentd's PATH)")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("git %s took too long", subcommand(args))
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return &commandError{args: args, code: exit.ExitCode(), stderr: stderr}
	}
	return fmt.Errorf("git %s: %w", subcommand(args), err)
}

// output runs git and returns its trimmed stdout, which must be small.
func output(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := command(ctx, dir, env, args...)
	stdout, stderr := &limitedBuffer{max: maxOutput}, &limitedBuffer{max: maxStderr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	if err == nil && stdout.overflow {
		return "", fmt.Errorf("git %s: unexpectedly large output", subcommand(args))
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", describe(args, err, stderr.String())
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func startStream(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (*Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := command(ctx, dir, env, args...)
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stdin, cmd.Stderr = stdin, stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, describe(args, err, "")
	}
	return &Stream{cmd: cmd, stdout: stdout, stderr: stderr, cancel: cancel, args: args}, nil
}

// records reads git's NUL-terminated output one record at a time. Every
// records ends with exactly one stop or wait, which reap git.
type records struct {
	*Stream
	sc *bufio.Scanner
	// stopped reports that the reader gave up before the end, so git's
	// exit status (it was killed) means nothing.
	stopped bool
	closed  bool
	err     error
}

func startRecords(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (*records, error) {
	s, err := startStream(ctx, dir, env, stdin, args...)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(s)
	sc.Buffer(make([]byte, 0, 4096), maxRecord)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return &records{Stream: s, sc: sc}, nil
}

// next returns the next record, valid until the following call.
func (r *records) next() ([]byte, bool) {
	if r.stopped || !r.sc.Scan() {
		return nil, false
	}
	return r.sc.Bytes(), true
}

// stop gives up on the rest of the output: git is killed and reaped.
func (r *records) stop() {
	if r.closed {
		return
	}
	r.stopped, r.closed = true, true
	r.Close() // not read to the end, so this kills git
}

// wait reads (and discards) whatever output is left, so git can finish,
// then reaps it and reports its failure. After stop it returns nil.
func (r *records) wait() error {
	if r.closed {
		return r.err
	}
	r.closed = true
	for !r.stopped && r.sc.Scan() {
	}
	if err := r.sc.Err(); err != nil {
		// Not at EOF: Close kills git.
		r.Close()
		r.err = fmt.Errorf("git %s: %w", subcommand(r.args), err)
		return r.err
	}
	r.err = r.Close()
	return r.err
}

// limitedBuffer keeps the first max bytes written to it and drops the rest,
// so a command's output can never grow the daemon's memory without bound.
type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
