package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/session"
)

// What a session produced: `agent diff`, `agent artifacts`, `agent
// artifact`, and the git section of `agent status`. The daemon reads git
// on its own machine; the CLI only shows what it sends.

// requireCapability fails unless the daemon advertised c in its Welcome.
func (c *client) requireCapability(cap, what string) error {
	w, err := c.welcome()
	if err != nil {
		return err
	}
	if protocol.HasCapability(w.Capabilities, cap) {
		return nil
	}
	where := "agentd"
	if c.host != nil {
		where = fmt.Sprintf("agentd on `%s`", c.host.Name)
	}
	return fmt.Errorf("%s (version %s) does not support %s; upgrade it", where, w.DaemonVersion, what)
}

func (c *client) gitState(id string) (*session.GitState, error) {
	if err := c.requireCapability(protocol.CapGitState, "git state"); err != nil {
		return nil, err
	}
	resp, err := c.call(&protocol.Request{GetGitState: &protocol.SessionRef{SessionID: id}}, 0, func(r *protocol.Response) bool { return r.GitState != nil })
	if err != nil {
		return nil, err
	}
	return resp.GitState, nil
}

// repoState is gitState for commands that need a repository.
func (c *client) repoState(id string) (*session.GitState, error) {
	st, err := c.gitState(id)
	switch {
	case err != nil:
		return nil, err
	case st.Error != "":
		return nil, fmt.Errorf("could not read git state for session `%s`: %s", id, st.Error)
	case !st.Repo:
		return nil, fmt.Errorf("session `%s` does not run in a git repository", id)
	}
	return st, nil
}

func (c *client) listArtifacts(id string) ([]session.Artifact, error) {
	if err := c.requireCapability(protocol.CapArtifacts, "artifacts"); err != nil {
		return nil, err
	}
	resp, err := c.call(&protocol.Request{ListArtifacts: &protocol.SessionRef{SessionID: id}}, 0, func(r *protocol.Response) bool { return r.Artifacts != nil })
	if err != nil {
		return nil, err
	}
	return *resp.Artifacts, nil
}

// streamArtifact downloads an artifact to w on a stream of its own, so a
// large one never delays the control stream or an attachment on the same
// connection. Chunks are written to w as they arrive; if w is slow, the
// stream's flow control slows the daemon (and git) down to match.
func (c *client) streamArtifact(id, name string, w io.Writer) error {
	if err := c.requireCapability(protocol.CapArtifacts, "artifacts"); err != nil {
		return err
	}
	s, err := c.open(context.Background())
	if err != nil {
		return err
	}
	defer s.Close()
	if err := protocol.WriteRequest(s, &protocol.Request{GetArtifact: &protocol.GetArtifact{SessionID: id, Name: name}}); err != nil {
		return err
	}
	r := bufio.NewReader(s)
	for {
		resp, err := protocol.ReadResponse(r)
		switch {
		case err != nil:
			return err
		case resp == nil:
			return fmt.Errorf("agentd closed the stream before the end of artifact `%s`", name)
		case resp.ArtifactChunk != nil:
			if _, err := w.Write(resp.ArtifactChunk.Data); err != nil {
				return err
			}
		case resp.EndOfStream != nil:
			return nil
		case resp.Error != nil:
			return errors.New(resp.Error.Message)
		default:
			return fmt.Errorf("unexpected response: %s", describeResponse(resp))
		}
	}
}

// diff runs `agent diff`.
func (c *client) diff(id string, stat, nameOnly bool) error {
	switch {
	case stat && nameOnly:
		return usageError{errors.New("use either --stat or --name-only, not both")}
	case nameOnly:
		st, err := c.repoState(id)
		if err != nil {
			return err
		}
		for _, f := range st.Files {
			fmt.Println(escapeControls(f.Path))
		}
		return truncationNote(st)
	case stat:
		st, err := c.repoState(id)
		if err != nil {
			return err
		}
		width := stdoutWidth()
		if width == 0 {
			width = 80
		}
		writeDiffStat(os.Stdout, st.Files, width, stdoutIsTerminal())
		return truncationNote(st)
	}
	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	var w io.Writer = out
	var colorizer *diffColorizer
	if stdoutIsTerminal() {
		colorizer = &diffColorizer{w: out}
		w = colorizer
	}
	err := c.streamArtifact(id, "diff", w)
	if colorizer != nil {
		colorizer.Close()
	}
	if flushErr := out.Flush(); err == nil {
		err = flushErr
	}
	return err
}

func truncationNote(st *session.GitState) error {
	if st.FilesTruncated {
		fmt.Fprintf(os.Stderr, "note: only the first %d changed files are listed\n", len(st.Files))
	}
	return nil
}

