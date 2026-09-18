package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// A tool that records when it started and finished, so the tests can assert the
// schedule rather than only its result: overlap is invisible in a return value.

type scheduleProbe struct {
	name string
	// mode is what the tool declares about overlapping.
	mode tool.Concurrency
	// hold is how long the call takes. Two calls that overlap have windows that
	// intersect; two that do not have windows that cannot.
	hold time.Duration

	mu      sync.Mutex
	events  *[]string
	started []time.Time
	ended   []time.Time
	// panicOn makes the call panic, which a tool must not be able to do to the
	// turn.
	panicOn bool
}

func (p *scheduleProbe) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: p.name, Desc: "probe"}, nil
}

// probeTool adapts a scheduleProbe to the registry's Tool interface, which is
// what the runner actually calls.
type probeTool struct {
	probe *scheduleProbe
}

func (p probeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: p.probe.name, Desc: "probe"}, nil
}

func (p probeTool) InvokableRun(ctx context.Context, args string, _ ...einotool.Option) (string, error) {
	pr := p.probe
	start := time.Now()
	pr.mu.Lock()
	pr.started = append(pr.started, start)
	if pr.events != nil {
		*pr.events = append(*pr.events, "start "+pr.name)
	}
	pr.mu.Unlock()

	if pr.hold > 0 {
		select {
		case <-time.After(pr.hold):
		case <-ctx.Done():
		}
	}

	end := time.Now()
	pr.mu.Lock()
	pr.ended = append(pr.ended, end)
	if pr.events != nil {
		*pr.events = append(*pr.events, "end "+pr.name)
	}
	pr.mu.Unlock()

	if pr.panicOn {
		panic("probe panicked on purpose")
	}
	return "ok:" + pr.name + ":" + args, nil
}

// windows returns each call's [start, end) interval, so overlap is a comparison
// rather than a timing assertion.
func (p *scheduleProbe) windows() [][2]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][2]time.Time, 0, len(p.started))
	for i := range p.started {
		out = append(out, [2]time.Time{p.started[i], p.ended[i]})
	}
	return out
}

func overlaps(a, b [2]time.Time) bool {
	return a[0].Before(b[1]) && b[0].Before(a[1])
}

// toolCall builds one entry of a model reply's tool calls.
func toolCall(name, args string) schema.ToolCall {
	return schema.ToolCall{
		ID:       "call-" + name,
		Type:     "function",
		Function: schema.FunctionCall{Name: name, Arguments: args},
	}
}

