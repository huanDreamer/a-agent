package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/workspace"
)

// Background tools: the other half of the shell story.
//
// The bash tool kills the whole process group when a call ends — deliberately,
// because a tool call that leaves processes behind leaks them with nobody
// tracking them. That rule is also why a dev server, a --watch build or a
// resident database cannot be started at all: they are defined by outliving the
// call that started them.
//
// These four tools make "resident" an explicit request instead of a side effect:
// bash_background starts one, bash_jobs lists them, bash_output reads what they
// printed, bash_stop ends one. Everything they start is supervised by
// internal/jobs, which bounds the output, tracks the process group, and
// terminates what is left when the agent exits.
//
// What is deliberately shared with bash, and why: the same policy checks
// (read-only workspace, deny patterns, cwd resolution, the shell seam) run before
// anything is spawned, because "the operator refused this command" must not
// depend on which tool asked for it.

// Background tool names. They are constants because three places have to agree
// on them: the constructors below, the per-turn workspace rebinding that decides
// which tools a conversation may see, and the console's tool list.
const (
	BackgroundToolName       = "bash_background"
	BackgroundListToolName   = "bash_jobs"
	BackgroundOutputToolName = "bash_output"
	BackgroundStopToolName   = "bash_stop"
)

// BackgroundPolicy bounds what the background tools may do, and records what the
// jobs they start belong to.
//
// It has no timeout on purpose. A timeout is exactly what a background job must
// not have; what bounds it instead is the concurrent-job limit, the log cap, and
// the life of the agent process — see jobs.Options.
type BackgroundPolicy struct {
	// Enabled turns the tools on at all. When false every call is refused; the
	// caller is expected to withhold the tools instead, so the model is never
	// offered something it may not use.
	Enabled bool
	// DenyPatterns are RE2 patterns matched against the command; a match refuses
	// the call before anything runs. They are the same patterns the bash tool
	// refuses on, with the same speed-bump caveat: this is not a security
	// boundary and is trivially evaded by anyone who wants to.
	DenyPatterns []string
	// Workspace is the label of the workspace these tools are bound to. It is
	// recorded on every job so the console can show where a process runs.
	Workspace string
	// Surface names the caller: web, cli or feishu.
	Surface string
	// Scope returns the conversation a job belongs to, read from the turn's
	// context. Nil (or a function returning "") leaves jobs unscoped, which is
	// what a surface with no conversations gets.
	//
	// It is a function rather than a string because the answer is per turn: one
	// tool set serves every conversation of a surface, so the scope can only be
	// read when a call is actually being made.
	Scope func(ctx context.Context) string
}

// DefaultBackgroundPolicy is the policy a caller gets when it has no opinion:
// enabled, with the same deny list as the bash tool.
func DefaultBackgroundPolicy() BackgroundPolicy {
	return BackgroundPolicy{
		Enabled:      true,
		DenyPatterns: append([]string(nil), bashDefaultDenyPatterns...),
	}
}

// backgroundConfig is the validated snapshot of a BackgroundPolicy: patterns
// compiled once at construction instead of on every call.
type backgroundConfig struct {
	enabled   bool
	deny      []bashDenyRule
	workspace string
	surface   string
	// scope reads the conversation a call belongs to, from the turn's context.
	// Nil when the surface has no conversations.
	scope func(ctx context.Context) string
}

// NewBackgroundTools returns the four background-process tools bound to ws and
// sharing one manager.
//
// ws scopes where a job starts and which jobs a conversation may see: a job
// belongs to the workspace it was started in, and managing another workspace's
// process from this one is refused rather than allowed quietly. mgr supervises
// the processes themselves; a nil manager is refused, because tools that cannot
// work are worse than tools that are not offered.
func NewBackgroundTools(ws *workspace.Workspace, mgr *jobs.Manager, policy BackgroundPolicy) ([]tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("bash_background: a workspace is required")
	}
	if mgr == nil {
		return nil, fmt.Errorf("bash_background: a job manager is required")
	}
	cfg, err := backgroundConfigFrom(policy)
	if err != nil {
		return nil, err
	}

	start, err := utils.InferTool(BackgroundToolName, backgroundStartDescription(),
		func(ctx context.Context, in BackgroundStartInput) (BackgroundStartOutput, error) {
			return backgroundStart(ctx, ws, mgr, cfg, in)
		})
	if err != nil {
		return nil, err
	}
	list, err := utils.InferTool(BackgroundListToolName, backgroundListDescription(),
		func(ctx context.Context, in BackgroundListInput) (BackgroundListOutput, error) {
			return backgroundList(ws, mgr, in)
		})
	if err != nil {
		return nil, err
	}
	output, err := utils.InferTool(BackgroundOutputToolName, backgroundOutputDescription(),
		func(ctx context.Context, in BackgroundOutputInput) (BackgroundOutputResult, error) {
			return backgroundOutput(ctx, ws, mgr, in)
		})
	if err != nil {
		return nil, err
	}
	stop, err := utils.InferTool(BackgroundStopToolName, backgroundStopDescription(),
		func(ctx context.Context, in BackgroundStopInput) (BackgroundStopOutput, error) {
			return backgroundStop(ctx, ws, mgr, in)
		})
	if err != nil {
		return nil, err
	}
	return []tool.InvokableTool{start, list, output, stop}, nil
}

