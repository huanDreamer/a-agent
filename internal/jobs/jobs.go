// Package jobs runs and supervises the long-lived processes an agent starts.
//
// The problem it exists for: the bash tool kills the whole process group when a
// call ends, on purpose and correctly — a tool call that leaves processes behind
// leaks them with nobody tracking them. That guarantee is also why a dev server,
// a --watch compiler or a resident database cannot be started at all: they are
// defined by outliving the call that started them, and the usual workaround
// (`nohup ... &`) produces exactly the process nobody can find, read or stop.
//
// So this package makes "resident" an explicit, supervised thing. What it
// provides, and what it does not:
//
//   - a job runs in its own session and process group, so a signal aimed at the
//     agent, or a group kill aimed at another job, cannot reach it;
//   - its stdout and stderr are captured into a bounded in-memory window and a
//     capped log file, so neither memory nor disk grows with its lifetime, and
//     readers work in absolute byte offsets so a gap is reported rather than
//     papered over;
//   - "running" means the process *group* still has a member, which is the
//     question a user actually asks ("is my server still up?") when the shell
//     that was started has already forked and exited;
//   - nothing outlives the agent: Close terminates every job, so a restart never
//     leaves an orphan with no one tracking it;
//   - it is NOT a sandbox. A job runs with the agent user's full rights, has no
//     PTY and no standard input, gets no CPU/memory quota, and does not survive
//     the agent process. Services that must survive a reboot belong to launchd,
//     systemd or a container.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// Status is where a job is in its life.
type Status string

const (
	// StatusRunning means the job's process group still has a member.
	StatusRunning Status = "running"
	// StatusExited means the job ended on its own, whatever its exit code.
	StatusExited Status = "exited"
	// StatusStopped means the job ended because Stop (or Close) asked it to.
	StatusStopped Status = "stopped"
)

// Terminal reports whether a status means the job is over.
func (s Status) Terminal() bool { return s == StatusExited || s == StatusStopped }

// Errors callers branch on, so errors.Is finds them through whatever wrapping a
// method adds.
//
// Only ErrClosed carries the package prefix: it is returned as is. The other
// three are always wrapped together with the job id, and a second "jobs:" in the
// middle of "jobs: job-4f2a91: jobs: no such job" is the kind of noise that makes
// a message read like a bug report about itself.
var (
	// ErrClosed is returned by Start once the manager has been closed, and by
	// anything that would need to start a process afterwards.
	ErrClosed = errors.New("jobs: the manager is closed")
	// ErrUnknownJob is returned for an id this manager never issued — including
	// one from a previous run of the agent, since ids are not reused.
	ErrUnknownJob = errors.New("no such job")
	// ErrStillRunning is returned when an operation requires a finished job
	// (Forget) but the job is still running.
	ErrStillRunning = errors.New("the job is still running")
	// ErrNotRunning is returned when a job was asked to stop but had already
	// ended: a conflict, not a failure of the request.
	ErrNotRunning = errors.New("the job has already ended")
)

// Defaults applied when Options leaves a field unset. They are deliberately
// small: this supervises a handful of development services, not a workload.
const (
	// DefaultMaxJobs bounds how many jobs may run at once.
	DefaultMaxJobs = 8
	// DefaultMaxLogBytes caps one job's log file on disk.
	DefaultMaxLogBytes int64 = 8 << 20
	// DefaultWindowBytes caps the in-memory window a reader can be served from.
	DefaultWindowBytes = 256 << 10
	// DefaultMaxFinished bounds how many finished job records are kept. Records
	// are dropped oldest-first; their log files are not (deleting a log is an
	// explicit act, not a side effect of running many jobs).
	DefaultMaxFinished = 20
	// DefaultStartGrace is how long Start watches a new job before reporting it
	// as running. A command that dies immediately — a busy port, a missing
	// binary, a typo — is then reported in the start result instead of becoming
	// a job the caller has to poll to understand.
	DefaultStartGrace = 500 * time.Millisecond
	// DefaultStopGrace is how long a stopping job gets between SIGTERM and
	// SIGKILL. It is generous on purpose: a database flushes on SIGTERM and
	// losing that is worse than waiting.
	DefaultStopGrace = 5 * time.Second
)

// Defaults for reads and for the two internal watchdogs.
const (
	// DefaultReadBytes is how much output one read returns when it does not say.
	DefaultReadBytes = 16 << 10
	// MaxReadBytes caps one read, so a single call cannot pull the whole window
	// into a model's context.
	MaxReadBytes = 1 << 20
	// DefaultWait is how long a read waits for new output when it does not say.
	DefaultWait = 0
	// MaxWait caps one read's wait, so a call cannot park the agent for minutes.
	MaxWait = 30 * time.Second

	// groupPollInterval is how often a job whose shell has exited is probed for
	// surviving group members.
	groupPollInterval = 250 * time.Millisecond
)

