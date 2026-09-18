package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// fakeModel is a scripted streaming chat model: each call to Stream returns the
// next scripted message, and WithTools records the tool infos it was given.
type fakeModel struct {
	mu    sync.Mutex
	turns []*schema.Message
	calls int

	withToolsErr error
	gotTools     []*schema.ToolInfo
	gotMessages  [][]*schema.Message
}

// reset replaces the scripted turns and rewinds the call counter, so one test
// can run several turns against fresh scripts.
func (f *fakeModel) reset(turns []*schema.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = turns
	f.calls = 0
}

func (f *fakeModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	if f.withToolsErr != nil {
		return nil, f.withToolsErr
	}
	f.mu.Lock()
	f.gotTools = infos
	f.mu.Unlock()
	return f, nil
}

func (f *fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, errors.New("not used")
}

func (f *fakeModel) Stream(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.mu.Lock()
	f.gotMessages = append(f.gotMessages, msgs)
	idx := f.calls
	f.calls++
	f.mu.Unlock()

	if idx >= len(f.turns) {
		return nil, errors.New("fakeModel: no scripted turn left")
	}
	return streamOf(f.turns[idx]), nil
}

// streamOf turns one message into a single-chunk stream.
func streamOf(m *schema.Message) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{m})
}

// streamOfChunks turns several chunks into one stream (a streaming response).
func streamOfChunks(chunks ...*schema.Message) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray(chunks)
}

// chunkedModel streams a response as several deltas, to exercise delta
// emission and reassembly.
type chunkedModel struct {
	mu      sync.Mutex
	turns   [][]*schema.Message
	calls   int
	gotMsgs [][]*schema.Message
}

func (c *chunkedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return c, nil
}
func (c *chunkedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("not used")
}
func (c *chunkedModel) Stream(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	c.mu.Lock()
	c.gotMsgs = append(c.gotMsgs, msgs)
	idx := c.calls
	c.calls++
	c.mu.Unlock()
	if idx >= len(c.turns) {
		return nil, errors.New("chunkedModel: no scripted turn left")
	}
	return streamOfChunks(c.turns[idx]...), nil
}

// fakeTool is a test tool.
type fakeTool struct {
	name   string
	desc   string
	params string
	run    func(ctx context.Context, args string) (string, error)
}

func (t *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{Name: t.name, Desc: t.desc}
	if t.params != "" {
		var js jsonschema.Schema
		if err := json.Unmarshal([]byte(t.params), &js); err != nil {
			return nil, err
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&js)
	}
	return info, nil
}

func (t *fakeTool) InvokableRun(ctx context.Context, args string, _ ...einotool.Option) (string, error) {
	return t.run(ctx, args)
}

func newRegistry(t *testing.T, tools ...*fakeTool) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	for _, ft := range tools {
		if err := reg.Register(ft); err != nil {
			t.Fatalf("register tool %s: %v", ft.name, err)
		}
	}
	return reg
}

// collect records every emitted event.
func collect() (*[]Event, Emitter) {
	var mu sync.Mutex
	events := &[]Event{}
	return events, func(e Event) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

// typesOf lists the event types in order.
func typesOf(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// textOf concatenates every text_delta payload.
func textOf(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == EventTextDelta {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func TestRun_PlainAnswer(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, Content: "hello there"},
	}}
	r, err := New(Config{Model: m, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "hi"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hello there" {
		t.Errorf("Text = %q, want hello there", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1", res.Steps)
	}
	if got := textOf(*events); got != "hello there" {
		t.Errorf("streamed text = %q, want hello there", got)
	}
	got := typesOf(*events)
	if got[len(got)-1] != EventDone {
		t.Errorf("last event = %v, want done", got[len(got)-1])
	}
	if got[0] != EventStepStart {
		t.Errorf("first event = %v, want step_start", got[0])
	}
}

func TestRun_ReasoningIsStreamedSeparately(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, Content: "answer", ReasoningContent: "because reasons"},
	}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "why"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var reasoning strings.Builder
	for _, e := range events2Slice(events) {
		if e.Type == EventReasoningDelta {
			reasoning.WriteString(e.Text)
		}
	}
	if reasoning.String() != "because reasons" {
		t.Errorf("reasoning = %q, want %q", reasoning.String(), "because reasons")
	}
	if res.Reasoning != "because reasons" {
		t.Errorf("Result.Reasoning = %q", res.Reasoning)
	}
	// Reasoning must not leak into the answer text.
	if res.Text != "answer" {
		t.Errorf("Text = %q, want answer", res.Text)
	}
	if got := textOf(events2Slice(events)); strings.Contains(got, "because") {
		t.Errorf("reasoning leaked into text deltas: %q", got)
	}
}

// events2Slice dereferences the collected event slice.
func events2Slice(p *[]Event) []Event {
	if p == nil {
		return nil
	}
	return *p
}

func TestRun_ToolCallAndResult(t *testing.T) {
	ran := 0
	tl := &fakeTool{
		name: "calc", desc: "add numbers", params: `{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`,
		run: func(_ context.Context, args string) (string, error) {
			ran++
			if !strings.Contains(args, "1+1") {
				t.Errorf("tool got args %q, want the model's arguments", args)
			}
			return "2", nil
		},
	}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "call_1", Type: "function",
			Function: schema.FunctionCall{Name: "calc", Arguments: `{"expr":"1+1"}`},
		}}},
		{Role: schema.Assistant, Content: "the answer is 2"},
	}}
	r, err := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "what is 1+1"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if ran != 1 {
		t.Errorf("tool ran %d times, want 1", ran)
	}
	if res.Text != "the answer is 2" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2 (tool round + final)", res.Steps)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "calc" || res.Tools[0].Result != "2" {
		t.Errorf("Tools = %+v", res.Tools)
	}

	// The UI must see the call announced before its result.
	var callIdx, resultIdx = -1, -1
	for i, e := range *events {
		switch e.Type {
		case EventToolCall:
			callIdx = i
			if e.ToolName != "calc" || e.ToolCallID != "call_1" {
				t.Errorf("tool_call event = %+v", e)
			}
			if e.ToolArgs != `{"expr":"1+1"}` {
				t.Errorf("tool_call args = %q", e.ToolArgs)
			}
		case EventToolResult:
			resultIdx = i
			if e.ToolResult != "2" || e.ToolError != "" {
				t.Errorf("tool_result event = %+v", e)
			}
		}
	}
	if callIdx < 0 || resultIdx < 0 || callIdx > resultIdx {
		t.Errorf("callIdx=%d resultIdx=%d, want call before result", callIdx, resultIdx)
	}

	// The second model call must include the assistant tool_call and the tool
	// observation, or the model cannot use the result.
	if len(m.gotMessages) != 2 {
		t.Fatalf("model called %d times, want 2", len(m.gotMessages))
	}
	second := m.gotMessages[1]
	if second[len(second)-1].Role != schema.Tool {
		t.Errorf("last message role = %v, want tool", second[len(second)-1].Role)
	}
	if second[len(second)-1].ToolCallID != "call_1" {
		t.Errorf("tool message ToolCallID = %q, want call_1",
			second[len(second)-1].ToolCallID)
	}
}