// backgroundConfigFrom validates a policy and compiles its deny patterns.
func backgroundConfigFrom(policy BackgroundPolicy) (backgroundConfig, error) {
	deny, err := compileDenyPatterns(policy.DenyPatterns)
	if err != nil {
		return backgroundConfig{}, err
	}
	return backgroundConfig{
		enabled:   policy.Enabled,
		deny:      deny,
		workspace: strings.TrimSpace(policy.Workspace),
		surface:   strings.TrimSpace(policy.Surface),
		scope:     policy.Scope,
	}, nil
}

// scopeOf reads the conversation this call belongs to. A nil reader (a surface
// with no conversations) and an empty answer mean the same thing — unscoped —
// so a caller never has to distinguish "not supported" from "not known".
func (c backgroundConfig) scopeOf(ctx context.Context) string {
	if c.scope == nil {
		return ""
	}
	return strings.TrimSpace(c.scope(ctx))
}

// backgroundStartDescription is the model-facing description of bash_background.
//
// It states what the tool does not do as carefully as what it does: no PTY and no
// standard input, one interleaved output stream, no resource limits, no
// confinement beyond the working directory, and a lifetime bounded by the agent
// process. A model that believes otherwise will use it badly — a server started
// here dies when the agent does, and a command that prompts will fail rather
// than wait.
func backgroundStartDescription() string {
	return "Start a long-lived process in the background and return a job id for it. " +
		"Use it for things that are meant to keep running: a dev server, a --watch build, a resident API or database. " +
		"For a command that finishes on its own — a build, a test suite, git — use bash instead, which waits for the result. " +
		"The job keeps running after this call returns, so its output is not in the result: read it with bash_output and stop it with bash_stop. " +
		"If the process dies within a short startup grace the result says so and carries its output. " +
		"Limits and honest caveats: a job runs with the full rights of the agent's user and is not sandboxed beyond its working directory. " +
		"It has no terminal and its standard input is /dev/null so a command that prompts fails instead of hanging: start it with the non-interactive flags it offers (-y, --yes, --no-input, CI=1) rather than expecting to answer a prompt once it is running. " +
		"stdout and stderr are captured together as one stream and cannot be told apart. " +
		"There are no CPU or memory quotas. " +
		"Everything started here is stopped when the agent exits, so this is not a way to leave a service running after the agent is gone."
}

// backgroundListDescription is the model-facing description of bash_jobs.
func backgroundListDescription() string {
	return "List the background jobs started in this workspace with their status, process id, run time and log file. " +
		"Jobs that have already ended are included until their record is dropped or forgotten, which is how a server that crashed while you were not looking is found. " +
		"Only jobs of this conversation's workspace are listed: a process started in another workspace is not visible here."
}

// backgroundOutputDescription is the model-facing description of bash_output.
func backgroundOutputDescription() string {
	return "Read what a background job has printed. " +
		"Omit from to get the tail of the output. " +
		"Pass the returned next value as from to continue where the previous call stopped, so following a server's log costs only the new lines. " +
		"wait_ms waits for new output before returning, which is how one call can watch a server start instead of polling. " +
		"The beginning of a long-running job's output is dropped once it grows past the buffered window; that is always reported rather than silently returned as a shorter stream."
}

// backgroundStopDescription is the model-facing description of bash_stop.
func backgroundStopDescription() string {
	return "Stop a background job by signalling its whole process group: SIGTERM first and SIGKILL after a grace period. " +
		"Sending it to the group is what actually stops a server that forked workers. " +
		"Set forget to also drop the job's record and delete its log file, which is only allowed once the job has ended. " +
		"Calling this on a job that already ended is not an error: the result reports how it ended."
}

