package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// askQuestion is the question every test in this file puts to the user, so an
// assertion about a label is also an assertion about the card's contents.
func askQuestion() tool.Question {
	return tool.Question{
		Text:   "新服务用哪个数据库？",
		Header: "数据库选型",
		Options: []tool.Option{
			{Label: "Postgres", Description: "已有实例"},
			{Label: "SQLite"},
		},
		AllowCustom: true,
	}
}

// askRecorder collects what a turnAsker emitted, and remembers the id of the
// question it announced — which is what a test answers, exactly as the browser
// does.
type askRecorder struct {
	mu     sync.Mutex
	events []chat.Event
	asked  string
}

func (r *askRecorder) emit(e chat.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if e.Ask != nil {
		r.asked = e.Ask.ID
	}
}

func (r *askRecorder) snapshot() []chat.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chat.Event(nil), r.events...)
}

func (r *askRecorder) announced() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

// collectAsker builds a turnAsker whose emitted events land in the recorder.
func collectAsker(hub *questionHub, timeout time.Duration) (*turnAsker, *askRecorder) {
	rec := &askRecorder{}
	asker := &turnAsker{
		hub:     hub,
		session: "sess-1",
		timeout: timeout,
		logger:  zap.NewNop(),
		emit:    rec.emit,
	}
	return asker, rec
}

