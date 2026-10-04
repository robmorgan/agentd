package worker

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// processName is the short command name of pid ("" if unknown).
func processName(pid int) string {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ""
	}
	comm := info.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return string(comm)
}