// BackgroundStartInput is the parameter schema for bash_background.
type BackgroundStartInput struct {
	Command string `json:"command" jsonschema:"description=Shell command that starts a long-lived process such as a dev server or a watch build. Passed verbatim to the shell with no quoting added by the tool, required"`
	// The optional fields carry json:",omitempty" so the schema does not
	// advertise them as required — the reflector treats a field without
	// omitempty as required.
	Name        string `json:"name,omitempty" jsonschema:"description=Short label for this job in listings such as vite or postgres. Optional but it makes bash_jobs readable"`
	Description string `json:"description,omitempty" jsonschema:"description=Short note about why this process is needed. Echoed back in the result so the transcript explains itself"`
	Cwd         string `json:"cwd,omitempty" jsonschema:"description=Directory to run in relative to the workspace root. Defaults to the workspace root"`
}

// BackgroundStartOutput is what bash_background returns.
type BackgroundStartOutput struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Command and Cwd are echoed back so the result explains itself without the
	// model having to re-read its own arguments.
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	// PID is the process group leader; signals go to the group.
	PID int `json:"pid"`
	// Status is running, exited or stopped. Running means the process group
	// still has a member, so a server that forked is still reported as running.
	Status jobs.Status `json:"status"`
	// Running is the same fact as a boolean, for convenience.
	Running bool `json:"running"`
	// ExitCode is set only when the job already ended.
	ExitCode *int `json:"exit_code,omitempty"`
	// LogPath is the durable log file, readable with bash_output or by the
	// operator.
	LogPath string `json:"log_path"`
	// Output carries the captured output when the job already ended during the
	// startup grace: a busy port or a missing binary is then explained here
	// instead of costing another call.
	Output string `json:"output,omitempty"`
	// Note explains anything the rest of the result does not say on its own.
	Note string `json:"note,omitempty"`
}

// backgroundStart starts one job.
func backgroundStart(ctx context.Context, ws *workspace.Workspace, mgr *jobs.Manager, cfg backgroundConfig, in BackgroundStartInput) (BackgroundStartOutput, error) {
	if ws == nil {
		return BackgroundStartOutput{}, fmt.Errorf("%s: a workspace is required", BackgroundToolName)
	}
	command := in.Command
	if strings.TrimSpace(command) == "" {
		return BackgroundStartOutput{}, fmt.Errorf("%s: command is required", BackgroundToolName)
	}
	// Everything that must precede execution is shared with bash, so the same
	// command is refused the same way whichever tool asks for it.
	if err := bashGuard(BackgroundToolName, ws, cfg.enabled, cfg.deny, command); err != nil {
		return BackgroundStartOutput{}, err
	}
	cwd, err := bashResolveCwd(ws, in.Cwd)
	if err != nil {
		return BackgroundStartOutput{}, err
	}
	shell, flags, err := bashShell()
	if err != nil {
		return BackgroundStartOutput{}, err
	}

	job, err := mgr.Start(ctx, jobs.Spec{
		Command:       command,
		Shell:         shell,
		ShellArgs:     flags,
		Cwd:           cwd,
		Env:           bashEnviron(cwd),
		Name:          strings.TrimSpace(in.Name),
		Workspace:     cfg.workspace,
		WorkspaceRoot: ws.Root(),
		RequestedBy:   cfg.surface,
		Scope:         cfg.scopeOf(ctx),
	})
	if err != nil {
		return BackgroundStartOutput{}, fmt.Errorf("%s: %w", BackgroundToolName, err)
	}

	out := BackgroundStartOutput{
		ID:      job.ID,
		Name:    job.Name,
		Command: job.Command,
		Cwd:     ws.Rel(job.Cwd),
		PID:     job.PID,
		Status:  job.Status,
		Running: !job.Status.Terminal(),
		LogPath: job.LogPath,
	}
	if job.Status.Terminal() {
		// It died during the startup grace. Hand back the tail: the reason is
		// almost always in the first lines, and making the model ask for them
		// would be a wasted step on the most common failure path.
		out.ExitCode = job.ExitCode
		if read, rerr := mgr.Output(ctx, job.ID, jobs.ReadOptions{MaxBytes: 4 << 10}); rerr == nil {
			out.Output = read.Data
		}
		out.Note = "the process ended during the startup grace: this job is already finished. " +
			"Its output is above and its exit code explains the failure. " +
			"Use bash_output with this id for more of it."
	}
	return out, nil
}

