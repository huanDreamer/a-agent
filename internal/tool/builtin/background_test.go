package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/workspace"
)

// These tests run real background processes against a real workspace rooted in a
// temporary directory. The properties that matter — that a forked server keeps
// running after the call, that a refused start executes nothing, that a job
// started in one workspace cannot be stopped from another — only exist in the
// interaction with the operating system.

// bgTools builds the four tools over a manager whose grace is short, and returns
// them by name alongside the manager so a test can inspect what it started.
func bgTools(t *testing.T, ws *workspace.Workspace, policy BackgroundPolicy) (map[string]tool.InvokableTool, *jobs.Manager) {
	t.Helper()
	mgr, err := jobs.New(jobs.Options{
		Dir:        t.TempDir(),
		StartGrace: 50 * time.Millisecond,
		StopGrace:  500 * time.Millisecond,
		Logger:     zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(mgr.Close)
	return bgToolsOver(t, mgr, ws, policy), mgr
}

// bgToolsOver builds a tool set bound to one workspace over an existing manager.
//
// Sharing the manager is what one agent process actually does: every workspace
// gets its own bound tools, while the supervisor — and therefore the process
// list and the job limits — belongs to the process.
func bgToolsOver(t *testing.T, mgr *jobs.Manager, ws *workspace.Workspace, policy BackgroundPolicy) map[string]tool.InvokableTool {
	t.Helper()
	list, err := NewBackgroundTools(ws, mgr, policy)
	if err != nil {
		t.Fatalf("NewBackgroundTools: %v", err)
	}
	out := make(map[string]tool.InvokableTool, len(list))
	for _, tl := range list {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		out[info.Name] = tl
	}
	return out
}

// bgRun invokes a tool and decodes its JSON result. A Go error is handed back to
// the caller rather than failing the test, because a refusal is a result too.
func bgRun[T any](t *testing.T, it tool.InvokableTool, in any) (T, error) {
	t.Helper()
	var zero T
	args, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	raw, err := it.InvokableRun(context.Background(), string(args))
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode result %q: %v", raw, err)
	}
	return out, nil
}

// bgMust runs a tool and fails the test on any Go error.
func bgMust[T any](t *testing.T, it tool.InvokableTool, in any) T {
	t.Helper()
	out, err := bgRun[T](t, it, in)
	if err != nil {
		t.Fatalf("tool call %+v: %v", in, err)
	}
	return out
}

// bgWorkspace returns a workspace rooted in a fresh temporary directory.
func bgWorkspace(t *testing.T, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// TestNewBackgroundTools_RefusesWhatItCannotWork checks the constructor's
// contract: a tool bound to nothing must fail loudly at build time, because a
// registered tool that can only fail is worse than an absent one.
func TestNewBackgroundTools_RefusesWhatItCannotWork(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	mgr, err := jobs.New(jobs.Options{Dir: t.TempDir(), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	if _, err := NewBackgroundTools(nil, mgr, DefaultBackgroundPolicy()); err == nil {
		t.Error("a nil workspace was accepted")
	}
	if _, err := NewBackgroundTools(ws, nil, DefaultBackgroundPolicy()); err == nil {
		t.Error("a nil job manager was accepted")
	}
	bad := BackgroundPolicy{Enabled: true, DenyPatterns: []string{"("}}
	if _, err := NewBackgroundTools(ws, mgr, bad); err == nil {
		t.Error("an invalid deny pattern was accepted")
	}
}

// TestBackgroundTools_NamesAreTheDocumentedFour guards the names three places
// have to agree on: the constructors, the per-turn workspace rebinding, and the
// console's tool list.
func TestBackgroundTools_NamesAreTheDocumentedFour(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	want := []string{BackgroundToolName, BackgroundListToolName, BackgroundOutputToolName, BackgroundStopToolName}
	for _, name := range want {
		if _, ok := tools[name]; !ok {
			t.Errorf("%s is missing: the set is %v", name, toolNamesOf(tools))
		}
	}
	if len(tools) != len(want) {
		t.Errorf("built %d tools, want %d: %v", len(tools), len(want), toolNamesOf(tools))
	}
}

// TestBackgroundTools_DescriptionsSayHowToRunNonInteractively covers the half of
// the stdin caveat that costs a turn when it is missing: saying that a command
// which prompts fails, without saying what to do instead, leaves the model with
// a prompt it cannot answer.
func TestBackgroundTools_DescriptionsSayHowToRunNonInteractively(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	info, err := tools[BackgroundToolName].Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	for _, want := range []string{"/dev/null", "non-interactive", "--yes", "CI=1"} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("description is missing %q:\n%s", want, info.Desc)
		}
	}
}

// TestBackgroundStart_RecordsTheConversationScope covers the link between a job
// and the conversation that started it: the console shows the count on that
// session, which is only possible if the tool recorded which session it was in.
func TestBackgroundStart_RecordsTheConversationScope(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	policy := DefaultBackgroundPolicy()
	policy.Scope = func(context.Context) string { return "web:session-1" }
	tools, mgr := bgTools(t, ws, policy)

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "sleep 30",
		Name:    "scoped",
		// The scope reader is handed the call's context, so a background job
		// carries the same conversation the turn belonged to.
	})
	job, err := mgr.Get(started.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Scope != "web:session-1" {
		t.Errorf("scope = %q, want %q", job.Scope, "web:session-1")
	}
}

// TestBackgroundStart_UnscopedIsAValidState keeps a surface with no
// conversations (a plain CLI run) working: without a scope reader the job is
// recorded unscoped rather than the call being refused.
func TestBackgroundStart_UnscopedIsAValidState(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, mgr := bgTools(t, ws, DefaultBackgroundPolicy())

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "sleep 30",
		Name:    "unscoped",
	})
	job, err := mgr.Get(started.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Scope != "" {
		t.Errorf("scope = %q, want empty for a surface with no conversations", job.Scope)
	}
}

