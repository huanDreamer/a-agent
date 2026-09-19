package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// The hook seam is where ClaudeCode compatibility mode reaches a turn, and every
// branch of it changes what the model is told or what the tool receives — which
// is exactly the kind of code that looks right and silently stops blocking after
// a refactor. These tests drive a real Runner (scripted model, real registry) so
// the assertions are about the turn's behaviour rather than about the seam.

// recordingHooks is a scripted Hooks implementation: each method appends the call
// it was given and returns the decision it was told to.
type recordingHooks struct {
	mu        sync.Mutex
	preCalls  []HookCall
	postCalls []HookCall
	stopCalls []HookStop

	pre  HookDecision
	post HookDecision
	stop HookStopDecision

	// stepFn, when set, runs before each Stop decision is returned. It lets a
	// test change the script between calls — the way a hook that only blocks
	// once behaves.
	stepFn func()
}

func (h *recordingHooks) PreToolUse(_ context.Context, call HookCall) HookDecision {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.preCalls = append(h.preCalls, call)
	return h.pre
}

func (h *recordingHooks) PostToolUse(_ context.Context, call HookCall) HookDecision {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.postCalls = append(h.postCalls, call)
	return h.post
}

func (h *recordingHooks) Stop(_ context.Context, stop HookStop) HookStopDecision {
	h.mu.Lock()
	step := h.stepFn
	decision := h.stop
	h.stopCalls = append(h.stopCalls, stop)
	h.mu.Unlock()
	if step != nil {
		step()
	}
	return decision
}

func (h *recordingHooks) counts() (int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.preCalls), len(h.postCalls), len(h.stopCalls)
}

// echoTool records the arguments it was called with and answers with its own
// text, so a test can tell "the hook rewrote the input" from "the tool ran with
// what the model wrote".
type echoTool struct {
	name string

	mu    sync.Mutex
	args  []string
	reply string
	fail  error
}

func (e *echoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: e.name, Desc: "echo"}, nil
}

func (e *echoTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	e.mu.Lock()
	e.args = append(e.args, args)
	reply, fail := e.reply, e.fail
	e.mu.Unlock()
	if fail != nil {
		return "", fail
	}
	return reply, nil
}

func (e *echoTool) calledWith() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.args...)
}

// hookHarness is a Runner with one tool, a scripted model and a scripted hook
// dispatcher.
type hookHarness struct {
	runner *Runner
	model  *fakeModel
	tool   *echoTool
	hooks  *recordingHooks
}

func newHookHarness(t *testing.T, hooks Hooks) *hookHarness {
	t.Helper()
	e := &echoTool{name: "probe", reply: "tool-output"}
	reg := tool.NewRegistry()
	if err := reg.Register(probeAsTool{e}); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	mdl := &fakeModel{}
	r, err := New(Config{
		Model:    mdl,
		Tools:    reg,
		MaxSteps: 4,
		Hooks:    hooks,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &hookHarness{runner: r, model: mdl, tool: e}
	if rec, ok := hooks.(*recordingHooks); ok {
		h.hooks = rec
	}
	return h
}

// probeAsTool adapts the echo tool to the registry's Tool interface.
type probeAsTool struct{ *echoTool }

func (p probeAsTool) Capability() tool.Capability   { return tool.CapRead }
func (p probeAsTool) Concurrency() tool.Concurrency { return tool.Serial }

// hookToolCallTurn scripts one model reply that asks for a tool call on the
// named tool. It is separate from runner_test.go's toolCallTurn, which is fixed
// to its own "loop" tool.
func hookToolCallTurn(id, name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:   id,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      name,
				Arguments: args,
			},
		}},
	}
}

// answerTurn scripts a final answer.
func answerTurn(text string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, Content: text}
}

// lastToolMessage returns the tool result the model was given, which is where a
// rewritten result or a block reason must land.
func lastToolMessage(t *testing.T, msgs []*schema.Message) *schema.Message {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.Tool {
			return msgs[i]
		}
	}
	t.Fatal("the model was never given a tool result")
	return nil
}

func TestPreToolUseBlockSkipsTheCallAndTellsTheModelWhy(t *testing.T) {
	hooks := &recordingHooks{pre: HookDecision{Block: true, BlockReason: "生产环境不允许"}}
	h := newHookHarness(t, hooks)
	h.model.reset([]*schema.Message{
		hookToolCallTurn("c1", "probe", `{"x":1}`),
		answerTurn("好的"),
	})

	res, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := h.tool.calledWith(); len(got) != 0 {
		t.Fatalf("a blocked call must not reach the tool, got args %v", got)
	}
	msgs := h.model.gotMessages[1]
	toolMsg := lastToolMessage(t, msgs)
	if !strings.Contains(toolMsg.Content, "生产环境不允许") {
		t.Errorf("tool result = %q, want the hook's reason", toolMsg.Content)
	}
	if len(res.Tools) != 1 || res.Tools[0].Err == "" {
		t.Errorf("the blocked call must be recorded as failed: %+v", res.Tools)
	}
	pre, post, _ := hooks.counts()
	if pre != 1 {
		t.Errorf("PreToolUse ran %d times, want 1", pre)
	}
	// A call that never ran has no result to report, so PostToolUse is not asked
	// about it: the protocol's PostToolUse describes an execution.
	if post != 0 {
		t.Errorf("PostToolUse ran %d times for a blocked call, want 0", post)
	}
}