// BackgroundListInput is the parameter schema for bash_jobs.
type BackgroundListInput struct {
	RunningOnly bool `json:"running_only,omitempty" jsonschema:"description=List only jobs that are still running. Defaults to listing every job including the ones that have ended"`
}

// BackgroundJobSummary is one job in a bash_jobs listing.
type BackgroundJobSummary struct {
	ID      string      `json:"id"`
	Name    string      `json:"name,omitempty"`
	Command string      `json:"command"`
	Cwd     string      `json:"cwd"`
	Status  jobs.Status `json:"status"`
	Running bool        `json:"running"`
	PID     int         `json:"pid"`
	// ExitCode is set only for a job that has ended.
	ExitCode *int `json:"exit_code,omitempty"`
	// UptimeMS is how long it ran, or has been running.
	UptimeMS int64  `json:"uptime_ms"`
	LogPath  string `json:"log_path"`
	// LogBytes is how much output was written to the log file, and LogCapped
	// reports that the file hit its cap and stopped there.
	LogBytes  int64 `json:"log_bytes"`
	LogCapped bool  `json:"log_capped,omitempty"`
	// TotalBytes is all output ever captured; DiscardedBytes is how much of it
	// is no longer buffered.
	TotalBytes     int64 `json:"total_bytes"`
	DiscardedBytes int64 `json:"discarded_bytes,omitempty"`
	// OutputOpen reports a process that left the job's group but still holds the
	// output pipe.
	OutputOpen bool `json:"output_open,omitempty"`
}

// BackgroundListOutput is what bash_jobs returns.
type BackgroundListOutput struct {
	Jobs []BackgroundJobSummary `json:"jobs"`
	// Running counts the jobs still alive, and Total the jobs listed.
	Running int    `json:"running"`
	Total   int    `json:"total"`
	Note    string `json:"note,omitempty"`
}

// backgroundList lists the jobs of the workspace the tools are bound to.
func backgroundList(ws *workspace.Workspace, mgr *jobs.Manager, in BackgroundListInput) (BackgroundListOutput, error) {
	if ws == nil {
		return BackgroundListOutput{}, fmt.Errorf("%s: a workspace is required", BackgroundListToolName)
	}
	out := BackgroundListOutput{Jobs: []BackgroundJobSummary{}}
	root := ws.Root()
	elsewhere := 0
	for _, job := range mgr.List() {
		if job.WorkspaceRoot != "" && job.WorkspaceRoot != root {
			// Counted, not listed: the model does not get to see or manage
			// another workspace's processes, but saying that some exist is more
			// honest than an empty-looking world.
			if !job.Status.Terminal() {
				elsewhere++
			}
			continue
		}
		if in.RunningOnly && job.Status.Terminal() {
			continue
		}
		out.Jobs = append(out.Jobs, BackgroundJobSummary{
			ID:             job.ID,
			Name:           job.Name,
			Command:        job.Command,
			Cwd:            ws.Rel(job.Cwd),
			Status:         job.Status,
			Running:        !job.Status.Terminal(),
			PID:            job.PID,
			ExitCode:       job.ExitCode,
			UptimeMS:       job.UptimeMS,
			LogPath:        job.LogPath,
			LogBytes:       job.LogBytes,
			LogCapped:      job.LogCapped,
			TotalBytes:     job.TotalBytes,
			DiscardedBytes: job.DiscardedBytes,
			OutputOpen:     job.OutputOpen,
		})
	}
	out.Total = len(out.Jobs)
	for _, j := range out.Jobs {
		if j.Running {
			out.Running++
		}
	}
	if elsewhere > 0 {
		out.Note = fmt.Sprintf("%d more job(s) are running in other workspaces: they are not listed here and cannot be managed from this conversation. The admin console lists every job of this process.", elsewhere)
	}
	return out, nil
}

// BackgroundOutputInput is the parameter schema for bash_output.
type BackgroundOutputInput struct {
	ID string `json:"id" jsonschema:"description=Job id returned by bash_background or listed by bash_jobs, required"`
	// From is int64 so a long-running job's offsets survive well past 2 GiB of
	// output, which is not as unlikely as it sounds for a chatty watcher.
	From     int64 `json:"from,omitempty" jsonschema:"description=Byte offset to start reading at. Omit for the tail. Pass the next value from the previous call to continue where it stopped"`
	MaxBytes int   `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes of output to return. Defaults to 16384 and is capped at 1048576"`
	WaitMS   int   `json:"wait_ms,omitempty" jsonschema:"description=Milliseconds to wait for new output before returning. Use it to watch a server start instead of polling. Capped at 30000"`
}

