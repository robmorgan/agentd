package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

const sampleDiff = `diff --git a/a.txt b/a.txt
index 1111111..2222222 100644
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 keep
--- a removed line that looks like a header
+++ an added line that looks like a header
diff --git a/b.bin b/b.bin
new file mode 100644
Binary files /dev/null and b/b.bin differ
`

// The colorizer gives the same output however the stream is split, colours
// lines by where they are in the diff rather than how they start, and
// leaves the text itself untouched.
func TestDiffColorizer(t *testing.T) {
	var whole bytes.Buffer
	c := &diffColorizer{w: &whole}
	c.Write([]byte(sampleDiff))
	c.Close()
	for _, size := range []int{1, 2, 7, 64} {
		var split bytes.Buffer
		c := &diffColorizer{w: &split}
		for rest := []byte(sampleDiff); len(rest) > 0; {
			n := min(size, len(rest))
			c.Write(rest[:n])
			rest = rest[n:]
		}
		c.Close()
		if split.String() != whole.String() {
			t.Fatalf("split every %d bytes:\n%q\nwhole:\n%q", size, split.String(), whole.String())
		}
	}
	out := whole.String()
	if stripANSI(out) != sampleDiff {
		t.Fatalf("text changed:\n%s", stripANSI(out))
	}
	for _, want := range []string{
		"\x1b[1mdiff --git a/a.txt b/a.txt\x1b[0m\n",
		"\x1b[1m--- a/a.txt\x1b[0m\n",
		"\x1b[36m@@ -1,3 +1,3 @@\x1b[0m\n",
		" keep\n",
		"\x1b[31m--- a removed line that looks like a header\x1b[0m\n",
		"\x1b[32m+++ an added line that looks like a header\x1b[0m\n",
		"\x1b[1mBinary files /dev/null and b/b.bin differ\x1b[0m\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%q", want, out)
		}
	}

	// A line longer than the colorizer holds passes through in pieces,
	// coloured once, and an unterminated last line is reset.
	var long bytes.Buffer
	c = &diffColorizer{w: &long, inHunk: true}
	line := "+" + strings.Repeat("x", 3*maxColorLine)
	c.Write([]byte(line[:maxColorLine+10]))
	c.Write([]byte(line[maxColorLine+10:]))
	c.Close()
	if got := long.String(); got != "\x1b[32m"+line+"\x1b[0m" {
		t.Fatalf("long line: %d bytes, starts %q ends %q", len(got), got[:10], got[len(got)-10:])
	}
}

func TestDiffStat(t *testing.T) {
	files := []session.GitFile{
		{Path: "src/main.go", Status: session.FileModified, Additions: 10, Deletions: 2},
		{Path: "new.go", OldPath: "old.go", Status: session.FileRenamed},
		{Path: "logo.png", Status: session.FileUntracked, Binary: true},
		{Path: "huge.txt", Status: session.FileAdded, Additions: 1000},
	}
	var b bytes.Buffer
	writeDiffStat(&b, files, 60, false)
	got := b.String()
	want := " src/main.go      |   12 +-\n" +
		" old.go => new.go |    0 \n" +
		" logo.png         |  Bin\n" +
		" huge.txt         | 1000 " + strings.Repeat("+", 60-16-4-5) + "\n" +
		" 4 files changed, 1010 insertions(+), 2 deletions(-)\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if runeLen(line) > 60 {
			t.Errorf("line wider than 60: %q", line)
		}
	}
}

func TestPrintGitState(t *testing.T) {
	now := time.Unix(1_700_003_600, 0)
	st := &session.GitState{
		Repo: true, Root: "/src/app", Branch: "agent/fix", Head: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Base: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseBranch: "main",
		Upstream: "origin/agent/fix", Ahead: 2, Behind: 0, CommitCount: 2,
		Commits: []session.GitCommit{
			{Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Subject: "fix \x1b[31mit", Author: "Agent", Time: time.Unix(1_700_000_000, 0)},
			{Hash: "cccccccccccccccccccccccccccccccccccccccc", Subject: "test it", Author: "Agent", Time: time.Unix(1_700_003_000, 0)},
		},
		Files: []session.GitFile{
			{Path: "a.go", Status: session.FileModified, Additions: 3, Deletions: 1},
			{Path: "b.go", Status: session.FileAdded, Additions: 9},
			{Path: "c.txt", Status: session.FileUntracked, Additions: 1},
		},
	}
	var b bytes.Buffer
	printGitState(&b, st, now)
	want := `git_root: /src/app
git_branch: agent/fix
git_head: bbbbbbbbbbbb
git_base: aaaaaaaaaaaa (main)
git_upstream: origin/agent/fix (ahead 2, behind 0)
git_commits: 2 since base
  bbbbbbbbbbbb fix \u{1b}[31mit (Agent, 1h ago)
  cccccccccccc test it (Agent, 10m ago)
git_changed_files: 3 (1 added, 1 modified, 1 untracked), +13 -1
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}

	for _, tc := range []struct {
		st   session.GitState
		want string
	}{
		{session.GitState{}, "git: not a repository\n"},
		{session.GitState{Error: "git is not installed"}, "git: unavailable (git is not installed)\n"},
		{session.GitState{Repo: true, Root: "/r", Branch: "main"}, "git_root: /r\ngit_branch: main (no commits yet)\ngit_base: none recorded\ngit_changed_files: 0\n"},
		{session.GitState{Repo: true, Root: "/r", Head: "dddddddddddddddd", Base: "eeee", BaseMissing: true},
			"git_root: /r\ngit_branch: (detached at dddddddddddd)\ngit_head: dddddddddddd\ngit_base: eeee, no longer in the repository; changes shown since HEAD\ngit_changed_files: 0\n"},
	} {
		var b bytes.Buffer
		printGitState(&b, &tc.st, now)
		if b.String() != tc.want {
			t.Errorf("%+v:\ngot:\n%s\nwant:\n%s", tc.st, b.String(), tc.want)
		}
	}
}

func TestPrintArtifacts(t *testing.T) {
	size := uint64(2048)
	var b bytes.Buffer
	printArtifacts(&b, []session.Artifact{
		{Name: "diff", Kind: "diff", Description: "changes"},
		{Name: "history", Kind: "history", Description: "terminal history", Size: &size},
	})
	if want := "diff\tdiff\t-\tchanges\nhistory\thistory\t2.0 KiB\tterminal history\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}
