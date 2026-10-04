package worker

import (
	"os"
	"strconv"
	"strings"
)

// processName is the short command name of pid ("" if unknown).
func processName(pid int) string {
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(comm))
}