// runWithProbes runs one step whose reply asks for the given probes, in order.
func runWithProbes(t *testing.T, maxParallel int, probes ...*scheduleProbe) (*Result, []Event) {
	t.Helper()

	reg := tool.NewRegistry()
	calls := make([]schema.ToolCall, 0, len(probes))
	for _, p := range probes {
		wrapped := tool.WithConcurrency(probeTool{probe: p}, p.mode)
		if err := reg.Register(wrapped); err != nil {
			t.Fatalf("register %s: %v", p.name, err)
		}
		calls = append(calls, toolCall(p.name, `{"n":1}`))
	}

	mdl := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: calls},
		{Role: schema.Assistant, Content: "done"},
	}}
	runner, err := New(Config{
		Model:       mdl,
		Tools:       reg,
		MaxSteps:    4,
		MaxParallel: maxParallel,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	res, err := runner.Run(context.Background(), Request{Messages: []*schema.Message{schema.UserMessage("go")}},
		func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, events
}

// TestParallelSafeCallsOverlap is the feature: two reads in one reply no longer
// wait for each other.
func TestParallelSafeCallsOverlap(t *testing.T) {
	a := &scheduleProbe{name: "read_a", mode: tool.ParallelSafe, hold: 120 * time.Millisecond}
	b := &scheduleProbe{name: "read_b", mode: tool.ParallelSafe, hold: 120 * time.Millisecond}
	c := &scheduleProbe{name: "read_c", mode: tool.ParallelSafe, hold: 120 * time.Millisecond}

	start := time.Now()
	res, _ := runWithProbes(t, 4, a, b, c)
	elapsed := time.Since(start)

	// Three 120ms calls that overlap take about 120ms in total, not 360ms. The
	// bound is loose on purpose: this asserts overlap, not a benchmark.
	if elapsed > 300*time.Millisecond {
		t.Errorf("three parallel-safe calls took %v; they did not overlap (serial would be ~360ms)", elapsed)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(res.Tools))
	}
	for _, p := range []*scheduleProbe{a, b, c} {
		w := p.windows()
		if len(w) != 1 {
			t.Fatalf("%s ran %d times, want 1", p.name, len(w))
		}
	}
	if !overlaps(a.windows()[0], b.windows()[0]) || !overlaps(b.windows()[0], c.windows()[0]) {
		t.Errorf("the calls did not overlap: a=%v b=%v c=%v", a.windows(), b.windows(), c.windows())
	}
}

// TestSerialCallIsABarrier is the correctness half, and the reason the schedule
// is not "run everything at once": "write the config, then run the tests".
func TestSerialCallIsABarrier(t *testing.T) {
	read1 := &scheduleProbe{name: "read1", mode: tool.ParallelSafe, hold: 60 * time.Millisecond}
	write := &scheduleProbe{name: "write", mode: tool.Serial, hold: 30 * time.Millisecond}
	read2 := &scheduleProbe{name: "read2", mode: tool.ParallelSafe, hold: 60 * time.Millisecond}

	_, _ = runWithProbes(t, 4, read1, write, read2)

	r1, w, r2 := read1.windows()[0], write.windows()[0], read2.windows()[0]
	if overlaps(r1, w) {
		t.Error("the write overlapped a call that came before it")
	}
	if overlaps(w, r2) {
		t.Error("the call after the write started before the write finished")
	}
	if !r1[1].Before(w[0]) && !r1[1].Equal(w[0]) {
		t.Error("the write started before the earlier read finished")
	}
	if !w[1].Before(r2[0]) && !w[1].Equal(r2[0]) {
		t.Error("the later read started before the write finished")
	}
}

// TestWritesNeverOverlapEachOther: two writes are a barrier even though they name
// different files, because their order is what the model meant.
func TestWritesNeverOverlapEachOther(t *testing.T) {
	w1 := &scheduleProbe{name: "write1", mode: tool.Serial, hold: 50 * time.Millisecond}
	w2 := &scheduleProbe{name: "write2", mode: tool.Serial, hold: 50 * time.Millisecond}

	_, _ = runWithProbes(t, 4, w1, w2)

	a, b := w1.windows()[0], w2.windows()[0]
	if overlaps(a, b) {
		t.Errorf("two writes overlapped: %v and %v", a, b)
	}
	if b[0].Before(a[1]) {
		t.Error("the second write started before the first finished")
	}
}

// TestResultsAreInProgramOrder: the model's calls are matched to their results by
// position, so the order of the *results* may not follow the order of completion.
// Here the first call is the slowest, which is exactly when a completion-ordered
// implementation would get it wrong.
func TestResultsAreInProgramOrder(t *testing.T) {
	slow := &scheduleProbe{name: "slow", mode: tool.ParallelSafe, hold: 120 * time.Millisecond}
	fast := &scheduleProbe{name: "fast", mode: tool.ParallelSafe, hold: 5 * time.Millisecond}
	quick := &scheduleProbe{name: "quick", mode: tool.ParallelSafe, hold: 1 * time.Millisecond}

	res, _ := runWithProbes(t, 4, slow, fast, quick)

	want := []string{"slow", "fast", "quick"}
	for i, name := range want {
		if res.Tools[i].Name != name {
			t.Errorf("result %d is %q, want %q: the results must stay in the order the model asked", i, res.Tools[i].Name, name)
		}
		if res.Tools[i].ID != "call-"+name {
			t.Errorf("result %d carries id %q", i, res.Tools[i].ID)
		}
	}
	// And the observation the model reads is its own call's result, not a
	// neighbour's.
	if !strings.Contains(res.Tools[0].Result, "ok:slow") {
		t.Errorf("result 0 = %q, want the slow call's output", res.Tools[0].Result)
	}
	if !strings.Contains(res.Tools[2].Result, "ok:quick") {
		t.Errorf("result 2 = %q, want the quick call's output", res.Tools[2].Result)
	}
}

// TestMaxParallelOneIsTheOldBehaviour: the regression gate for every deployment
// that does not opt in.
func TestMaxParallelOneIsTheOldBehaviour(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			a := &scheduleProbe{name: "a", mode: tool.ParallelSafe, hold: 40 * time.Millisecond}
			b := &scheduleProbe{name: "b", mode: tool.ParallelSafe, hold: 40 * time.Millisecond}

			start := time.Now()
			_, _ = runWithProbes(t, limit, a, b)
			elapsed := time.Since(start)

			if overlaps(a.windows()[0], b.windows()[0]) {
				t.Error("calls overlapped with a limit of one")
			}
			if elapsed < 70*time.Millisecond {
				t.Errorf("two 40ms calls finished in %v; they overlapped", elapsed)
			}
		})
	}
}