func TestQuestionHub_ResolveDeliversAnAnswerOnce(t *testing.T) {
	hub := newQuestionHub()
	q := askQuestion()
	q.ID = "q1"
	pending := hub.register("sess-1", q)
	if hub.count() != 1 {
		t.Fatalf("pending count = %d, want 1", hub.count())
	}

	want := tool.Answer{Status: tool.AnswerAnswered, Selected: []string{"Postgres"}}
	if err := hub.resolve("q1", "sess-1", want); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	select {
	case got := <-pending.answers:
		if got.Status != want.Status || got.Selected[0] != "Postgres" {
			t.Errorf("delivered %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the answer never reached the waiting tool")
	}

	// The question is consumed: a second submission is refused rather than
	// overwriting an answer the model may already be reading.
	if err := hub.resolve("q1", "sess-1", tool.Answer{Status: tool.AnswerAnswered, Text: "again"}); err == nil {
		t.Error("a second answer was accepted for the same question")
	}
	if hub.count() != 0 {
		t.Errorf("pending count = %d, want 0 after being answered", hub.count())
	}
}

func TestQuestionHub_RefusesAnotherSessionsQuestion(t *testing.T) {
	hub := newQuestionHub()
	q := askQuestion()
	q.ID = "q1"
	hub.register("sess-1", q)

	// The id is the handle, but the session is the authority: answering another
	// conversation's question would inject text into a turn the caller cannot see.
	if _, ok := hub.peek("q1", "sess-2"); ok {
		t.Error("another session could read the question")
	}
	if err := hub.resolve("q1", "sess-2", tool.Answer{Status: tool.AnswerAnswered, Text: "hijack"}); err == nil {
		t.Error("another session could answer the question")
	}
	if hub.count() != 1 {
		t.Errorf("pending count = %d, want the question still waiting", hub.count())
	}
}

func TestQuestionHub_ForgetMakesALateAnswerStale(t *testing.T) {
	hub := newQuestionHub()
	q := askQuestion()
	q.ID = "q1"
	hub.register("sess-1", q)
	hub.forget("q1")

	if err := hub.resolve("q1", "sess-1", tool.Answer{Status: tool.AnswerAnswered, Text: "too late"}); err == nil {
		t.Error("a question nobody waits on accepted an answer")
	}
}

func TestTurnAsker_AnnouncesThenSettlesWithTheAnswer(t *testing.T) {
	hub := newQuestionHub()
	asker, rec := collectAsker(hub, time.Minute)

	// A browser on the other end: wait for the announcement, then submit against
	// the id it carried.
	submitted := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if id := rec.announced(); id != "" {
				submitted <- hub.resolve(id, "sess-1", tool.Answer{
					Status: tool.AnswerAnswered, Selected: []string{"SQLite"},
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
		submitted <- errors.New("the question was never announced")
	}()

	answer, err := asker.Ask(context.Background(), askQuestion())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := <-submitted; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !answer.Answered() || answer.Selected[0] != "SQLite" {
		t.Fatalf("answer = %+v, want SQLite", answer)
	}

	evs := rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("emitted %d events, want 2 (announcement + outcome): %+v", len(evs), evs)
	}
	if evs[0].Type != chat.EventAsk || evs[0].Ask == nil || evs[0].AskStatus != chat.AskPending {
		t.Errorf("first event = %+v, want a pending question", evs[0])
	}
	if evs[0].Ask.ID == "" {
		t.Error("the announced question has no id, so nobody could answer it")
	}
	if len(evs[0].Ask.Options) != 2 {
		t.Errorf("announced options = %+v, want the two offered", evs[0].Ask.Options)
	}
	if evs[1].Type != chat.EventAsk || evs[1].AskID != evs[0].Ask.ID {
		t.Errorf("second event = %+v, want an outcome for %q", evs[1], evs[0].Ask.ID)
	}
	if evs[1].AskStatus != string(tool.AnswerAnswered) {
		t.Errorf("outcome status = %q, want answered", evs[1].AskStatus)
	}
	if evs[1].AskAnswer == nil || evs[1].AskAnswer.Selected[0] != "SQLite" {
		t.Errorf("outcome answer = %+v, want the submitted selection", evs[1].AskAnswer)
	}
	if hub.count() != 0 {
		t.Errorf("pending count = %d, want 0 once answered", hub.count())
	}
}

func TestTurnAsker_TimesOutWithoutAnAnswer(t *testing.T) {
	hub := newQuestionHub()
	asker, rec := collectAsker(hub, 20*time.Millisecond)

	started := time.Now()
	answer, err := asker.Ask(context.Background(), askQuestion())
	if err != nil {
		t.Fatalf("a timeout must not be an error: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Errorf("returned after %s, want it to have waited", elapsed)
	}
	if answer.Status != tool.AnswerTimeout {
		t.Errorf("status = %q, want timeout", answer.Status)
	}
	if answer.Answered() {
		t.Error("a timeout reported itself as answered")
	}

	evs := rec.snapshot()
	if len(evs) != 2 || evs[1].AskStatus != string(tool.AnswerTimeout) {
		t.Fatalf("events = %+v, want an announcement then a timeout outcome", evs)
	}
	if evs[1].AskAnswer != nil {
		t.Error("a timeout carried an answer object, which would look like a person chose nothing")
	}
	if hub.count() != 0 {
		t.Errorf("pending count = %d, want the question dropped after the timeout", hub.count())
	}
}

func TestTurnAsker_CancelledTurnEndsTheWait(t *testing.T) {
	hub := newQuestionHub()
	// No timeout: only the context can end this wait, which is the case a stop
	// button or a closed tab produces.
	asker, rec := collectAsker(hub, 0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	answer, err := asker.Ask(ctx, askQuestion())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer.Status != tool.AnswerCancelled {
		t.Errorf("status = %q, want cancelled", answer.Status)
	}
	evs := rec.snapshot()
	if len(evs) != 2 || evs[1].AskStatus != string(tool.AnswerCancelled) {
		t.Fatalf("events = %+v, want an announcement then a cancelled outcome", evs)
	}
	if hub.count() != 0 {
		t.Errorf("pending count = %d, want 0 after cancellation", hub.count())
	}
}

func TestNewQuestionID_IsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := newQuestionID()
		if err != nil {
			t.Fatalf("newQuestionID: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("id = %q, want 32 hex characters", id)
		}
		if seen[id] {
			t.Fatalf("id %q was handed out twice", id)
		}
		seen[id] = true
	}
}

// newAskHarness builds a server whose chat runs on a scripted model, with a
// configurable ask_user wait limit.
func newAskHarness(t *testing.T, turns [][]*schema.Message, tools *tool.Registry, askTimeout time.Duration) *harness {
	t.Helper()

	runner, err := chat.New(chat.Config{
		Model:    &scriptedModel{turns: turns},
		Tools:    tools,
		MaxSteps: 4,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Runner:     runner,
		Tools:      tools,
		AskTimeout: askTimeout,
	}})
	startHarness(t, srv)
	return &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
}

// askRegistry is a tool registry holding only ask_user: the model under test
// calls exactly that.
func askRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	tl, err := builtin.NewAskUserTool()
	if err != nil {
		t.Fatalf("NewAskUserTool: %v", err)
	}
	if err := reg.Register(tool.WithCapability(tl, tool.CapRead)); err != nil {
		t.Fatalf("register ask_user: %v", err)
	}
	return reg
}

// askTurn is the model's first step: it asks the user which database to use and
// stops there (the tool call ends the step).
func askTurn() []*schema.Message {
	return []*schema.Message{{
		Role:    schema.Assistant,
		Content: "我需要确认一下再动手。",
		ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name: builtin.AskUserToolName,
				Arguments: `{"question":"新服务用哪个数据库？","header":"数据库选型",` +
					`"options":[{"label":"Postgres","description":"已有实例"},{"label":"SQLite"}]}`,
			},
		}},
	}}
}

