package cli

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios      = unix.TIOCGETA
	ioctlSetTermios      = unix.TIOCSETA
	ioctlSetTermiosFlush = unix.TIOCSETAF
	posixVDisable        = 0xff
)
