package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/workspace"
)

// BashPolicy bounds what the shell tool may do.
//
// "Bounds" is the honest word: only the working directory is confined. The
// command itself runs as an ordinary child of this process, with the full
// rights of the user the agent runs as, so it can read and write anything that
// user can (inside the workspace or not) and can use the network. Real
// confinement needs a kernel facility — namespaces, seatbelt, a container —
// which this package does not have, and no comment or tool description here may
// pretend otherwise.
type BashPolicy struct {
	// Enabled turns the tool on at all. When false, every call is refused.
	Enabled bool

	// Timeout bounds one command. <=0 uses DefaultBashTimeout.
	Timeout time.Duration

	// MaxOutputBytes caps captured stdout and stderr separately. <=0 uses
	// DefaultBashMaxOutputBytes.
	MaxOutputBytes int

	// DenyPatterns are RE2 regular expressions matched against the command
	// string; a match refuses the call before anything runs.
	//
	// This is a speed bump for obviously destructive commands, not a security
	// boundary: an LLM (or anyone else) evades it trivially with globs, $IFS,
	// base64, an indirection through a variable, or a script it wrote earlier.
	// It exists to stop the careless catastrophe, not the determined one, and it
	// must never be described as preventing arbitrary commands — least of all as
	// preventing network access, which it cannot do at all.
	DenyPatterns []string
}

const (
	// DefaultBashTimeout bounds one command when the policy does not say.
	DefaultBashTimeout = 120 * time.Second
	// DefaultBashMaxOutputBytes caps captured stdout and stderr separately.
	DefaultBashMaxOutputBytes = 128 << 10 // 128 KiB
)

const (
	// bashWaitDelay bounds how long Wait keeps waiting for the output pipes
	// after the shell has exited. Without it, a grandchild that inherited the
	// pipes and escaped the process-group kill would hang the call forever;
	// with it, Wait gives up, closes the pipes and reports ErrWaitDelay, which
	// the timeout path already knows how to report.
	bashWaitDelay = 2 * time.Second

	// bashTruncatedNoteFmt is appended to a stream that hit the output cap. It
	// names the byte counts because "truncated" alone leaves the model unable
	// to tell whether it is missing two lines or two megabytes.
	bashTruncatedNoteFmt = "\n[bash] output truncated: kept the first %d bytes, discarded %d bytes\n"

	// bashTimeoutNoteFmt is appended to stderr when the timeout fired, so the
	// model learns why the output stops mid-stream instead of receiving a bare
	// context error with no output attached. It reports the time actually spent
	// as well as the configured limit, because a caller's own deadline can cut a
	// call short before this tool's limit is reached, and a note naming only the
	// limit would then describe something that never happened.
	bashTimeoutNoteFmt = "\n[bash] timed out after %s and the whole process group was killed; the output above is partial (the limit configured for one command is %s)\n"
)

// bashDefaultDenyPatterns is the list DefaultBashPolicy ships. It is short and
// literal on purpose: a cleverer pattern (say, one that tries to catch every
// spelling of a recursive delete) either misses the dangerous forms or starts
// refusing ordinary work, and it would still be evaded by anyone who cared.
var bashDefaultDenyPatterns = []string{
	`\brm\s+(-[A-Za-z]+\s+)*/(\s|$|\*)`,   // rm -rf / and friends
	`\bmkfs(\.[A-Za-z0-9]+)?\b`,           // reformat a filesystem
	`\bdd\b[^\n]*\bof=/dev/`,              // write raw bytes to a device
	`:\(\)\s*\{`,                          // the classic fork bomb shape
	`\b(shutdown|reboot|halt|poweroff)\b`, // take the machine down
}

// DefaultBashPolicy is the policy a caller gets when it has no opinion: enabled,
// with the default timeout and output cap, plus the small conservative deny list
// above.
func DefaultBashPolicy() BashPolicy {
	return BashPolicy{
		Enabled:        true,
		Timeout:        DefaultBashTimeout,
		MaxOutputBytes: DefaultBashMaxOutputBytes,
		DenyPatterns:   append([]string(nil), bashDefaultDenyPatterns...),
	}
}

