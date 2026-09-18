package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/prompt"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// runCtx builds a command whose stdin is the given reader, the way `run` reads a
// piped task.
func runCtx(stdin string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(stdin))
	return cmd
}

// TestResolveRunPrompt pins the three input forms and, more importantly, that
// stdin is read only when the caller asked for it.
//
// The negative case is the one that matters: a git hook's stdin is not empty
// (pre-push receives the refs being pushed), so a "stdin is a pipe, so use it"
// heuristic would silently append a list of refs to the instruction.
func TestResolveRunPrompt(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{name: "bare prompt", args: []string{"say hi"}, want: "say hi"},
		{name: "prompt is trimmed", args: []string{"  say hi  "}, want: "say hi"},
		{name: "empty prompt", args: []string{"   "}, wantErr: true},
		{name: "no args", wantErr: true},

		{name: "dash reads the whole prompt", args: []string{"-"}, stdin: "review the diff\n", want: "review the diff"},
		{name: "dash with empty stdin", args: []string{"-"}, stdin: "\n", wantErr: true},

		{
			name:  "dash plus instruction appends stdin",
			args:  []string{"-", "review this"},
			stdin: "--- a/x.go\n+++ b/x.go\n",
			want:  "review this" + stdinDataBanner + "--- a/x.go\n+++ b/x.go\n```\n",
		},
		{
			name:  "empty pipe with an instruction is not an error",
			args:  []string{"-", "review this"},
			stdin: "",
			want:  "review this",
		},
		{name: "two args must start with dash", args: []string{"nope", "review this"}, wantErr: true},
		{name: "empty instruction", args: []string{"-", "  "}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRunPrompt(runCtx(tt.stdin), tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRunPrompt: %v", err)
			}
			if got != tt.want {
				t.Errorf("prompt =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestExitCodes are the contract a script branches on, so the mapping from an
// error to a code is pinned rather than left to main()'s default.
func TestExitCodes(t *testing.T) {
	if got := exitCodeFor(nil); got != exitOK {
		t.Errorf("no error = %d, want %d", got, exitOK)
	}
	if got := exitCodeFor(context.DeadlineExceeded); got != exitAgentError {
		t.Errorf("a plain error = %d, want %d (the default for every other command)", got, exitAgentError)
	}
	for _, code := range []int{exitUsage, exitConfig, exitTimeout, exitBudget, exitToolFailure} {
		err := withExitCode(code, context.DeadlineExceeded)
		if got := exitCodeFor(err); got != code {
			t.Errorf("withExitCode(%d) mapped to %d", code, got)
		}
		// The wrapped error still has to be reachable: callers use errors.Is on
		// it to tell a timeout from an ordinary failure.
		if err.Error() == "" {
			t.Errorf("code %d produced an empty message", code)
		}
	}
	if withExitCode(exitUsage, nil) != nil {
		t.Error("withExitCode(nil) must stay nil, or a success would look like a failure")
	}
}

// TestRunWriterKeepsStdoutClean is the whole contract of the command in one test.
//
// A one-shot run is routinely piped into something else, so a log line or a
// progress dot landing on stdout does not merely look untidy — it corrupts the
// caller's data. The emitter and the report are exercised together because the
// leak would come from either.
func TestRunWriterKeepsStdoutClean(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := newRunOutput(false, false, &stdout, &stderr, nil)

	o.emit(chat.Event{Type: chat.EventToolCall, Step: 1, ToolName: "read_file", ToolArgs: `{"path":"x"}`})
	o.emit(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "Hel"})
	o.emit(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "lo"})
	o.emit(chat.Event{Type: chat.EventToolResult, Step: 1, ToolName: "read_file", DurationMs: 3})
	o.emit(chat.Event{Type: chat.EventUsage, Step: 1, Usage: &chat.Usage{TotalTokens: 10}})
	o.emit(chat.Event{Type: chat.EventReasoningDelta, Step: 1, Text: "thinking"})

	res := &chat.Result{
		Text:       "Hello",
		Usage:      chat.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
		StopReason: "",
		Plan: []chat.Step{{Index: 1, Tools: []chat.ToolRun{{Name: "read_file", DurationMs: 3}},
			Text: "Hel", Reasoning: "hmm"}},
	}
	o.report(res, "m", "p", "sess", nil)

	if got := stdout.String(); got != "Hello\n" {
		t.Errorf("stdout = %q, want exactly the answer plus a newline", got)
	}
	for _, forbidden := range []string{"read_file", "%!", "INFO", "WARN", "step(s)"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Errorf("stdout contains %q; progress and logs must go to stderr", forbidden)
		}
	}
	if stderr.Len() == 0 {
		t.Error("stderr is empty; the progress lines and the summary belong there")
	}
	if !strings.Contains(stderr.String(), "read_file") {
		t.Errorf("stderr should name the tool it ran, got:\n%s", stderr.String())
	}
}

// TestRunWriterReasoningNeverReachesStdout: the model's thinking is for the
// operator watching, not for the caller's pipeline. Putting it on stdout would
// splice scratch work into the answer.
func TestRunWriterReasoningNeverReachesStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := newRunOutput(false, true, &stdout, &stderr, nil)
	o.emit(chat.Event{Type: chat.EventReasoningDelta, Text: "let me think about this"})
	o.report(&chat.Result{Text: "the answer"}, "m", "p", "s", nil)

	if strings.Contains(stdout.String(), "think") {
		t.Errorf("reasoning leaked to stdout: %q", stdout.String())
	}
	if got := stdout.String(); got != "the answer\n" {
		t.Errorf("stdout = %q, want just the answer", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("--quiet still wrote to stderr: %q", stderr.String())
	}
}

// TestRunWriterJSONIsOneObject: JSON mode buffers the answer instead of
// streaming it, so the caller gets exactly one parseable object and nothing else.
func TestRunWriterJSONIsOneObject(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := newRunOutput(true, true, &stdout, &stderr, nil)

	o.emit(chat.Event{Type: chat.EventTextDelta, Text: "partial"})
	o.emit(chat.Event{Type: chat.EventToolCall, ToolName: "bash"})
	o.report(&chat.Result{
		Text:  "the answer",
		Usage: chat.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, DurationMs: 42},
		Plan: []chat.Step{
			{Index: 1, Tools: []chat.ToolRun{{Name: "bash", DurationMs: 5}}},
			{Index: 2, Tools: []chat.ToolRun{{Name: "write_file", Err: "disk full"}}},
		},
	}, "deepseek-chat", "deepseek", "sess-1", nil)

	var got runResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\ngot: %s", err, stdout.String())
	}
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Errorf("expected exactly one line, got:\n%s", stdout.String())
	}
	if got.Answer != "the answer" || got.Model != "deepseek-chat" || got.Provider != "deepseek" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session_id = %q", got.SessionID)
	}
	if got.StopReason != runStopDone {
		t.Errorf("stop_reason = %q, want %q for a run that produced an answer", got.StopReason, runStopDone)
	}
	if got.Usage.TotalTokens != 13 || got.Usage.PromptTokens != 10 || got.Usage.DurationMs != 42 {
		t.Errorf("usage wrong: %+v", got.Usage)
	}
	if got.Usage.Cost == nil || got.Usage.Cost.Priced {
		t.Errorf("an unpriced run must say so rather than report 0: %+v", got.Usage.Cost)
	}
	if got.ToolFailures != 1 || len(got.Failures) != 1 {
		t.Fatalf("tool_failures = %d, failures = %+v", got.ToolFailures, got.Failures)
	}
	if got.Failures[0].Tool != "write_file" || got.Failures[0].Step != 2 {
		t.Errorf("failure should name the tool and its step: %+v", got.Failures[0])
	}
	if len(got.Steps) != 2 || got.Steps[0].Tools[0].Name != "bash" || !got.Steps[0].Tools[0].OK {
		t.Errorf("steps wrong: %+v", got.Steps)
	}
	if got.Steps[1].Tools[0].OK {
		t.Error("a tool call that errored must not be reported ok")
	}
}

