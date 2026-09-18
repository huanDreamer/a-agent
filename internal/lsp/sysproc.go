package lsp

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// This file is the platform seam for process handling, in the same spirit as
// internal/jobs/sysproc.go: everything that depends on the operating system
// lives here, so a port has one file to write.

// detachAttr puts the language server in its own session and process group.
//
// A language server may spawn helpers (gopls starts a `go list` and, for the
// first run, a toolchain download). Killing only the process we started leaves
// those running with nothing left to stop them, so the server is started as a
// group leader and the whole group is signalled.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess stops a language server and everything it started, giving it
// a grace period to exit on SIGTERM first.
//
// SIGTERM before SIGKILL is not politeness: a server asked to stop flushes its
// caches, and a server killed outright leaves a lock file behind that makes the
// next start slower (or fails).
func terminateProcess(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// A negative pid targets the group.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()

	if grace <= 0 {
		grace = 2 * time.Second
	}
	select {
	case <-done:
		return
	case <-time.After(grace):
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// processAlive reports whether the group still exists. A language server that
// exited on its own is a normal event (a crash, a version mismatch); the manager
// uses this to tell "still working" from "gone" without waiting on Wait.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
