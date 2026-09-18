package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/retry"
)

// flakyModel is a model whose first attempt dies mid-answer and whose later
// attempts succeed. It is the shape this feature exists for: the failure the llm
// wrapper refuses to replay (the reader has already been handed text) and that
// only the step can take back.
type flakyModel struct {
	mu sync.Mutex
	// scripts is one function per attempt; the last one repeats.
	scripts []func() (*schema.StreamReader[*schema.Message], error)
	calls   int
	// sent records the message count of every attempt, to prove a retry re-sends
	// the same history.
	sent [][]*schema.Message
}

func (m *flakyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *flakyModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("not used")
}

func (m *flakyModel) Stream(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.sent = append(m.sent, msgs)
	m.mu.Unlock()
	if len(m.scripts) == 0 {
		return nil, errors.New("flakyModel: no script")
	}
	if idx >= len(m.scripts) {
		idx = len(m.scripts) - 1
	}
	return m.scripts[idx]()
}

func (m *flakyModel) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// streamThenFail delivers chunks and then an error, which is a stream that died
// mid-answer.
func streamThenFail(chunks []*schema.Message, err error) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		for _, c := range chunks {
			if closed := sw.Send(c, nil); closed {
				return
			}
		}
		_ = sw.Send(nil, err)
	}()
	return sr, nil
}

// fastStepRetry retries in microseconds, so the test exercises the loop rather
// than the clock.
func fastStepRetry(attempts int) retry.Policy {
	return retry.Policy{MaxAttempts: attempts, BaseDelay: time.Millisecond, Multiplier: 2}
}

// collectEvents runs one turn against the scripted model and returns the events.
func collectEvents(t *testing.T, m model.BaseChatModel, policy retry.Policy, req Request) (*Result, error, []Event) {
	t.Helper()
	runner, err := New(Config{Model: m, StepRetry: policy})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	var events []Event
	res, runErr := runner.Run(context.Background(), req, func(e Event) {
		events = append(events, e)
	})
	return res, runErr, events
}

func TestRun_RetriesAStepThatDiedMidStream(t *testing.T) {
	partial := []*schema.Message{{Role: schema.Assistant, Content: "让我先看一下"}}
	final := []*schema.Message{{Role: schema.Assistant, Content: "看完了，答案是 42。"}}
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamThenFail(partial, errors.New("unexpected EOF"))
		},
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamOfChunks(final...), nil
		},
	}}

	res, runErr, events := collectEvents(t, m, fastStepRetry(3), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "算一下"}},
	})
	if runErr != nil {
		t.Fatalf("Run() error = %v, want the retry to recover", runErr)
	}
	if res.Text != "看完了，答案是 42。" {
		t.Fatalf("answer = %q, want the second attempt's answer", res.Text)
	}
	if got := m.attempts(); got != 2 {
		t.Fatalf("model attempts = %d, want 2", got)
	}

	// The retry is announced BEFORE the retry's own deltas, because that is what
	// tells the client to drop what it already showed.
	var retryEvents []Event
	var retryAt, doneAt = -1, -1
	for i, e := range events {
		switch e.Type {
		case EventStepRetry:
			retryEvents = append(retryEvents, e)
			if retryAt < 0 {
				retryAt = i
			}
		case EventDone:
			doneAt = i
		}
	}
	if len(retryEvents) != 1 {
		t.Fatalf("step_retry events = %d, want 1", len(retryEvents))
	}
	got := retryEvents[0]
	if got.Step != 1 || got.Attempt != 2 || got.MaxAttempts != 3 {
		t.Fatalf("step_retry = step %d attempt %d/%d, want step 1 attempt 2/3",
			got.Step, got.Attempt, got.MaxAttempts)
	}
	if got.DelayMs <= 0 {
		t.Fatalf("step_retry delay = %dms, want the backoff that was waited out", got.DelayMs)
	}
	if !strings.Contains(got.Error, "unexpected EOF") {
		t.Fatalf("step_retry error = %q, want the failure that caused it", got.Error)
	}
	if retryAt > doneAt {
		t.Fatal("step_retry arrived after done")
	}
	// The retried attempt's text comes after the announcement.
	for i := retryAt + 1; i < doneAt; i++ {
		if events[i].Type == EventTextDelta && events[i].Text == "看完了，答案是 42。" {
			return
		}
	}
	t.Fatal("the recovered answer did not arrive as a delta after the retry")
}

func TestRun_RetryResendsTheSameHistory(t *testing.T) {
	// A retry that sent a different history would be a second question rather
	// than a second attempt at the same one — and a half-answer fed back in would
	// be the model arguing with itself.
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamThenFail([]*schema.Message{{Role: schema.Assistant, Content: "半句"}}, errors.New("boom"))
		},
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamOfChunks(&schema.Message{Role: schema.Assistant, Content: "完整回答"}), nil
		},
	}}
	history := []*schema.Message{
		{Role: schema.System, Content: "规则"},
		{Role: schema.User, Content: "问题"},
	}
	if _, err, _ := collectEvents(t, m, fastStepRetry(2), Request{Messages: history}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(m.sent) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(m.sent))
	}
	if len(m.sent[0]) != len(m.sent[1]) {
		t.Fatalf("retry sent %d messages, the attempt it replaced sent %d", len(m.sent[1]), len(m.sent[0]))
	}
	for i := range m.sent[0] {
		if m.sent[0][i].Content != m.sent[1][i].Content {
			t.Fatalf("message %d differs between attempts: %q vs %q",
				i, m.sent[0][i].Content, m.sent[1][i].Content)
		}
	}
}

