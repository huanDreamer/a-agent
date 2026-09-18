package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/retry"
	"github.com/huan/huan-agent/internal/store"
)

// dyingStreamModel delivers a partial answer and then dies, once; every later
// attempt answers normally. It is the failure the client has to be told about:
// text was already on screen when the stream broke.
type dyingStreamModel struct {
	mu    sync.Mutex
	calls int
	// first is what the first attempt streams before dying.
	first []*schema.Message
	// answer is what every later attempt streams.
	answer string
}

func (m *dyingStreamModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *dyingStreamModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("not used")
}

func (m *dyingStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.mu.Unlock()

	if idx == 0 {
		sr, sw := schema.Pipe[*schema.Message](8)
		go func() {
			defer sw.Close()
			for _, c := range m.first {
				if closed := sw.Send(c, nil); closed {
					return
				}
			}
			_ = sw.Send(nil, errors.New("unexpected EOF"))
		}()
		return sr, nil
	}
	return schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: m.answer},
	}), nil
}

func (m *dyingStreamModel) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// TestTurn_StepRetryIsVisibleAndLeavesNoDuplicate is the reported failure end to
// end: a stream dies after the model started answering, the step is run again,
// the reader is told to drop what it had, and what is stored is one answer.
func TestTurn_StepRetryIsVisibleAndLeavesNoDuplicate(t *testing.T) {
	mdl := &dyingStreamModel{
		first:  []*schema.Message{{Role: schema.Assistant, Content: "让我先看一下这个文件"}},
		answer: "看完了：答案就在这里。",
	}
	runner, err := chat.New(chat.Config{
		Model: mdl,
		// Wait a millisecond between attempts: the test is about what the reader
		// and the database end up with, not about the backoff.
		StepRetry: retry.Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, Multiplier: 2},
		MaxSteps:  4,
		Logger:    zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "retry"}), &created)
	sessionID := created.Session.ID

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "看一下这个文件"})

	var (
		retries  []sseEvent
		deltaAt  = -1
		retryAt  = -1
		answer   = answerText(events)
		types    = eventTypes(events)
		lastType string
	)
	if len(types) > 0 {
		lastType = types[len(types)-1]
	}
	for i, e := range events {
		switch kind, _ := e["type"].(string); kind {
		case string(chat.EventStepRetry):
			retries = append(retries, e)
			if retryAt < 0 {
				retryAt = i
			}
		case "text_delta":
			if text, _ := e["text"].(string); strings.Contains(text, "看完了") && deltaAt < 0 {
				deltaAt = i
			}
		}
	}

	if len(retries) != 1 {
		t.Fatalf("step_retry events = %d, want 1", len(retries))
	}
	got := retries[0]
	if attempt, _ := got["attempt"].(float64); attempt != 2 {
		t.Errorf("attempt = %v, want 2", got["attempt"])
	}
	if maxAttempts, _ := got["max_attempts"].(float64); maxAttempts != 3 {
		t.Errorf("max_attempts = %v, want 3", got["max_attempts"])
	}
	if delay, _ := got["delay_ms"].(float64); delay <= 0 {
		t.Errorf("delay_ms = %v, want the backoff that was waited out", got["delay_ms"])
	}
	if text, _ := got["error"].(string); !strings.Contains(text, "unexpected EOF") {
		t.Errorf("error = %q, want the failure that caused the retry", text)
	}
	// The announcement has to arrive before the replacement text, or the client
	// appends the retry's answer to the half-answer it was never told to drop.
	if retryAt < 0 || deltaAt < 0 || retryAt > deltaAt {
		t.Fatalf("step_retry at %d, recovered text at %d: the client cannot undo text it has not been warned about", retryAt, deltaAt)
	}
	if lastType != "stream_end" {
		t.Errorf("stream ends with %q, want stream_end", lastType)
	}

	// What the reader was handed includes the discarded attempt (that is what the
	// event is for); what is *stored* must not.
	if !strings.Contains(answer, "让我先看一下这个文件") {
		t.Errorf("the reader never saw the first attempt's text: %q", answer)
	}
	msgs, err := st.ListChatMessages(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var stored string
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			stored = m.Content
		}
	}
	if stored != "看完了：答案就在这里。" {
		t.Fatalf("stored answer = %q, want only the surviving attempt", stored)
	}
	if got := mdl.attempts(); got != 2 {
		t.Fatalf("model attempts = %d, want 2", got)
	}
}
