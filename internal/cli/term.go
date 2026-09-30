package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// rawMode is a terminal switched to raw mode; restore puts it back.
type rawMode struct {
	fd   int
	orig *unix.Termios
}

// enterRaw puts stdin's terminal into raw mode, as cfmakeraw does. With
// attach set it also turns off the literal-next and quit characters, so
// Ctrl-V and Ctrl-\ reach the attach input parser (and through it the
// agent) instead of the line discipline. Without a terminal it does
// nothing, so piped input still works.
func enterRaw(attach bool) (*rawMode, error) {
	fd := int(os.Stdin.Fd())
	orig, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return &rawMode{fd: fd}, nil
	}
	raw := *orig
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if attach {
		raw.Cc[unix.VLNEXT] = posixVDisable
		raw.Cc[unix.VQUIT] = posixVDisable
	}
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	return &rawMode{fd: fd, orig: orig}, nil
}

// restore puts the terminal back, discarding input not yet read (as
// TCSAFLUSH does), so keys typed at the agent are not replayed to the shell.
func (m *rawMode) restore() {
	if m != nil && m.orig != nil {
		unix.IoctlSetTermios(m.fd, ioctlSetTermiosFlush, m.orig)
		m.orig = nil
	}
}

// geometry is the terminal size sent with attach and resize requests. The
// pixel size lets agents that draw images size them; 0 means unknown.
func terminalGeometry() (cols, rows, pixelWidth, pixelHeight uint16) {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 && ws.Row > 0 {
			return ws.Col, ws.Row, ws.Xpixel, ws.Ypixel
		}
	}
	return 80, 24, 0, 0
}

// stdoutWidth is the width of the terminal stdout writes to, or 0 when it
// is not a terminal.
func stdoutWidth() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

func stdoutIsTerminal() bool { return isTerminal(os.Stdout) }