// BashInput is the parameter schema for the "bash" tool.
type BashInput struct {
	Command string `json:"command" jsonschema:"description=Shell command to run. Passed verbatim to 'sh -c' with no quoting added by the tool, required"`
	// The optional fields carry json:",omitempty" so the generated schema does
	// not advertise them as required — the reflector treats a field without
	// omitempty as required, and telling a model to always send a cwd would
	// only invite invented values.
	Description string `json:"description,omitempty" jsonschema:"description=Short note about what this command is for. Echoed back in the result so the transcript explains itself"`
	Cwd         string `json:"cwd,omitempty" jsonschema:"description=Directory to run in relative to the workspace root. Defaults to the workspace root"`
	TimeoutMS   int    `json:"timeout_ms,omitempty" jsonschema:"description=Timeout for this call in milliseconds. May only shorten the configured default and never extend it"`
}

// BashOutput is what the bash tool returns.
//
// Description is echoed back because the model wrote it to explain itself, and
// the reason for a command is worth more next to its result than in the
// argument it was originally sent in. It is omitted when empty so the shape
// stays the documented one for callers that never send it.
type BashOutput struct {
	Command         string `json:"command"`
	Description     string `json:"description,omitempty"`
	Cwd             string `json:"cwd"`
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	DurationMS      int64  `json:"duration_ms"`
	TimedOut        bool   `json:"timed_out"`
}

// bashDenyRule keeps the configured pattern text next to its compiled form, so a
// refusal can quote back the pattern the operator actually wrote rather than a
// regular expression the model cannot connect to anything.
type bashDenyRule struct {
	pattern string
	re      *regexp.Regexp
}

// bashConfig is the validated snapshot of a BashPolicy: defaults applied and
// deny patterns compiled once at construction instead of on every call.
type bashConfig struct {
	enabled   bool
	timeout   time.Duration
	maxOutput int
	deny      []bashDenyRule
}

// NewBashTool returns a command-execution tool whose working directory is
// confined to ws.
//
// Confinement means the working directory, nothing more: every call either
// names a cwd inside the workspace or takes the root as its default, and the
// tool refuses to run with a directory outside. The command itself is not
// jailed — see BashPolicy for what that does and does not buy.
func NewBashTool(ws *workspace.Workspace, policy BashPolicy) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("bash: a workspace is required")
	}

	cfg, err := bashConfigFrom(policy)
	if err != nil {
		return nil, err
	}

	return utils.InferTool("bash", bashDescription(cfg.timeout, cfg.maxOutput),
		func(ctx context.Context, in BashInput) (BashOutput, error) {
			return bashRun(ctx, ws, cfg, in)
		})
}

// bashDescription builds the model-facing description around the limits this
// tool was really built with, so a deployment that changed the timeout or the
// cap does not leave the model reading about defaults that no longer apply.
func bashDescription(timeout time.Duration, maxOutput int) string {
	return "Run a shell command and return its exit code, stdout and stderr. " +
		"Use it for builds, test suites, git and package managers while working inside a repository. " +
		"A non-zero exit status is reported in exit_code, not as an error. " +
		"The working directory is confined to the workspace: cwd must name a directory inside it (default: the workspace root) and PWD is set to it. " +
		"That confines the working directory only, not the command: it is not sandboxed and runs with the full rights of the agent's user, so it can read and write files anywhere that user can — inside the workspace or outside it — and it can reach the network. " +
		"Commands matching a configured deny pattern are refused before they run, but that list is a speed bump against obviously destructive commands, not a security boundary, and it is easy to evade. " +
		fmt.Sprintf("stdout and stderr are captured separately and each is capped at %d bytes; when the cap is hit the head is kept and an explicit marker reports how many bytes were discarded. ", maxOutput) +
		fmt.Sprintf("Commands are killed after %s; timeout_ms can shorten that for one call but never extend it. ", timeout) +
		"Standard input is /dev/null, so a command that prompts for input fails instead of hanging. " +
		"Anything still running in the process group the command started is killed when the call ends, so do not rely on a background process outliving it."
}