// toolNamesOf lists the tool names of a set, for a failure message.
func toolNamesOf(set map[string]tool.InvokableTool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// TestBackgroundStart_JobOutlivesTheCall is the point of the whole change: the
// process must still be running after the tool call returns, and its output must
// be readable afterwards.
func TestBackgroundStart_JobOutlivesTheCall(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, mgr := bgTools(t, ws, DefaultBackgroundPolicy())

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "echo listening on 5173; sleep 30",
		Name:    "vite",
	})
	if !started.Running || started.Status != jobs.StatusRunning {
		t.Fatalf("status = %q running = %v, want a running job", started.Status, started.Running)
	}
	if started.ID == "" || started.PID <= 0 || started.LogPath == "" {
		t.Fatalf("result = %+v, want an id, a pid and a log path", started)
	}
	if started.Cwd != "." {
		t.Errorf("cwd = %q, want the workspace root as .", started.Cwd)
	}

	// Reading with wait_ms is how a caller watches a server come up; the line was
	// printed before this call, so it comes back at once.
	out := bgMust[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: started.ID})
	if !strings.Contains(out.Output, "listening on 5173") {
		t.Fatalf("output = %q, want the line the process printed", out.Output)
	}
	if out.Status != jobs.StatusRunning || !out.Running {
		t.Errorf("status = %q, want running", out.Status)
	}

	// It must still be alive: the tool call ending must not have ended it.
	if mgr.Running() != 1 {
		t.Fatalf("running jobs = %d, want 1: the job was killed by the call ending", mgr.Running())
	}

	// The listing sees it, with the label it was given.
	list := bgMust[BackgroundListOutput](t, tools[BackgroundListToolName], BackgroundListInput{})
	if list.Total != 1 || list.Running != 1 {
		t.Fatalf("list = %+v, want one running job", list)
	}
	if list.Jobs[0].Name != "vite" || list.Jobs[0].ID != started.ID {
		t.Errorf("listing = %+v, want the job that was started", list.Jobs[0])
	}

	// And stopping it works, and reports how it ended.
	stopped := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID})
	if !stopped.Stopped || stopped.Status != jobs.StatusStopped {
		t.Fatalf("stop = %+v, want a stopped job", stopped)
	}
	if stopped.ExitCode == nil {
		t.Error("a stopped job has no exit code")
	}
	if mgr.Running() != 0 {
		t.Errorf("running jobs = %d after stop, want 0", mgr.Running())
	}
}

