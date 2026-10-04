package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/robmorgan/agentd/internal/protocol"
	"github.com/robmorgan/agentd/internal/repo"
	"github.com/robmorgan/agentd/internal/session"
	"github.com/robmorgan/agentd/internal/transport"
)

// What a session produced: its git state and its artifacts. The daemon
// reads git in the session's directory (internal/repo) and never changes
// the repository. This works for sessions that have ended too, as long as
// their directory is still there.

const (
	// gitStateTimeout bounds a one-shot git request (GetGitState,
	// ListArtifacts). The lists it reads are capped, so only a very large
	// or very slow repository comes near it.
	gitStateTimeout = 30 * time.Second
	// gitProbeTimeout bounds reading a new session's base, which delays
	// its creation.
	gitProbeTimeout = 5 * time.Second
)

// Artifact names.
const (
	artifactDiff      = "diff"
	artifactPatch     = "patch"
	artifactHistory   = "history"
	artifactHistoryVT = "history.vt"
)

// probeBase reads the commit and branch a session starting in cwd will be
// measured from. Anything but a repository with a commit records nothing.
func probeBase(cwd string) repo.Base {
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	base, _ := repo.ProbeBase(ctx, cwd)
	return base
}

// sessionBase looks up a session and the base it recorded.
func (s *Server) sessionBase(id string) (*session.Record, repo.Base, error) {
	rec, err := s.getSession(id)
	if err != nil {
		return nil, repo.Base{}, err
	}
	if rec == nil {
		return nil, repo.Base{}, fmt.Errorf("session `%s` not found", id)
	}
	commit, branch, _, err := s.db.GitBase(id)
	if err != nil {
		return nil, repo.Base{}, err
	}
	return rec, repo.Base{Commit: commit, Branch: branch}, nil
}

