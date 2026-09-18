package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/huan/huan-agent/internal/retry"
)

// scriptedModel is a chat model whose every call follows a script: each entry is
// what one attempt should do. It is a fake rather than a transport double because
// the behaviour under test is "what the wrapper does with a failure", and a
// server would only make that harder to read.
type scriptedModel struct {
	mu    sync.Mutex
	calls int
	// generate behaves like Stream for the streaming tests but reports through
	// Generate.
	generate bool
	// outcomes is what each attempt returns, in order. Running past the end
	// repeats the last entry.
	outcomes []outcome
	// lastHistory records the messages each attempt was called with, so a test
	// can prove a retry re-sends exactly the same request.
	lastHistory [][]*schema.Message
	tools       int
}

type outcome struct {
	// chunks are delivered one by one, then the call ends.
	chunks []*schema.Message
	// err is returned instead of a stream (Stream) or as the call's error
	// (Generate).
	err error
	// midErr is delivered after the chunks, which is how a stream that dies
	// mid-answer is expressed.
	midErr error
}

func (m *scriptedModel) next() outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.outcomes) == 0 {
		return outcome{}
	}
	idx := m.calls
	m.calls++
	if idx >= len(m.outcomes) {
		idx = len(m.outcomes) - 1
	}
	return m.outcomes[idx]
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.record(msgs)
	o := m.next()
	if o.err != nil {
		return nil, o.err
	}
	if len(o.chunks) == 0 {
		return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
	}
	return o.chunks[0], nil
}

func (m *scriptedModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(msgs)
	o := m.next()
	if o.err != nil {
		return nil, o.err
	}
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		for _, chunk := range o.chunks {
			if closed := sw.Send(chunk, nil); closed {
				return
			}
		}
		if o.midErr != nil {
			_ = sw.Send(nil, o.midErr)
		}
	}()
	return sr, nil
}

func (m *scriptedModel) record(msgs []*schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastHistory = append(m.lastHistory, msgs)
}

func (m *scriptedModel) attemptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// fastPolicy retries in microseconds, so the tests exercise the loop rather than
// the clock.
func fastPolicy(attempts int) retry.Policy {
	return retry.Policy{MaxAttempts: attempts, BaseDelay: time.Millisecond, Multiplier: 2}
}

func TestRetryingModel_GenerateRetriesATransientFailure(t *testing.T) {
	inner := &scriptedModel{
		generate: true,
		outcomes: []outcome{
			{err: &LLMError{Provider: "p", StatusCode: 429, Err: errors.New("slow down")}},
			{err: &LLMError{Provider: "p", StatusCode: 503, Err: errors.New("unavailable")}},
			{chunks: []*schema.Message{{Role: schema.Assistant, Content: "third time"}}},
		},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	msg, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("Generate() = %v, want success on the third attempt", err)
	}
	if msg.Content != "third time" {
		t.Fatalf("content = %q, want the third attempt's answer", msg.Content)
	}
	if got := inner.attemptCount(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRetryingModel_GenerateDoesNotRetryAPermanentFailure(t *testing.T) {
	inner := &scriptedModel{
		generate: true,
		outcomes: []outcome{{err: &LLMError{Provider: "p", StatusCode: 401, Err: errors.New("bad key")}}},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(5)})

	_, err := m.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("Generate() = nil, want the 401")
	}
	if got := inner.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a 401 fails identically however often it is paid for", got)
	}
}

