package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

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
	if got := typesOf(*events); got[len(got)-1] != EventDone {
		t.Errorf("last event = %v, want done", got[len(got)-1])
	}
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