// BackgroundOutputResult is what bash_output returns.
type BackgroundOutputResult struct {
	ID       string      `json:"id"`
	Command  string      `json:"command"`
	Status   jobs.Status `json:"status"`
	Running  bool        `json:"running"`
	ExitCode *int        `json:"exit_code,omitempty"`
	// Output is the requested window of the stream.
	Output string `json:"output"`
	// From is where Output starts, Next is what to pass as from to continue,
	// and TotalBytes is everything captured so far.
	From       int64 `json:"from"`
	Next       int64 `json:"next"`
	TotalBytes int64 `json:"total_bytes"`
	// DiscardedBytes counts output the buffer no longer holds at all, and
	// SkippedBytes what was skipped to serve this call because the requested
	// offset was older than the buffer. Both are zero in the common case and
	// reported in Note when they are not.
	DiscardedBytes int64  `json:"discarded_bytes,omitempty"`
	SkippedBytes   int64  `json:"skipped_bytes,omitempty"`
	LogPath        string `json:"log_path"`
	Note           string `json:"note,omitempty"`
}

// backgroundOutput reads one window of a job's output.
func backgroundOutput(ctx context.Context, ws *workspace.Workspace, mgr *jobs.Manager, in BackgroundOutputInput) (BackgroundOutputResult, error) {
	job, err := backgroundOwnJob(BackgroundOutputToolName, ws, mgr, in.ID)
	if err != nil {
		return BackgroundOutputResult{}, err
	}
	wait := time.Duration(0)
	if in.WaitMS > 0 {
		wait = time.Duration(in.WaitMS) * time.Millisecond
	}
	read, err := mgr.Output(ctx, job.ID, jobs.ReadOptions{
		From:     in.From,
		MaxBytes: in.MaxBytes,
		Wait:     wait,
	})
	if err != nil {
		return BackgroundOutputResult{}, fmt.Errorf("%s: %w", BackgroundOutputToolName, err)
	}

	out := BackgroundOutputResult{
		ID:             job.ID,
		Command:        job.Command,
		Status:         read.Status,
		Running:        !read.Status.Terminal(),
		ExitCode:       read.ExitCode,
		Output:         read.Data,
		From:           read.From,
		Next:           read.Next,
		TotalBytes:     read.TotalBytes,
		DiscardedBytes: read.DiscardedBytes,
		SkippedBytes:   read.SkippedBytes,
		LogPath:        job.LogPath,
	}
	notes := make([]string, 0, 3)
	if read.SkippedBytes > 0 {
		notes = append(notes, fmt.Sprintf("%d bytes between the offset you asked for and the buffer's oldest byte were skipped", read.SkippedBytes))
	}
	if read.DiscardedBytes > 0 {
		notes = append(notes, fmt.Sprintf("the first %d bytes of this job's output are no longer buffered", read.DiscardedBytes))
	}
	if job.LogCapped {
		notes = append(notes, "the log file also hit its cap and stopped there")
	}
	if read.OutputOpen {
		notes = append(notes, "a process left this job's group and still holds its output pipe so output may still arrive")
	}
	if read.Status.Terminal() && read.Next >= read.TotalBytes {
		notes = append(notes, fmt.Sprintf("the job has ended (status %s) and this is the end of its captured output", read.Status))
	}
	if len(notes) > 0 {
		out.Note = strings.Join(notes, "; ") + ": the whole output up to its cap is in the log file at " + job.LogPath
	}
	return out, nil
}

// BackgroundStopInput is the parameter schema for bash_stop.
type BackgroundStopInput struct {
	ID string `json:"id" jsonschema:"description=Job id returned by bash_background or listed by bash_jobs, required"`
	// Signal is a string rather than a number: a model choosing between "term"
	// and "kill" is making a decision it can reason about, and one choosing
	// between 15 and 9 is guessing.
	Signal string `json:"signal,omitempty" jsonschema:"description=term to ask the process to stop or kill to force it. Defaults to term and escalates to kill after the configured grace period"`
	Forget bool   `json:"forget,omitempty" jsonschema:"description=Also drop the job record and delete its log file. Only allowed once the job has ended"`
}

