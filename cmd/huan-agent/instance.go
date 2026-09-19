package main

// The single-instance guard for the long-running commands.
//
// Two servers competing for one port is not a race anyone wins: the second one
// fails at bind, and if it got past that it would fight the first over the
// SQLite file. So a start is a restart — whatever instance the pid file names is
// stopped first, then this process takes over. See internal/pidfile for what
// "stopped" means and why the file is verified before anything is signalled.

import (
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/pidfile"
)

// pidFilePath is where the running instance records its pid. It is a flag rather
// than a config key because it describes where the process was started from, not
// how it behaves.
var pidFilePath string

func init() {
	rootCmd.PersistentFlags().StringVar(&pidFilePath, "pid-file", pidfile.DefaultPath,
		"file recording the running instance's pid; an instance found there is stopped before this one starts")
}

// claimInstance stops the instance the pid file names, if any, and records this
// process in it.
//
// Call it before the store is opened and before the port is bound: the previous
// instance holds exactly those two things, and the point of stopping it is to
// have them free by the time this process asks for them.
func claimInstance(logger *zap.Logger) (*pidfile.Handle, error) {
	h, res, err := pidfile.Acquire(pidfile.Options{Path: pidFilePath, Logger: logger})
	if err != nil {
		return nil, err
	}
	switch {
	case res.Kept != "":
		// Acquire already warned. Repeating it here would say the same thing
		// twice in one startup.
	case res.Terminated:
		logger.Info("previous instance stopped; this process is taking over",
			zap.Int("previous_pid", res.PreviousPID),
			zap.Bool("killed", res.Escalated))
	case res.Stale:
		logger.Info("no instance was running; the pid file was stale",
			zap.Int("previous_pid", res.PreviousPID))
	}
	logger.Info("instance registered",
		zap.Int("pid", h.PID()), zap.String("pid_file", h.Path()))
	return h, nil
}

// releaseInstance gives up the claim on the way out. A failure is logged rather
// than returned: the process is exiting either way, and a pid file that outlives
// it is handled by the next start as a stale one.
func releaseInstance(h *pidfile.Handle, logger *zap.Logger) {
	if h == nil {
		return
	}
	if err := h.Release(); err != nil {
		logger.Warn("could not remove the pid file", zap.Error(err))
	}
}
