package daemon

import "golang.org/x/sys/unix"

// physicalMemory is the machine's RAM in bytes, or 0 if unknown.
func physicalMemory() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
