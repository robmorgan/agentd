package worker

import (
	"fmt"

	"go.mitchellh.com/libghostty"
)

// terminalState wraps a libghostty terminal that shadows the PTY stream so a
// reattaching client can be rehydrated with a snapshot of the screen.
type terminalState struct {
	term    *libghostty.Terminal
	pending [][]byte
	size    libghostty.SizeReportSize
}

func newTerminalState(cols, rows uint16, maxScrollbackBytes uint) (*terminalState, error) {
	s := &terminalState{size: libghostty.SizeReportSize{Rows: rows, Columns: cols}}
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackBytes(maxScrollbackBytes),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			s.pending = append(s.pending, append([]byte(nil), data...))
		}),
		libghostty.WithSizeReport(func(_ *libghostty.Terminal) (libghostty.SizeReportSize, bool) {
			return s.size, true
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create terminal state: %w", err)
	}
	s.term = term
	return s, nil
}

// feed parses PTY output and returns any responses the terminal wants to
// write back to the PTY (mode reports, size reports, and so on).
func (s *terminalState) feed(data []byte) [][]byte {
	s.term.VTWrite(data)
	writes := s.pending
	s.pending = nil
	return writes
}

func (s *terminalState) resize(cols, rows uint16, cellWidthPx, cellHeightPx uint32) error {
	s.size = libghostty.SizeReportSize{Rows: rows, Columns: cols, CellWidth: cellWidthPx, CellHeight: cellHeightPx}
	return s.term.Resize(cols, rows, cellWidthPx, cellHeightPx)
}

func (s *terminalState) format(vt bool) ([]byte, error) {
	format := libghostty.FormatterFormatPlain
	if vt {
		format = libghostty.FormatterFormatVT
	}
	f, err := libghostty.NewFormatter(s.term,
		libghostty.WithFormatterFormat(format),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterUnwrap(false),
		libghostty.WithFormatterExtraPalette(true),
		libghostty.WithFormatterExtraModes(true),
		libghostty.WithFormatterExtraScrollingRegion(true),
		libghostty.WithFormatterExtraKeyboard(true),
		libghostty.WithFormatterExtraCursor(true),
		libghostty.WithFormatterExtraStyle(true),
		libghostty.WithFormatterExtraHyperlink(true),
		libghostty.WithFormatterExtraProtection(true),
		libghostty.WithFormatterExtraKittyKeyboard(true),
		libghostty.WithFormatterExtraCharsets(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create formatter: %w", err)
	}
	defer f.Close()
	return f.Format()
}

// scrollbackRows is the number of rows of history above the screen.
func (s *terminalState) scrollbackRows() uint64 {
	n, err := s.term.ScrollbackRows()
	if err != nil {
		return 0
	}
	return uint64(n)
}

func (s *terminalState) close() {
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
}
