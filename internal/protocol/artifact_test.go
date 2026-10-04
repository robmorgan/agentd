package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

func u64p(v uint64) *uint64 { return &v }

func sampleGitState() *session.GitState {
	return &session.GitState{
		Repo: true, Root: "/w", Branch: "main", Head: strings.Repeat("b", 40),
		Base: strings.Repeat("a", 40), BaseBranch: "main", Upstream: "origin/main", Ahead: 2, Behind: 1,
		CommitCount: 2,
		Commits: []session.GitCommit{
			{Hash: strings.Repeat("b", 40), Subject: "fix it", Author: "Ada", Time: time.Unix(1_700_000_000, 0).UTC()},
			{Hash: strings.Repeat("c", 40), Subject: "test it", Author: "Bo", Time: time.Unix(1_700_000_100, 0).UTC()},
		},
		Files: []session.GitFile{
			{Path: "a.go", Status: session.FileModified, Additions: 3, Deletions: 1},
			{Path: "new.go", OldPath: "old.go", Status: session.FileRenamed},
			{Path: "img.png", Status: session.FileUntracked, Binary: true},
		},
		FilesTruncated: true,
	}
}

func TestArtifactRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{GetGitState: &SessionRef{"demo"}})
	roundTripRequest(t, &Request{ListArtifacts: &SessionRef{"demo"}})
	roundTripRequest(t, &Request{GetArtifact: &GetArtifact{SessionID: "demo", Name: "history.vt"}})
	roundTripResponse(t, &Response{GitState: sampleGitState()})
	roundTripResponse(t, &Response{GitState: &session.GitState{Error: "git is not installed", Base: "x", Commits: []session.GitCommit{}, Files: []session.GitFile{}}})
	roundTripResponse(t, &Response{Artifacts: &[]session.Artifact{
		{Name: "diff", Kind: "diff", Description: "working tree since base"},
		{Name: "history", Kind: "history", Description: "terminal history", Size: u64p(1 << 40)},
	}})
	roundTripResponse(t, &Response{ArtifactChunk: &Bytes{Data: []byte{0, 1, 255}}})

	// Every status survives the trip; anything else is refused.
	for _, s := range fileStatuses[1:] {
		roundTripResponse(t, &Response{GitState: &session.GitState{Commits: []session.GitCommit{}, Files: []session.GitFile{{Path: "p", Status: s}}}})
	}
	if err := WriteResponse(io.Discard, &Response{GitState: &session.GitState{Files: []session.GitFile{{Status: "weird"}}}}); err == nil {
		t.Fatal("unknown file status encoded")
	}
}

func TestArtifactGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{GetGitState: &SessionRef{"ab"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x1e, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 30
		0x06, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 'a', 'b',
	})
	assertRequestGolden(t, &Request{ListArtifacts: &SessionRef{"ab"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x1f, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 31
		0x06, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 'a', 'b',
	})
	assertRequestGolden(t, &Request{GetArtifact: &GetArtifact{SessionID: "ab", Name: "diff"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 32
		0x0e, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 'a', 'b',
		0x04, 0x00, 0x00, 0x00, 'd', 'i', 'f', 'f',
	})
	assertResponseGolden(t, &Response{ArtifactChunk: &Bytes{Data: []byte("hi")}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 132
		0x06, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 'h', 'i',
	})
	assertResponseGolden(t, &Response{Artifacts: &[]session.Artifact{{Name: "d", Kind: "k", Description: "x", Size: u64p(5)}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x83, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 131
		0x1c, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, // one artifact
		0x01, 0x00, 0x00, 0x00, 'd',
		0x01, 0x00, 0x00, 0x00, 'k',
		0x01, 0x00, 0x00, 0x00, 'x',
		0x01, 0x05, 0, 0, 0, 0, 0, 0, 0, // size
	})
	assertResponseGolden(t, &Response{GitState: &session.GitState{
		Repo: true, Root: "/r", Branch: "m", Head: "h", Base: "b", BaseBranch: "m",
		Ahead: 1, Behind: 2, CommitCount: 3,
		Commits:        []session.GitCommit{{Hash: "c", Subject: "s", Author: "a", Time: time.Unix(1_700_000_000, 0).UTC()}},
		Files:          []session.GitFile{{Path: "p", Status: session.FileRenamed, Additions: 5, Deletions: 6}},
		FilesTruncated: true,
	}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x82, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 130
		0x67, 0x00, 0x00, 0x00,
		0x01,                   // repo
		0x00, 0x00, 0x00, 0x00, // error
		0x02, 0x00, 0x00, 0x00, '/', 'r', // root
		0x01, 0x00, 0x00, 0x00, 'm', // branch
		0x01, 0x00, 0x00, 0x00, 'h', // head
		0x01, 0x00, 0x00, 0x00, 'b', // base
		0x01, 0x00, 0x00, 0x00, 'm', // base branch
		0x00,                   // base missing
		0x00, 0x00, 0x00, 0x00, // upstream
		0x01, 0x00, 0x00, 0x00, // ahead
		0x02, 0x00, 0x00, 0x00, // behind
		0x03, 0x00, 0x00, 0x00, // commit count
		0x01, 0x00, 0x00, 0x00, // one commit
		0x01, 0x00, 0x00, 0x00, 'c',
		0x01, 0x00, 0x00, 0x00, 's',
		0x01, 0x00, 0x00, 0x00, 'a',
		0x00, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // time
		0x01, 0x00, 0x00, 0x00, // one file
		0x01, 0x00, 0x00, 0x00, 'p',
		0x00, 0x00, 0x00, 0x00, // old path
		0x04,                   // renamed
		0x05, 0x00, 0x00, 0x00, // additions
		0x06, 0x00, 0x00, 0x00, // deletions
		0x00, // binary
		0x01, // truncated
	})
}

func TestArtifactStreamsHaveTheirOwnRole(t *testing.T) {
	for req, want := range map[*Request]StreamRole{
		{GetArtifact: &GetArtifact{}}:  RoleArtifact,
		{GetGitState: &SessionRef{}}:   RoleRequest,
		{ListArtifacts: &SessionRef{}}: RoleRequest,
	} {
		if got := req.Role(); got != want {
			t.Errorf("%#v: role %v, want %v", req, got, want)
		}
	}
	for _, c := range []string{CapGitState, CapArtifacts} {
		if !HasCapability(Capabilities(), c) {
			t.Errorf("this build does not advertise %s", c)
		}
	}
}

// CopyArtifact builds exactly the frames WriteResponse would, filling each
// chunk even from a reader that returns a byte at a time.
func TestCopyArtifactFrames(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (2*ArtifactChunkSize+1000)/16)
	var got bytes.Buffer
	n, rerr, werr := CopyArtifact(&got, iotest.HalfReader(bytes.NewReader(data)))
	if n != int64(len(data)) || rerr != nil || werr != nil {
		t.Fatalf("CopyArtifact = %d, %v, %v", n, rerr, werr)
	}
	var want bytes.Buffer
	for rest := data; len(rest) > 0; {
		c := rest[:min(len(rest), ArtifactChunkSize)]
		rest = rest[len(c):]
		WriteResponse(&want, &Response{ArtifactChunk: &Bytes{Data: c}})
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatal("CopyArtifact frames differ from WriteResponse's")
	}
	frames := 0
	for {
		resp, err := ReadResponse(&got)
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil {
			break
		}
		frames++
	}
	if frames != 3 {
		t.Fatalf("%d frames, want 3", frames)
	}

	// An empty source writes nothing.
	got.Reset()
	if n, rerr, werr := CopyArtifact(&got, strings.NewReader("")); n != 0 || rerr != nil || werr != nil || got.Len() != 0 {
		t.Fatalf("empty: %d %v %v %d bytes", n, rerr, werr, got.Len())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("gone") }

func TestCopyArtifactSeparatesReadAndWriteFailures(t *testing.T) {
	boom := errors.New("boom")
	if _, rerr, werr := CopyArtifact(io.Discard, iotest.ErrReader(boom)); !errors.Is(rerr, boom) || werr != nil {
		t.Fatalf("read failure: %v, %v", rerr, werr)
	}
	if _, rerr, werr := CopyArtifact(failingWriter{}, strings.NewReader("x")); rerr != nil || werr == nil {
		t.Fatalf("write failure: %v, %v", rerr, werr)
	}
}

func TestLyingArtifactCountsDoNotAllocate(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, ProtocolVersion, uint16(kArtifactsResponse), []byte{0xff, 0xff, 0xff, 0xff})
	allocs := testing.AllocsPerRun(1, func() {
		if _, err := ReadResponse(bytes.NewReader(buf.Bytes())); err == nil {
			t.Fatal("lying count accepted")
		}
	})
	if allocs > 20 {
		t.Fatalf("%v allocations", allocs)
	}
}