// bgWaitFor polls until cond holds, so a test does not depend on how fast a
// shell reaches a state — only on it getting there.
func bgWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBackgroundStart_KeepsWorkingAfterTheCall is the complaint this whole
// change answers, tested the only way that proves it: the process must not just
// still exist after the tool call returns, it must keep doing its work.
//
// A heartbeat file is used rather than a real server so the test needs nothing
// installed, and the count of lines is a direct observation of a process that is
// still running.
func TestBackgroundStart_KeepsWorkingAfterTheCall(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	beat := filepath.Join(ws.Root(), "heartbeat")
	beats := func() int {
		b, err := os.ReadFile(beat)
		if err != nil {
			return 0
		}
		return strings.Count(string(b), "\n")
	}

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "while true; do echo beat >> heartbeat; sleep 0.05; done",
	})
	if !started.Running {
		t.Fatalf("the job is not running: %+v", started)
	}
	bgWaitFor(t, "the process to write its first heartbeat", func() bool { return beats() > 0 })

	// The call that started it has returned; the process must carry on.
	before := beats()
	time.Sleep(300 * time.Millisecond)
	after := beats()
	if after <= before {
		t.Fatalf("heartbeats went from %d to %d after the call returned: the process was killed with the call", before, after)
	}

	// And stopping it must actually stop the work, not just the shell that
	// started it.
	if out := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID}); !out.Stopped {
		t.Fatalf("stop = %+v", out)
	}
	time.Sleep(200 * time.Millisecond)
	stopped := beats()
	time.Sleep(300 * time.Millisecond)
	if final := beats(); final != stopped {
		t.Errorf("the process kept writing after being stopped: %d then %d beats", stopped, final)
	}
}

// TestBackgroundStart_ReportsAnImmediateFailure is the busy-port case: it must
// arrive in one call, with the exit code and the output, rather than costing a
// poll.
func TestBackgroundStart_ReportsAnImmediateFailure(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	out := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "echo 'port 5173 is already in use' >&2; exit 1",
	})
	if out.Running {
		t.Fatalf("result = %+v, want a job that already ended", out)
	}
	if out.Status != jobs.StatusExited {
		t.Errorf("status = %q, want %q", out.Status, jobs.StatusExited)
	}
	if out.ExitCode == nil || *out.ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", out.ExitCode)
	}
	if !strings.Contains(out.Output, "already in use") {
		t.Errorf("output = %q, want the message the command printed", out.Output)
	}
	if out.Note == "" {
		t.Error("the result does not explain that the job died during the startup grace")
	}
}

// TestBackgroundTools_RefusalsExecuteNothing covers the shared policy: a refused
// start must leave no trace, which is only provable by looking for the file the
// command would have created.
func TestBackgroundTools_RefusalsExecuteNothing(t *testing.T) {
	cases := []struct {
		name   string
		wsOpts workspace.Options
		policy BackgroundPolicy
		in     BackgroundStartInput
	}{
		{
			name:   "read-only workspace",
			wsOpts: workspace.Options{ReadOnly: true},
			policy: DefaultBackgroundPolicy(),
			in:     BackgroundStartInput{Command: "touch marker"},
		},
		{
			name:   "disabled by policy",
			wsOpts: workspace.Options{},
			policy: BackgroundPolicy{Enabled: false},
			in:     BackgroundStartInput{Command: "touch marker"},
		},
		{
			name:   "deny pattern",
			wsOpts: workspace.Options{},
			policy: BackgroundPolicy{Enabled: true, DenyPatterns: []string{`\btouch\b`}},
			in:     BackgroundStartInput{Command: "touch marker"},
		},
		{
			name:   "cwd outside the workspace",
			wsOpts: workspace.Options{},
			policy: DefaultBackgroundPolicy(),
			in:     BackgroundStartInput{Command: "touch marker", Cwd: "../.."},
		},
		{
			name:   "empty command",
			wsOpts: workspace.Options{},
			policy: DefaultBackgroundPolicy(),
			in:     BackgroundStartInput{Command: "   "},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := bgWorkspace(t, tc.wsOpts)
			tools, mgr := bgTools(t, ws, tc.policy)

			if _, err := bgRun[BackgroundStartOutput](t, tools[BackgroundToolName], tc.in); err == nil {
				t.Fatal("the call was accepted")
			}
			if _, err := os.Stat(filepath.Join(ws.Root(), "marker")); !os.IsNotExist(err) {
				t.Errorf("the refused command ran: marker exists (err = %v)", err)
			}
			if jobs := mgr.List(); len(jobs) != 0 {
				t.Errorf("a refused start left %d job record(s)", len(jobs))
			}
		})
	}
}