// bashConfigFrom validates a policy and applies the defaults.
func bashConfigFrom(policy BashPolicy) (bashConfig, error) {
	cfg := bashConfig{
		enabled:   policy.Enabled,
		timeout:   policy.Timeout,
		maxOutput: policy.MaxOutputBytes,
	}
	if cfg.timeout <= 0 {
		cfg.timeout = DefaultBashTimeout
	}
	if cfg.maxOutput <= 0 {
		cfg.maxOutput = DefaultBashMaxOutputBytes
	}
	for _, pattern := range policy.DenyPatterns {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return bashConfig{}, fmt.Errorf("bash: deny pattern %q: %w", pattern, err)
		}
		cfg.deny = append(cfg.deny, bashDenyRule{pattern: pattern, re: re})
	}
	return cfg, nil
}

// bashRun executes one command and shapes the result for the model.
//
// Every return before the shell is started happens while nothing has been
// executed, so a refusal — read-only workspace, disabled tool, denied pattern,
// unusable cwd — never has a side effect the caller has to undo.
func bashRun(ctx context.Context, ws *workspace.Workspace, cfg bashConfig, in BashInput) (BashOutput, error) {
	if ws == nil {
		// NewBashTool already refuses a nil workspace; this is the belt to that
		// pair of braces, because a panic inside the agent loop costs a whole
		// conversation and an error costs one tool call.
		return BashOutput{}, fmt.Errorf("bash: a workspace is required")
	}

	command := in.Command
	if strings.TrimSpace(command) == "" {
		return BashOutput{}, fmt.Errorf("bash: command is required")
	}
	out := BashOutput{Command: command, Description: strings.TrimSpace(in.Description)}

	if ws.ReadOnly() {
		return BashOutput{}, fmt.Errorf("bash: %w: no command was run", workspace.ErrReadOnly)
	}
	if !cfg.enabled {
		return BashOutput{}, fmt.Errorf("bash: command execution is disabled by policy (BashPolicy.Enabled is false)")
	}

	// The deny list is checked before anything else can have an effect, so a
	// refused command leaves no trace at all.
	for _, rule := range cfg.deny {
		if rule.re.MatchString(command) {
			return BashOutput{}, fmt.Errorf("bash: command refused by deny pattern %q: it looks destructive and nothing was run", rule.pattern)
		}
	}

	cwd, err := bashResolveCwd(ws, in.Cwd)
	if err != nil {
		return BashOutput{}, err
	}
	out.Cwd = ws.Rel(cwd)

	shell, flags, err := bashShell()
	if err != nil {
		return BashOutput{}, err
	}

	// The timeout is enforced through a context, which gives the kill two
	// independent chances to happen: cmd.Cancel below (the os/exec-native path,
	// taken while the shell itself is still running) and the watchdog goroutine
	// after Start (which also covers a shell that already exited and left a
	// child holding the output pipes).
	timeout := bashTimeout(cfg.timeout, in.TimeoutMS)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The command is passed through verbatim as one argument to sh -c. Joining
	// arguments here would mean owning the quoting rules of a shell we are not
	// writing, and quoting bugs in a tool that runs model-supplied commands are
	// how "just list the files" becomes something else entirely.
	args := make([]string, 0, len(flags)+1)
	args = append(args, flags...)
	args = append(args, command)

	stdout := &bashCapture{limit: cfg.maxOutput}
	stderr := &bashCapture{limit: cfg.maxOutput}

	cmd := exec.CommandContext(runCtx, shell, args...)
	cmd.Dir = cwd
	cmd.Env = bashEnviron(cwd)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil // /dev/null: a command that prompts must fail rather than hang
	// Put the shell in its own process group so a timeout can kill the tree it
	// spawned and not just the shell. Setpgid and the negative pid used to kill
	// the group are shared by darwin and linux; a Windows port needs its own
	// files for both, which is why bashShell() is a separate seam.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel is the os/exec-native cancellation. It is necessary but not
	// sufficient: exec only calls it while the command's own process is still
	// running, and the interesting leak — `sh -c 'sleep 300 &'` — exits the
	// shell immediately while the child it left behind holds the output pipes.
	cmd.Cancel = func() error { return bashKillGroup(cmd) }
	cmd.WaitDelay = bashWaitDelay

	started := time.Now()
	if startErr := cmd.Start(); startErr != nil {
		// The shell never came up (no /bin/sh, no permission, a cwd that
		// vanished between the stat and the exec): a tool failure, not an exit
		// code the model should try to interpret.
		return BashOutput{}, fmt.Errorf("bash: start command: %w", startErr)
	}

	// The watchdog closes the gap exec leaves, and is what actually enforces the
	// timeout when a command backgrounds a child and exits: it kills the process
	// group the moment the context is done, whether or not the shell is still
	// there. It starts after Start so cmd.Process is already set and never
	// mutated again, and it stops as soon as Wait returns.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			bashReapGroup(cmd)
		case <-watchDone:
		}
	}()

	waitErr := cmd.Wait()
	close(watchDone)
	elapsed := time.Since(started)
	out.DurationMS = elapsed.Milliseconds()

	// Nothing started by a tool call gets to outlive it: a command that
	// backgrounds a child and exits must not leak a process that no later call
	// can see or stop.
	bashReapGroup(cmd)

	var discarded int64
	out.Stdout, discarded = stdout.text()
	out.StdoutTruncated = discarded > 0
	out.Stderr, discarded = stderr.text()
	out.StderrTruncated = discarded > 0

	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		out.ExitCode = 0
	case timedOut:
		// The kill came from a deadline, so whatever status the process ended
		// with describes the signal, not the command's own work.
		out.ExitCode = -1
	case ctx.Err() != nil:
		// The caller's context ended first: the run was abandoned rather than
		// timed out by this tool. Say so instead of reporting a meaningless
		// exit code for a process that was killed from the outside.
		return BashOutput{}, fmt.Errorf("bash: run command: %w", ctx.Err())
	case errors.As(waitErr, &exitErr):
		// A non-zero exit is a result, not a failure of the tool: the model
		// asked for the command to run and needs to read the status. ExitCode()
		// is -1 when the process died from a signal.
		out.ExitCode = exitErr.ExitCode()
	default:
		// Wait itself failed — it gave up on pipes a stray process kept open,
		// or hit an I/O error. That is a tool failure, and inventing an exit
		// code for it would hide it.
		return BashOutput{}, fmt.Errorf("bash: run command: %w", waitErr)
	}

	if timedOut {
		out.TimedOut = true
		out.ExitCode = -1
		out.Stderr += fmt.Sprintf(bashTimeoutNoteFmt, elapsed.Round(time.Millisecond), timeout)
	}
	return out, nil
}