func TestPreToolUseUpdatedInputReplacesTheArguments(t *testing.T) {
	hooks := &recordingHooks{pre: HookDecision{UpdatedArgs: `{"rewritten":true}`}}
	h := newHookHarness(t, hooks)
	h.model.reset([]*schema.Message{
		hookToolCallTurn("c1", "probe", `{"original":true}`),
		answerTurn("done"),
	})

	if _, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := h.tool.calledWith()
	if len(got) != 1 {
		t.Fatalf("the tool ran %d times, want 1", len(got))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got[0]), &decoded); err != nil {
		t.Fatalf("the rewritten arguments are not JSON: %v (%s)", err, got[0])
	}
	if decoded["rewritten"] != true {
		t.Errorf("the tool received %s, want the hook's rewrite", got[0])
	}
	// The hook must have been given what the model wrote, not the rewrite: a
	// second PreToolUse hook that could not see the original input would be
	// deciding on text it never read.
	if hooks.preCalls[0].Args != `{"original":true}` {
		t.Errorf("PreToolUse saw %q, want the model's own arguments", hooks.preCalls[0].Args)
	}
}

func TestPostToolUseRewritesTheResultAndAddsContext(t *testing.T) {
	hooks := &recordingHooks{post: HookDecision{
		UpdatedResult:    "由 hook 改写的结果",
		HasUpdatedResult: true,
		Context:          []string{"这个文件是生成的，不要手改。"},
	}}
	h := newHookHarness(t, hooks)
	h.model.reset([]*schema.Message{
		hookToolCallTurn("c1", "probe", `{}`),
		answerTurn("done"),
	})

	res, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := h.model.gotMessages[1]
	toolMsg := lastToolMessage(t, msgs)
	if toolMsg.Content != "由 hook 改写的结果" {
		t.Errorf("tool result = %q, want the hook's rewrite", toolMsg.Content)
	}
	// The injected context must be a system message, and it must come after the
	// result it comments on.
	var sawContext bool
	for i, m := range msgs {
		if strings.Contains(m.Content, "不要手改") {
			if m.Role != schema.System {
				t.Errorf("injected context role = %q, want system", m.Role)
			}
			if i < len(msgs)-1 && msgs[i].Role != schema.System {
				t.Errorf("injected context should follow the tool result, got index %d", i)
			}
			sawContext = true
		}
	}
	if !sawContext {
		t.Error("the PostToolUse context never reached the model")
	}
	if len(res.Tools) != 1 || res.Tools[0].Result != "由 hook 改写的结果" {
		t.Errorf("recorded result = %+v, want the rewrite (the console and the stored turn read this)", res.Tools)
	}
	// The hook saw what the tool actually produced, which is what lets it decide.
	if hooks.postCalls[0].Result != "tool-output" {
		t.Errorf("PostToolUse saw %q, want the tool's own output", hooks.postCalls[0].Result)
	}
}

func TestPostToolUseSeesAFailedCall(t *testing.T) {
	hooks := &recordingHooks{}
	h := newHookHarness(t, hooks)
	h.tool.fail = errProbeFailed
	h.model.reset([]*schema.Message{
		hookToolCallTurn("c1", "probe", `{}`),
		answerTurn("done"),
	})

	if _, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(hooks.postCalls) != 1 {
		t.Fatalf("PostToolUse ran %d times, want 1 even though the tool failed", len(hooks.postCalls))
	}
	if hooks.postCalls[0].Error == "" {
		t.Error("PostToolUse was not told the call failed, so a hook cannot log failures")
	}
}

// errProbeFailed is the failure the echo tool reports in the failing case.
var errProbeFailed = probeError("probe failed on purpose")

type probeError string

func (e probeError) Error() string { return string(e) }