// TestBackgroundTools_CannotManageAnotherWorkspacesJob keeps the workspace layer
// meaningful: a job id from elsewhere must be refused, not silently obeyed.
func TestBackgroundTools_CannotManageAnotherWorkspacesJob(t *testing.T) {
	alpha := bgWorkspace(t, workspace.Options{})
	beta := bgWorkspace(t, workspace.Options{})
	// One supervisor, two workspaces: the shape the agent process actually has.
	alphaTools, mgr := bgTools(t, alpha, DefaultBackgroundPolicy())
	betaTools := bgToolsOver(t, mgr, beta, DefaultBackgroundPolicy())

	started := bgMust[BackgroundStartOutput](t, alphaTools[BackgroundToolName], BackgroundStartInput{
		Command: "sleep 30",
		Name:    "alpha-server",
	})

	// Beta's listing does not show it, and says nothing about other workspaces
	// unless something is actually running there.
	list := bgMust[BackgroundListOutput](t, betaTools[BackgroundListToolName], BackgroundListInput{})
	if list.Total != 0 {
		t.Errorf("beta sees %d job(s) of alpha: %+v", list.Total, list.Jobs)
	}
	if !strings.Contains(list.Note, "other workspaces") {
		t.Errorf("note = %q, want a line saying other workspaces have running jobs", list.Note)
	}

	for _, name := range []string{BackgroundOutputToolName, BackgroundStopToolName} {
		switch name {
		case BackgroundOutputToolName:
			if _, err := bgRun[BackgroundOutputResult](t, betaTools[name], BackgroundOutputInput{ID: started.ID}); err == nil {
				t.Errorf("%s read another workspace's job", name)
			}
		case BackgroundStopToolName:
			if _, err := bgRun[BackgroundStopOutput](t, betaTools[name], BackgroundStopInput{ID: started.ID}); err == nil {
				t.Errorf("%s stopped another workspace's job", name)
			}
		}
	}
	// Alpha can still stop its own.
	if out := bgMust[BackgroundStopOutput](t, alphaTools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID}); !out.Stopped {
		t.Errorf("alpha could not stop its own job: %+v", out)
	}
}

// TestBackgroundStop_ForgetAndSecondCall checks the two awkward cases: stopping
// something that already ended is reported rather than treated as a failure, and
// forgetting is only possible once it has ended.
func TestBackgroundStop_ForgetAndSecondCall(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{Command: "sleep 30"})

	if out := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID}); !out.Stopped {
		t.Fatalf("first stop = %+v, want it stopped", out)
	}
	second := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID})
	if second.Stopped {
		t.Error("the second stop claims to have signalled a job that was already over")
	}
	if second.Note == "" {
		t.Error("the second stop does not explain that the job had already ended")
	}

	forgotten := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID, Forget: true})
	if !forgotten.Forgotten {
		t.Fatalf("forget = %+v, want the record dropped", forgotten)
	}
	if _, err := os.Stat(started.LogPath); !os.IsNotExist(err) {
		t.Errorf("the log file survived forget (err = %v)", err)
	}
	if _, err := bgRun[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: started.ID}); err == nil {
		t.Error("a forgotten job is still readable")
	}
}

// TestBackgroundStop_ForgetOnARunningJobKeepsItVisible is the one refusal that
// protects the operator: deleting the record of a running process would hide a
// process nothing can stop afterwards.
func TestBackgroundStop_ForgetOnARunningJobKeepsItVisible(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	// A process that ignores SIGTERM outlives the short stop grace this test's
	// manager was built with, so the "still running" branch is reachable.
	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "trap '' TERM; sleep 30",
	})
	out := bgMust[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: started.ID, Forget: true})
	// Either the SIGKILL escalation finished it (then forget is allowed) or it
	// survived (then the record must still be there). Both are honest outcomes;
	// what must never happen is a forgotten job that is still running.
	if out.Forgotten && out.Running {
		t.Fatalf("a running job was forgotten: %+v", out)
	}
	if !out.Forgotten {
		if _, err := bgRun[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: started.ID}); err != nil {
			t.Errorf("the record was dropped without being forgotten: %v", err)
		}
	}
}