func TestRun_ToolFailureIsReportedNotFatal(t *testing.T) {
	tl := &fakeTool{
		name: "flaky", desc: "fails",
		run: func(context.Context, string) (string, error) {
			return "", errors.New("boom")
		},
	}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "flaky", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "the tool failed, sorry"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit)
	if err != nil {
		t.Fatalf("a failing tool must not fail the run: %v", err)
	}
	if res.Text != "the tool failed, sorry" {
		t.Errorf("Text = %q, want the model's follow-up", res.Text)
	}

	var found bool
	for _, e := range *events {
		if e.Type == EventToolResult {
			found = true
			if e.ToolError != "boom" {
				t.Errorf("ToolError = %q, want boom", e.ToolError)
			}
		}
	}
	if !found {
		t.Error("no tool_result event was emitted for the failure")
	}
	// The model must be told about the failure.
	second := m.gotMessages[1]
	last := second[len(second)-1]
	if !strings.Contains(last.Content, "boom") {
		t.Errorf("tool observation = %q, want it to mention the error", last.Content)
	}
}

func TestRun_UnknownToolIsRefused(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "nonexistent", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t), Logger: zap.NewNop()})

	events, emit := collect()
	_, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, e := range *events {
		if e.Type == EventToolResult && !strings.Contains(e.ToolError, "unknown tool") {
			t.Errorf("ToolError = %q, want an unknown-tool refusal", e.ToolError)
		}
	}
}

func TestRun_DisallowedToolIsRefused(t *testing.T) {
	tl := &fakeTool{name: "danger", desc: "d", run: func(context.Context, string) (string, error) {
		t.Error("a disallowed tool must not run")
		return "", nil
	}}
	safe := &fakeTool{name: "safe", desc: "s", run: func(context.Context, string) (string, error) {
		return "ok", nil
	}}
	reg := newRegistry(t, tl, safe)
	if err := reg.SetAllowList([]string{"safe"}); err != nil {
		t.Fatalf("SetAllowList: %v", err)
	}

	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "danger", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "refused"},
	}}
	r, _ := New(Config{Model: m, Tools: reg, Logger: zap.NewNop()})

	events, emit := collect()
	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawRefusal bool
	for _, e := range *events {
		if e.Type == EventToolResult && strings.Contains(e.ToolError, "not permitted") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Error("a disallowed tool should be refused with a not-permitted error")
	}
}

func TestNew_RefusesBudgetsItCannotHonour(t *testing.T) {
	m := &fakeModel{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"step budget above the ceiling", Config{Model: m, MaxSteps: MaxStepsCeiling + 1}},
		{"negative token budget", Config{Model: m, MaxTokens: -1}},
		{"negative deadline", Config{Model: m, Deadline: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("New accepted a budget it cannot honour")
			}
		})
	}
	// The ceiling itself is allowed: refusing it would make the documented
	// maximum unreachable.
	if _, err := New(Config{Model: m, MaxSteps: MaxStepsCeiling}); err != nil {
		t.Errorf("New(MaxSteps=%d) = %v, want the ceiling to be accepted", MaxStepsCeiling, err)
	}
}

func TestRun_MaxStepsExhausted(t *testing.T) {
	// Every turn asks for another tool call, so the loop must stop at the cap.
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return "again", nil
	}}
	turns := make([]*schema.Message, 0, 5)
	for i := 0; i < 5; i++ {
		turns = append(turns, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "loop", Arguments: "{}"},
		}}})
	}
	m := &fakeModel{turns: turns}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), MaxSteps: 3, Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3 (the cap)", res.Steps)
	}
	if !strings.Contains(res.Text, "最大工具调用步数") {
		t.Errorf("Text = %q, want an explanation of the step cap", res.Text)
	}
	// A budget stop is reported, not inferred: the reason travels on the result
	// and on its own event, and the turn still ends with done rather than error.
	if res.StopReason != StopSteps {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopSteps)
	}
	if !res.BudgetExhausted() {
		t.Error("BudgetExhausted = false, want true")
	}
	got := typesOf(*events)
	if len(got) < 2 || got[len(got)-2] != EventBudgetStop || got[len(got)-1] != EventDone {
		t.Fatalf("event tail = %v, want ... budget_stop, done", got)
	}
	last := (*events)[len(*events)-2]
	if last.Reason != StopSteps || last.Step != 3 {
		t.Errorf("budget_stop = %+v, want reason=%s step=3", last, StopSteps)
	}
	// The note has to be actionable, not just apologetic.
	if !strings.Contains(res.Text, "继续") {
		t.Errorf("Text = %q, want it to say what to do next", res.Text)
	}
}