// groupDrainGrace is how long a job whose group is already empty is given to
// close its output pipe before it is declared finished anyway. It only matters
// for a process that left the group and kept the pipe (a double fork that called
// setsid itself): that process is untracked by design, and waiting for it forever
// would report a finished job as running.
//
// It is a variable rather than a constant so the tests can shorten it; nothing
// else writes to it.
var groupDrainGrace = 3 * time.Second

// logCapNote is appended to a log file that hit its cap. It is written to the
// file rather than only logged, because the file is what a human opens later
// and "the output just stops here" is exactly the kind of silence that wastes an
// afternoon.
const logCapNote = "\n[jobs] log capped: this file stops here, this job is still running. " +
	"The most recent output is readable through bash_output or 设置 → 后台进程; " +
	"earlier output is gone (raise tools.background_log_max_mb to keep more).\n"

// jobLogHeaderFmt is the header written at the top of a job's log file. It is
// not captured into the readable window: the window is the process's own output,
// and a reader asking "what did it print" should get exactly that.
const jobLogHeaderFmt = "# job %s: %s\n# cwd: %s\n# started: %s\n\n"

// Options configures a Manager. Every zero value takes the matching default.
type Options struct {
	// Dir is where per-job log files are written. Required: a manager without a
	// place to put logs cannot honour the promise that a job's output survives
	// the window being overwritten.
	Dir string
	// MaxJobs bounds concurrently running jobs.
	MaxJobs int
	// MaxLogBytes caps one job's log file.
	MaxLogBytes int64
	// WindowBytes caps the in-memory window each job keeps.
	WindowBytes int
	// MaxFinished bounds how many finished records are kept.
	MaxFinished int
	// StartGrace is how long Start watches a new job before reporting it as
	// running. Zero takes the default; a negative value means "do not watch at
	// all", which is only ever what a test wants.
	StartGrace time.Duration
	// StopGrace is the SIGTERM-to-SIGKILL grace for Stop and Close.
	StopGrace time.Duration
	// Logger receives lifecycle warnings (a job that fails to start, a log file
	// that cannot be written). Nil logs nowhere.
	Logger *zap.Logger
}

// applyDefaults fills in every unset field.
func (o *Options) applyDefaults() {
	if o.MaxJobs <= 0 {
		o.MaxJobs = DefaultMaxJobs
	}
	if o.MaxLogBytes <= 0 {
		o.MaxLogBytes = DefaultMaxLogBytes
	}
	if o.WindowBytes <= 0 {
		o.WindowBytes = DefaultWindowBytes
	}
	if o.MaxFinished <= 0 {
		o.MaxFinished = DefaultMaxFinished
	}
	if o.StartGrace == 0 {
		o.StartGrace = DefaultStartGrace
	}
	if o.StartGrace < 0 {
		// An explicit negative value is "do not watch", which is a different
		// thing from leaving the field unset.
		o.StartGrace = 0
	}
	if o.StopGrace <= 0 {
		o.StopGrace = DefaultStopGrace
	}
}

// Spec is one job to start.
type Spec struct {
	// Command is the shell command line, passed verbatim to the interpreter with
	// no quoting added here — the same rule the bash tool follows, and for the
	// same reason.
	Command string
	// Shell and ShellArgs are the interpreter and its leading arguments, e.g.
	// "/bin/sh" and ["-c"]. They come from the caller so that the one place that
	// knows which shells exist is the one place a port has to touch.
	Shell     string
	ShellArgs []string
	// Cwd is the working directory; it must exist.
	Cwd string
	// Env is the child's entire environment. It is passed through rather than
	// built here so a caller can set PWD to the directory it resolved.
	Env []string
	// Name is an optional short label for listings ("vite", "postgres").
	Name string
	// Workspace and WorkspaceRoot record where the job was started, so the
	// console can show it and a tool can decide whether a job is its own.
	Workspace     string
	WorkspaceRoot string
	// RequestedBy names the surface that started it: web, cli or feishu.
	RequestedBy string
	// Scope names the conversation that started it — the same scope the runner
	// publishes on the turn's context (a web session id, a Feishu open_id).
	//
	// It is what makes a job belong to a conversation rather than merely to a
	// directory: two conversations in one workspace share the files on disk but
	// not the dev server each of them started, and the console shows the count
	// on the conversation that owns it. Empty for a caller that has no scopes,
	// which is why a reader must treat "unscoped" as a real state.
	Scope string
}