// TestRunWriterJSONReportsFailure: a failing run still emits a legal object, so a
// caller can read why instead of finding empty output and a non-zero code.
func TestRunWriterJSONReportsFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := newRunOutput(true, true, &stdout, &stderr, nil)
	o.report(nil, "m", "p", "s", context.DeadlineExceeded)

	var got runResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("a failed run produced unparseable output: %v (%s)", err, stdout.String())
	}
	if got.Error == "" {
		t.Error("error should be reported in the object")
	}
	if got.Steps == nil || got.Failures == nil {
		t.Error("steps and failures must be arrays, not null: a caller should not have to nil-check them")
	}
}

// TestRunWriterBudgetStopReason keeps the reason visible: --output json's
// stop_reason is how a script tells "the model answered" from "we cut it off".
func TestRunWriterBudgetStopReason(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := newRunOutput(true, true, &stdout, &stderr, nil)
	o.report(&chat.Result{Text: "half an answer", StopReason: chat.StopSteps}, "m", "p", "s", nil)

	var got runResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StopReason != chat.StopSteps {
		t.Errorf("stop_reason = %q, want %q", got.StopReason, chat.StopSteps)
	}
}

// TestRunSurfaceWithholdsInteractiveTools pins what the surface name buys: a
// one-shot run must not be offered a tool whose every call could only fail or
// hang. ask_user would park the turn waiting for a person who is not there.
func TestRunSurfaceWithholdsInteractiveTools(t *testing.T) {
	st := openTempStore(t)
	// Both registries come from the same config, differing only in the surface,
	// so a difference in the result can only be the surface.
	newReg := func(surface string) *tool.Registry {
		t.Helper()
		cfg := &config.Config{}
		cfg.Tools.Workspace = t.TempDir()
		cfg.Tools.EnableBash = true
		cfg.Chat.Plan.Enable = true
		reg := tool.NewRegistry()
		if err := registerBuiltinTools(reg, cfg, st, zap.NewNop(), nil,
			toolSetOptions{Surface: surface}); err != nil {
			t.Fatalf("registerBuiltinTools(%s): %v", surface, err)
		}
		return reg
	}

	runOnly, web := newReg(prompt.SurfaceRun), newReg(prompt.SurfaceWeb)

	for _, name := range []string{"ask_user", "plan_create", "plan_update", "plan_read"} {
		if _, ok := runOnly.Get(name); ok {
			t.Errorf("%s must not exist on the run surface: nothing can answer it", name)
		}
		if _, ok := web.Get(name); !ok {
			t.Errorf("sanity: %s should exist on the web surface (its absence would make this test vacuous)", name)
		}
	}
	// The tools a one-shot run is actually for are present.
	for _, name := range []string{"read_file", "grep", "write_file", "bash"} {
		if _, ok := runOnly.Get(name); !ok {
			t.Errorf("%s should be available on the run surface", name)
		}
	}
}