func TestRun_TokenBudgetStopsBeforeTheNextCall(t *testing.T) {
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return "again", nil
	}}
	// Two calls of 600 tokens each: the second crosses the 1000 budget, so the
	// third call must never happen.
	m := &fakeModel{turns: []*schema.Message{usageTurn(600), usageTurn(600), usageTurn(600)}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), MaxSteps: 10, MaxTokens: 1000, Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopTokens {
		t.Fatalf("StopReason = %q, want %q (text: %q)", res.StopReason, StopTokens, res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2: the budget stops the call it would have paid for", res.Steps)
	}
	if m.calls != 2 {
		t.Errorf("model called %d times, want 2", m.calls)
	}
	if !strings.Contains(res.Text, "token 预算") {
		t.Errorf("Text = %q, want it to name the token budget", res.Text)
	}
	if got := typesOf(*events); got[len(got)-2] != EventBudgetStop {
		t.Errorf("event tail = %v, want a budget_stop before done", got)
	}
}

func TestRun_DeadlineStopsBeforeTheFirstCall(t *testing.T) {
	// A deadline already in the past: the check happens before the first model
	// call, so an expired budget costs nothing.
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "never"}}}
	r, _ := New(Config{Model: m, MaxSteps: 5, Deadline: time.Nanosecond, Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopDeadline {
		t.Fatalf("StopReason = %q, want %q", res.StopReason, StopDeadline)
	}
	if m.calls != 0 {
		t.Errorf("model called %d times, want 0 (the budget was already spent)", m.calls)
	}
	if res.Steps != 0 {
		t.Errorf("Steps = %d, want 0", res.Steps)
	}
	// No assistant text exists to fall back on, so the note has to stand alone.
	if !strings.Contains(res.Text, "未能得出最终回答") {
		t.Errorf("Text = %q, want the no-answer form", res.Text)
	}
}

func TestRun_TokenBudgetNeedsReportedUsage(t *testing.T) {
	// A provider that reports no usage cannot be bounded by tokens. The runner
	// must not pretend otherwise, so the turn runs to the step cap instead.
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return "again", nil
	}}
	m := &fakeModel{turns: []*schema.Message{toolCallTurn("c1"), toolCallTurn("c2")}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), MaxSteps: 2, MaxTokens: 1, Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopSteps {
		t.Errorf("StopReason = %q, want %q when no usage is reported", res.StopReason, StopSteps)
	}
}

// fakeCondenser records what it was asked to compress and folds the middle away,
// so a test can see the window the model actually received.
type fakeCondenser struct {
	mu    sync.Mutex
	calls int
	heads []int
	pins  []bool
	err   error
}

func (f *fakeCondenser) CompressKeeping(_ context.Context, msgs []*schema.Message, head int, pinLastUser bool) ([]*schema.Message, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.heads = append(f.heads, head)
	f.pins = append(f.pins, pinLastUser)
	if f.err != nil {
		return nil, "", f.err
	}
	if len(msgs) <= head+3 {
		return msgs, "", nil
	}
	out := make([]*schema.Message, 0, head+3)
	out = append(out, msgs[:head]...)
	out = append(out, &schema.Message{Role: schema.System, Content: "SUMMARY"})
	out = append(out, msgs[len(msgs)-2:]...)
	return out, "SUMMARY", nil
}

func TestRun_CondenserBoundsTheWindowEveryStep(t *testing.T) {
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return strings.Repeat("tool output ", 20), nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		toolCallTurn("c1"), toolCallTurn("c2"), toolCallTurn("c3"), toolCallTurn("c4"),
		{Role: schema.Assistant, Content: "done"},
	}}
	cd := &fakeCondenser{}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), MaxSteps: 20, Condenser: cd, Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{
			{Role: schema.System, Content: "SYSTEM"},
			{Role: schema.User, Content: "GOAL"},
		},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" || res.StopReason != "" {
		t.Fatalf("res = %+v, want a normal answer", res)
	}
	if cd.calls < 2 {
		t.Fatalf("condenser called %d times, want once per step", cd.calls)
	}
	for i, head := range cd.heads {
		if head != 1 {
			t.Errorf("call %d pinned %d head messages, want 1 (the system prompt)", i, head)
		}
	}
	for i, pin := range cd.pins {
		if !pin {
			t.Errorf("call %d did not pin the user message", i)
		}
	}
	// The window the model saw must stay bounded even though the turn kept
	// adding tool output: that is the whole point of condensing every step.
	last := m.gotMessages[len(m.gotMessages)-1]
	if len(last) > 6 {
		t.Errorf("model saw %d messages on the last step, want a bounded window: %v",
			len(last), contentsOf(last))
	}
	if last[0].Content != "SYSTEM" {
		t.Errorf("the system prompt was dropped from the window: %v", contentsOf(last))
	}
}