// BackgroundStopOutput is what bash_stop returns.
type BackgroundStopOutput struct {
	ID string `json:"id"`
	// Stopped reports that this call sent a signal to a running job.
	Stopped bool        `json:"stopped"`
	Status  jobs.Status `json:"status"`
	Running bool        `json:"running"`
	// ExitCode is how it ended, when it did.
	ExitCode *int `json:"exit_code,omitempty"`
	// Forgotten reports that the record and the log file are gone.
	Forgotten bool `json:"forgotten,omitempty"`
	// Output is the tail of what the process printed before it stopped, which is
	// where a crash on shutdown explains itself.
	Output  string `json:"output,omitempty"`
	LogPath string `json:"log_path,omitempty"`
	Note    string `json:"note,omitempty"`
}

// backgroundStop stops a job, and optionally forgets it.
func backgroundStop(ctx context.Context, ws *workspace.Workspace, mgr *jobs.Manager, in BackgroundStopInput) (BackgroundStopOutput, error) {
	job, err := backgroundOwnJob(BackgroundStopToolName, ws, mgr, in.ID)
	if err != nil {
		return BackgroundStopOutput{}, err
	}

	signal := syscall.SIGTERM
	if strings.EqualFold(strings.TrimSpace(in.Signal), "kill") {
		signal = syscall.SIGKILL
	}

	out := BackgroundStopOutput{ID: job.ID, Status: job.Status, Running: !job.Status.Terminal(), ExitCode: job.ExitCode, LogPath: job.LogPath}
	final := job
	switch stopped, serr := mgr.Stop(ctx, job.ID, signal, 0); {
	case serr == nil:
		final = stopped
		out.Stopped = true
		out.Status = stopped.Status
		out.Running = !stopped.Status.Terminal()
		out.ExitCode = stopped.ExitCode
	case errors.Is(serr, jobs.ErrNotRunning):
		// Already over: not a failure, and not worth an error the model has to
		// interpret. The result reports how it ended.
		out.Note = "the job had already ended before this call: no signal was sent"
	default:
		return BackgroundStopOutput{}, fmt.Errorf("%s: %w", BackgroundStopToolName, serr)
	}

	if read, rerr := mgr.Output(ctx, job.ID, jobs.ReadOptions{MaxBytes: 4 << 10}); rerr == nil {
		out.Output = read.Data
	}
	if out.Running {
		// Truthful rather than reassuring: the group survived even SIGKILL, so
		// the process is still there and pretending otherwise would be worse
		// than the bad news.
		out.Note = joinNotes(out.Note, "the process group is still alive after the signal")
	}

	if in.Forget {
		if out.Running {
			out.Note = joinNotes(out.Note, "the record was kept because the job is still running: a running job must stay visible so it can be stopped")
			return out, nil
		}
		if _, ferr := mgr.Forget(final.ID); ferr != nil {
			return BackgroundStopOutput{}, fmt.Errorf("%s: forget: %w", BackgroundStopToolName, ferr)
		}
		out.Forgotten = true
		out.Note = joinNotes(out.Note, "the job record and its log file were deleted")
	}
	return out, nil
}

// backgroundOwnJob resolves an id and refuses one that belongs to another
// workspace.
//
// The refusal is deliberate rather than a silent "unknown id": the id could only
// have come from a console listing or an older conversation, and stopping another
// project's dev server because an id looked familiar is exactly the mistake the
// workspace layer exists to prevent. A job with no recorded workspace (started by
// a surface that has none) is manageable from anywhere.
func backgroundOwnJob(name string, ws *workspace.Workspace, mgr *jobs.Manager, id string) (jobs.Job, error) {
	if ws == nil {
		return jobs.Job{}, fmt.Errorf("%s: a workspace is required", name)
	}
	if mgr == nil {
		return jobs.Job{}, fmt.Errorf("%s: background jobs are not available in this deployment", name)
	}
	job, err := mgr.Get(id)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("%s: %w", name, err)
	}
	if job.WorkspaceRoot != "" && job.WorkspaceRoot != ws.Root() {
		return jobs.Job{}, fmt.Errorf(
			"%s: job %s was started in workspace %q (%s): it does not belong to this conversation's workspace and cannot be managed from here",
			name, job.ID, job.Workspace, job.WorkspaceRoot)
	}
	return job, nil
}

// joinNotes joins two notes, tolerating either being empty.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}
