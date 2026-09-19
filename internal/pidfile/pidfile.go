// Package pidfile is the single-instance guard for huan-agent's long-running
// commands.
//
// The file holds one number: the pid of the process serving this working
// directory. Starting again is therefore a restart rather than a second
// instance — the pid already recorded is stopped first (SIGTERM, then SIGKILL if
// it will not go), and only then is this process's own pid written.
//
// The guard is deliberately a pid file rather than an OS lock. What it has to
// survive is a crash or a SIGKILL, which is exactly the case where an exclusive
// lock would be released automatically and a pid file is proven stale by the
// next start. The cost is that two *simultaneous* cold starts from a state with
// no file are not prevented; a start that finds a pid and a start that finds
// none are, which is every restart.
package pidfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultPath is where a long-running command records the pid serving its
// working directory.
//
// It is a bare, dot-prefixed name in the working directory because the next
// start knows nothing else: the file has to be found from where the operator
// runs, without any state of its own. Every long-running command shares it — see
// the callers — so they have to be started from the same directory for the guard
// to bite.
const DefaultPath = ".huan-agent.pid"

// DefaultTermGrace is how long a previous instance is given to exit on SIGTERM
// before it is killed outright.
//
// It is longer than the admin server's own shutdown budget (five seconds of
// draining plus a second), so a normal restart always takes the graceful path:
// the outgoing process finishes the request it is on and flushes its usage rows.
// A previous instance that ignores SIGTERM costs this much before startup
// continues.
const DefaultTermGrace = 10 * time.Second

// killWait bounds the wait after SIGKILL. A process still visible after it is not
// running code — it is a zombie its parent has not reaped — so waiting longer
// buys nothing.
const killWait = 2 * time.Second

// pollInterval is how often the guard re-asks whether the previous instance is
// gone: short enough that a clean restart is not noticeably slower (a SIGTERM'd
// server exits in milliseconds), long enough not to spin a core.
const pollInterval = 20 * time.Millisecond

// Options configures Acquire. Every zero value takes the matching default.
type Options struct {
	// Path is the pid file. Empty means DefaultPath, relative to the working
	// directory.
	Path string
	// Logger receives what the guard found and did. Nil logs nowhere.
	Logger *zap.Logger
	// TermGrace is how long a previous instance has to exit on SIGTERM before
	// it is killed outright. Zero takes DefaultTermGrace; a negative value means
	// "do not wait at all", which is only ever what a test wants.
	TermGrace time.Duration
}

func (o *Options) applyDefaults() {
	if strings.TrimSpace(o.Path) == "" {
		o.Path = DefaultPath
	}
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.TermGrace == 0 {
		o.TermGrace = DefaultTermGrace
	}
	if o.TermGrace < 0 {
		o.TermGrace = 0
	}
}

// Result describes what Acquire found and what it did about it. It is the record
// a test asserts on and a caller logs; nothing needs it to keep working.
type Result struct {
	// Path is the pid file, resolved to an absolute path.
	Path string
	// PreviousPID is the pid read from the file, or 0 when there was no file or
	// nothing in it that parses as a pid.
	PreviousPID int
	// Stale is true when the file named a pid that no longer exists.
	Stale bool
	// Terminated is true when a previous instance was running and is now
	// stopped.
	Terminated bool
	// Escalated is true when SIGTERM was not enough and SIGKILL was needed.
	Escalated bool
	// Kept is why a live pid was left running instead of being stopped. Non-empty
	// means no signal was sent to it.
	Kept string
}

// Handle is the claim on the pid file. Release it on the way out.
type Handle struct {
	path     string
	pid      int
	released atomic.Bool
}

