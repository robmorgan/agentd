package worker

import (
	"bytes"
	"compress/gzip"
	"testing"

	"go.mitchellh.com/libghostty"
)

// BenchmarkSnapshotFormat compares the two ways a client could be given a
// session's terminal state on attach: the VT snapshot the worker sends today
// (formatted here, replayed into a fresh terminal by the client) and
// libghostty's binary snapshot (encoded here, decoded into a terminal by a
// client that embeds libghostty). Steps:
//
//	vt-format      ts.snapshot(), what the worker does per attach
//	vt-restore     a fresh terminal plus VTWrite of the snapshot
//	binary-encode  Terminal.Snapshot()
//	binary-decode  SnapshotDecoder.Decode() into a new terminal
//
// Each reports the payload size and its gzip size (a transfer could be
// compressed; neither is today).
func BenchmarkSnapshotFormat(b *testing.B) {
	type session struct {
		name       string
		cols, rows uint16
		data       []byte
	}
	flood := func(n int, after string) []byte {
		chunk := floodChunk()
		var data []byte
		for range n {
			data = append(data, chunk...)
		}
		return append(data, after...)
	}
	sessions := []session{
		{"idle", defaultPtyCols, defaultPtyRows, []byte("$ ")},
		// 1 MB of line output fills the scrollback limit at 160 columns.
		{"full-scrollback", defaultPtyCols, defaultPtyRows, flood(128, "")},
		{"full-scrollback-alternate", defaultPtyCols, defaultPtyRows, flood(128, "\x1b[?1049h\x1b[2Jan editor\x1b[H")},
	}
	for _, s := range loadStreams(b) {
		data := s.data
		if i := bytes.LastIndex(data, []byte("\x1b[?1049l")); i > 0 {
			data = data[:i]
		}
		sessions = append(sessions, session{s.name, streamCols, streamRows, data})
	}

	for _, s := range sessions {
		ts, err := newTerminalState(s.cols, s.rows, maxScrollbackBytes)
		if err != nil {
			b.Fatal(err)
		}
		ts.feed(s.data)
		vt, err := ts.snapshot(allScrollback)
		if err != nil {
			b.Fatal(err)
		}
		bin, err := ts.term.Snapshot()
		if err != nil {
			b.Fatal(err)
		}

		b.Run(s.name+"/vt-format", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ts.snapshot(allScrollback); err != nil {
					b.Fatal(err)
				}
			}
			reportSizes(b, vt)
		})
		b.Run(s.name+"/vt-restore", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				client, err := newClientTerminal(s.cols, s.rows)
				if err != nil {
					b.Fatal(err)
				}
				client.VTWrite([]byte("\x1b[2J\x1b[H"))
				client.VTWrite(vt)
				client.Close()
			}
			reportSizes(b, vt)
		})
		b.Run(s.name+"/binary-encode", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ts.term.Snapshot(); err != nil {
					b.Fatal(err)
				}
			}
			reportSizes(b, bin)
		})
		b.Run(s.name+"/binary-decode", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				dec, err := libghostty.NewSnapshotDecoderBytes(bin)
				if err != nil {
					b.Fatal(err)
				}
				term, err := dec.Decode()
				if err != nil {
					b.Fatal(err)
				}
				term.Close()
				dec.Close()
			}
			reportSizes(b, bin)
		})
		ts.close()
	}
}

func reportSizes(b *testing.B, payload []byte) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(payload)
	zw.Close()
	b.ReportMetric(float64(len(payload)), "bytes")
	b.ReportMetric(float64(buf.Len()), "gzip-bytes")
}