func TestRun_CondenserFailureDoesNotFailTheTurn(t *testing.T) {
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return "again", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		toolCallTurn("c1"), {Role: schema.Assistant, Content: "answered anyway"},
	}}
	cd := &fakeCondenser{err: errors.New("summarizer exploded")}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), MaxSteps: 5, Condenser: cd, Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "answered anyway" {
		t.Errorf("Text = %q, want the answer: a summary failure must not abort a long task", res.Text)
	}
}

// usageTurn is an assistant message that asks for another tool call and reports
// the given token usage, which is what a token budget measures.
func usageTurn(total int) *schema.Message {
	m := toolCallTurn("c")
	m.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: total, PromptTokens: total}}
	return m
}

// toolCallTurn is an assistant message whose only content is a tool call, which
// is what keeps the loop going.
func toolCallTurn(id string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Type: "function",
		Function: schema.FunctionCall{Name: "loop", Arguments: "{}"},
	}}}
}

func contentsOf(msgs []*schema.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Role) + ":" + m.Content
	}
	return out
}

func TestRun_ContextCancelled(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "unused"}}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events, emit := collect()
	if _, err := r.Run(ctx, Request{Messages: []*schema.Message{{Role: schema.User, Content: "x"}}}, emit); err == nil {
		t.Fatal("want an error for a cancelled context")
	}
	types := typesOf(*events)
	if types[len(types)-1] != EventError {
		t.Errorf("last event = %v, want error", types[len(types)-1])
	}
}

func TestRun_ModelErrorIsReported(t *testing.T) {
	m := &fakeModel{} // no scripted turns -> Stream errors
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	events, emit := collect()
	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, emit); err == nil {
		t.Fatal("want an error when the model fails")
	}
	types := typesOf(*events)
	if types[len(types)-1] != EventError {
		t.Errorf("last event = %v, want error", types[len(types)-1])
	}
}

func TestRun_NoToolsSkipsToolBinding(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "hi"}}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(m.gotTools) != 0 {
		t.Errorf("no tools should be bound, got %d infos", len(m.gotTools))
	}
}

func TestRun_ToolsArePassedToTheModel(t *testing.T) {
	tl := &fakeTool{
		name: "echo", desc: "echoes",
		params: `{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`,
		run:    func(context.Context, string) (string, error) { return "ok", nil },
	}
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "hi"}}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(m.gotTools) != 1 || m.gotTools[0].Name != "echo" {
		t.Fatalf("bound tools = %+v, want the echo tool", m.gotTools)
	}
	// The parameter schema must survive, or the model cannot pass arguments.
	if m.gotTools[0].ParamsOneOf == nil {
		t.Error("tool parameter schema was dropped; the model could not call it with args")
	}
}

func TestRun_ModelRejectingToolsStillAnswers(t *testing.T) {
	tl := &fakeTool{name: "t", desc: "d", run: func(context.Context, string) (string, error) {
		return "x", nil
	}}
	m := &fakeModel{
		turns:        []*schema.Message{{Role: schema.Assistant, Content: "answered anyway"}},
		withToolsErr: errors.New("model does not support tools"),
	}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil)
	if err != nil {
		t.Fatalf("a model without tool support should still answer: %v", err)
	}
	if res.Text != "answered anyway" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestRun_StreamsDeltasAndReassembles(t *testing.T) {
	m := &chunkedModel{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "Hel"},
		{Role: schema.Assistant, Content: "lo ", ReasoningContent: "think"},
		{Role: schema.Assistant, Content: "world"},
	}}}
	r, err := New(Config{Model: m, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := textOf(*events); got != "Hello world" {
		t.Errorf("streamed text = %q, want %q", got, "Hello world")
	}
	if res.Text != "Hello world" {
		t.Errorf("reassembled Text = %q, want %q", res.Text, "Hello world")
	}
	if res.Reasoning != "think" {
		t.Errorf("Reasoning = %q, want think", res.Reasoning)
	}
}

func TestRun_UsageIsCaptured(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{
		Role:    schema.Assistant,
		Content: "hi",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		}},
	}}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.PromptTokens != 10 || res.Usage.CompletionTokens != 5 || res.Usage.TotalTokens != 15 {
		t.Errorf("Usage = %+v, want 10/5/15", res.Usage)
	}
}

func TestNew_RequiresModel(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want an error when the model is missing")
	}
}

