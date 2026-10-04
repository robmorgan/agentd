//go:build linux

package procstat

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the unit of the CPU times in /proc/<pid>/stat. The kernel fixes
// it at 100 for userspace on every architecture Go supports.
const userHZ = 100

func read(pid int) (Process, error) {
	dir := fmt.Sprintf("/proc/%d", pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return Process{}, fmt.Errorf("process %d: %w", pid, err)
	}
	// The command name (field 2) is in parentheses and may contain spaces
	// or parentheses itself, so fields are counted from the last ')'.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return Process{}, fmt.Errorf("process %d: malformed stat", pid)
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is field 3 (state): utime is field 14, stime 15,
	// num_threads 20, rss (pages) 24.
	if len(fields) < 22 {
		return Process{}, fmt.Errorf("process %d: short stat", pid)
	}
	num := func(i int) uint64 {
		v, _ := strconv.ParseUint(fields[i-3], 10, 64)
		return v
	}
	p := Process{
		CPUUser:   time.Duration(num(14)) * time.Second / userHZ,
		CPUSystem: time.Duration(num(15)) * time.Second / userHZ,
		Threads:   int(num(20)),
		RSS:       num(24) * uint64(os.Getpagesize()),
	}
	if f, err := os.Open(dir + "/smaps_rollup"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			key, rest, ok := strings.Cut(sc.Text(), ":")
			if !ok || (key != "Private_Clean" && key != "Private_Dirty") {
				continue
			}
			kb, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			p.Private += kb * 1024
		}
		f.Close()
	}
	if entries, err := os.ReadDir(dir + "/fd"); err == nil {
		p.FDs = len(entries)
		if pid == os.Getpid() {
			p.FDs-- // the directory being read
		}
	}
	return p, nil
}