func TestRetryingModel_GenerateGivesUpAfterTheLastAttempt(t *testing.T) {
	inner := &scriptedModel{
		generate: true,
		outcomes: []outcome{{err: &LLMError{Provider: "p", StatusCode: 500, Err: errors.New("down")}}},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	if _, err := m.Generate(context.Background(), nil); err == nil {
		t.Fatal("Generate() = nil, want the failure to surface")
	}
	if got := inner.attemptCount(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRetryingModel_GenerateStopsOnCancellation(t *testing.T) {
	inner := &scriptedModel{
		generate: true,
		outcomes: []outcome{{err: errors.New("boom")}},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(4)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Generate(ctx, nil); err == nil {
		t.Fatal("Generate() = nil, want a failure")
	}
	if got := inner.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a stopped turn must not retry", got)
	}
}

func TestRetryingModel_StreamRetriesBeforeTheFirstChunk(t *testing.T) {
	inner := &scriptedModel{
		outcomes: []outcome{
			{err: &LLMError{Provider: "p", StatusCode: 429, Err: errors.New("slow down")}},
			{chunks: []*schema.Message{{Role: schema.Assistant, Content: "hello"}}},
		},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	sr, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("Stream() = %v, want a retry to recover", err)
	}
	defer sr.Close()

	msg, err := sr.Recv()
	if err != nil {
		t.Fatalf("Recv() = %v, want the answer", err)
	}
	if msg.Content != "hello" {
		t.Fatalf("content = %q, want hello", msg.Content)
	}
	if _, err := sr.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv() = %v, want EOF", err)
	}
	if got := inner.attemptCount(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestRetryingModel_StreamRetriesWhenTheStreamDiesBeforeAnyChunk(t *testing.T) {
	// The connection drops between the request and the first token: nothing has
	// been shown to anyone, so replaying it is invisible.
	inner := &scriptedModel{
		outcomes: []outcome{
			{midErr: errors.New("unexpected EOF")},
			{chunks: []*schema.Message{{Role: schema.Assistant, Content: "recovered"}}},
		},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	sr, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream() = %v, want a retry to recover", err)
	}
	defer sr.Close()

	msg, err := sr.Recv()
	if err != nil {
		t.Fatalf("Recv() = %v, want the recovered answer", err)
	}
	if msg.Content != "recovered" {
		t.Fatalf("content = %q, want recovered", msg.Content)
	}
	if got := inner.attemptCount(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestRetryingModel_StreamDoesNotReplayAfterTheFirstChunk(t *testing.T) {
	// A stream that dies mid-answer must NOT be retried here: the reader already
	// holds the beginning, and replaying would show it twice. The caller (the
	// chat runner) owns this case, because only it can tell its client to drop
	// the partial output.
	inner := &scriptedModel{
		outcomes: []outcome{{
			chunks: []*schema.Message{{Role: schema.Assistant, Content: "half an ans"}},
			midErr: errors.New("unexpected EOF"),
		}},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	sr, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream() = %v, want the first attempt to be accepted", err)
	}
	defer sr.Close()

	first, err := sr.Recv()
	if err != nil || first.Content != "half an ans" {
		t.Fatalf("first Recv() = (%v, %v), want the partial text", first, err)
	}
	if _, err := sr.Recv(); err == nil {
		t.Fatal("second Recv() = nil, want the mid-stream failure to surface")
	}
	if got := inner.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a retry would duplicate the text the reader already has", got)
	}
}

func TestRetryingModel_StreamPassesThroughAnEmptyStream(t *testing.T) {
	// An empty stream is an answer, not a failure: retrying would buy the same
	// nothing a second time.
	inner := &scriptedModel{outcomes: []outcome{{}}}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3)})

	sr, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream() = %v, want nil", err)
	}
	defer sr.Close()
	if _, err := sr.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() = %v, want EOF", err)
	}
	if got := inner.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestRetryingModel_RetriesTheSameRequest(t *testing.T) {
	// A retry that sent something different would be a second question, not a
	// second attempt at the same one.
	inner := &scriptedModel{
		outcomes: []outcome{
			{err: errors.New("boom")},
			{chunks: []*schema.Message{{Role: schema.Assistant, Content: "ok"}}},
		},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(2)})
	history := []*schema.Message{{Role: schema.System, Content: "rules"}, {Role: schema.User, Content: "hi"}}

	if _, err := m.Stream(context.Background(), history); err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if len(inner.lastHistory) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(inner.lastHistory))
	}
	if inner.lastHistory[0][1].Content != inner.lastHistory[1][1].Content {
		t.Fatal("the retry sent a different request than the attempt it replaced")
	}
}

func TestRetryingModel_WithToolsKeepsRetrying(t *testing.T) {
	inner := &toolCapableModel{}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(2)})

	tcm, ok := m.(model.ToolCallingChatModel)
	if !ok {
		t.Fatal("the wrapped model does not satisfy ToolCallingChatModel: the runner would silently lose tools")
	}
	bound, err := tcm.WithTools([]*schema.ToolInfo{{Name: "time"}})
	if err != nil {
		t.Fatalf("WithTools() = %v", err)
	}
	retrying, ok := bound.(*retryingModel)
	if !ok {
		t.Fatalf("WithTools returned %T, want a retrying model", bound)
	}
	if got := retrying.policy.Attempts(); got != 2 {
		t.Fatalf("bound policy attempts = %d, want 2", got)
	}
	if inner.bound != 1 {
		t.Fatalf("inner WithTools called %d times, want 1", inner.bound)
	}
}

func TestRetryingModel_WithToolsOnAPlainModelRefuses(t *testing.T) {
	// The wrapper always satisfies the interface, so a model that cannot call
	// tools has to say so here rather than silently pretending.
	m := withRetry(&scriptedModel{}, adapterOptions{retry: fastPolicy(2)})
	tcm := m.(model.ToolCallingChatModel)
	if _, err := tcm.WithTools(nil); err == nil {
		t.Fatal("WithTools() = nil error, want a refusal for a non-tool model")
	}
}