func TestRun_DoesNotMutateCallerHistory(t *testing.T) {
	tl := &fakeTool{name: "t", desc: "d", run: func(context.Context, string) (string, error) {
		return "r", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "t", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	history := []*schema.Message{{Role: schema.User, Content: "x"}}
	if _, err := r.Run(context.Background(), Request{Messages: history}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("caller history grew to %d messages; the runner must not mutate it", len(history))
	}
}

func TestTruncate_IsRuneSafe(t *testing.T) {
	// A cut in the middle of a multi-byte rune must not produce invalid UTF-8.
	s := strings.Repeat("中", 100)
	got := truncate(s, 10)
	for i := 0; i < len(got); i++ {
		if got[i]&0xC0 == 0x80 {
			continue // continuation byte, fine
		}
	}
	if !isValidUTF8(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > 13 { // 10 bytes' worth + ellipsis
		t.Errorf("truncate(%d) = %q, too long", 10, got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestToolResultContent(t *testing.T) {
	if got := toolResultContent(ToolRun{Result: "ok"}); got != "ok" {
		t.Errorf("success = %q, want ok", got)
	}
	if got := toolResultContent(ToolRun{Err: "bad"}); got != "error: bad" {
		t.Errorf("failure = %q, want it prefixed with error:", got)
	}
}

func TestTruncate_ShortStringUnchanged(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate = %q, want short", got)
	}
}

func TestStreamOnce_TracerSeesGeneration(t *testing.T) {
	tr := &recordingTracer{}
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "x"}}}
	r, _ := New(Config{Model: m, Tracer: tr, Logger: zap.NewNop()})

	if _, err := r.Run(context.Background(), Request{
		Messages:  []*schema.Message{{Role: schema.User, Content: "q"}},
		SessionID: "sess-1",
		UserID:    "user-1",
	}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(tr.traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(tr.traces))
	}
	if tr.traces[0].SessionID != "sess-1" || tr.traces[0].UserID != "user-1" {
		t.Errorf("trace info = %+v, want the session and user", tr.traces[0])
	}
	if len(tr.generations) != 1 {
		t.Errorf("generations = %d, want 1", len(tr.generations))
	}
	if !tr.traceEnded {
		t.Error("the trace was not ended")
	}
}

func TestNopTracer_IsInert(t *testing.T) {
	var tr NopTracer
	ctx := context.Background()
	if id := tr.StartTrace(ctx, TraceInfo{}); id != "" {
		t.Errorf("StartTrace = %q, want empty", id)
	}
	if id := tr.StartSpan(ctx, SpanInfo{}); id != "" {
		t.Errorf("StartSpan = %q, want empty", id)
	}
	if id := tr.StartGeneration(ctx, GenInfo{}); id != "" {
		t.Errorf("StartGeneration = %q, want empty", id)
	}
	// None of these may panic.
	tr.EndTrace(ctx, "", nil)
	tr.EndSpan(ctx, "", nil, "")
	tr.EndGeneration(ctx, "", nil, Usage{}, "")
}

// recordingTracer captures tracing calls.
type recordingTracer struct {
	traces      []TraceInfo
	spans       []SpanInfo
	generations []GenInfo
	traceEnded  bool
	lastSpanErr string
}

func (t *recordingTracer) StartTrace(_ context.Context, i TraceInfo) string {
	t.traces = append(t.traces, i)
	return "trace-1"
}
func (t *recordingTracer) EndTrace(context.Context, string, any) { t.traceEnded = true }
func (t *recordingTracer) StartSpan(_ context.Context, i SpanInfo) string {
	t.spans = append(t.spans, i)
	return "span-1"
}
func (t *recordingTracer) EndSpan(_ context.Context, _ string, _ any, errMsg string) {
	t.lastSpanErr = errMsg
}
func (t *recordingTracer) StartGeneration(_ context.Context, i GenInfo) string {
	t.generations = append(t.generations, i)
	return "gen-1"
}
func (t *recordingTracer) EndGeneration(context.Context, string, any, Usage, string) {}

func TestRun_UsageEventIsEmitted(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{
		Role:    schema.Assistant,
		Content: "hi",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10,
		}},
	}}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawUsage bool
	for _, e := range *events {
		if e.Type == EventUsage {
			sawUsage = true
			if e.Usage == nil || e.Usage.TotalTokens != 10 {
				t.Errorf("usage event = %+v, want 10 total", e.Usage)
			}
		}
	}
	if !sawUsage {
		t.Error("no usage event was emitted; the UI cannot show token cost per turn")
	}
	if res.Usage.TotalTokens != 10 {
		t.Errorf("Result.Usage.TotalTokens = %d, want 10", res.Usage.TotalTokens)
	}
}

func TestRun_UsageAccumulatesAcrossSteps(t *testing.T) {
	tl := &fakeTool{name: "t", desc: "d", run: func(context.Context, string) (string, error) {
		return "r", nil
	}}
	usage := func(p, c int) *schema.ResponseMeta {
		return &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: p, CompletionTokens: c, TotalTokens: p + c,
		}}
	}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ResponseMeta: usage(10, 5), ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "t", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done", ResponseMeta: usage(20, 8)},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both model calls must be counted; reporting only the last would
	// under-bill every tool-using turn.
	if res.Usage.PromptTokens != 30 || res.Usage.CompletionTokens != 13 || res.Usage.TotalTokens != 43 {
		t.Errorf("Usage = %+v, want 30/13/43 summed across steps", res.Usage)
	}
}