func TestRun_NoRetryWhenThePolicyIsOff(t *testing.T) {
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamThenFail([]*schema.Message{{Role: schema.Assistant, Content: "半句"}}, errors.New("boom"))
		},
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamOfChunks(&schema.Message{Role: schema.Assistant, Content: "不该被调用"}), nil
		},
	}}
	_, err, events := collectEvents(t, m, retry.Policy{}, Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "问题"}},
	})
	if err == nil {
		t.Fatal("Run() = nil error, want the failure to surface")
	}
	if got := m.attempts(); got != 1 {
		t.Fatalf("model attempts = %d, want 1: the zero policy means no retrying", got)
	}
	for _, e := range events {
		if e.Type == EventStepRetry {
			t.Fatal("a step_retry was emitted although the policy allows one attempt")
		}
	}
}

func TestRun_GivesUpAfterTheLastAttempt(t *testing.T) {
	fail := func() (*schema.StreamReader[*schema.Message], error) {
		return streamThenFail(nil, errors.New("still down"))
	}
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){fail, fail}}
	_, err, events := collectEvents(t, m, fastStepRetry(3), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "问题"}},
	})
	if err == nil {
		t.Fatal("Run() = nil error, want a failure after every attempt")
	}
	if got := m.attempts(); got != 3 {
		t.Fatalf("model attempts = %d, want 3", got)
	}
	retries := 0
	for _, e := range events {
		if e.Type == EventStepRetry {
			retries++
		}
	}
	if retries != 2 {
		t.Fatalf("step_retry events = %d, want 2 (one per retry, not one per attempt)", retries)
	}
}

func TestRun_DoesNotRetryAPermanentFailure(t *testing.T) {
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		func() (*schema.StreamReader[*schema.Message], error) {
			return nil, &permanentError{msg: "invalid api key"}
		},
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamOfChunks(&schema.Message{Role: schema.Assistant, Content: "不该被调用"}), nil
		},
	}}
	_, err, events := collectEvents(t, m, fastStepRetry(4), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "问题"}},
	})
	if err == nil {
		t.Fatal("Run() = nil error, want the permanent failure")
	}
	if got := m.attempts(); got != 1 {
		t.Fatalf("model attempts = %d, want 1: a permanent failure is not retried", got)
	}
	for _, e := range events {
		if e.Type == EventStepRetry {
			t.Fatal("a permanent failure was announced as a retry")
		}
	}
}

func TestRun_DoesNotRetryOnceTheBudgetIsSpent(t *testing.T) {
	// The turn's wall clock is the outer bound: a retry that starts after it
	// expired would spend a call the turn may no longer make.
	m := &flakyModel{scripts: []func() (*schema.StreamReader[*schema.Message], error){
		func() (*schema.StreamReader[*schema.Message], error) {
			// The first attempt outlives the turn's deadline, which is the only
			// way this test says anything: a deadline that had already passed
			// before the step started would stop the turn instead of the retry.
			time.Sleep(30 * time.Millisecond)
			return streamThenFail(nil, errors.New("boom"))
		},
		func() (*schema.StreamReader[*schema.Message], error) {
			return streamOfChunks(&schema.Message{Role: schema.Assistant, Content: "不该被调用"}), nil
		},
	}}
	runner, err := New(Config{Model: m, StepRetry: retry.Policy{
		MaxAttempts: 4,
		BaseDelay:   time.Hour, // the wait alone would outlast the turn
		Multiplier:  2,
	}})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	_, runErr := runner.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "问题"}},
		Deadline: 5 * time.Millisecond,
	}, func(Event) {})
	if runErr == nil {
		t.Fatal("Run() = nil error, want a failure")
	}
	if got := m.attempts(); got != 1 {
		t.Fatalf("model attempts = %d, want 1: the deadline passed while the first attempt ran", got)
	}
}

func TestStepRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped cancel", errors.Join(errors.New("stream ended"), context.Canceled), false},
		{"plain", errors.New("unexpected EOF"), true},
		{"wrapped permanent", errors.Join(errors.New("request failed"), &permanentError{msg: "bad key"}), false},
		{"wrapped transient", errors.Join(errors.New("request failed"), &transientError{}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepRetryable(tc.err); got != tc.want {
				t.Fatalf("stepRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// permanentError is an error that knows a retry cannot help it — the shape
// internal/llm's *LLMError has, reached here through a one-method interface so
// this package never imports a provider implementation.
type permanentError struct{ msg string }

func (e *permanentError) Error() string   { return e.msg }
func (e *permanentError) Retryable() bool { return false }

// transientError is the other half of that interface.
type transientError struct{}

func (e *transientError) Error() string   { return "transient" }
func (e *transientError) Retryable() bool { return true }

func TestTurnBudget_RetryShare(t *testing.T) {
	// Unlimited turns hand the cap back to the policy.
	if got := (turnBudget{}).retryShare(time.Now()); got != 0 {
		t.Fatalf("retryShare on an unlimited turn = %s, want 0", got)
	}
	// A turn with a deadline offers what is left of it.
	b := turnBudget{deadline: 10 * time.Second}
	got := b.retryShare(time.Now())
	if got <= 0 || got > 10*time.Second {
		t.Fatalf("retryShare = %s, want (0, 10s]", got)
	}
	// A turn that is already past its deadline offers nothing, so the wait is
	// capped to zero and the retry decision (which checks the budget too) refuses.
	expired := turnBudget{deadline: time.Nanosecond}
	time.Sleep(time.Millisecond)
	if got := expired.retryShare(time.Now().Add(-time.Second)); got != 0 {
		t.Fatalf("retryShare past the deadline = %s, want 0", got)
	}
}
