//go:build darwin && cgo

package procstat

/*
#include <errno.h>
#include <stdlib.h>
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <mach/mach_time.h>

// fd_count returns the number of open file descriptors of pid, or -1.
// PROC_PIDLISTFDS with no buffer only estimates (it reports the size of
// the descriptor table, with slack), so the list is fetched and counted.
static int agentd_fd_count(int pid) {
	int size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (size <= 0) return -1;
	size += 32 * (int)sizeof(struct proc_fdinfo);
	struct proc_fdinfo *fds = malloc(size);
	if (fds == NULL) return -1;
	int n = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, size);
	free(fds);
	if (n <= 0) return -1;
	return n / (int)sizeof(struct proc_fdinfo);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// timebase converts the Mach absolute time units libproc reports CPU time
// in to nanoseconds. They are nanoseconds on Intel but not on Apple
// silicon (125/3 ns per unit there).
var timebase = sync.OnceValue(func() C.mach_timebase_info_data_t {
	var tb C.mach_timebase_info_data_t
	C.mach_timebase_info(&tb)
	if tb.denom == 0 {
		tb.numer, tb.denom = 1, 1
	}
	return tb
})

func machToDuration(v C.uint64_t) time.Duration {
	tb := timebase()
	return time.Duration(uint64(v) * uint64(tb.numer) / uint64(tb.denom))
}

func read(pid int) (Process, error) {
	var ti C.struct_proc_taskinfo
	n := C.proc_pidinfo(C.int(pid), C.PROC_PIDTASKINFO, 0, unsafe.Pointer(&ti), C.int(unsafe.Sizeof(ti)))
	if int(n) != int(unsafe.Sizeof(ti)) {
		return Process{}, fmt.Errorf("process %d: no task info (exited, or not ours)", pid)
	}
	p := Process{
		RSS:       uint64(ti.pti_resident_size),
		CPUUser:   machToDuration(ti.pti_total_user),
		CPUSystem: machToDuration(ti.pti_total_system),
		Threads:   int(ti.pti_threadnum),
	}
	var ru C.struct_rusage_info_v2
	if C.proc_pid_rusage(C.int(pid), C.RUSAGE_INFO_V2, (*C.rusage_info_t)(unsafe.Pointer(&ru))) == 0 {
		p.Private = uint64(ru.ri_phys_footprint)
	}
	if fds := C.agentd_fd_count(C.int(pid)); fds >= 0 {
		p.FDs = int(fds)
	}
	return p, nil
}