// TestRunSurfaceWithholdsBackgroundTools covers the other half: a background
// process outlives the run that started it, and nobody is around afterwards to
// stop it. It takes an explicit flag.
func TestRunSurfaceWithholdsBackgroundTools(t *testing.T) {
	base := &config.Config{}
	base.Tools.Workspace = t.TempDir()
	base.Tools.EnableBash = true // the background tools sit behind the shell
	base.Tools.EnableBackground = true
	st := openTempStore(t)

	jobMgr, err := newJobManager(base, zap.NewNop())
	if err != nil {
		t.Skipf("no job manager available in this environment: %v", err)
	}
	defer jobMgr.Close()

	withheld := tool.NewRegistry()
	if err := registerBuiltinTools(withheld, base, st, zap.NewNop(), nil, toolSetOptions{
		Jobs:    jobMgr,
		Surface: prompt.SurfaceRun,
	}); err != nil {
		t.Fatalf("registerBuiltinTools: %v", err)
	}
	if _, ok := withheld.Get("bash_background"); ok {
		t.Error("bash_background must be withheld on the run surface by default")
	}

	allowed := tool.NewRegistry()
	if err := registerBuiltinTools(allowed, base, st, zap.NewNop(), nil, toolSetOptions{
		Jobs:            jobMgr,
		Surface:         prompt.SurfaceRun,
		AllowBackground: true,
	}); err != nil {
		t.Fatalf("registerBuiltinTools with --allow-background: %v", err)
	}
	if _, ok := allowed.Get("bash_background"); !ok {
		t.Error("--allow-background must register the background tools")
	}
}

