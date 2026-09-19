//go:build darwin

package pidfile

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// This file is the platform seam for reading a process's command name, in the
// same spirit as internal/jobs/sysproc.go: everything the guard's identity check
// depends on lives behind processName, with one file per platform.

// processName returns the command name the kernel recorded for pid.
//
// On darwin that name is the executable's base name truncated to MAXCOMLEN (16)
// bytes, which is why the caller matches names by prefix as well as exactly: the
// truncation is invisible in the file it came from.
func processName(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	// P_comm is a fixed-size array holding a NUL-terminated name; the kernel
	// does not necessarily clear the rest of it, which is why the read goes
	// through commandName rather than trimming the padding.
	return commandName(kp.Proc.P_comm[:]), nil
}