// validate checks the spec before anything is spawned.
func (s Spec) validate() error {
	if strings.TrimSpace(s.Command) == "" {
		return errors.New("jobs: a command is required")
	}
	if strings.TrimSpace(s.Shell) == "" {
		return errors.New("jobs: a shell is required")
	}
	if strings.TrimSpace(s.Cwd) == "" {
		return errors.New("jobs: a working directory is required")
	}
	info, err := os.Stat(s.Cwd)
	if err != nil {
		return fmt.Errorf("jobs: working directory %s: %w", s.Cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("jobs: working directory %s is not a directory", s.Cwd)
	}
	return nil
}

// Job is an immutable snapshot of one job, shaped for JSON.
type Job struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Command is the command line as given.
	Command string `json:"command"`
	// Cwd is the absolute working directory.
	Cwd string `json:"cwd"`
	// Workspace is the workspace label the job was started in, and
	// WorkspaceRoot its directory, which is what a tool compares to decide
	// whether the job is one of its own.
	Workspace     string `json:"workspace,omitempty"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	RequestedBy   string `json:"requested_by,omitempty"`
	// Scope is the conversation that started the job, recorded from the turn's
	// context. The console uses it to show a session its own processes; a
	// deployment with no scopes leaves every job unscoped.
	Scope string `json:"scope,omitempty"`
	// PID is the group leader: the shell the job started. It doubles as the
	// process group id, which is what signals are sent to.
	PID int `json:"pid"`
	// Status is running, exited or stopped.
	Status Status `json:"status"`
	// ExitCode is nil while the job runs. A negative value means it died from a
	// signal; 143 is the usual "terminated by SIGTERM".
	ExitCode *int `json:"exit_code"`
	// StartedAt is when the process was spawned; EndedAt is nil while it runs.
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// UptimeMS is the running time: so far for a running job, total for a
	// finished one.
	UptimeMS int64 `json:"uptime_ms"`
	// LogPath is the durable log file, and LogBytes how much output was written
	// to it (the file also carries a short header, and a closing note when the
	// cap was hit — see LogCapped).
	LogPath   string `json:"log_path"`
	LogBytes  int64  `json:"log_bytes"`
	LogCapped bool   `json:"log_capped"`
	// TotalBytes is every byte of output ever captured, and DiscardedBytes how
	// much of it the in-memory window has since thrown away — the reason a
	// reader can be told honestly that it is missing part of the stream.
	TotalBytes     int64 `json:"total_bytes"`
	DiscardedBytes int64 `json:"discarded_bytes"`
	// OutputOpen reports that a process which left the job's process group is
	// still holding the output pipe: the job is over, but output may still be
	// arriving from something this manager can no longer stop.
	OutputOpen bool `json:"output_open"`
}

// Manager owns every job one agent process starts.
//
// Concurrency: the map and the running count are guarded by mu, each job's own
// state by its own lock, so a listing never waits behind a reading of a job's
// output and neither waits behind a job that is slow to die.
type Manager struct {
	opts   Options
	logger *zap.Logger

	mu     sync.Mutex
	jobs   map[string]*process
	order  []string
	closed bool
}

// New returns a manager that writes its log files into opts.Dir, creating the
// directory if it is missing.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("jobs: a log directory is required")
	}
	opts.applyDefaults()
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("jobs: create log directory %s: %w", opts.Dir, err)
	}
	return &Manager{
		opts:   opts,
		logger: opts.Logger,
		jobs:   map[string]*process{},
	}, nil
}

// Options reports the effective options, defaults applied. The console reads
// the log directory and the job limit from here rather than keeping its own copy
// of the configuration.
func (m *Manager) Options() Options {
	if m == nil {
		return Options{}
	}
	return m.opts
}

// Start spawns one job and returns it once it has survived StartGrace or ended,
// whichever happens first. A job that ends during that window is returned as
// exited (or stopped) with its exit code, which is how a busy port or a missing
// binary reaches the caller in one call instead of one poll.
//
// ctx bounds the startup, not the job: a job is supposed to outlive the call
// that started it, so it is deliberately not tied to this context. A cancelled
// ctx before the spawn refuses the call; cancel it afterwards and the job keeps
// running, which is the entire point.
func (m *Manager) Start(ctx context.Context, spec Spec) (*Job, error) {
	if m == nil {
		return nil, errors.New("jobs: no manager")
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("jobs: start: %w", err)
	}

	m.mu.Lock()
	p, err := m.spawnLocked(spec)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}

	m.await(p, m.opts.StartGrace)
	if snap := p.snapshot(); snap.Status.Terminal() {
		m.logger.Info("background job ended during its startup grace",
			zap.String("id", p.id), zap.Int("exit_code", snap.exitCodeOrSentinel()),
			zap.String("command", spec.Command))
	}
	return p.snapshot(), nil
}

// spawnLocked starts one process. It holds the manager lock for the whole spawn
// — a fork/exec costs a few milliseconds — because that is what makes the
// running count exact: two concurrent Start calls cannot both see room for the
// last slot.
func (m *Manager) spawnLocked(spec Spec) (*process, error) {
	if m.closed {
		return nil, ErrClosed
	}
	running := 0
	for _, p := range m.jobs {
		if p.running() {
			running++
		}
	}
	if running >= m.opts.MaxJobs {
		return nil, fmt.Errorf(
			"jobs: %d jobs are already running (tools.background_max_jobs is %d): stop one first",
			running, m.opts.MaxJobs)
	}

	id, err := m.freeIDLocked()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(m.opts.Dir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("jobs: open log file: %w", err)
	}
	header := fmt.Sprintf(jobLogHeaderFmt, id, spec.Command, spec.Cwd, time.Now().Format(time.RFC3339))
	if _, err := logFile.WriteString(header); err != nil {
		// A log file we cannot write is not fatal to the job, but it is worth
		// saying: it is the only durable record of what the process printed.
		m.logger.Warn("job log file is not writable",
			zap.String("id", id), zap.String("path", logPath), zap.Error(err))
	}

	// One pipe shared by stdout and stderr: an interleaved stream is what a
	// server's output actually is, and two streams would either merge in a
	// different order than they were written or need timestamps this does not
	// have. The cost is that a reader cannot tell the two apart, which the tool
	// descriptions state.
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return nil, fmt.Errorf("jobs: output pipe: %w", err)
	}

	args := make([]string, 0, len(spec.ShellArgs)+1)
	args = append(args, spec.ShellArgs...)
	args = append(args, spec.Command)

	// Deliberately exec.Command and not exec.CommandContext: tying the process to
	// a context is how the bash tool guarantees nothing outlives a call, and it
	// is exactly what must not happen here.
	cmd := exec.Command(spec.Shell, args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = nil // /dev/null: a command that prompts must fail, not hang
	cmd.Stdout = pw
	cmd.Stderr = pw
	// A new session: the job leaves the agent's process group, so a group signal
	// aimed at the agent (or at another job) cannot reach it. That is also why
	// Close has to terminate jobs itself — nothing else will.
	cmd.SysProcAttr = detachAttr()

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return nil, fmt.Errorf("jobs: start %q: %w", spec.Command, err)
	}
	// The child holds its own copy of the write end; closing ours is what lets
	// the reader see EOF once every process holding it has exited.
	_ = pw.Close()

	p := &process{
		id:        id,
		spec:      spec,
		cmd:       cmd,
		pgid:      cmd.Process.Pid,
		logPath:   logPath,
		logFile:   logFile,
		logger:    m.logger,
		maxLog:    m.opts.MaxLogBytes,
		window:    newByteWindow(m.opts.WindowBytes),
		startedAt: startedAt,
		status:    StatusRunning,
		done:      make(chan struct{}),
		poke:      make(chan struct{}, 1),
		stopPoll:  make(chan struct{}),
	}
	m.jobs[id] = p
	m.order = append(m.order, id)
	m.pruneLocked()

	go p.drain(pr)
	go p.supervise()
	return p, nil
}

// freeIDLocked returns an unused job id. Ids are short and random rather than
// sequential for one reason: a conversation outlives the agent process, so a
// model (or a person) can hold an id from a previous run, and "job-1" would then
// quietly name a different process.
func (m *Manager) freeIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("jobs: generate id: %w", err)
		}
		id := "job-" + hex.EncodeToString(b[:])
		if _, taken := m.jobs[id]; !taken {
			return id, nil
		}
	}
	return "", errors.New("jobs: could not allocate a free job id")
}

// pruneLocked drops the oldest finished records once more than MaxFinished are
// kept. Log files stay on disk: they are the evidence a finished job ever ran,
// and deleting them is Forget's job, not housekeeping's.
func (m *Manager) pruneLocked() {
	finished := make([]string, 0, len(m.order))
	for _, id := range m.order {
		if p, ok := m.jobs[id]; ok && p.finished() {
			finished = append(finished, id)
		}
	}
	if len(finished) <= m.opts.MaxFinished {
		return
	}
	drop := make(map[string]bool, len(finished)-m.opts.MaxFinished)
	for _, id := range finished[:len(finished)-m.opts.MaxFinished] {
		drop[id] = true
		delete(m.jobs, id)
	}
	kept := m.order[:0]
	for _, id := range m.order {
		if !drop[id] {
			kept = append(kept, id)
		}
	}
	m.order = kept
}

// await waits until p is finished or the grace expires.
func (m *Manager) await(p *process, grace time.Duration) {
	if grace <= 0 {
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	}
}

// List returns every job this manager still holds, oldest first.
func (m *Manager) List() []Job {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	procs := make([]*process, 0, len(m.order))
	for _, id := range m.order {
		if p, ok := m.jobs[id]; ok {
			procs = append(procs, p)
		}
	}
	m.mu.Unlock()

	out := make([]Job, 0, len(procs))
	for _, p := range procs {
		out = append(out, *p.snapshot())
	}
	return out
}

// Get returns one job by id, or ErrUnknownJob.
func (m *Manager) Get(id string) (Job, error) {
	p, err := m.lookup(id)
	if err != nil {
		return Job{}, err
	}
	return *p.snapshot(), nil
}

// Running counts the jobs whose process group is still alive.
func (m *Manager) Running() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.jobs {
		if p.running() {
			n++
		}
	}
	return n
}

// lookup resolves an id, refusing anything this manager never issued.
func (m *Manager) lookup(id string) (*process, error) {
	if m == nil {
		return nil, errors.New("jobs: no manager")
	}
	want := strings.TrimSpace(id)
	if want == "" {
		return nil, fmt.Errorf("jobs: a job id is required: %w", ErrUnknownJob)
	}
	m.mu.Lock()
	p, ok := m.jobs[want]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("jobs: %s: %w", want, ErrUnknownJob)
	}
	return p, nil
}

// ReadOptions is one request for job output.
type ReadOptions struct {
	// From is the absolute byte offset to start at, as returned in Read.Next. A
	// value <= 0 means "the tail": the last MaxBytes of output, which is what a
	// first look wants and what an offset smaller than the window can serve
	// would degrade to anyway.
	From int64
	// MaxBytes caps this read. <=0 uses DefaultReadBytes, above MaxReadBytes it
	// is clamped.
	MaxBytes int
	// Wait blocks until there is new output, the job ends, Wait expires, or the
	// caller's context ends — whichever comes first. This is what makes "start a
	// server and check what it printed" one call instead of a poll loop.
	Wait time.Duration
}

// Read is one window of a job's output.
type Read struct {
	ID       string `json:"id"`
	Status   Status `json:"status"`
	ExitCode *int   `json:"exit_code"`
	// Data is the requested window of output, as text.
	Data string `json:"data"`
	// From is where Data starts (after any clamping), Next is the offset to pass
	// to the next read, and TotalBytes is everything captured so far.
	From       int64 `json:"from"`
	Next       int64 `json:"next"`
	TotalBytes int64 `json:"total_bytes"`
	// DiscardedBytes is how much output the window has thrown away in total:
	// non-zero means the beginning of the stream is no longer readable at all.
	DiscardedBytes int64 `json:"discarded_bytes"`
	// SkippedBytes is how much was skipped to serve this read, because the
	// requested offset was older than the window. It is reported rather than
	// silently returned as a shorter stream.
	SkippedBytes int64 `json:"skipped_bytes"`
	// OutputOpen mirrors Job.OutputOpen.
	OutputOpen bool `json:"output_open"`
}

// Output reads one window of a job's output.
func (m *Manager) Output(ctx context.Context, id string, opts ReadOptions) (Read, error) {
	p, err := m.lookup(id)
	if err != nil {
		return Read{}, err
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultReadBytes
	}
	if maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}

	wait := opts.Wait
	if wait < 0 {
		wait = 0
	}
	if wait > MaxWait {
		wait = MaxWait
	}
	deadline := time.Now().Add(wait)

	for {
		out := p.read(opts.From, maxBytes)
		// Stop waiting as soon as there is something to report: new bytes past
		// the requested offset, or a job that is over.
		if wait <= 0 || out.Next > out.From || !p.running() {
			return out, nil
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return out, nil
		}
		timer := time.NewTimer(remain)
		select {
		case <-p.poke:
		case <-p.done:
		case <-timer.C:
		case <-ctx.Done():
			// The caller gave up. Returning what was captured beats returning
			// nothing: the read is honest about its offsets either way.
			timer.Stop()
			return out, nil
		}
		timer.Stop()
	}
}

// Stop ends a running job: SIGTERM to its whole process group, then SIGKILL
// after grace (or immediately, when signal is SIGKILL). It returns the job's
// final snapshot.
//
// A job that had already ended yields ErrNotRunning, and one whose group somehow
// survives SIGKILL is returned as still running: this reports what happened
// rather than what was asked for.
func (m *Manager) Stop(ctx context.Context, id string, signal syscall.Signal, grace time.Duration) (Job, error) {
	p, err := m.lookup(id)
	if err != nil {
		return Job{}, err
	}
	if !p.running() {
		return *p.snapshot(), fmt.Errorf("jobs: %s: %w", id, ErrNotRunning)
	}
	if grace <= 0 {
		grace = m.opts.StopGrace
	}
	if signal != syscall.SIGKILL {
		signal = syscall.SIGTERM
	}

	// Mark the intent before signalling: a job that dies a microsecond later is
	// then reported as stopped rather than as having exited on its own, which is
	// the difference between "we killed it" and "it crashed".
	p.markStopAsked()
	p.logger.Info("background job stopping",
		zap.String("id", id), zap.String("signal", signal.String()), zap.String("command", p.spec.Command))

	killGroup(p.pgid, signal)
	if signal == syscall.SIGTERM {
		if !waitChannel(ctx, p.done, grace) {
			p.logger.Warn("background job ignored SIGTERM, sending SIGKILL",
				zap.String("id", id), zap.Duration("grace", grace))
			killGroup(p.pgid, syscall.SIGKILL)
			// Bound the wait after SIGKILL too: an unkillable process (a stuck
			// disk wait) must not hold the tool call open. The snapshot then
			// reports the truth, which is that it is still there.
			waitChannel(ctx, p.done, groupDrainGrace)
		}
	} else {
		waitChannel(ctx, p.done, groupDrainGrace)
	}
	return *p.snapshot(), nil
}

// Forget drops a finished job's record and deletes its log file. It refuses a
// running job: the record and the log are how a running process can be found and
// stopped, so losing them while it lives is the one outcome worth refusing.
func (m *Manager) Forget(id string) (Job, error) {
	if m == nil {
		return Job{}, errors.New("jobs: no manager")
	}
	want := strings.TrimSpace(id)
	if want == "" {
		return Job{}, fmt.Errorf("jobs: a job id is required: %w", ErrUnknownJob)
	}
	m.mu.Lock()
	p, ok := m.jobs[want]
	if !ok {
		m.mu.Unlock()
		return Job{}, fmt.Errorf("jobs: %s: %w", want, ErrUnknownJob)
	}
	if p.running() {
		m.mu.Unlock()
		return *p.snapshot(), fmt.Errorf("jobs: %s: %w", want, ErrStillRunning)
	}
	delete(m.jobs, want)
	kept := m.order[:0]
	for _, oid := range m.order {
		if oid != want {
			kept = append(kept, oid)
		}
	}
	m.order = kept
	m.mu.Unlock()

	snap := *p.snapshot()
	if snap.LogPath != "" {
		if err := os.Remove(snap.LogPath); err != nil && !os.IsNotExist(err) {
			// The record is gone either way; a log file nobody references is
			// worth a warning, not a failed call.
			m.logger.Warn("job log file could not be removed",
				zap.String("id", want), zap.String("path", snap.LogPath), zap.Error(err))
		}
	}
	return snap, nil
}

// Close terminates every running job and marks the manager closed: SIGTERM to
// each group, then SIGKILL after StopGrace. It is what makes "nothing outlives
// the agent" true rather than aspirational, and it is safe to call twice.
//
// Jobs are stopped in parallel — a grace period is spent once, not once per job
// — and every record is finalized even if its group survived, so nothing that
// was waiting on a job can wait forever during shutdown.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	running := make([]*process, 0, len(m.order))
	for _, id := range m.order {
		if p, ok := m.jobs[id]; ok && p.running() {
			running = append(running, p)
		}
	}
	m.mu.Unlock()

	if len(running) == 0 {
		return
	}
	m.logger.Info("stopping background jobs", zap.Int("jobs", len(running)))

	var wg sync.WaitGroup
	for _, p := range running {
		wg.Add(1)
		go func(p *process) {
			defer wg.Done()
			p.markStopAsked()
			killGroup(p.pgid, syscall.SIGTERM)
			if waitChannel(context.Background(), p.done, m.opts.StopGrace) {
				return
			}
			killGroup(p.pgid, syscall.SIGKILL)
			waitChannel(context.Background(), p.done, groupDrainGrace)
		}(p)
	}
	wg.Wait()

	// Anything whose group outlived SIGKILL is still recorded as running; the
	// process is going away, so finalize the record rather than leave a channel
	// that will never close.
	for _, p := range running {
		p.forceFinish()
		close(p.stopPoll)
	}
}

// waitChannel waits for ch to close, the deadline to pass or the context to end,
// and reports whether ch closed.
func waitChannel(ctx context.Context, ch <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// process is one supervised job.
//
// Two goroutines run per job: one drains the shared output pipe until every
// writer has closed it, and one reaps the shell and then waits for the process
// group to empty. A job is finished only when all of that has happened, which is
// why the state below is a small pile of flags rather than a single status: the
// interesting case (the shell exits while a forked server keeps running) is
// exactly the one a single flag would get wrong.
type process struct {
	id   string
	spec Spec

	cmd     *exec.Cmd
	pgid    int
	logPath string
	logger  *zap.Logger
	maxLog  int64
	window  *byteWindow

	// done closes when the job reaches a terminal state; poke carries a
	// level-triggered hint that new output arrived, so a waiting reader wakes up
	// and re-checks its offsets instead of guessing.
	done     chan struct{}
	poke     chan struct{}
	stopPoll chan struct{}

	mu         sync.Mutex
	startedAt  time.Time
	endedAt    time.Time
	status     Status
	exitCode   *int
	logFile    *os.File
	logBytes   int64
	logCapped  bool
	reaped     bool
	groupGone  bool
	readerDone bool
	outputOpen bool
	stopAsked  bool
	finalized  bool
}

// drain copies the output pipe into the window and the log file until every
// process holding the write end has closed it.
func (p *process) drain(pr *os.File) {
	defer func() { _ = pr.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			p.capture(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				p.logger.Warn("background job output read failed",
					zap.String("id", p.id), zap.Error(err))
			}
			break
		}
	}
	p.finishReading()
}

// capture appends one chunk of output to the window and the log file.
func (p *process) capture(b []byte) {
	p.mu.Lock()
	p.window.Write(b)
	p.writeLogLocked(b)
	p.mu.Unlock()
	select {
	case p.poke <- struct{}{}:
	default: // a wake-up is already pending; readers re-read their offsets
	}
}

// writeLogLocked appends to the log file until its cap is reached, then stops
// and records why. The cap is what keeps a chatty server from filling the disk
// over a weekend.
func (p *process) writeLogLocked(b []byte) {
	if p.logFile == nil || p.logCapped {
		return
	}
	room := p.maxLog - p.logBytes
	if room <= 0 {
		p.capLogLocked()
		return
	}
	if int64(len(b)) > room {
		n, err := p.logFile.Write(b[:room])
		p.logBytes += int64(n)
		if err != nil {
			p.detachLogLocked(err)
			return
		}
		p.capLogLocked()
		return
	}
	n, err := p.logFile.Write(b)
	p.logBytes += int64(n)
	if err != nil {
		p.detachLogLocked(err)
	}
}

// capLogLocked writes the closing note and stops appending.
func (p *process) capLogLocked() {
	if p.logFile == nil || p.logCapped {
		return
	}
	p.logCapped = true
	if _, err := p.logFile.WriteString(logCapNote); err != nil {
		p.detachLogLocked(err)
		return
	}
	_ = p.logFile.Sync()
	_ = p.logFile.Close()
	p.logFile = nil
	p.logger.Warn("background job log file reached its cap",
		zap.String("id", p.id), zap.String("path", p.logPath), zap.Int64("cap_bytes", p.maxLog))
}

// detachLogLocked gives up on a log file that cannot be written, without
// stopping the job: the process is still useful, and the window still works.
func (p *process) detachLogLocked(err error) {
	if p.logFile != nil {
		_ = p.logFile.Close()
		p.logFile = nil
	}
	p.logCapped = true
	p.logger.Warn("background job log file is no longer writable",
		zap.String("id", p.id), zap.String("path", p.logPath), zap.Error(err))
}

// finishReading closes the log file and finalizes if everything else is done.
func (p *process) finishReading() {
	p.mu.Lock()
	if p.logFile != nil {
		_ = p.logFile.Sync()
		_ = p.logFile.Close()
		p.logFile = nil
	}
	p.readerDone = true
	p.mu.Unlock()
	p.tryFinalize()
}

// supervise reaps the shell and then waits for the process group to empty.
//
// Waiting for the group is the difference between an honest answer and a
// convenient one: `npm run dev` execs into node, but a wrapper script, a
// pipeline or anything that forks leaves the server behind after the shell
// exits. A job reported as finished while its server still listens would be
// wrong in the one way that matters.
func (p *process) supervise() {
	err := p.cmd.Wait()

	p.mu.Lock()
	p.reaped = true
	switch {
	case err == nil:
		zero := 0
		p.exitCode = &zero
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			p.exitCode = &code
		} else {
			// Wait itself failed: no exit code to report, the group probe and
			// the log still tell the story.
			p.logger.Warn("background job wait failed",
				zap.String("id", p.id), zap.Error(err))
		}
	}
	p.mu.Unlock()

	p.awaitGroupGone()
	p.tryFinalize()
}

// awaitGroupGone blocks until the job's process group has no members left, or
// the manager is closing.
func (p *process) awaitGroupGone() {
	if groupIsGone(p.pgid) {
		p.markGroupGone()
		return
	}
	ticker := time.NewTicker(groupPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopPoll:
			// Closing: Stop/Close already decided this job's fate, and Close
			// finalizes the record itself.
			return
		case <-ticker.C:
			if groupIsGone(p.pgid) {
				p.markGroupGone()
				return
			}
		}
	}
}

// markGroupGone records that the process group is empty. The drain deadline
// starts here: from this moment nothing in the group can write, so the pipe
// should close almost immediately, and only an escaped process could keep it
// open.
func (p *process) markGroupGone() {
	p.mu.Lock()
	p.groupGone = true
	p.mu.Unlock()
	time.AfterFunc(groupDrainGrace, func() {
		if p.readingDone() {
			return
		}
		p.mu.Lock()
		p.outputOpen = true
		p.mu.Unlock()
		p.tryFinalize()
	})
}

// readingDone reports whether the reader has reached EOF.
func (p *process) readingDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readerDone
}

// tryFinalize moves the job to its terminal state once the shell has been
// reaped, the group is empty, and the output has been drained — or once an
// escaped process has been given its drain grace.
func (p *process) tryFinalize() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finalized || !p.reaped || !p.groupGone {
		return
	}
	if !p.readerDone && !p.outputOpen {
		return
	}
	p.finalizeLocked()
}

// finalizeLocked records the end of the job and wakes every waiter.
func (p *process) finalizeLocked() {
	if p.finalized {
		return
	}
	p.finalized = true
	p.endedAt = time.Now()
	if p.logFile != nil {
		_ = p.logFile.Sync()
		_ = p.logFile.Close()
		p.logFile = nil
	}
	// A job we asked to stop is reported as stopped even if it also exited on
	// its own in the same instant: the caller's intent is the more useful fact.
	if p.stopAsked {
		p.status = StatusStopped
	} else {
		p.status = StatusExited
	}
	close(p.done)
}

// forceFinish finalizes a job during shutdown even though its group may still
// exist, so that nothing waiting on it can wait forever.
func (p *process) forceFinish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groupGone = true
	p.readerDone = true
	p.finalizeLocked()
}

// markStopAsked records that this job was asked to stop.
func (p *process) markStopAsked() {
	p.mu.Lock()
	p.stopAsked = true
	p.mu.Unlock()
}

// running reports whether the job is still alive.
func (p *process) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.finalized
}

// finished reports whether the job is over (and its record therefore prunable).
func (p *process) finished() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.finalized
}

// read serves one window of output, clamped to what the window still holds.
func (p *process) read(from int64, maxBytes int) Read {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := p.window.Total()
	start := p.window.Start()
	req := from
	skipped := int64(0)
	switch {
	case req <= 0:
		// The tail: what a first look wants, and the only thing an offset older
		// than the window could be served as anyway.
		req = total - int64(maxBytes)
		if req < start {
			req = start
		}
	case req < start:
		skipped = start - req
		req = start
	}
	if req > total {
		req = total
	}
	data := p.window.Slice(req, maxBytes)
	return Read{
		ID:             p.id,
		Status:         p.status,
		ExitCode:       p.exitCode,
		Data:           string(data),
		From:           req,
		Next:           req + int64(len(data)),
		TotalBytes:     total,
		DiscardedBytes: total - int64(p.window.Len()),
		SkippedBytes:   skipped,
		OutputOpen:     p.outputOpen,
	}
}

// snapshot copies the job's state for a caller.
func (p *process) snapshot() *Job {
	p.mu.Lock()
	defer p.mu.Unlock()

	ended := p.endedAt
	uptime := time.Since(p.startedAt)
	if !ended.IsZero() {
		uptime = ended.Sub(p.startedAt)
	}
	out := &Job{
		ID:             p.id,
		Name:           p.spec.Name,
		Command:        p.spec.Command,
		Cwd:            p.spec.Cwd,
		Workspace:      p.spec.Workspace,
		WorkspaceRoot:  p.spec.WorkspaceRoot,
		RequestedBy:    p.spec.RequestedBy,
		Scope:          p.spec.Scope,
		PID:            p.pgid,
		Status:         p.status,
		ExitCode:       p.exitCode,
		StartedAt:      p.startedAt,
		UptimeMS:       uptime.Milliseconds(),
		LogPath:        p.logPath,
		LogBytes:       p.logBytes,
		LogCapped:      p.logCapped,
		TotalBytes:     p.window.Total(),
		DiscardedBytes: p.window.Total() - int64(p.window.Len()),
		OutputOpen:     p.outputOpen,
	}
	if !ended.IsZero() {
		e := ended
		out.EndedAt = &e
	}
	return out
}

// exitCodeOrSentinel renders an absent exit code for a log line.
func (j Job) exitCodeOrSentinel() int {
	if j.ExitCode == nil {
		return -1
	}
	return *j.ExitCode
}