// bashResolveCwd turns the model's cwd argument into an absolute directory
// inside the workspace. An empty value means the workspace root.
func bashResolveCwd(ws *workspace.Workspace, cwd string) (string, error) {
	dir := ws.Root()
	if want := strings.TrimSpace(cwd); want != "" {
		resolved, err := ws.Resolve(want)
		if err != nil {
			// Resolve reports ErrOutsideWorkspace or ErrInvalidPath itself;
			// wrapping keeps errors.Is working for callers that branch on it.
			return "", fmt.Errorf("bash: cwd %q: %w", cwd, err)
		}
		dir = resolved
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("bash: cwd %q: %w", cwd, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash: cwd %q is not a directory: %w", cwd, workspace.ErrInvalidPath)
	}
	return dir, nil
}

// bashTimeout applies a per-call override. The override may only shorten the
// policy timeout: a model must not be able to talk its way into a longer run
// than the operator allowed.
func bashTimeout(policy time.Duration, overrideMS int) time.Duration {
	if overrideMS <= 0 {
		return policy
	}
	want := time.Duration(overrideMS) * time.Millisecond
	if want <= 0 || want > policy { // want <= 0 catches an overflowing duration
		return policy
	}
	return want
}

// bashShell returns the interpreter for a command string on this platform, as
// the program plus its leading arguments; the caller appends the command itself
// and adds no quoting.
//
// This switch is the single seam a port has to touch. Anything not listed gets
// an error rather than a guess, because hand-running a model-supplied command
// through an interpreter whose behaviour nobody checked is not a mistake worth
// making quietly.
func bashShell() (string, []string, error) {
	switch runtime.GOOS {
	case "darwin", "linux":
		return "/bin/sh", []string{"-c"}, nil
	default:
		return "", nil, fmt.Errorf("bash: unsupported GOOS %q: no shell mapping is defined for it", runtime.GOOS)
	}
}

// bashEnviron is the parent environment with PWD pointed at dir.
//
// PWD is replaced rather than appended because getenv returns the first match,
// so an inherited PWD would win and a tool that trusts it would look in the
// wrong directory.
func bashEnviron(dir string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}

// bashKillGroup SIGKILLs the command's whole process group and is what
// cmd.Cancel calls when the timeout fires. Signalling -pid (the group) instead
// of pid (the shell) is the entire point: `sh -c 'sleep 300 &'` would otherwise
// leave the sleep running with nothing left to stop it.
//
// os.ErrProcessDone is returned when the group is already empty, which is the
// signal os/exec needs to treat the cancellation as a no-op instead of injecting
// a context error for a command that had already finished.
func bashKillGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		return os.ErrProcessDone
	}
	// The group could not be signalled at all; fall back to the leader rather
	// than leaving the command running.
	if killErr := cmd.Process.Kill(); killErr != nil {
		if errors.Is(killErr, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return killErr
	}
	return nil
}

