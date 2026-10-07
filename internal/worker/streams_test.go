package worker

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.mitchellh.com/libghostty"
)

// The recorded streams in testdata/streams are real PTY output from Claude
// Code, Codex, a shell, Vim, Neovim, go test and clang, recorded at
// streamCols x streamRows (see testdata/streams/README.md).
const (
	streamCols = 120
	streamRows = 40
)

type recordedStream struct {
	name string
	data []byte
}

func loadStreams(t testing.TB) []recordedStream {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "streams", "*.vt.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no recorded streams in testdata/streams")
	}
	var out []recordedStream
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(zr)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, recordedStream{name: strings.TrimSuffix(filepath.Base(p), ".vt.gz"), data: data})
	}
	return out
}

// replayCuts scales how many places TestSnapshotRestoresRecordedStreams
// cuts each stream at: about four times this many (see cutPoints). The
// default keeps the test quick under -race; raise it for a thorough run:
//
//	go test ./internal/worker -run RecordedStreams -replay-cuts 200
var replayCuts = flag.Int("replay-cuts", 6, "cut each recorded stream at about four times this many places")

// cutPoints picks where to interrupt a stream: both ends, evenly spaced
// points, and random points inside escape sequences and inside multi-byte
// UTF-8 characters.
func cutPoints(data []byte, n int) []int {
	rng := rand.New(rand.NewPCG(1, uint64(len(data))))
	cuts := []int{0, len(data)}
	for i := 1; i < n; i++ {
		cuts = append(cuts, len(data)*i/n)
	}
	if len(data) == 0 {
		return cuts
	}
	// Just after an ESC, two bytes into a sequence, and after a UTF-8 lead
	// byte, each found by searching forward from a random point.
	for range n {
		from := rng.IntN(len(data))
		if i := bytes.IndexByte(data[from:], 0x1b); i >= 0 {
			cuts = append(cuts, min(from+i+1, len(data)), min(from+i+2, len(data)))
		}
		if i := bytes.IndexFunc(data[from:], func(r rune) bool { return r > 0x7f }); i >= 0 {
			cuts = append(cuts, from+i+1)
		}
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}

// checkReplay plays a client reattaching partway through a stream: the
// worker's terminal sees data up to cut, the client gets the snapshot taken
// there and then the rest of the stream as live output. The client must
// match the worker's terminal right after the snapshot and again at the
// end, apart from what TestSnapshotKnownLimits shows is not restored.
//
// A snapshot is only ever taken with the parser between sequences (see
// ownerState.atGround), so a cut inside a sequence moves forward to where
// that sequence ends, as it does in the worker.
func checkReplay(data []byte, cut int) error {
	ts, err := newTerminalState(streamCols, streamRows, maxScrollbackBytes)
	if err != nil {
		return err
	}
	defer ts.close()
	ts.feed(data[:cut])
	if !ts.atGround() {
		n, _, _ := ts.feedUntilGround(data[cut:])
		cut += n
	}
	snap, err := ts.snapshot(allScrollback)
	if err != nil {
		return err
	}
	client, err := newClientTerminal(streamCols, streamRows)
	if err != nil {
		return err
	}
	defer client.Close()
	client.VTWrite([]byte("\x1b[2J\x1b[H"))
	client.VTWrite(snap)
	if d := diffRestored(ts.term, client); d != "" {
		return fmt.Errorf("restored at byte %d:\n%s", cut, d)
	}

	ts.feed(data[cut:])
	client.VTWrite(data[cut:])
	if d := diffRestored(ts.term, client); d != "" {
		return fmt.Errorf("restored at byte %d, then differs at the end of the stream:\n%s", cut, d)
	}
	return nil
}

// diffRestored compares a restored terminal with the original, leaving out
// what TestSnapshotKnownLimits shows a snapshot cannot restore.
func diffRestored(orig, restored *libghostty.Terminal) string {
	return diffImages(withoutKnownLimits(imageOf(orig)), withoutKnownLimits(imageOf(restored)))
}

func TestSnapshotRestoresRecordedStreams(t *testing.T) {
	n := *replayCuts
	if testing.Short() {
		n = max(n/4, 1)
	}
	for _, s := range loadStreams(t) {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			failures := 0
			for _, cut := range cutPoints(s.data, n) {
				if err := checkReplay(s.data, cut); err != nil {
					t.Errorf("cut at byte %d of %d: %v", cut, len(s.data), err)
					if failures++; failures == 3 {
						t.FailNow()
					}
				}
			}
		})
	}
}

// TestRecordedStreamsAreRealistic guards the fixtures themselves: each
// stream should exercise what it was recorded for.
func TestRecordedStreamsAreRealistic(t *testing.T) {
	want := map[string][]string{
		"claude":  {"\x1b[?2026h", "/help"},
		"codex":   {"\x1b[?2026h", "/model"},
		"shell":   {"\x1b[1;32mdemo", "\x1b[K"},
		"vim":     {"\x1b[?1049h", "\x1b[?1049l"},
		"nvim":    {"\x1b[?1049h", "\x1b[?1049l"},
		"go-test": {"--- FAIL", "--- PASS", "panic"},
		"clang":   {"\x1b[0;1;31merror: "},
	}
	streams := loadStreams(t)
	for _, s := range streams {
		for _, w := range want[s.name] {
			if !bytes.Contains(s.data, []byte(w)) {
				t.Errorf("%s: stream lacks %q", s.name, w)
			}
		}
		delete(want, s.name)
	}
	for name := range want {
		t.Errorf("missing recorded stream %s", name)
	}
}
