package jobs

import (
	"errors"
	"syscall"
)

// This file is the platform seam for process handling, in the same spirit as
// bashShell in the bash tool: everything that depends on the operating system
// lives here, so a port has one file to write and the manager stays readable.
//
// darwin and linux share these three calls; anything else needs its own file
// (and, on Windows, its own idea of a process group, which is a job object).

// detachAttr puts the child in a new session.
//
// setsid is the whole mechanism behind "the agent's signals cannot kill your dev
// server": the job leaves the agent's process group and session, so a group
// signal aimed at the agent (or at another job) does not reach it, and it has no
// controlling terminal to be logged out from. It is also why Close has to be the
// one that terminates jobs — nothing else is going to.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// killGroup signals a whole process group, falling back to the group leader when
// the group itself refuses the signal.
//
// Signalling -pgid rather than pgid is the point: a dev server forks workers, so
// killing only the shell leaves the server running with nothing left to stop it.
func killGroup(pgid int, sig syscall.Signal) {
	if pgid <= 0 {
		return
	}
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pgid, sig)
	}
}

// groupIsGone reports whether a process group has no members left.
//
// kill(-pgid, 0) is the probe: a process group exists exactly as long as it has
// a member. Once the leader has been reaped there is a theoretical pid-reuse race
// — the id could name an unrelated, brand-new group — but the probe that matters
// runs within microseconds of the reap, and the alternative is reporting a server
// that is still listening as finished.
func groupIsGone(pgid int) bool {
	if pgid <= 0 {
		return true
	}
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return false
	}
	// EPERM means something is there but not ours to signal: not gone.
	return errors.Is(err, syscall.ESRCH)
}