// TestResolveRunSession: an unknown --session is a configuration error, not a
// silently created conversation. A typo that looks like it worked is worse than
// a refusal.
func TestResolveRunSession(t *testing.T) {
	ctx := context.Background()
	st := openTempStore(t)

	id, err := resolveRunSession(ctx, st, "", "deepseek", "deepseek-chat", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("no session id returned")
	}
	if _, err := st.GetChatSession(ctx, id); err != nil {
		t.Errorf("the session was not persisted: %v", err)
	}

	again, err := resolveRunSession(ctx, st, id, "deepseek", "deepseek-chat", "")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if again != id {
		t.Errorf("--session reused the wrong conversation: %q vs %q", again, id)
	}

	if _, err := resolveRunSession(ctx, st, "no-such-session", "p", "m", ""); err == nil {
		t.Error("an unknown --session must be an error")
	} else if got := exitCodeFor(err); got != exitConfig {
		t.Errorf("unknown --session exited %d, want %d (configuration)", got, exitConfig)
	}
}

// TestResolveRunWorkspace: the run is bound to a real directory, reuses the
// workspace already pointing at it, and refuses a path that is not there.
func TestResolveRunWorkspace(t *testing.T) {
	ctx := context.Background()
	st := openTempStore(t)
	dir := t.TempDir()

	cfg := &config.Config{}
	cfg.Tools.Workspace = dir

	first, err := resolveRunWorkspace(ctx, cfg, st, zap.NewNop())
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first == "" {
		t.Fatal("no workspace name returned")
	}

	second, err := resolveRunWorkspace(ctx, cfg, st, zap.NewNop())
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second != first {
		t.Errorf("resolving the same directory twice produced two workspaces: %q then %q", first, second)
	}

	// --workspace pointing somewhere else registers that directory and reuses it
	// on the next call.
	other := t.TempDir()
	runWorkspace = other
	defer func() { runWorkspace = "" }()
	third, err := resolveRunWorkspace(ctx, cfg, st, zap.NewNop())
	if err != nil {
		t.Fatalf("--workspace resolve: %v", err)
	}
	fourth, err := resolveRunWorkspace(ctx, cfg, st, zap.NewNop())
	if err != nil {
		t.Fatalf("--workspace resolve again: %v", err)
	}
	if third != fourth {
		t.Errorf("--workspace registered a new workspace on the second call: %q then %q", third, fourth)
	}
	if third == first {
		t.Error("--workspace did not move the run to the directory it named")
	}

	runWorkspace = filepath.Join(other, "does-not-exist")
	if _, err := resolveRunWorkspace(ctx, cfg, st, zap.NewNop()); err == nil {
		t.Error("a --workspace that is not a directory must be refused")
	}
	runWorkspace = ""
}

// TestRunConfigDefaults: the one-shot command has to work with no configuration
// at all, which is what makes it usable from a hook.
func TestRunConfigDefaults(t *testing.T) {
	var c config.RunConfig
	if got := c.OutputOr(); got != config.RunOutputText {
		t.Errorf("default output = %q, want text (an unconfigured run prints the answer)", got)
	}
	if got := c.Timeout(); got != 0 {
		t.Errorf("default timeout = %v, want 0 (unlimited)", got)
	}
	if got := c.MaxStepsOr(12); got != 12 {
		t.Errorf("default max steps = %d, want the interactive cap 12", got)
	}
	if got := (config.RunConfig{MaxSteps: 3}).MaxStepsOr(12); got != 3 {
		t.Errorf("configured max steps = %d, want 3", got)
	}
	if got := (config.RunConfig{DefaultOutput: "JSON"}).OutputOr(); got != config.RunOutputJSON {
		t.Errorf("configured output = %q, want json", got)
	}

	// The shipped example config must load these keys.
	cfg, err := config.Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if cfg.Run.OutputOr() != config.RunOutputText {
		t.Errorf("example config output = %q", cfg.Run.OutputOr())
	}
}