// bashReapGroup kills whatever is still alive in the command's process group.
// It is called twice for a reason: once by the timeout watchdog, so a deadline
// takes the whole tree down even when the shell itself has already exited, and
// once after Wait, so a command that backgrounds a child and exits at once —
// where no timeout ever fires, because the shell's exit ends Wait immediately
// when the child redirected its output — leaves nothing behind either.
// Nothing started by a tool call is supposed to outlive that call.
//
// kill(-pgid, 0) is the probe: a process group exists only while it has a
// member, so a successful signal 0 means something is still in there. Once Wait
// has reaped the shell there is a theoretical pid-reuse race between reaping it
// and probing; the window is microseconds wide, and the alternative is leaking
// processes on every call that backgrounds anything.
func bashReapGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, 0); err != nil {
		return // ESRCH (empty group) or EPERM: either way there is nothing to do
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// bashCapture collects the head of one output stream up to a byte budget while
// counting everything that arrived.
//
// Write always claims the full length: reporting a short write would make
// os/exec treat the copy as failed and stop draining the pipe, which both loses
// output and can block the child against a full pipe.
type bashCapture struct {
	limit   int
	buf     []byte
	written int64
}

func (c *bashCapture) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	if room := c.limit - len(c.buf); room > 0 {
		take := len(p)
		if take > room {
			take = room
		}
		c.buf = append(c.buf, p[:take]...)
	}
	return len(p), nil
}

// text returns the captured text — with an explicit marker appended when bytes
// were dropped — plus how many bytes were discarded.
//
// Keeping the head is deliberate: the first lines of a build log, a test run or
// a stack trace are the part a coding agent can act on, while a mid-stream cut
// leaves it guessing what it missed.
func (c *bashCapture) text() (string, int64) {
	kept := bashClipRune(c.buf)
	if c.written <= int64(c.limit) {
		return string(kept), 0
	}
	discarded := c.written - int64(len(kept))
	return string(kept) + fmt.Sprintf(bashTruncatedNoteFmt, len(kept), discarded), discarded
}

// bashClipRune drops an incomplete UTF-8 sequence left at the end by the byte
// cut. The capped slice goes into JSON on its way to the model, and a half
// encoded rune would arrive as a replacement character that was never in the
// output.
func bashClipRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && i < len(b); i++ {
		c := b[len(b)-1-i]
		if c < utf8.RuneSelf { // ASCII: the cut landed on a boundary
			return b
		}
		if c&0xC0 == 0xC0 { // a leading byte: check the sequence it starts
			if r, size := utf8.DecodeRune(b[len(b)-1-i:]); r == utf8.RuneError && size <= 1 {
				return b[:len(b)-1-i]
			}
			return b
		}
		// A continuation byte: keep walking back to the leading byte.
	}
	return b
}