func TestRetryingModel_ZeroPolicyWrapsNothing(t *testing.T) {
	inner := &scriptedModel{outcomes: []outcome{{err: errors.New("boom")}}}
	// A single attempt is "no retrying", and it must not pay for a wrapper at all.
	passthrough := withRetry(inner, adapterOptions{retry: retry.Policy{MaxAttempts: 1}})
	if _, ok := passthrough.(*retryingModel); ok {
		t.Fatal("a policy of one attempt still built a retrying model")
	}
	off := withRetry(inner, adapterOptions{})
	if _, ok := off.(*retryingModel); ok {
		t.Fatal("the zero policy still built a retrying model")
	}
}

func TestRetryingModel_ForwardsNameAndProvider(t *testing.T) {
	inner := &openAIModel{provider: Provider{Name: "deepseek", Model: "deepseek-chat"}}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(2)})
	if got := m.(*retryingModel).Name(); got != "deepseek-chat" {
		t.Fatalf("Name() = %q, want the inner model's name", got)
	}
	if got := m.(*retryingModel).Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want the inner provider's name", got)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain transport error", errors.New("connection reset by peer"), true},
		{"net error", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"status 0", &LLMError{StatusCode: 0, Err: errors.New("eof")}, true},
		{"408", &LLMError{StatusCode: 408}, true},
		{"409", &LLMError{StatusCode: 409}, true},
		{"425", &LLMError{StatusCode: 425}, true},
		{"429 rate limit", &LLMError{StatusCode: 429}, true},
		{"500", &LLMError{StatusCode: 500}, true},
		{"502", &LLMError{StatusCode: 502}, true},
		{"503", &LLMError{StatusCode: 503}, true},
		{"400 bad request", &LLMError{StatusCode: 400}, false},
		{"401 unauthorized", &LLMError{StatusCode: 401}, false},
		{"402 payment required", &LLMError{StatusCode: 402}, false},
		{"403 forbidden", &LLMError{StatusCode: 403}, false},
		{"404", &LLMError{StatusCode: 404}, false},
		{"413 too large", &LLMError{StatusCode: 413}, false},
		{"422 unprocessable", &LLMError{StatusCode: 422}, false},
		{"wrapped canceled", errors.Join(errors.New("dial failed"), context.Canceled), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Fatalf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestLLMError_RetryableMethod(t *testing.T) {
	// The method is what internal/chat's step retry asks through, without
	// importing this package: it must agree with the predicate.
	var decided interface{ Retryable() bool } = &LLMError{StatusCode: 503}
	if !decided.Retryable() {
		t.Fatal("a 503 reports itself as non-retryable")
	}
	decided = &LLMError{StatusCode: 400}
	if decided.Retryable() {
		t.Fatal("a 400 reports itself as retryable")
	}
}

// toolCapableModel is a model that supports tool calling, for the WithTools test.
type toolCapableModel struct {
	bound int
}

func (m *toolCapableModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant}, nil
}

func (m *toolCapableModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Close()
	return sr, nil
}

func (m *toolCapableModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.bound++
	return m, nil
}

func TestWithRetryOption_EnablesOnlyWhenAsked(t *testing.T) {
	inner := &scriptedModel{}
	opts := adapterOptions{}
	if withRetry(inner, opts) != model.BaseChatModel(inner) {
		t.Fatal("no option did not return the model untouched")
	}
	built := withRetry(inner, applyOptions([]Option{WithRetry(fastPolicy(3), nil)}))
	if _, ok := built.(*retryingModel); !ok {
		t.Fatal("WithRetry(policy) did not wrap the model")
	}
}

func TestWithRetryOption_DisabledPolicyMeansNothingChanges(t *testing.T) {
	inner := &scriptedModel{}
	// What a config with llm.retry.enable = false produces: an attempt count of
	// one, and therefore exactly the behaviour of a build without this feature.
	disabled := retry.Policy{MaxAttempts: 1}
	if withRetry(inner, applyOptions([]Option{WithRetry(disabled, nil)})) != model.BaseChatModel(inner) {
		t.Fatal("a disabled policy still wrapped the model")
	}
}

func TestRetryLogging_MentionsTheAttemptAndTheDelay(t *testing.T) {
	// The log line is the only trace a successful-on-retry call leaves; it has to
	// say which attempt and how long it waited.
	var buf strings.Builder
	logger := testLogger(&buf)
	inner := &scriptedModel{
		generate: true,
		outcomes: []outcome{
			{err: &LLMError{Provider: "p", StatusCode: 503, Err: errors.New("unavailable")}},
			{chunks: []*schema.Message{{Role: schema.Assistant, Content: "ok"}}},
		},
	}
	m := withRetry(inner, adapterOptions{retry: fastPolicy(3), logger: logger})
	if _, err := m.Generate(context.Background(), nil); err != nil {
		t.Fatalf("Generate() = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"retrying", "attempt", "unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line %q does not mention %q", out, want)
		}
	}
}

// testLogger builds a zap logger that writes its console output into w, so a
// test can assert on what a retry would have logged without a real log sink.
func testLogger(w io.Writer) *zap.Logger {
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(w), zapcore.WarnLevel)
	return zap.New(core)
}
