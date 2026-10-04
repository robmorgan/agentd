package daemon

import "golang.org/x/sys/unix"

// physicalMemory is the machine's RAM in bytes, or 0 if unknown.
func physicalMemory() uint64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
