package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
)

// stubTool is a registered tool that does nothing, so a test can assert what a
// subagent is offered without any of them running.
type stubTool struct {
	name string
	cap  tool.Capability
	mode tool.Concurrency
}

func (s stubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: "stub"}, nil
}

func (s stubTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "ok", nil
}

func (s stubTool) Capability() tool.Capability   { return s.cap }
func (s stubTool) Concurrency() tool.Concurrency { return s.mode }

// parentRegistry is a registry shaped like a real turn's: reads, a write, a
// command, the interactive tools, and the plan tools.
func parentRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	for _, s := range []stubTool{
		{name: "read_file", cap: tool.CapRead, mode: tool.ParallelSafe},
		{name: "grep", cap: tool.CapRead, mode: tool.ParallelSafe},
		{name: "diagnostics", cap: tool.CapRead, mode: tool.ParallelSafe},
		{name: "write_file", cap: tool.CapWrite, mode: tool.Serial},
		{name: "bash", cap: tool.CapExec, mode: tool.Serial},
		{name: "ask_user", cap: tool.CapRead, mode: tool.Serial},
		{name: "plan_update", cap: tool.CapRead, mode: tool.Serial},
		{name: "plan_read", cap: tool.CapRead, mode: tool.Serial},
		{name: "save_document", cap: tool.CapRead, mode: tool.Serial},
		{name: "spawn_agent", cap: tool.CapRead, mode: tool.ParallelSafe},
	} {
		if err := reg.Register(s); err != nil {
			t.Fatalf("register %s: %v", s.name, err)
		}
	}
	return reg
}

// TestNarrowNeverGrantsTheThreeForbidden is the safety rule: three tools are
// never handed to a subagent, whatever the model asks for.
func TestNarrowNeverGrantsTheThreeForbidden(t *testing.T) {
	parent := parentRegistry(t)
	names := func(reg *tool.Registry) map[string]bool {
		out := map[string]bool{}
		specs, err := reg.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range specs {
			out[s.Name] = true
		}
		return out
	}

	// Default narrowing.
	got := names(mustNarrow(t, parent, nil))
	for _, forbidden := range []string{"ask_user", "spawn_agent", "plan_update", "plan_read"} {
		if got[forbidden] {
			t.Errorf("%s must never be handed to a subagent", forbidden)
		}
	}
	// And the reads it is for are there.
	for _, want := range []string{"read_file", "grep", "diagnostics"} {
		if !got[want] {
			t.Errorf("%s should be inherited by default (got %v)", want, got)
		}
	}
	// Without being asked for, a write and a command do not come along.
	for _, unwanted := range []string{"write_file", "bash"} {
		if got[unwanted] {
			t.Errorf("%s must not be inherited without being asked for", unwanted)
		}
	}

	// Asking explicitly does not make the forbidden three available.
	for _, forbidden := range []string{"ask_user", "spawn_agent", "plan_update"} {
		if _, err := Narrow(parent, []string{forbidden}); err == nil {
			t.Errorf("Narrow(%q) should refuse", forbidden)
		} else if !strings.Contains(err.Error(), forbidden) {
			t.Errorf("the refusal should name the tool, got: %v", err)
		}
	}
}

// TestNarrowGrantsWhatIsAskedFor: a subagent that must run tests is a legitimate
// request, and the grant still carries the parent's own policies because it is the
// same wrapped tool.
func TestNarrowGrantsWhatIsAskedFor(t *testing.T) {
	parent := parentRegistry(t)
	reg := mustNarrow(t, parent, []string{"bash", "write_file"})

	for _, want := range []string{"bash", "write_file", "read_file"} {
		if _, ok := reg.Get(want); !ok {
			t.Errorf("%s should be granted (got the registry: %v)", want, names(t, reg))
		}
	}
	// The granted tools are the parent's own instances, so the approval gate, the
	// checkpoint guard and the sandbox that wrap them here are the ones in force.
	parentBash, _ := parent.Get("bash")
	granted, _ := reg.Get("bash")
	if parentBash != granted {
		t.Error("the granted tool is a copy; it must be the parent's own wrapped tool, or the grant would bypass the parent's policies")
	}
}

// TestNarrowRefusesAnUnknownName: a typo must not become "granted nothing".
func TestNarrowRefusesAnUnknownName(t *testing.T) {
	parent := parentRegistry(t)
	if _, err := Narrow(parent, []string{"nonexistent"}); err == nil {
		t.Error("granting a tool the turn does not have must be refused, not ignored")
	}
}

