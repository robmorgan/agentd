package session

import "time"

// GitState describes the git repository a session's working directory is
// in, as the daemon read it just now. agentd only reads git state; it never
// changes a repository, its index or its refs.
//
// A session created in a repository records its base: the commit HEAD
// pointed at, and the branch, when the session started. What the agent
// produced is measured from there: Commits are the commits reachable from
// HEAD but not from the base, and Files is everything that differs from the
// base (committed since, staged, unstaged and untracked). Without a base (a
// session created before agentd recorded them, or in a directory that was
// not a repository with commits yet) both are measured from HEAD.
type GitState struct {
	// Repo reports that the working directory is inside a git work tree.
	// When it is false, or Error is set, the other fields are empty.
	Repo bool
	// Error says why the state could not be read: git is not installed,
	// the directory no longer exists, git timed out. It is empty for a
	// directory that simply is not a repository.
	Error string
	// Root is the work tree's top-level directory.
	Root string
	// Branch is the checked-out branch, or "" for a detached HEAD.
	Branch string
	// Head is the commit HEAD points at, or "" on a branch with no commits
	// yet.
	Head string
	// Base and BaseBranch are what the session recorded at creation, or ""
	// when it recorded nothing.
	Base, BaseBranch string
	// BaseMissing reports a recorded base that is no longer in the
	// repository (it was garbage collected, say); Commits and Files are
	// then measured from HEAD.
	BaseMissing bool
	// Upstream is the branch's upstream ("origin/main"), or "" for none.
	// Ahead and Behind count the commits HEAD has that the upstream does
	// not, and the other way round.
	Upstream      string
	Ahead, Behind uint32
	// CommitCount is the number of commits since the base. Commits lists
	// the newest of them, at most a fixed number, so it may be shorter.
	CommitCount uint32
	Commits     []GitCommit
	// Files are the changed files, sorted by path. FilesTruncated reports
	// that there were more than could be listed.
	Files          []GitFile
	FilesTruncated bool
}

// Detached reports a HEAD that points at a commit rather than a branch.
func (g *GitState) Detached() bool { return g.Head != "" && g.Branch == "" }

// MeasuredFrom is the commit Commits and Files are measured from: the base,
// or HEAD without one.
func (g *GitState) MeasuredFrom() string {
	if g.Base != "" && !g.BaseMissing {
		return g.Base
	}
	return g.Head
}

// GitCommit is one commit since the base.
type GitCommit struct {
	Hash    string
	Subject string
	Author  string
	Time    time.Time
}

// GitFile is one changed file.
type GitFile struct {
	// Path is the file's path relative to the work tree root. OldPath is
	// where a renamed or copied file came from, otherwise "".
	Path, OldPath string
	Status        FileStatus
	// Additions and Deletions count changed lines. Binary files have
	// none and set Binary.
	Additions, Deletions uint32
	Binary               bool
}

// FileStatus is how a file changed.
type FileStatus string

const (
	FileAdded       FileStatus = "added"
	FileModified    FileStatus = "modified"
	FileDeleted     FileStatus = "deleted"
	FileRenamed     FileStatus = "renamed"
	FileCopied      FileStatus = "copied"
	FileTypeChanged FileStatus = "type_changed"
	FileUnmerged    FileStatus = "unmerged"
	// FileUntracked is a new file git does not track yet.
	FileUntracked FileStatus = "untracked"
)

// Artifact is something a session produced that a client can download
// (GetArtifact), such as the diff of its working tree or its terminal
// history.
type Artifact struct {
	// Name identifies the artifact within its session: "diff", "patch",
	// "history" or "history.vt".
	Name string
	// Kind is what the content is: "diff" (a unified diff), "patch" (an
	// mbox of patches, for `git am`), "history" (terminal history).
	Kind string
	// Description is a one-line summary for people.
	Description string
	// Size is the artifact's size in bytes, when it is cheap to know
	// without producing it.
	Size *uint64
}