// Path is the pid file this handle claimed.
func (h *Handle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// PID is the pid recorded in the file.
func (h *Handle) PID() int {
	if h == nil {
		return 0
	}
	return h.pid
}

// Acquire takes the claim: it stops whatever instance the pid file names, then
// records this process.
//
// A pid is only ever signalled when it is verifiably a process of this program.
// A file naming anything else — a recycled pid, a hand-edited file — is left
// alone with a warning and overwritten, because killing an unrelated process is
// far worse than an instance that has to be stopped by hand.
func Acquire(opts Options) (*Handle, Result, error) {
	opts.applyDefaults()

	var res Result
	if abs, err := filepath.Abs(opts.Path); err == nil {
		res.Path = abs
	} else {
		res.Path = opts.Path
	}

	prev, err := read(opts.Path)
	if err != nil {
		return nil, res, err
	}
	res.PreviousPID = prev
	self := os.Getpid()

	switch {
	case prev == 0:
		// No file, or nothing in it: nothing to stop.
	case prev == self:
		// Already ours — a command that claims twice in one process. Writing the
		// pid again is all that is needed.
		opts.Logger.Info("pid file already names this process",
			zap.Int("pid", prev), zap.String("path", res.Path))
	case !alive(prev):
		res.Stale = true
		opts.Logger.Info("previous instance is gone; the pid file was left behind by a crash or a kill",
			zap.Int("pid", prev), zap.String("path", res.Path))
	default:
		name, nameErr := processName(prev)
		if !ourName(name) {
			if nameErr != nil {
				res.Kept = fmt.Sprintf("its command name could not be read (%v)", nameErr)
			} else {
				res.Kept = fmt.Sprintf("it is %q, not this program", name)
			}
			opts.Logger.Warn("pid file names a process that is not huan-agent; leaving it running",
				zap.Int("pid", prev),
				zap.String("reason", res.Kept),
				zap.String("path", res.Path),
				zap.String("hint", "if this file is stale (a pid reused after a crash or a reboot), delete it to silence this warning"),
			)
			break
		}
		escalated, err := terminate(prev, opts.TermGrace, opts.Logger)
		if err != nil {
			return nil, res, fmt.Errorf("pidfile: stop the previous instance (pid %d): %w", prev, err)
		}
		res.Terminated, res.Escalated = true, escalated
	}

	h := &Handle{path: res.Path, pid: self}
	if err := write(h.path, self); err != nil {
		return nil, res, err
	}
	return h, res, nil
}

// Release gives up the claim, removing the pid file.
//
// Only a file that still names this process is removed. An instance we stopped
// has already written its own pid by the time it exits, and deleting that would
// leave a running process with no guard at all — which is the one thing this
// package exists to prevent. Release is idempotent.
func (h *Handle) Release() error {
	if h == nil || !h.released.CompareAndSwap(false, true) {
		return nil
	}
	cur, err := read(h.path)
	if err != nil {
		return err
	}
	if cur != h.pid {
		return nil
	}
	if err := os.Remove(h.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pidfile: remove %s: %w", h.path, err)
	}
	return nil
}

// terminate stops pid: SIGTERM, then SIGKILL once grace has passed. It reports
// whether the escalation was needed.
//
// SIGTERM first is not politeness. The admin server flushes its usage rows and
// closes SQLite on the way out, so a kill outright loses work that a restart has
// no reason to lose; grace is sized so that even a busy drain finishes inside
// it.
func terminate(pid int, grace time.Duration, logger *zap.Logger) (bool, error) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// It exited between the liveness check and the signal.
			return false, nil
		}
		return false, err
	}
	logger.Info("stopping the previous instance",
		zap.Int("pid", pid), zap.Duration("grace", grace))

	if waitForExit(pid, grace) {
		return false, nil
	}

	logger.Warn("previous instance did not exit on SIGTERM; killing it",
		zap.Int("pid", pid), zap.Duration("waited", grace))
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, err
	}
	if !waitForExit(pid, killWait) {
		// Still visible after SIGKILL means it is not running code: it is a
		// zombie whose parent has not reaped it. It holds nothing we need — the
		// listening socket and the database file are released when a process
		// exits — so startup continues rather than failing on a ghost.
		logger.Warn("previous instance is still listed after SIGKILL; it awaits its parent's reap, "+
			"not a running server", zap.Int("pid", pid))
	}
	return true, nil
}

// waitForExit reports whether pid stopped existing before timeout.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !alive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// alive reports whether pid names a process.
//
// Signal 0 runs the kernel's permission and existence checks without delivering
// anything, which is the only portable liveness probe there is. EPERM means the
// process exists but belongs to another user: alive, and certainly not ours to
// stop — the caller's identity check is what keeps it safe.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// read returns the pid the file records, or 0 when the file is absent or holds
// nothing that parses as a pid.
//
// Garbage is not an error. The file is a runtime artifact: a truncated or
// hand-edited one is handled exactly like a stale one, which is to say
// overwritten, because refusing to start over it would be a worse failure than
// the one it reports.
func read(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("pidfile: read %s: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, nil
	}
	return pid, nil
}

// write records pid in the file.
//
// It truncates rather than refusing an existing file: a file left behind by a
// killed instance has to be replaceable, and what makes the guard correct is the
// identity check in Acquire, not the file's existence.
func write(path string, pid int) error {
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("pidfile: write %s: %w", path, err)
	}
	return nil
}

// commandName turns the raw bytes of a process-table command name into the name
// itself, cutting at the first NUL.
//
// Both platforms hand the name over in a fixed-size kernel buffer that is
// terminated by a NUL, and neither clears what follows it: a shorter name taking
// over a slot a longer one occupied earlier comes back with the old name's tail
// still attached (on darwin, a `tmp_commprobe` followed by a `sleep` on the same
// pid reads back as "sleep\x00mmprobe"). Only the bytes before the NUL are the
// name, so trimming the padding alone is not enough — it would leave the residue
// in place and hand it to the identity check.
func commandName(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// selfName is how this program appears in a process table: the base name of the
// running executable, falling back to the product name when the path cannot be
// read.
var selfName = executableName()

func executableName() string {
	p, err := os.Executable()
	if err != nil || strings.TrimSpace(p) == "" {
		return "huan-agent"
	}
	return filepath.Base(p)
}

// ourName reports whether a process-table command name belongs to this program.
//
// The name is taken from the running executable rather than hardcoded, so a
// binary built or renamed to something else still recognises its own instances.
// The product name is accepted as an alias because a process started by `go run`
// or `go test` carries a name of its own, and a process table truncates the name
// (16 characters on darwin), so a long binary name has to be matched by prefix.
//
// Only the bytes before the first NUL are considered. Both platforms hand the
// name over in a fixed-size buffer and truncate at a NUL, so anything the kernel
// left behind after it — a longer name that occupied the same slot earlier — is
// not part of the name. Reading past it would both print it and let the prefix
// match below run against the tail of an unrelated name.
func ourName(name string) bool {
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return false
	}
	if name == selfName {
		return true
	}
	return strings.HasPrefix(name, "huan-agent") || strings.HasPrefix(selfName, name)
}