// downloadArtifact runs `agent artifact`: to stdout, or to a file that only
// appears, complete, once the whole artifact has arrived.
func (c *client) downloadArtifact(id, name, output string) error {
	if output == "" || output == "-" {
		out := bufio.NewWriterSize(os.Stdout, 64<<10)
		err := c.streamArtifact(id, name, out)
		if flushErr := out.Flush(); err == nil {
			err = flushErr
		}
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), "."+filepath.Base(output)+".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := c.streamArtifact(id, name, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), output)
}

func printArtifacts(w io.Writer, artifacts []session.Artifact) {
	for _, a := range artifacts {
		size := "-"
		if a.Size != nil {
			size = formatBytes(*a.Size)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Name, a.Kind, size, escapeControls(a.Description))
	}
}

// shortHash abbreviates a commit hash for display.
func shortHash(h string) string { return h[:min(len(h), 12)] }

// maxStatusCommits bounds the commits `agent status` lists.
const maxStatusCommits = 10

// printGitState is the git section of `agent status`.
func printGitState(w io.Writer, st *session.GitState, now time.Time) {
	base := "none recorded"
	if st.Base != "" {
		base = shortHash(st.Base)
		if st.BaseBranch != "" {
			base += " (" + escapeControls(st.BaseBranch) + ")"
		}
		if st.BaseMissing {
			base += ", no longer in the repository; changes shown since HEAD"
		}
	}
	switch {
	case st.Error != "":
		fmt.Fprintf(w, "git: unavailable (%s)\n", escapeControls(st.Error))
		return
	case !st.Repo:
		fmt.Fprintln(w, "git: not a repository")
		return
	}
	fmt.Fprintf(w, "git_root: %s\n", escapeControls(st.Root))
	switch {
	case st.Detached():
		fmt.Fprintf(w, "git_branch: (detached at %s)\n", shortHash(st.Head))
	case st.Head == "":
		fmt.Fprintf(w, "git_branch: %s (no commits yet)\n", escapeControls(st.Branch))
	default:
		fmt.Fprintf(w, "git_branch: %s\n", escapeControls(st.Branch))
	}
	if st.Head != "" {
		fmt.Fprintf(w, "git_head: %s\n", shortHash(st.Head))
	}
	fmt.Fprintf(w, "git_base: %s\n", base)
	if st.Upstream != "" {
		fmt.Fprintf(w, "git_upstream: %s (ahead %d, behind %d)\n", escapeControls(st.Upstream), st.Ahead, st.Behind)
	}
	if st.Base != "" && !st.BaseMissing {
		fmt.Fprintf(w, "git_commits: %d since base\n", st.CommitCount)
		for i, cm := range st.Commits {
			if i == maxStatusCommits {
				fmt.Fprintf(w, "  ... and %d more\n", int(st.CommitCount)-i)
				break
			}
			fmt.Fprintf(w, "  %s %s (%s, %s ago)\n", shortHash(cm.Hash), escapeControls(cm.Subject),
				escapeControls(cm.Author), formatElapsed(max(int64(now.Sub(cm.Time)/time.Second), 0)))
		}
	}
	fmt.Fprintf(w, "git_changed_files: %s\n", changedSummary(st))
}

// changedSummary counts changed files by status, with line totals:
// "4 (1 added, 2 modified, 1 untracked), +12 -3".
func changedSummary(st *session.GitState) string {
	if len(st.Files) == 0 {
		return "0"
	}
	order := []session.FileStatus{
		session.FileAdded, session.FileModified, session.FileDeleted, session.FileRenamed,
		session.FileCopied, session.FileTypeChanged, session.FileUnmerged, session.FileUntracked,
	}
	counts := map[session.FileStatus]int{}
	var add, del uint64
	for _, f := range st.Files {
		counts[f.Status]++
		add += uint64(f.Additions)
		del += uint64(f.Deletions)
	}
	var parts []string
	for _, s := range order {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ReplaceAll(string(s), "_", " ")))
		}
	}
	total := fmt.Sprint(len(st.Files))
	if st.FilesTruncated {
		total += "+ (list truncated)"
	}
	return fmt.Sprintf("%s (%s), +%d -%d", total, strings.Join(parts, ", "), add, del)
}

