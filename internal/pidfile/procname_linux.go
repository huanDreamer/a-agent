//go:build linux

package pidfile

import (
	"fmt"
	"os"
)

// This file is the platform seam for reading a process's command name, in the
// same spirit as internal/jobs/sysproc.go: everything the guard's identity check
// depends on lives behind processName, with one file per platform.

// processName returns the command name the kernel recorded for pid.
//
// /proc/<pid>/comm holds the executable's base name, truncated to TASK_COMM_LEN
// (16) bytes including the newline — the same truncation darwin applies, which
// is why the caller matches names by prefix as well as exactly.
func processName(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", fmt.Errorf("read /proc/%d/comm: %w", pid, err)
	}
	// Cut at the first NUL and drop the trailing newline. The kernel writes the
	// name into a fixed-size buffer, so the residue darwin leaves behind is
	// possible here too; commandName handles both.
	return commandName(b), nil
}