// TestChatStream_AskUserCardAnswersMidTurn is the whole feature end to end: the
// model asks, the card goes out on the stream, the answer comes back on a second
// request, and the *same* turn finishes with the model's final answer.
func TestChatStream_AskUserCardAnswersMidTurn(t *testing.T) {
	h := newAskHarness(t, [][]*schema.Message{
		askTurn(),
		{{Role: schema.Assistant, Content: "按你的选择，用 Postgres。"}},
	}, askRegistry(t), 5*time.Second)
	h.login(t)
	sessionID := createSession(t, h)

	payload := strings.NewReader(`{"content":"帮我搭个新服务"}`)
	req, err := http.NewRequest(http.MethodPost, h.base+"/api/chat/sessions/"+sessionID+"/messages", payload)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	// Read the stream the way the browser does: act on the card the moment it
	// arrives, while the response is still open.
	events := make([]sseEvent, 0, 16)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	submitted := ""
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode frame %q: %v", line, err)
		}
		events = append(events, ev)

		if ev["type"] != "ask_user" {
			continue
		}
		ask, _ := ev["ask"].(map[string]any)
		qid, _ := ask["id"].(string)
		if qid == "" || submitted != "" {
			continue
		}
		submitted = qid
		// A second request, on a second connection, exactly like the card.
		answerResp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/questions/"+qid+"/answer",
			map[string]any{"selected": []string{"Postgres"}})
		defer func() { _ = answerResp.Body.Close() }()
		if answerResp.StatusCode != http.StatusOK {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(answerResp.Body)
			t.Fatalf("answer status = %d, want 200 (body: %s)", answerResp.StatusCode, buf.String())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if submitted == "" {
		t.Fatalf("no ask_user announcement arrived; events: %v", eventTypes(events))
	}

	types := eventTypes(events)
	for _, want := range []string{"tool_call", "ask_user", "tool_result", "text_delta", "done"} {
		if !contains(types, want) {
			t.Errorf("event types %v are missing %q", types, want)
		}
	}
	if idxAsk, idxResult := indexOf(types, "ask_user"), indexOf(types, "tool_result"); idxAsk > idxResult {
		t.Errorf("the card arrived after the tool result (%v)", types)
	}

	// The outcome event closes the card, and the tool result handed the answer to
	// the model — both are what the UI relies on.
	settled, ok := lastAskEvent(events)
	if !ok || settled["ask_status"] != "answered" {
		t.Errorf("outcome event = %v, want ask_status answered", settled)
	}
	result, ok := findEvent(events, "tool_result")
	if !ok {
		t.Fatal("no tool_result event")
	}
	if body, _ := result["tool_result"].(string); !strings.Contains(body, "Postgres") {
		t.Errorf("tool result %q does not carry the user's choice", body)
	}

	// The turn finished rather than stopping at the question.
	done, _ := findEvent(events, "done")
	if text, _ := done["text"].(string); !strings.Contains(text, "用 Postgres") {
		t.Errorf("final answer = %q, want the model's second step", text)
	}

	// And the whole exchange is persisted: a reload shows what was asked and what
	// was answered.
	var stored struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls string `json:"tool_calls"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+sessionID, http.StatusOK, &stored)
	var found bool
	for _, msg := range stored.Messages {
		if msg.Role != "assistant" || !strings.Contains(msg.ToolCalls, builtin.AskUserToolName) {
			continue
		}
		found = true
		if !strings.Contains(msg.ToolCalls, "Postgres") {
			t.Errorf("persisted tool calls %q do not contain the answer", msg.ToolCalls)
		}
		if !strings.Contains(msg.ToolCalls, "新服务用哪个数据库") {
			t.Errorf("persisted tool calls %q do not contain the question", msg.ToolCalls)
		}
	}
	if !found {
		t.Errorf("no persisted assistant message carries the ask_user call: %+v", stored.Messages)
	}
	if h.srv.questions.count() != 0 {
		t.Errorf("questions still pending after the turn: %d", h.srv.questions.count())
	}
}

// TestChatStream_AskUserTimeoutLetsTheModelCarryOn checks the path a forgotten
// card takes: nobody answers, and the turn still finishes with the model's own
// judgement rather than hanging.
func TestChatStream_AskUserTimeoutLetsTheModelCarryOn(t *testing.T) {
	h := newAskHarness(t, [][]*schema.Message{
		askTurn(),
		{{Role: schema.Assistant, Content: "没等到回答，我按 Postgres 继续。"}},
	}, askRegistry(t), 30*time.Millisecond)
	h.login(t)
	sessionID := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "帮我搭个新服务"})

	settled, ok := lastAskEvent(events)
	if !ok || settled["ask_status"] != "timeout" {
		t.Fatalf("outcome event = %v, want ask_status timeout", settled)
	}
	done, _ := findEvent(events, "done")
	if text, _ := done["text"].(string); !strings.Contains(text, "Postgres") {
		t.Errorf("final answer = %q, want the model to have carried on", text)
	}
	if h.srv.questions.count() != 0 {
		t.Errorf("questions still pending after the timeout: %d", h.srv.questions.count())
	}
}

// TestHandleAnswerQuestion_ValidatesAgainstTheCard covers the refusals. Each one
// protects the model from being handed an "answer" no person could have given.
func TestHandleAnswerQuestion_ValidatesAgainstTheCard(t *testing.T) {
	h := newAskHarness(t, nil, nil, time.Minute)
	h.login(t)
	sessionID := createSession(t, h)

	// Plant a waiting question directly: this test is about the handler, and a
	// real turn would only make it slower.
	plant := func(t *testing.T, q tool.Question) string {
		t.Helper()
		id, err := newQuestionID()
		if err != nil {
			t.Fatalf("newQuestionID: %v", err)
		}
		q.ID = id
		h.srv.questions.register(sessionID, q)
		t.Cleanup(func() { h.srv.questions.forget(id) })
		return id
	}

	multi := askQuestion()
	multi.MultiSelect = true
	locked := askQuestion()
	locked.AllowCustom = false

	tests := []struct {
		name     string
		question tool.Question
		body     map[string]any
		status   int
		want     string
	}{
		{
			name:     "nothing chosen",
			question: askQuestion(),
			body:     map[string]any{"selected": []string{}, "text": "  "},
			status:   http.StatusBadRequest,
			want:     "请选择一个选项",
		},
		{
			name:     "option the card never offered",
			question: askQuestion(),
			body:     map[string]any{"selected": []string{"MySQL"}},
			status:   http.StatusBadRequest,
			want:     "不在这个问题里",
		},
		{
			name:     "two options on a single-choice question",
			question: askQuestion(),
			body:     map[string]any{"selected": []string{"Postgres", "SQLite"}},
			status:   http.StatusBadRequest,
			want:     "只能选一个",
		},
		{
			name:     "custom text where the card forbids it",
			question: locked,
			body:     map[string]any{"text": "我想用 MySQL"},
			status:   http.StatusBadRequest,
			want:     "不能自己输入",
		},
		{
			name:     "two options where multi-select was allowed",
			question: multi,
			body:     map[string]any{"selected": []string{"Postgres", "SQLite"}},
			status:   http.StatusOK,
		},
		{
			name:     "custom text alongside an option",
			question: askQuestion(),
			body:     map[string]any{"selected": []string{"Postgres"}, "text": "先在测试环境"},
			status:   http.StatusOK,
		},
		{
			name:     "custom text alone",
			question: askQuestion(),
			body:     map[string]any{"text": "用 MySQL"},
			status:   http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := plant(t, tc.question)
			resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/questions/"+id+"/answer", tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				var buf bytes.Buffer
				_, _ = buf.ReadFrom(resp.Body)
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tc.status, buf.String())
			}
			if tc.want == "" {
				return
			}
			var out struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if !strings.Contains(out.Error, tc.want) {
				t.Errorf("error = %q, want it to mention %q", out.Error, tc.want)
			}
		})
	}
}

func TestHandleAnswerQuestion_UnknownOrForeignQuestionIsGone(t *testing.T) {
	h := newAskHarness(t, nil, nil, time.Minute)
	h.login(t)
	sessionID := createSession(t, h)
	otherID := createSession(t, h)

	q := askQuestion()
	q.ID = "planted"
	h.srv.questions.register(otherID, q)

	// The question exists, but belongs to another conversation.
	resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/questions/planted/answer",
		map[string]any{"selected": []string{"Postgres"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another session's question", resp.StatusCode)
	}
	var out struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out.Error, "不在等待中") {
		t.Errorf("error = %q, want an explanation the user can act on", out.Error)
	}

	// And an id that never existed is the same answer.
	resp2 := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/questions/nope/answer",
		map[string]any{"selected": []string{"Postgres"}})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown question", resp2.StatusCode)
	}
}

func TestHandleAnswerQuestion_RequiresAuth(t *testing.T) {
	h := newAskHarness(t, nil, nil, time.Minute)
	// No login: the route is behind the same session check as every other chat
	// endpoint, because answering can influence what the agent does next.
	resp := h.postJSON(t, "/api/chat/sessions/whatever/questions/q1/answer",
		map[string]any{"selected": []string{"A"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// lastAskEvent returns the most recent ask_user event, which is the outcome.
func lastAskEvent(events []sseEvent) (sseEvent, bool) {
	var found sseEvent
	ok := false
	for _, e := range events {
		if typ, _ := e["type"].(string); typ == "ask_user" {
			found, ok = e, true
		}
	}
	return found, ok
}

func contains(list []string, want string) bool { return indexOf(list, want) >= 0 }

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}