// TestBackgroundList_RunningOnly checks the filter, since a long-lived session
// accumulates finished records.
func TestBackgroundList_RunningOnly(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	finished := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{Command: "echo done"})
	running := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{Command: "sleep 30"})
	if finished.Running {
		t.Fatalf("the first job is unexpectedly running: %+v", finished)
	}

	all := bgMust[BackgroundListOutput](t, tools[BackgroundListToolName], BackgroundListInput{})
	if all.Total != 2 || all.Running != 1 {
		t.Errorf("all = %+v, want 2 jobs of which 1 running", all)
	}
	only := bgMust[BackgroundListOutput](t, tools[BackgroundListToolName], BackgroundListInput{RunningOnly: true})
	if only.Total != 1 || only.Jobs[0].ID != running.ID {
		t.Errorf("running_only = %+v, want only %s", only, running.ID)
	}
}

// TestBackgroundOutput_FollowsByOffset checks the loop a log view uses: read the
// tail, then keep reading from next, and be told when bytes were dropped.
func TestBackgroundOutput_FollowsByOffset(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	started := bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{
		Command: "echo first; sleep 0.4; echo second; sleep 30",
	})

	// Wait for the second line rather than sleeping: the read returns as soon as
	// there is something new past the offset.
	first := bgMust[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: started.ID})
	second := bgMust[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{
		ID:     started.ID,
		From:   first.Next,
		WaitMS: 3000,
	})
	if !strings.Contains(second.Output, "second") {
		t.Fatalf("follow-up read = %q, want the line printed after the first read", second.Output)
	}
	if second.From != first.Next || second.Next < second.From {
		t.Errorf("offsets went wrong: first.Next=%d second.From=%d second.Next=%d", first.Next, second.From, second.Next)
	}
	if second.SkippedBytes != 0 || second.DiscardedBytes != 0 {
		t.Errorf("a contiguous follow-up reported gaps: %+v", second)
	}

	// Reading again with nothing new returns nothing and stays put.
	empty := bgMust[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: started.ID, From: second.Next})
	if empty.Output != "" {
		t.Errorf("read past the end returned %q", empty.Output)
	}
	if empty.Next != second.Next {
		t.Errorf("offset moved from %d to %d without new output", second.Next, empty.Next)
	}
}

// TestBackgroundTools_UnknownIdIsRefused checks the error a stale id from an
// earlier conversation gets.
func TestBackgroundTools_UnknownIdIsRefused(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	tools, _ := bgTools(t, ws, DefaultBackgroundPolicy())

	for _, id := range []string{"job-000000", ""} {
		if _, err := bgRun[BackgroundOutputResult](t, tools[BackgroundOutputToolName], BackgroundOutputInput{ID: id}); err == nil {
			t.Errorf("bash_output accepted the id %q", id)
		}
		if _, err := bgRun[BackgroundStopOutput](t, tools[BackgroundStopToolName], BackgroundStopInput{ID: id}); err == nil {
			t.Errorf("bash_stop accepted the id %q", id)
		}
	}
}

// TestBackgroundTools_MaxJobsIsReported checks that the limit reaches the model
// as an explanation rather than as an opaque failure.
func TestBackgroundTools_MaxJobsIsReported(t *testing.T) {
	ws := bgWorkspace(t, workspace.Options{})
	mgr, err := jobs.New(jobs.Options{
		Dir: t.TempDir(), MaxJobs: 1, StartGrace: 50 * time.Millisecond, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(mgr.Close)
	list, err := NewBackgroundTools(ws, mgr, DefaultBackgroundPolicy())
	if err != nil {
		t.Fatalf("NewBackgroundTools: %v", err)
	}
	tools := map[string]tool.InvokableTool{}
	for _, tl := range list {
		info, _ := tl.Info(context.Background())
		tools[info.Name] = tl
	}

	bgMust[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{Command: "sleep 30"})
	_, err = bgRun[BackgroundStartOutput](t, tools[BackgroundToolName], BackgroundStartInput{Command: "sleep 30"})
	if err == nil {
		t.Fatal("a second job was accepted with a limit of one")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
}