// --- the agent ---

// scriptedRunner records what it was asked to run and returns a scripted result.
type scriptedRunner struct {
	mu       sync.Mutex
	requests []NestedRequest
	result   NestedResult
	err      error
	hold     time.Duration
	onRun    func()
}

func (s *scriptedRunner) RunNested(ctx context.Context, in NestedRequest) (NestedResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, in)
	onRun, hold := s.onRun, s.hold
	res, err := s.result, s.err
	s.mu.Unlock()

	if onRun != nil {
		onRun()
	}
	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
		}
	}
	return res, err
}

func (s *scriptedRunner) lastRequest(t *testing.T) NestedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the nested run was never started")
	}
	return s.requests[len(s.requests)-1]
}

// newAgent builds an agent over a scripted runner.
func newAgent(t *testing.T, runner Runner, limits Limits) *Agent {
	t.Helper()
	a, err := New(runner, limits, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// parent publishes what a turn publishes, so Spawn has something to derive from.
func parent(t *testing.T) tool.TurnResources {
	t.Helper()
	return tool.TurnResources{
		Registry:     parentRegistry(t),
		Model:        &stubModel{},
		SessionID:    "sess-1",
		Scope:        "scope-1",
		SystemPrompt: "你是这个部署的 agent。",
	}
}

// stubModel satisfies the model interface without doing anything: nothing in these
// tests reaches a provider, because the runner is scripted.
type stubModel struct{}

func (stubModel) Generate(context.Context, []*schema.Message, ...einomodel.Option) (*schema.Message, error) {
	return nil, errors.New("stub model: nothing in these tests reaches a provider")
}

func (stubModel) Stream(context.Context, []*schema.Message, ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stub model: nothing in these tests reaches a provider")
}

// TestSpawnSendsOnlyTheBrief: the subagent sees a system prompt and the task, and
// nothing of the parent's conversation. That isolation is the whole feature.
func TestSpawnSendsOnlyTheBrief(t *testing.T) {
	runner := &scriptedRunner{result: NestedResult{Text: "结论", Steps: 3,
		Tools: []string{"grep", "grep", "read_file"}, Usage: Usage{TotalTokens: 1200}}}
	a := newAgent(t, runner, Limits{MaxSteps: 6})

	report, err := a.Spawn(context.Background(), Options{
		Prompt: "调研一下 internal/chat 的工具循环",
		Parent: parent(t),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	req := runner.lastRequest(t)
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want a system prompt and the task", len(req.Messages))
	}
	if req.Messages[0].Role != schema.System || !strings.Contains(req.Messages[0].Content, "子 agent") {
		t.Errorf("the system message does not introduce a subagent: %q", req.Messages[0].Content)
	}
	if !strings.Contains(req.Messages[0].Content, "你是这个部署的 agent。") {
		t.Error("the subagent should inherit the deployment's prompt rather than starting from nothing")
	}
	if req.Messages[1].Role != schema.User || req.Messages[1].Content != "调研一下 internal/chat 的工具循环" {
		t.Errorf("the task is not the second message: %+v", req.Messages[1])
	}
	if req.MaxSteps != 6 {
		t.Errorf("max steps = %d, want the configured 6", req.MaxSteps)
	}
	// The nested run is attributable to the parent turn.
	if req.SessionID != "sess-1" || req.Scope != "scope-1" {
		t.Errorf("the nested run lost its attribution: %+v", req)
	}
	// And the report carries what the footer needs.
	if !strings.Contains(report.Text, "grep×2") || !strings.Contains(report.Text, "1200 tokens") {
		t.Errorf("the footer does not summarise how the report was produced:\n%s", report.Text)
	}
	if !strings.Contains(report.Text, "3 步") {
		t.Errorf("the footer should count the steps:\n%s", report.Text)
	}
}

// TestSpawnTruncatesAndSaysSo: a report that was cut must not read as complete.
func TestSpawnTruncatesAndSaysSo(t *testing.T) {
	long := strings.Repeat("很长的结论。", 200)
	runner := &scriptedRunner{result: NestedResult{Text: long, Steps: 1}}
	a := newAgent(t, runner, Limits{MaxReportChars: 100})

	report, err := a.Spawn(context.Background(), Options{Prompt: "x", Parent: parent(t)})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !report.Truncated {
		t.Error("the report should be marked truncated")
	}
	if len([]rune(report.Text)) > 100+120 { // the truncation note adds a footer
		t.Errorf("the report is %d runes, far over the 100-rune cap", len([]rune(report.Text)))
	}
	if !strings.Contains(report.Text, "报告过长已截断") {
		t.Errorf("truncation must be stated:\n%s", report.Text)
	}
}

// TestSpawnFailureIsAReport: a failed subagent is an observation the parent acts
// on, not a turn failure.
func TestSpawnFailureIsAReport(t *testing.T) {
	runner := &scriptedRunner{err: errors.New("模型调用失败")}
	a := newAgent(t, runner, Limits{})

	report, err := a.Spawn(context.Background(), Options{Prompt: "x", Parent: parent(t)})
	if err != nil {
		t.Fatalf("a failed subagent must not be an error: %v", err)
	}
	if !report.Failed {
		t.Error("the report should be marked failed")
	}
	if !strings.Contains(report.Text, "没有跑完") || !strings.Contains(report.Text, "模型调用失败") {
		t.Errorf("the report should say what went wrong:\n%s", report.Text)
	}
	if !strings.Contains(report.Text, "改用更小的任务") {
		t.Error("the failure should tell the model what to do next")
	}
}

// TestSpawnRefusesWithoutTurnResources: a tool that cannot know which registry to
// hand on must refuse rather than guess.
func TestSpawnRefusesWithoutTurnResources(t *testing.T) {
	a := newAgent(t, &scriptedRunner{}, Limits{})
	if _, err := a.Spawn(context.Background(), Options{Prompt: "x"}); err == nil {
		t.Error("a spawn with no registry or model must be refused")
	}
	if _, err := a.Spawn(context.Background(), Options{Prompt: "   ", Parent: parent(t)}); err == nil {
		t.Error("an empty prompt must be refused")
	}
}

// TestSpawnReadOnlyRefusesWhenNothingIsUsable: a subagent with no tools at all can
// only talk, which is not what anybody asked for.
func TestSpawnRefusesWithNoUsableTools(t *testing.T) {
	empty := tool.NewRegistry()
	a := newAgent(t, &scriptedRunner{}, Limits{})
	_, err := a.Spawn(context.Background(), Options{
		Prompt: "x",
		Parent: tool.TurnResources{Registry: empty, Model: &stubModel{}},
	})
	if err == nil {
		t.Error("a subagent with no tools should be refused")
	}
}

// TestSpawnGateBoundsConcurrency is the cost bound: "spawn three explorations" is
// the intended use, and four parents each spawning four is how a fan-out becomes a
// bill.
func TestSpawnGateBoundsConcurrency(t *testing.T) {
	var (
		mu       sync.Mutex
		inflight int
		peak     int
	)
	runner := &scriptedRunner{hold: 60 * time.Millisecond}
	runner.onRun = func() {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
	}
	// The release has to happen after the hold, so it rides the runner's return by
	// way of a second hook: simplest is to decrement when RunNested returns, which
	// this wrapper does by wrapping onRun's scope.
	wrapped := &releaseRunner{inner: runner, release: func() {
		mu.Lock()
		inflight--
		mu.Unlock()
	}}
	a := newAgent(t, wrapped, Limits{MaxConcurrent: 2, MaxSteps: 2})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Spawn(context.Background(), Options{Prompt: "任务", Parent: parent(t)}); err != nil {
				t.Errorf("Spawn: %v", err)
			}
		}()
	}
	wg.Wait()

	if peak > 2 {
		t.Errorf("peak subagent concurrency was %d, want at most 2", peak)
	}
	if peak < 2 {
		t.Errorf("peak was %d; six spawns with a gate of 2 should reach it", peak)
	}
	if got := a.Gate(); got != 0 {
		t.Errorf("the gate holds %d slots after every spawn returned, want 0", got)
	}
}

// releaseRunner decrements the in-flight counter when a nested run returns.
type releaseRunner struct {
	inner   *scriptedRunner
	release func()
}

func (r *releaseRunner) RunNested(ctx context.Context, in NestedRequest) (NestedResult, error) {
	res, err := r.inner.RunNested(ctx, in)
	r.release()
	return res, err
}

// TestSpawnTimeout: a nested run is bounded, because it runs inside a turn that is
// bounded.
func TestSpawnTimeout(t *testing.T) {
	runner := &scriptedRunner{hold: 500 * time.Millisecond, result: NestedResult{Text: "late"}}
	a := newAgent(t, runner, Limits{MaxSteps: 2})

	start := time.Now()
	// A parent context with no deadline of its own, and a per-spawn timeout.
	report, err := a.Spawn(context.Background(), Options{
		Prompt:  "x",
		Parent:  parent(t),
		Timeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("the spawn took %v; the per-spawn timeout is not in effect", elapsed)
	}
	// The scripted runner returns its result regardless, so what this asserts is
	// that the context was cancelled rather than that the report changed.
	_ = report
}

// TestSpawnHonoursParentCancellation: a stopped turn stops its subagents.
func TestSpawnHonoursParentCancellation(t *testing.T) {
	runner := &scriptedRunner{hold: 2 * time.Second}
	a := newAgent(t, runner, Limits{MaxSteps: 2})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The gate acquisition is what watches the context when the gate is full;
		// here it is free, so the nested run sees the cancellation.
		_, _ = a.Spawn(ctx, Options{Prompt: "x", Parent: parent(t)})
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled parent left the subagent running")
	}
}

// TestSpawnStepsAreCappedByTheLowerOfTheTwo: a per-spawn cap can only lower the
// configuration, never raise it.
func TestSpawnStepsCappedBothWays(t *testing.T) {
	runner := &scriptedRunner{result: NestedResult{Text: "ok"}}
	a := newAgent(t, runner, Limits{MaxSteps: 5})

	if _, err := a.Spawn(context.Background(), Options{Prompt: "x", Parent: parent(t), MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if got := runner.lastRequest(t).MaxSteps; got != 2 {
		t.Errorf("a lower per-spawn cap was ignored: %d", got)
	}
	if _, err := a.Spawn(context.Background(), Options{Prompt: "x", Parent: parent(t), MaxSteps: 20}); err != nil {
		t.Fatal(err)
	}
	if got := runner.lastRequest(t).MaxSteps; got != 5 {
		t.Errorf("a per-spawn cap raised the configured one: %d, want 5", got)
	}
}

// TestSpawnNamesTheRun: the log and the card need something to call it.
func TestSpawnNamesTheRun(t *testing.T) {
	runner := &scriptedRunner{result: NestedResult{Text: "ok"}}
	a := newAgent(t, runner, Limits{})

	named, err := a.Spawn(context.Background(), Options{Prompt: "x", Name: "auth-survey", Parent: parent(t)})
	if err != nil {
		t.Fatal(err)
	}
	if named.Name != "auth-survey" {
		t.Errorf("name = %q, want the one asked for", named.Name)
	}

	derived, err := a.Spawn(context.Background(), Options{
		Prompt: "调研一下整个 internal/chat 目录里工具调用的执行顺序以及它和审计日志的关系",
		Parent: parent(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if derived.Name == "" || len([]rune(derived.Name)) > 30 {
		t.Errorf("a derived name should be short and non-empty: %q", derived.Name)
	}
}

// --- helpers ---

func mustNarrow(t *testing.T, parent *tool.Registry, extra []string) *tool.Registry {
	t.Helper()
	reg, err := Narrow(parent, extra)
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	return reg
}

func names(t *testing.T, reg *tool.Registry) []string {
	t.Helper()
	specs, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// TestReportSaysWhenStepsAreUnknown: a runner that cannot count steps must not
// report zero of them. Eino's ReAct loop returns only the final message, and "0
// steps, no tools" for a subagent that worked for a minute reads as "it did
// nothing" — a parent model that believes it will distrust a good report, which is
// what happened the first time the CLI ran one.
func TestReportSaysWhenStepsAreUnknown(t *testing.T) {
	runner := &scriptedRunner{result: NestedResult{Text: "结论", StepsUnavailable: true}}
	a := newAgent(t, runner, Limits{})

	report, err := a.Spawn(context.Background(), Options{Prompt: "x", Parent: parent(t)})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(report.Text, "不可得") {
		t.Errorf("the footer should say the count is unavailable:\n%s", report.Text)
	}
	if strings.Contains(report.Text, "0 步") {
		t.Errorf("the footer claims zero steps, which is a lie with consequences:\n%s", report.Text)
	}
	// The report itself is unaffected.
	if !strings.Contains(report.Text, "结论") {
		t.Errorf("the report was lost:\n%s", report.Text)
	}
}