// gitState answers GetGitState. git failing (not installed, the directory
// gone) is reported inside the state, not as an error.
func (s *Server) gitState(id string) *protocol.Response {
	rec, base, err := s.sessionBase(id)
	if err != nil {
		return protocol.ErrorResponsef("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitStateTimeout)
	defer cancel()
	st := repo.State(ctx, rec.Cwd, base)
	return &protocol.Response{GitState: &st}
}

// listArtifacts answers ListArtifacts: the git artifacts when the session's
// directory is in a repository, and its terminal history while it runs or
// once it has been saved.
func (s *Server) listArtifacts(id string) *protocol.Response {
	rec, base, err := s.sessionBase(id)
	if err != nil {
		return protocol.ErrorResponsef("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitStateTimeout)
	defer cancel()
	out := []session.Artifact{}
	// A directory that is not a repository (or where git fails) simply has
	// no git artifacts; `agent diff` explains why if asked.
	if sum, err := repo.Summarize(ctx, rec.Cwd, base); err == nil {
		desc := "changes since HEAD (no base recorded), untracked files included"
		if sum.Base != "" {
			desc = fmt.Sprintf("changes since the session started at %s, untracked files included", short(sum.Base))
		}
		out = append(out, session.Artifact{Name: artifactDiff, Kind: "diff", Description: desc})
		if sum.CommitCount > 0 {
			out = append(out, session.Artifact{Name: artifactPatch, Kind: "patch",
				Description: fmt.Sprintf("%d %s since %s as an mbox, for `git am`", sum.CommitCount, plural(sum.CommitCount, "commit"), short(sum.Base))})
		}
	}
	live := rec.Status == session.StatusRunning && s.workerAnswers(id)
	for _, h := range []struct {
		name, path, desc string
	}{
		{artifactHistory, s.paths.RenderedLogPath(id), "terminal history as plain text"},
		{artifactHistoryVT, s.paths.LogPath(id), "terminal history with escape sequences"},
	} {
		a := session.Artifact{Name: h.name, Kind: "history", Description: h.desc}
		if !live {
			// Saved when the session ended; its size is known.
			info, err := os.Stat(h.path)
			if err != nil {
				continue
			}
			size := uint64(info.Size())
			a.Size = &size
		}
		out = append(out, a)
	}
	return &protocol.Response{Artifacts: &out}
}

func short(hash string) string { return hash[:min(len(hash), 12)] }

func plural(n uint32, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// serveArtifact serves an artifact stream (protocol.RoleArtifact): the
// artifact as ArtifactChunk frames, then EndOfStream, or an Error if it
// could not be produced whole.
//
// Backpressure: chunks are written as git (or the file) produces them, one
// at a time from one buffer (protocol.CopyArtifact). A client that stops
// reading blocks the write, which stops this handler reading git's stdout,
// which blocks git on its full pipe: nothing accumulates in the daemon, and
// the transfer resumes when the client reads again. It ends when the client
// closes the stream or its connection dies (a dead remote peer is noticed
// within transport.DeadPeerTimeout), or at shutdown; git is then killed.
// There is deliberately no stall timeout, since a reader such as a pager
// may pause for as long as its user does.
func (s *Server) serveArtifact(conn transport.Stream, req *protocol.GetArtifact) error {
	// The context ends git when this handler returns, and at shutdown even
	// while git has produced nothing to write yet.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()

	src, err := s.openArtifact(ctx, req)
	if err != nil {
		return protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
	}
	_, readErr, writeErr := protocol.CopyArtifact(conn, src)
	if writeErr != nil {
		// The client went away or gave up on the artifact (closing the
		// stream is how it does that): nothing to report.
		cancel()
		src.Close()
		return nil
	}
	if err := src.Close(); err != nil && readErr == nil {
		readErr = err
	}
	if readErr != nil {
		return protocol.WriteResponse(conn, protocol.ErrorResponsef("artifact `%s` of session `%s` is incomplete: %v", req.Name, req.SessionID, readErr))
	}
	return protocol.WriteResponse(conn, protocol.EndOfStreamResponse())
}

// openArtifact starts producing an artifact. Closing the result stops it
// and reports whether it was produced whole.
func (s *Server) openArtifact(ctx context.Context, req *protocol.GetArtifact) (io.ReadCloser, error) {
	id := req.SessionID
	rec, base, err := s.sessionBase(id)
	if err != nil {
		return nil, err
	}
	gitErr := func(err error) error {
		if errors.Is(err, repo.ErrNotRepository) {
			return fmt.Errorf("session `%s` does not run in a git repository (%s)", id, rec.Cwd)
		}
		return err
	}
	switch req.Name {
	case artifactDiff:
		src, err := repo.Diff(ctx, rec.Cwd, base)
		if err != nil {
			return nil, gitErr(err)
		}
		return src, nil
	case artifactPatch:
		src, err := repo.Patch(ctx, rec.Cwd, base)
		if errors.Is(err, repo.ErrNoCommits) {
			return nil, fmt.Errorf("session `%s` has no commits since it started", id)
		}
		if err != nil {
			return nil, gitErr(err)
		}
		return src, nil
	case artifactHistory, artifactHistoryVT:
		return s.historySource(id, req.Name == artifactHistoryVT)
	}
	return nil, fmt.Errorf("session `%s` has no artifact `%s`; see `agent artifacts %s`", id, req.Name, id)
}

// historySource is a session's history: live from its worker while it runs
// (one frame from the worker, so at most protocol.MaxFramePayload in
// memory), otherwise streamed from the log the worker saved when the
// session ended. A worker that exits between the two is covered by the
// fallback.
func (s *Server) historySource(id string, vt bool) (io.ReadCloser, error) {
	if worker, err := transport.DialUnix(s.paths.SessionSocketPath(id), workerDialTimeout); err == nil {
		resp, err := exchange(worker, &protocol.Request{GetHistory: &protocol.GetHistory{SessionID: id, VT: vt}})
		worker.Close()
		if err == nil && resp.History != nil {
			return io.NopCloser(strings.NewReader(resp.History.Data)), nil
		}
	}
	rec, err := s.getSession(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("session `%s` not found", id)
	}
	path := s.paths.RenderedLogPath(id)
	if vt {
		path = s.paths.LogPath(id)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("history for session `%s` is not available", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %v", path, err)
	}
	return f, nil
}
