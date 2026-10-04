package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/robmorgan/agentd/internal/session"
)

// Git state and artifacts.
//
// GetGitState and ListArtifacts are one-shot requests (on the control
// stream). GetArtifact starts an artifact stream: the daemon answers with
// ArtifactChunk frames carrying the artifact's bytes in order, then
// EndOfStream. An Error frame instead of EndOfStream (possibly after some
// chunks) means the artifact is incomplete and must be discarded.
//
// Chunks are written as they are produced, straight from git's stdout or
// the file, and at most one chunk is in memory at a time: the daemon never
// holds a whole artifact. A client that stops reading holds the transfer
// back through the stream's flow control, which holds back git through its
// pipe; nothing is buffered on its behalf.

// GetArtifact asks for one of a session's artifacts (see session.Artifact)
// on a stream of its own.
type GetArtifact struct {
	SessionID string
	Name      string
}

// ArtifactChunkSize bounds the data in one ArtifactChunk frame.
const ArtifactChunkSize = 256 << 10

// artifactChunkPrefix is the bytes of an ArtifactChunk frame before its
// data: the frame header and the data's length.
const artifactChunkPrefix = frameHeaderLen + 4

// CopyArtifact streams r to w as ArtifactChunk frames of at most
// ArtifactChunkSize bytes, until r ends. It reads into one buffer reused for
// every frame, so a transfer holds one chunk in memory however large it is,
// and copies nothing: each frame is built around the data in place.
//
// readErr is a failure reading r; writeErr a failure writing w, which
// usually means the client went away. The caller ends the stream with
// EndOfStream, or with an Error after a readErr.
func CopyArtifact(w io.Writer, r io.Reader) (n int64, readErr, writeErr error) {
	buf := make([]byte, artifactChunkPrefix+ArtifactChunkSize)
	for {
		// Fill the chunk, so a source that trickles out small reads (a
		// pipe) still makes full frames.
		got, err := io.ReadFull(r, buf[artifactChunkPrefix:])
		if got > 0 {
			putFrameHeader(buf[:frameHeaderLen], ProtocolVersion, uint16(kArtifactChunkResponse), 0, 4+got)
			binary.LittleEndian.PutUint32(buf[frameHeaderLen:artifactChunkPrefix], uint32(got))
			if _, werr := w.Write(buf[:artifactChunkPrefix+got]); werr != nil {
				return n, nil, werr
			}
			n += int64(got)
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return n, nil, nil
		default:
			return n, err, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Encoding

func (e *encoder) gitState(g *session.GitState) {
	e.bool(g.Repo)
	e.str(g.Error)
	e.str(g.Root)
	e.str(g.Branch)
	e.str(g.Head)
	e.str(g.Base)
	e.str(g.BaseBranch)
	e.bool(g.BaseMissing)
	e.str(g.Upstream)
	e.u32(g.Ahead)
	e.u32(g.Behind)
	e.u32(g.CommitCount)
	e.length(len(g.Commits))
	for _, c := range g.Commits {
		e.str(c.Hash)
		e.str(c.Subject)
		e.str(c.Author)
		e.datetime(c.Time)
	}
	e.length(len(g.Files))
	for _, f := range g.Files {
		e.str(f.Path)
		e.str(f.OldPath)
		e.fileStatus(f.Status)
		e.u32(f.Additions)
		e.u32(f.Deletions)
		e.bool(f.Binary)
	}
	e.bool(g.FilesTruncated)
}

func (d *decoder) gitState() session.GitState {
	g := session.GitState{
		Repo: d.bool(), Error: d.str(), Root: d.str(), Branch: d.str(), Head: d.str(),
		Base: d.str(), BaseBranch: d.str(), BaseMissing: d.bool(),
		Upstream: d.str(), Ahead: d.u32(), Behind: d.u32(), CommitCount: d.u32(),
	}
	n := d.length()
	g.Commits = make([]session.GitCommit, 0, d.capacity(n))
	for i := 0; i < n && d.err == nil; i++ {
		g.Commits = append(g.Commits, session.GitCommit{Hash: d.str(), Subject: d.str(), Author: d.str(), Time: d.datetime()})
	}
	n = d.length()
	g.Files = make([]session.GitFile, 0, d.capacity(n))
	for i := 0; i < n && d.err == nil; i++ {
		g.Files = append(g.Files, session.GitFile{
			Path: d.str(), OldPath: d.str(), Status: d.fileStatus(),
			Additions: d.u32(), Deletions: d.u32(), Binary: d.bool(),
		})
	}
	g.FilesTruncated = d.bool()
	return g
}

var fileStatuses = []session.FileStatus{
	1: session.FileAdded,
	2: session.FileModified,
	3: session.FileDeleted,
	4: session.FileRenamed,
	5: session.FileCopied,
	6: session.FileTypeChanged,
	7: session.FileUnmerged,
	8: session.FileUntracked,
}

func (e *encoder) fileStatus(s session.FileStatus) {
	for i, v := range fileStatuses {
		if i > 0 && v == s {
			e.u8(uint8(i))
			return
		}
	}
	e.err = fmt.Errorf("invalid file status %q", s)
}

func (d *decoder) fileStatus() session.FileStatus {
	v := d.u8()
	if v == 0 || int(v) >= len(fileStatuses) {
		d.fail("invalid file status `%d`", v)
		return ""
	}
	return fileStatuses[v]
}