func TestRun_ReasoningAccumulatesAcrossSteps(t *testing.T) {
	tl := &fakeTool{name: "t", desc: "d", run: func(context.Context, string) (string, error) {
		return "r", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "first ", ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "t", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done", ReasoningContent: "second"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reasoning != "first second" {
		t.Errorf("Reasoning = %q, want %q", res.Reasoning, "first second")
	}
}

// TestRun_PlanPairsEachThoughtWithItsTools is the whole point of the plan: a
// reader must be able to say which reasoning asked for which tool call, which a
// flat list of tool runs and one concatenated block of reasoning cannot express.
func TestRun_PlanPairsEachThoughtWithItsTools(t *testing.T) {
	ran := []string{}
	tl := &fakeTool{name: "read", desc: "d", run: func(ctx context.Context, args string) (string, error) {
		ran = append(ran, args)
		return "content of " + args, nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant,
			ReasoningContent: "先看 a", Content: "我先读一下 a。",
			ToolCalls: []schema.ToolCall{{ID: "c1", Type: "function",
				Function: schema.FunctionCall{Name: "read", Arguments: "a.txt"}}}},
		{Role: schema.Assistant,
			ReasoningContent: "再看 b",
			ToolCalls: []schema.ToolCall{{ID: "c2", Type: "function",
				Function: schema.FunctionCall{Name: "read", Arguments: "b.txt"}}}},
		{Role: schema.Assistant, ReasoningContent: "可以答了", Content: "答案是 42"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	_, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Plan) != 3 {
		t.Fatalf("Plan has %d steps, want 3: %+v", len(res.Plan), res.Plan)
	}
	want := []struct {
		index     int
		reasoning string
		text      string
		tool      string
	}{
		{1, "先看 a", "我先读一下 a。", "a.txt"},
		{2, "再看 b", "", "b.txt"},
		{3, "可以答了", "答案是 42", ""},
	}
	for i, w := range want {
		got := res.Plan[i]
		if got.Index != w.index {
			t.Errorf("step %d Index = %d, want %d", i, got.Index, w.index)
		}
		if got.Reasoning != w.reasoning {
			t.Errorf("step %d Reasoning = %q, want %q", w.index, got.Reasoning, w.reasoning)
		}
		if got.Text != w.text {
			t.Errorf("step %d Text = %q, want %q", w.index, got.Text, w.text)
		}
		if w.tool == "" {
			if len(got.Tools) != 0 {
				t.Errorf("step %d has %d tools, want none (it answered)", w.index, len(got.Tools))
			}
			continue
		}
		if len(got.Tools) != 1 || got.Tools[0].Args != w.tool {
			t.Fatalf("step %d tools = %+v, want the call with args %q", w.index, got.Tools, w.tool)
		}
		if got.Tools[0].Step != w.index {
			t.Errorf("tool in step %d carries Step = %d", w.index, got.Tools[0].Step)
		}
		if !strings.Contains(got.Tools[0].Result, w.tool) {
			t.Errorf("step %d tool result = %q, want the tool's output", w.index, got.Tools[0].Result)
		}
	}
	// The flat list is still filled, and now says which step each run came from.
	if len(res.Tools) != 2 || res.Tools[0].Step != 1 || res.Tools[1].Step != 2 {
		t.Errorf("Tools = %+v, want two runs tagged 1 and 2", res.Tools)
	}
	if len(ran) != 2 {
		t.Errorf("the tool ran %d times, want 2", len(ran))
	}
}

// TestRun_StepEndAnnouncesTheActionBoundary pins the event a client needs in
// order to keep a preamble out of the answer: it has to arrive after that step's
// text and before the tool call, and it must not mark the answering step.
func TestRun_StepEndAnnouncesTheActionBoundary(t *testing.T) {
	tl := &fakeTool{name: "act", desc: "d", run: func(context.Context, string) (string, error) {
		return "ok", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, Content: "我先动手。", ToolCalls: []schema.ToolCall{{
			ID: "c1", Function: schema.FunctionCall{Name: "act", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "做完了"},
	}}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	events, emit := collect()
	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var ends []Event
	var callIdx = -1
	for i, e := range *events {
		switch e.Type {
		case EventStepEnd:
			ends = append(ends, e)
		case EventToolCall:
			callIdx = i
		}
	}
	if len(ends) != 1 {
		t.Fatalf("step_end emitted %d times, want once (the answering step must not emit it)", len(ends))
	}
	if ends[0].Step != 1 || ends[0].Text != "我先动手。" {
		t.Errorf("step_end = %+v, want step 1 carrying its text", ends[0])
	}
	if stepEndIdx := indexOfType(*events, EventStepEnd); stepEndIdx > callIdx {
		t.Errorf("step_end at %d came after the tool call at %d", stepEndIdx, callIdx)
	}
}

// indexOfType returns the index of the first event of that type, or -1.
func indexOfType(events []Event, t EventType) int {
	for i, e := range events {
		if e.Type == t {
			return i
		}
	}
	return -1
}

// TestRun_DirectAnswerHasOneStepAndNoStepEnd: a model that just answers has no
// process to fold up, and nothing to announce as an action boundary.
func TestRun_DirectAnswerHasOneStepAndNoStepEnd(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "hi"}}}
	r, _ := New(Config{Model: m, Logger: zap.NewNop()})

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if indexOfType(*events, EventStepEnd) >= 0 {
		t.Error("a direct answer emitted step_end; there is no action to announce")
	}
	if len(res.Plan) != 1 || res.Plan[0].Text != "hi" || len(res.Plan[0].Tools) != 0 {
		t.Errorf("Plan = %+v, want one step holding the answer", res.Plan)
	}
}

// TestStepsHaveTools: the fold-up affordance exists only when something ran.
func TestStepsHaveTools(t *testing.T) {
	if StepsHaveTools(nil) {
		t.Error("no steps have tools")
	}
	if StepsHaveTools([]Step{{Index: 1, Reasoning: "thought"}}) {
		t.Error("a step that only thought has no tools")
	}
	if !StepsHaveTools([]Step{{Index: 1}, {Index: 2, Tools: []ToolRun{{ID: "c", Name: "t"}}}}) {
		t.Error("a step with a tool call must be reported")
	}
}

// TestStep_JSONShapeIsSnakeCase: the plan is stored with the answer and served
// to the UI, so its field names are part of the API.
func TestStep_JSONShapeIsSnakeCase(t *testing.T) {
	b, err := json.Marshal([]Step{{
		Index: 2, Reasoning: "why", Text: "doing it",
		Tools: []ToolRun{{ID: "c1", Name: "t", Args: "{}", Result: "ok", Step: 2}},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"index":2`, `"reasoning":"why"`, `"text":"doing it"`,
		`"tools":[`, `"step":2`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON %s is missing %s", got, want)
		}
	}
	// A step with nothing to say is still a step: index is never omitted, or a
	// client could not order what it received.
	b2, err := json.Marshal([]Step{{Index: 1}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b2) != `[{"index":1}]` {
		t.Errorf("empty step = %s, want just its index", b2)
	}
}

func TestRun_TracerRecordsToolSpans(t *testing.T) {
	tl := &fakeTool{name: "spanned", desc: "d", run: func(context.Context, string) (string, error) {
		return "out", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "spanned", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	tr := &recordingTracer{}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Tracer: tr, Logger: zap.NewNop()})

	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tr.spans) != 1 {
		t.Fatalf("spans = %d, want 1 per tool call", len(tr.spans))
	}
	if tr.spans[0].Name != "tool.spanned" {
		t.Errorf("span name = %q, want tool.spanned", tr.spans[0].Name)
	}
	if tr.spans[0].TraceID != "trace-1" {
		t.Errorf("span trace = %q, want the run's trace", tr.spans[0].TraceID)
	}
}

func TestRun_TracerMarksFailedSpan(t *testing.T) {
	tl := &fakeTool{name: "bad", desc: "d", run: func(context.Context, string) (string, error) {
		return "", errors.New("kaput")
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "bad", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	tr := &recordingTracer{}
	r, _ := New(Config{Model: m, Tools: newRegistry(t, tl), Tracer: tr, Logger: zap.NewNop()})

	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.lastSpanErr != "kaput" {
		t.Errorf("span error = %q, want kaput so the trace shows the failure", tr.lastSpanErr)
	}
}

func TestToolRun_JSONShapeIsSnakeCase(t *testing.T) {
	// The tool-run blob is stored with a message and served to the UI, so its
	// field names are part of the API and must match the rest of it.
	b, err := json.Marshal([]ToolRun{{
		ID: "c1", Name: "time", Args: "{}", Result: "ok", DurationMs: 3,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"id":"c1"`, `"name":"time"`, `"args":"{}"`, `"result":"ok"`, `"duration_ms":3`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON %s is missing %s", got, want)
		}
	}
	// Empty fields are omitted rather than sent as zero values.
	b2, err := json.Marshal([]ToolRun{{ID: "c2", Name: "t", Result: "r"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b2), `"err"`) || strings.Contains(string(b2), `"duration_ms"`) {
		t.Errorf("empty fields should be omitted, got %s", b2)
	}
}

// TestToolInfos_AlwaysHaveAnObjectSchema guards the regression that broke real
// providers: a parameter schema whose type was null is rejected outright with
// "Invalid schema for function ... got 'type: null'".
func TestToolInfos_AlwaysHaveAnObjectSchema(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{"declared schema", `{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`},
		{"empty object", `{}`},
		{"empty string", ``},
		{"explicit null", `null`},
		{"object without a type", `{"properties":{"a":{"type":"string"}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl := &fakeTool{name: "t", desc: "d", params: tc.params,
				run: func(context.Context, string) (string, error) { return "", nil }}
			r, err := New(Config{Model: &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "x"}}},
				Tools: newRegistry(t, tl), Logger: zap.NewNop()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			infos, err := r.toolInfos(context.Background(), r.tools)
			if err != nil {
				t.Fatalf("toolInfos: %v", err)
			}
			if len(infos) != 1 {
				t.Fatalf("infos = %d, want 1 (a tool must never be dropped for an odd schema)", len(infos))
			}
			if infos[0].ParamsOneOf == nil {
				t.Fatal("ParamsOneOf is nil; the provider would receive no schema at all")
			}
			js, err := infos[0].ParamsOneOf.ToJSONSchema()
			if err != nil {
				t.Fatalf("ToJSONSchema: %v", err)
			}
			if js == nil {
				t.Fatal("ToJSONSchema returned nil")
			}
			if js.Type != "object" {
				t.Errorf("schema type = %q, want object (providers reject null)", js.Type)
			}
		})
	}
}

// TestToolInfos_PreservesDeclaredProperties verifies the conversion does not
// flatten a real schema into an empty object.
func TestToolInfos_PreservesDeclaredProperties(t *testing.T) {
	tl := &fakeTool{
		name: "calc", desc: "calculates",
		params: `{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"]}`,
		run:    func(context.Context, string) (string, error) { return "1", nil },
	}
	r, _ := New(Config{Model: &fakeModel{}, Tools: newRegistry(t, tl), Logger: zap.NewNop()})

	infos, err := r.toolInfos(context.Background(), r.tools)
	if err != nil {
		t.Fatalf("toolInfos: %v", err)
	}
	js, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	b, err := json.Marshal(js)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), "expression") {
		t.Errorf("declared property was lost: %s", b)
	}
	if !strings.Contains(string(b), `"required"`) {
		t.Errorf("required list was lost: %s", b)
	}
}

func TestRun_ToolDurationIsMeasured(t *testing.T) {
	// Regression: the duration used to be assigned in a deferred closure that
	// ran after the event was emitted, so every tool reported 0ms in the UI and
	// in the audit log.
	slow := &fakeTool{name: "slow", desc: "d", run: func(context.Context, string) (string, error) {
		time.Sleep(25 * time.Millisecond)
		return "done", nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "slow", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "ok"},
	}}
	r, err := New(Config{Model: m, Tools: newRegistry(t, slow), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "x"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var fromEvent int64 = -1
	for _, e := range *events {
		if e.Type == EventToolResult {
			fromEvent = e.DurationMs
		}
	}
	if fromEvent < 20 {
		t.Errorf("tool_result duration = %dms, want >= 20ms (the tool slept 25ms)", fromEvent)
	}
	if len(res.Tools) != 1 || res.Tools[0].DurationMs < 20 {
		t.Errorf("Result tool duration = %dms, want >= 20ms", res.Tools[0].DurationMs)
	}
}

// TestRun_ScopeIsVisibleToTools: a tool must be able to see which scope its turn
// belongs to, because that is what a per-scope capability (a workspace) is
// resolved from, and it has to be the turn's scope rather than the runner's.
func TestRun_ScopeIsVisibleToTools(t *testing.T) {
	var seen []string
	tl := &fakeTool{name: "where", desc: "d", run: func(ctx context.Context, _ string) (string, error) {
		return ScopeFrom(ctx), nil
	}}
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Function: schema.FunctionCall{Name: "where", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	r, err := New(Config{Model: m, Tools: newRegistry(t, tl), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Two turns with different scopes: each must see its own, and the second
	// must not inherit the first's. Each turn gets a fresh scripted model,
	// because a turn consumes as many scripted replies as it takes steps.
	for _, scope := range []string{"web:s1", "feishu:ou_1"} {
		m.reset([]*schema.Message{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Function: schema.FunctionCall{Name: "where", Arguments: "{}"},
			}}},
			{Role: schema.Assistant, Content: "done"},
		})
		res, err := r.Run(context.Background(), Request{
			SessionID: "sess", Scope: scope,
			Messages: []*schema.Message{{Role: schema.User, Content: "where am i"}},
		}, func(Event) {})
		if err != nil {
			t.Fatalf("Run(%s): %v", scope, err)
		}
		if len(res.Tools) != 1 {
			t.Fatalf("expected 1 tool run, got %d", len(res.Tools))
		}
		seen = append(seen, res.Tools[0].Result)
	}
	if seen[0] != "web:s1" || seen[1] != "feishu:ou_1" {
		t.Errorf("tools saw %v, want each turn's own scope", seen)
	}
}

// TestRun_UnscopedTurnHasNoScope: a deployment without scopes must not have one
// invented for it.
func TestRun_UnscopedTurnHasNoScope(t *testing.T) {
	if got := ScopeFrom(WithScope(context.Background(), "")); got != "" {
		t.Errorf("WithScope(\"\") produced %q, want no scope", got)
	}
	if got := ScopeFrom(context.Background()); got != "" {
		t.Errorf("ScopeFrom on a bare context = %q, want empty", got)
	}
	// A nil context must not panic: ScopeFrom is called from tool paths.
	if got := ScopeFrom(nil); got != "" {
		t.Errorf("ScopeFrom(nil) = %q, want empty", got)
	}
}

// TestRun_ToolsForOverridesRegistry: the per-turn registry is what a
// workspace-bound turn runs against, so it has to win over the static one — and
// it has to be resolved once per turn, not once per step.
func TestRun_ToolsForOverridesRegistry(t *testing.T) {
	static := &fakeTool{name: "static", desc: "d", run: func(context.Context, string) (string, error) {
		return "static", nil
	}}
	perTurn := &fakeTool{name: "perturn", desc: "d", run: func(context.Context, string) (string, error) {
		return "perturn", nil
	}}

	calls := 0
	m := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Function: schema.FunctionCall{Name: "perturn", Arguments: "{}"},
		}}},
		{Role: schema.Assistant, Content: "done"},
	}}
	r, err := New(Config{
		Model: m,
		Tools: newRegistry(t, static),
		ToolsFor: func(ctx context.Context) (*tool.Registry, error) {
			calls++
			if ScopeFrom(ctx) != "web:s9" {
				t.Errorf("ToolsFor saw scope %q, want web:s9", ScopeFrom(ctx))
			}
			return newRegistry(t, perTurn), nil
		},
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Run(context.Background(), Request{
		Scope:    "web:s9",
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("ToolsFor was called %d times, want exactly 1 per turn", calls)
	}
	if len(res.Tools) != 1 || res.Tools[0].Result != "perturn" {
		t.Fatalf("the turn did not run against the per-turn registry: %+v", res.Tools)
	}
	// The static registry's own tool must not have been reachable.
	if res.Tools[0].Name != "perturn" {
		t.Errorf("tool %q ran, want perturn", res.Tools[0].Name)
	}
}

// TestRun_ToolsForFailureIsARunFailure: a caller that cannot decide which tools a
// turn may use must not have the turn run with the wrong ones.
func TestRun_ToolsForFailureIsARunFailure(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "x"}}}
	r, err := New(Config{
		Model:    m,
		Tools:    newRegistry(t, &fakeTool{name: "static", desc: "d"}),
		ToolsFor: func(context.Context) (*tool.Registry, error) { return nil, errors.New("no workspace") },
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err == nil {
		t.Fatal("Run succeeded although the tool set could not be resolved")
	}
	if !strings.Contains(err.Error(), "no workspace") {
		t.Errorf("error %v does not explain the cause", err)
	}
}

// TestRun_ToolsForNilMeansNoTools: falling back to the static registry would run
// the turn against tools bound to a different workspace, which is worse than
// running without any.
func TestRun_ToolsForNilMeansNoTools(t *testing.T) {
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "plain"}}}
	r, err := New(Config{
		Model:    m,
		Tools:    newRegistry(t, &fakeTool{name: "static", desc: "d"}),
		ToolsFor: func(context.Context) (*tool.Registry, error) { return nil, nil },
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "hi"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "plain" {
		t.Errorf("text = %q, want the plain answer", res.Text)
	}
}
