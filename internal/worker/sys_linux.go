package worker

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitReadable blocks until fd (the PTY master) has something to read (or
// is hung up), or wake becomes readable. It reports whether fd is ready.
// Linux's poll(2) supports PTYs.
func waitReadable(fd, wake int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		// POLLHUP and POLLERR count as ready: the read reports them.
		return fds[0].Revents != 0, nil
	}
}

// waitExited blocks until the child pid has exited, without reaping it
// (WNOWAIT), so that whoever reaps it (see watchAgent) decides when.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