func TestStopHookCanSendTheModelBackToWork(t *testing.T) {
	// The first answer is refused once, then the second is accepted: that is the
	// shape a Stop hook has to support, and it must terminate.
	hooks := &recordingHooks{stop: HookStopDecision{Block: true, Reason: "测试还没跑"}}
	h := newHookHarness(t, hooks)
	h.model.reset([]*schema.Message{
		answerTurn("我先说答案"),
		answerTurn("这次真的结束了"),
	})

	// Unblock after the first refusal, so the test cannot pass by looping until
	// the step budget runs out.
	var calls int
	hooks.stepFn = func() {
		calls++
		if calls >= 1 {
			hooks.stop = HookStopDecision{}
		}
	}

	res, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "这次真的结束了" {
		t.Errorf("answer = %q, want the answer after the hook let it finish", res.Text)
	}
	// The reason must have reached the model as a system message, and the model's
	// own refused answer must still be in the history it was given next.
	msgs := h.model.gotMessages[1]
	var (
		sawReason bool
		sawFirst  bool
	)
	for _, m := range msgs {
		if m.Role == schema.System && strings.Contains(m.Content, "测试还没跑") {
			sawReason = true
		}
		if m.Role == schema.Assistant && strings.Contains(m.Content, "我先说答案") {
			sawFirst = true
		}
	}
	if !sawReason {
		t.Error("the Stop hook's reason never reached the model")
	}
	if !sawFirst {
		t.Error("the refused answer was dropped from the history, so the reason answers nothing")
	}
	if len(hooks.stopCalls) != 2 {
		t.Errorf("Stop ran %d times, want 2 (once refused, once accepted)", len(hooks.stopCalls))
	}
	if hooks.stopCalls[0].AlreadyBlocked {
		t.Error("the first Stop call must not report an earlier block")
	}
	if !hooks.stopCalls[1].AlreadyBlocked {
		t.Error("the second Stop call must report that a hook already blocked once, which is how a hook avoids looping")
	}
}

func TestStopHookBlockWithoutAReasonStillTellsTheModelSomething(t *testing.T) {
	// A hook that blocks and says nothing is the one case where the harness has
	// to invent the instruction, because "continue" with no reason is not
	// something a model can act on. The turn must also still terminate.
	hooks := &recordingHooks{stop: HookStopDecision{Block: true}}
	var calls int
	hooks.stepFn = func() {
		calls++
		if calls >= 1 {
			hooks.stop = HookStopDecision{}
		}
	}
	h := newHookHarness(t, hooks)
	h.model.reset([]*schema.Message{answerTurn("先说一遍"), answerTurn("正式答案")})

	res, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "正式答案" {
		t.Errorf("answer = %q, want the answer produced after the block", res.Text)
	}
	var sawFallback bool
	for _, m := range h.model.gotMessages[1] {
		if m.Role == schema.System && strings.Contains(m.Content, "Stop hook") {
			sawFallback = true
		}
	}
	if !sawFallback {
		t.Error("a block with no reason must still say something the model can act on")
	}
	if len(hooks.stopCalls) > 4 {
		t.Errorf("Stop ran %d times, more than the step budget allows", len(hooks.stopCalls))
	}
}

func TestNoHooksCostsNothing(t *testing.T) {
	h := newHookHarness(t, nil)
	h.model.reset([]*schema.Message{
		hookToolCallTurn("c1", "probe", `{}`),
		answerTurn("done"),
	})
	res, err := h.runner.Run(context.Background(), Request{SessionID: "s"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("answer = %q", res.Text)
	}
	if got := h.tool.calledWith(); len(got) != 1 {
		t.Errorf("the tool ran %d times, want 1", len(got))
	}
}

func TestParallelCallsKeepTheirOwnHookContext(t *testing.T) {
	// Two parallel-safe calls, each with its own context line: the lines must not
	// be attributed to the wrong call, which is what a shared slice would do.
	hooks := &recordingHooks{}

	// Two parallel-safe calls need a registry of their own: the harness's probe
	// tool is declared serial, which would place the two calls in separate groups
	// and prove nothing about overlap.
	reg := tool.NewRegistry()
	if err := reg.Register(parallelProbe{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mdl := &fakeModel{}
	r, err := New(Config{Model: mdl, Tools: reg, MaxSteps: 3, MaxParallel: 4, Hooks: hooks, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mdl.reset([]*schema.Message{
		{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{
				{ID: "a", Type: "function", Function: schema.FunctionCall{Name: "par", Arguments: `{"which":"a"}`}},
				{ID: "b", Type: "function", Function: schema.FunctionCall{Name: "par", Arguments: `{"which":"b"}`}},
			},
		},
		answerTurn("done"),
	})

	if _, err := r.Run(context.Background(), Request{SessionID: "s"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(hooks.postCalls) != 2 {
		t.Fatalf("PostToolUse ran %d times, want 2", len(hooks.postCalls))
	}
	// Each call's own arguments must be what its hook saw.
	seen := map[string]bool{}
	for _, call := range hooks.postCalls {
		if strings.Contains(call.Args, `"a"`) {
			seen["a"] = true
		}
		if strings.Contains(call.Args, `"b"`) {
			seen["b"] = true
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("the hook calls did not carry their own arguments: %+v", hooks.postCalls)
	}
}

// parallelProbe is a parallel-safe tool that answers with its own arguments.
type parallelProbe struct{}

func (parallelProbe) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "par", Desc: "parallel probe"}, nil
}

func (parallelProbe) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	return "ok:" + args, nil
}

func (parallelProbe) Capability() tool.Capability   { return tool.CapRead }
func (parallelProbe) Concurrency() tool.Concurrency { return tool.ParallelSafe }
