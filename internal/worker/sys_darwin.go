package worker

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// waitReadable blocks until fd (the PTY master) has something to read (or
// is hung up), or wake becomes readable. It reports whether fd is ready.
// macOS's poll(2) and kqueue do not support terminal devices, so this uses
// select(2), as tmux does there.
func waitReadable(fd, wake int) (bool, error) {
	if fd >= unix.FD_SETSIZE || wake >= unix.FD_SETSIZE {
		return false, fmt.Errorf("descriptor %d is too large for select", max(fd, wake))
	}
	for {
		var r unix.FdSet
		r.Set(fd)
		r.Set(wake)
		_, err := unix.Select(max(fd, wake)+1, &r, nil, nil, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		return r.IsSet(fd), nil
	}
}

// szomb is a zombie process's p_stat (sys/proc.h).
const szomb = 5

// waitExited blocks until the child pid has exited, without reaping it, so
// that whoever reaps it (see watchAgent) decides when. kqueue's NOTE_EXIT
// fires on exit; a child that had already exited is found as a zombie.
func waitExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	unix.CloseOnExec(kq)
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil // already exited
		}
		return err
	}
	if p, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil && p.Proc.P_stat == szomb {
		return nil
	}
	out := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, out, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n > 0 {
			return err
		}
	}
}
