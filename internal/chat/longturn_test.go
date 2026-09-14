package chat

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	gctx "github.com/huan/huan-agent/internal/context"
)

// These tests run the runner against the *real* context manager rather than a
// double, because the property they exist for is a property of the pair: a turn
// can only stay inside a model's window if the loop asks the manager to condense
// and then actually sends what came back. A fake on either side would prove the
// seam exists, not that a long turn survives.

// countingSummarizer stands in for the summarization model call.
type countingSummarizer struct {
	mu    sync.Mutex
	calls int
	seen  int
}

func (c *countingSummarizer) Summarize(_ context.Context, msgs []*schema.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.seen = len(msgs)
	return "SUMMARY of the earlier steps", nil
}

func TestRun_LongTurnStaysInsideItsWindow(t *testing.T) {
	// Forty tool calls of bulky output, then an answer: the shape of a real long
	// task, and the shape that overflows a context window if nothing condenses.
	const steps = 40
	turns := make([]*schema.Message, 0, steps+1)
	for i := 0; i < steps; i++ {
		turns = append(turns, toolCallTurn("c"))
	}
	turns = append(turns, &schema.Message{Role: schema.Assistant, Content: "all done"})

	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return strings.Repeat("tool output that is not small. ", 60), nil
	}}
	m := &fakeModel{turns: turns}
	sum := &countingSummarizer{}
	mgr, err := gctx.NewManager(gctx.Budget{MaxTokens: 2000, KeepRecent: 4, Summarizer: sum}, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	r, err := New(Config{
		Model:     m,
		Tools:     newRegistry(t, tl),
		MaxSteps:  60,
		Condenser: mgr,
		Logger:    zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{
			{Role: schema.System, Content: "SYSTEM PROMPT"},
			{Role: schema.User, Content: "refactor the thing"},
		},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The turn ran to its own conclusion, not to a budget.
	if res.Text != "all done" || res.StopReason != "" {
		t.Fatalf("res = %+v, want a normal answer", res)
	}
	if res.Steps != steps+1 {
		t.Errorf("Steps = %d, want %d (the whole task)", res.Steps, steps+1)
	}
	if sum.calls == 0 {
		t.Fatal("the window was never condensed: 40 steps of tool output would have been resent every step")
	}

	// The window the model saw must not grow with the number of steps taken.
	maxSeen := 0
	for _, window := range m.gotMessages {
		if len(window) > maxSeen {
			maxSeen = len(window)
		}
	}
	if maxSeen > 12 {
		t.Errorf("largest window = %d messages, want a bounded one (condenser ran %d times)", maxSeen, sum.calls)
	}

	// The rules and the goal survive every pass: condensing that drops the
	// system prompt or the request produces a turn that forgets what it is doing.
	last := m.gotMessages[len(m.gotMessages)-1]
	if last[0].Content != "SYSTEM PROMPT" {
		t.Errorf("the system prompt was dropped: %v", contentsOf(last))
	}
	if !windowHas(last, "refactor the thing") {
		t.Errorf("the pinned goal was dropped: %v", contentsOf(last))
	}
}

func TestRun_LongTurnStopsOnItsTokenBudgetNotTheContextLimit(t *testing.T) {
	// The same shape with a token budget in place: the turn must end with a
	// reason the caller can act on, rather than with a provider error about the
	// context length.
	m := &fakeModel{turns: []*schema.Message{
		usageTurn(400), usageTurn(400), usageTurn(400), usageTurn(400), usageTurn(400),
	}}
	tl := &fakeTool{name: "loop", desc: "l", run: func(context.Context, string) (string, error) {
		return strings.Repeat("output ", 50), nil
	}}
	mgr, err := gctx.NewManager(gctx.Budget{
		MaxTokens: 2000, KeepRecent: 3, Summarizer: &countingSummarizer{},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r, err := New(Config{
		Model:     m,
		Tools:     newRegistry(t, tl),
		MaxSteps:  60,
		MaxTokens: 1000,
		Condenser: mgr,
		Logger:    zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopTokens {
		t.Fatalf("StopReason = %q, want %q (text: %q)", res.StopReason, StopTokens, res.Text)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3: 3 × 400 tokens crosses the 1000 budget", res.Steps)
	}
}

// TestRun_TypedNilCondenserIsJustNoCompression guards the failure mode that a
// live run found: a nil *context.Manager stored in the Condenser interface is a
// non-nil interface holding a nil pointer. Treating it as "configured" turned
// every turn of an unconfigured deployment into a nil dereference.
func TestRun_TypedNilCondenserIsJustNoCompression(t *testing.T) {
	var mgr *gctx.Manager // nil pointer, non-nil interface once boxed
	m := &fakeModel{turns: []*schema.Message{{Role: schema.Assistant, Content: "fine"}}}
	r, err := New(Config{Model: m, MaxSteps: 3, Condenser: mgr, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.condenser == nil {
		t.Fatal("the interface is nil, so this test is no longer reproducing the case it exists for")
	}

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "go"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "fine" {
		t.Errorf("Text = %q, want the answer", res.Text)
	}
	// And a nil manager must behave like no compression rather than like an
	// always-condensing one: the window is what the caller sent plus the answer.
	if got := m.gotMessages[0]; len(got) != 1 {
		t.Errorf("window = %v, want the caller's message untouched", contentsOf(got))
	}
}

// windowHas reports whether any message in the window carries the text, on any
// role: a pinned goal is a user message, and the summary that replaced the
// middle is a system one.
func windowHas(msgs []*schema.Message, text string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}