// writeDiffStat prints a diffstat like `git diff --stat`: one line per file
// with a bar of +/- scaled to fit width, then the totals.
func writeDiffStat(w io.Writer, files []session.GitFile, width int, color bool) {
	if len(files) == 0 {
		return
	}
	names := make([]string, len(files))
	nameWidth, countWidth := 0, 0
	maxChanges := uint32(0)
	var add, del uint64
	for i, f := range files {
		names[i] = escapeControls(f.Path)
		if f.OldPath != "" {
			names[i] = escapeControls(f.OldPath) + " => " + names[i]
		}
		nameWidth = max(nameWidth, runeLen(names[i]))
		changes := f.Additions + f.Deletions
		maxChanges = max(maxChanges, changes)
		countWidth = max(countWidth, len(fmt.Sprint(changes)))
		if f.Binary {
			countWidth = max(countWidth, len("Bin"))
		}
		add += uint64(f.Additions)
		del += uint64(f.Deletions)
	}
	// Leave room for " | ", the count and a bar of at least 10.
	nameWidth = min(nameWidth, max(width-countWidth-3-1-10, 10))
	barWidth := max(width-nameWidth-countWidth-5, 1)
	green, red, reset := "", "", ""
	if color {
		green, red, reset = "\x1b[32m", "\x1b[31m", ansiReset
	}
	for i, f := range files {
		name := names[i]
		if runeLen(name) > nameWidth {
			name = "..." + lastRunes(name, nameWidth-3)
		}
		if f.Binary {
			fmt.Fprintf(w, " %s | %*s\n", padRight(name, nameWidth), countWidth, "Bin")
			continue
		}
		changes := f.Additions + f.Deletions
		plus, minus := int(f.Additions), int(f.Deletions)
		if maxChanges > uint32(barWidth) {
			// Scale, keeping at least one mark for any change.
			scale := func(n uint32) int {
				if n == 0 {
					return 0
				}
				return max(1, int(uint64(n)*uint64(barWidth)/uint64(maxChanges)))
			}
			plus, minus = scale(f.Additions), scale(f.Deletions)
		}
		fmt.Fprintf(w, " %s | %*d %s%s%s%s%s\n", padRight(name, nameWidth), countWidth, changes,
			green, strings.Repeat("+", plus), red, strings.Repeat("-", minus), reset)
	}
	fmt.Fprintf(w, " %d %s changed, %d %s(+), %d %s(-)\n", len(files), pluralize(len(files), "file"),
		add, pluralize(int(add), "insertion"), del, pluralize(int(del), "deletion"))
}

func pluralize(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// diffColorizer colours a unified diff for a terminal as it streams through,
// the way git does: file headers bold, hunk headers cyan, added lines green
// and removed lines red. It holds at most one line (up to maxColorLine
// bytes; a longer one is passed through in pieces).
type diffColorizer struct {
	w   io.Writer
	buf []byte
	// mid reports that the start of the current line was written already,
	// in colour cur.
	mid bool
	cur string
	// inHunk distinguishes a hunk's "---" line (a removed line) from a file
	// header's.
	inHunk bool
}

const maxColorLine = 64 << 10

func (c *diffColorizer) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	rest := c.buf
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		if err := c.writeLine(rest[:i], true); err != nil {
			return 0, err
		}
		rest = rest[i+1:]
	}
	if len(rest) >= maxColorLine {
		if err := c.writeLine(rest, false); err != nil {
			return 0, err
		}
		rest = nil
	}
	c.buf = append(c.buf[:0], rest...)
	return len(p), nil
}

// Close writes what is left and resets the colour.
func (c *diffColorizer) Close() error {
	if len(c.buf) > 0 {
		if err := c.writeLine(c.buf, false); err != nil {
			return err
		}
		c.buf = c.buf[:0]
	}
	if c.mid && c.cur != "" {
		_, err := io.WriteString(c.w, ansiReset)
		c.mid = false
		return err
	}
	return nil
}

func (c *diffColorizer) writeLine(text []byte, complete bool) error {
	var out []byte
	if !c.mid {
		c.cur = c.classify(text)
		out = append(out, c.cur...)
	}
	out = append(out, text...)
	if complete {
		if c.cur != "" {
			out = append(out, ansiReset...)
		}
		out = append(out, '\n')
		c.mid = false
	} else {
		c.mid = true
	}
	_, err := c.w.Write(out)
	return err
}

func (c *diffColorizer) classify(line []byte) string {
	switch {
	case bytes.HasPrefix(line, []byte("diff ")):
		c.inHunk = false
		return ansiEmphasis
	case bytes.HasPrefix(line, []byte("@@")):
		c.inHunk = true
		return "\x1b[36m"
	case !c.inHunk:
		return ansiEmphasis
	case bytes.HasPrefix(line, []byte("+")):
		return "\x1b[32m"
	case bytes.HasPrefix(line, []byte("-")):
		return "\x1b[31m"
	}
	return ""
}