// TestMaxParallelBoundsTheFanOut: the limit is a bound, not a hint.
func TestMaxParallelBoundsTheFanOut(t *testing.T) {
	var (
		mu       sync.Mutex
		inflight int
		peak     int
	)
	makeProbe := func(name string) *scheduleProbe {
		return &scheduleProbe{name: name, mode: tool.ParallelSafe, hold: 40 * time.Millisecond}
	}
	probes := []*scheduleProbe{makeProbe("a"), makeProbe("b"), makeProbe("c"), makeProbe("d"), makeProbe("e"), makeProbe("f")}

	// Wrap each probe's run so the test can see how many were in flight at once.
	reg := tool.NewRegistry()
	calls := make([]schema.ToolCall, 0, len(probes))
	for _, p := range probes {
		inner := p
		counting := &countingTool{
			name: inner.name,
			onStart: func() {
				mu.Lock()
				inflight++
				if inflight > peak {
					peak = inflight
				}
				mu.Unlock()
			},
			onEnd: func() {
				mu.Lock()
				inflight--
				mu.Unlock()
			},
			hold: inner.hold,
		}
		if err := reg.Register(tool.WithConcurrency(counting, tool.ParallelSafe)); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, toolCall(inner.name, `{}`))
	}

	mdl := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: calls},
		{Role: schema.Assistant, Content: "done"},
	}}
	runner, err := New(Config{Model: mdl, Tools: reg, MaxSteps: 3, MaxParallel: 2, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Request{Messages: []*schema.Message{schema.UserMessage("go")}}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if peak > 2 {
		t.Errorf("peak concurrency was %d, want at most 2", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d; six parallel-safe calls with a limit of 2 should reach it", peak)
	}
}

// TestToolPanicDoesNotKillTheTurn: a panicking tool is a broken tool, not a broken
// agent. Serially a panic was fatal too, but serially nobody had made ten of them
// happen at once.
func TestToolPanicDoesNotKillTheTurn(t *testing.T) {
	boom := &scheduleProbe{name: "boom", mode: tool.ParallelSafe, panicOn: true}
	fine := &scheduleProbe{name: "fine", mode: tool.ParallelSafe}

	res, events := runWithProbes(t, 4, boom, fine)

	if len(res.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(res.Tools))
	}
	if res.Tools[0].Err == "" || !strings.Contains(res.Tools[0].Err, "panic") {
		t.Errorf("the panicking call should be reported as a failed tool, got %+v", res.Tools[0])
	}
	if res.Tools[1].Err != "" {
		t.Errorf("the sibling call failed too: %+v", res.Tools[1])
	}
	if res.Text != "done" {
		t.Errorf("the turn did not finish: %q", res.Text)
	}
	// The failure is announced on the stream, or the console shows a call that
	// never returned.
	var announced bool
	for _, e := range events {
		if e.Type == EventToolResult && e.ToolCallID == "call-boom" && e.ToolError != "" {
			announced = true
		}
	}
	if !announced {
		t.Error("the panic was not announced as a tool result")
	}
}

// TestOneFailureDoesNotCancelItsSiblings: a tool call failing is an observation
// the model reasons about, not a reason to abandon the other calls it asked for.
func TestOneFailureDoesNotCancelItsSiblings(t *testing.T) {
	failing := &scheduleProbe{name: "failing", mode: tool.ParallelSafe, hold: 20 * time.Millisecond}
	ok := &scheduleProbe{name: "ok", mode: tool.ParallelSafe, hold: 20 * time.Millisecond}

	reg := tool.NewRegistry()
	if err := reg.Register(tool.WithConcurrency(
		errorTool{name: "failing"}, tool.ParallelSafe)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tool.WithConcurrency(probeTool{probe: ok}, tool.ParallelSafe)); err != nil {
		t.Fatal(err)
	}
	_ = failing

	mdl := &fakeModel{turns: []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			toolCall("failing", `{}`), toolCall("ok", `{}`),
		}},
		{Role: schema.Assistant, Content: "done"},
	}}
	runner, err := New(Config{Model: mdl, Tools: reg, MaxSteps: 3, MaxParallel: 4, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(context.Background(), Request{Messages: []*schema.Message{schema.UserMessage("go")}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Tools[0].Err == "" {
		t.Error("the failing call did not report a failure")
	}
	if res.Tools[1].Err != "" || !strings.Contains(res.Tools[1].Result, "ok:ok") {
		t.Errorf("the sibling call was affected: %+v", res.Tools[1])
	}
	if len(ok.windows()) != 1 {
		t.Errorf("the sibling ran %d times", len(ok.windows()))
	}
}

// errAlways is what errorTool returns.
var errAlways = errors.New("this tool always fails")

// errorTool always fails.
type errorTool struct{ name string }

func (e errorTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: e.name, Desc: "always fails"}, nil
}

func (e errorTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "", errAlways
}

// countingTool reports how many calls are in flight.
type countingTool struct {
	name    string
	onStart func()
	onEnd   func()
	hold    time.Duration
}

func (c *countingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: c.name, Desc: "counting"}, nil
}

func (c *countingTool) InvokableRun(ctx context.Context, _ string, _ ...einotool.Option) (string, error) {
	c.onStart()
	defer c.onEnd()
	select {
	case <-time.After(c.hold):
	case <-ctx.Done():
	}
	return "ok", nil
}