// openTempStore opens a throwaway database for a test.
func openTempStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "run.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestOneLine keeps the progress lines to one line: a progress line that wraps
// over ten lines is worse than no progress line.
func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\nc"); got != "a b c" {
		t.Errorf("oneLine = %q", got)
	}
	long := strings.Repeat("x", 500)
	got := oneLine(long)
	if len([]rune(got)) > 210 {
		t.Errorf("oneLine did not bound the length: %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated line should say so")
	}
}

// TestSameDir covers the comparison that decides whether a --workspace run
// reuses an existing workspace or registers a second one for the same checkout.
func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	if !sameDir(dir, dir) {
		t.Error("identical paths must compare equal")
	}
	if !sameDir(dir, dir+"/") {
		t.Error("a trailing separator must not make two paths different directories")
	}
	if sameDir(dir, t.TempDir()) {
		t.Error("different directories must not compare equal")
	}
	if sameDir("", dir) || sameDir(dir, "") {
		t.Error("an empty path must never compare equal")
	}
}

// TestPromptSurfaceRunExists keeps the surface wired: the tool set withholds
// tools by surface name, so a name the prompt package does not know would mean
// the model gets no non-interactive instructions at all.
func TestPromptSurfaceRunExists(t *testing.T) {
	got := prompt.For(prompt.SurfaceRun)
	if got == prompt.Base() {
		t.Fatal("the run surface has no prompt section; the model would not know it cannot ask questions")
	}
	if !strings.Contains(got, "ask_user") && !strings.Contains(got, "提问") {
		t.Error("the run surface's section should say that questions are impossible")
	}
}

// TestPersistRunAnswer stores the turn so the console can show it.
func TestPersistRunAnswer(t *testing.T) {
	ctx := context.Background()
	st := openTempStore(t)

	sessID, err := resolveRunSession(ctx, st, "", "p", "m", "")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	res := &chat.Result{
		Text:    "the answer",
		Usage:   chat.Usage{TotalTokens: 7, PromptTokens: 5, CompletionTokens: 2},
		Plan:    []chat.Step{{Index: 1, Tools: []chat.ToolRun{{Name: "bash"}}}},
		Steps:   1,
		TraceID: "t-1",
	}
	if err := persistRunAnswer(ctx, st, sessID, res, zap.NewNop()); err != nil {
		t.Fatalf("persistRunAnswer: %v", err)
	}

	msgs, err := st.ListChatMessages(ctx, sessID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].Content != "the answer" || msgs[0].Role != "assistant" {
		t.Errorf("stored message wrong: %+v", msgs[0])
	}
	if !strings.Contains(msgs[0].Steps, "bash") {
		t.Errorf("the step detail was not stored, so the console cannot render the turn: %q", msgs[0].Steps)
	}
	if msgs[0].UsageJSON == "" {
		t.Error("usage was not stored with the answer")
	}
}

// TestRunResultTimesOut is a smoke test for the deadline path: a context that is
// already expired must not look like a successful run.
func TestRunResultTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	var stdout, stderr bytes.Buffer
	o := newRunOutput(false, true, &stdout, &stderr, nil)
	rr := o.report(nil, "m", "p", "s", ctx.Err())
	if rr.Error == "" {
		t.Error("a timed-out run must record why")
	}
	if stdout.Len() != 0 {
		t.Errorf("a timed-out run wrote to stdout: %q", stdout.String())
	}
}
