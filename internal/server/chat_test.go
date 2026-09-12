package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/tool"
)

// scriptedModel answers with a fixed sequence of streamed turns, so the SSE
// path can be exercised without an LLM.
type scriptedModel struct {
	turns [][]*schema.Message
	calls int
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	idx := m.calls
	m.calls++
	if idx >= len(m.turns) {
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "done"}}), nil
	}
	return schema.StreamReaderFromArray(m.turns[idx]), nil
}

// newChatHarness builds a server with the chat endpoints wired to a scripted
// model, plus a real tool registry.
func newChatHarness(t *testing.T, turns [][]*schema.Message, tools *tool.Registry) *harness {
	t.Helper()

	mdl := &scriptedModel{turns: turns}
	runner, err := chat.New(chat.Config{
		Model:    mdl,
		Tools:    tools,
		MaxSteps: 4,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{
		seed: nil,
		chat: ChatDeps{
			Runner: runner,
			Tools:  tools,
		},
	})
	startHarness(t, srv)
	return &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}
}

// sseEvent is one decoded SSE data frame.
type sseEvent map[string]any

// readSSE performs the streaming POST and returns every decoded event.
func readSSE(t *testing.T, client *http.Client, url string, body any) []sseEvent {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("stream status = %d, want 200 (body: %s)", resp.StatusCode, buf.String())
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	events := make([]sseEvent, 0, 16)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue // frame separator or heartbeat comment
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", line, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return events
}

// eventTypes lists the type field of each event, in order.
func eventTypes(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		s, _ := e["type"].(string)
		out = append(out, s)
	}
	return out
}

// findEvent returns the first event of a type.
func findEvent(events []sseEvent, typ string) (sseEvent, bool) {
	for _, e := range events {
		if s, _ := e["type"].(string); s == typ {
			return e, true
		}
	}
	return nil, false
}

func TestChatSessions_CRUD(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)

	// Create.
	var created struct {
		Session struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"session"`
	}
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "我的对话"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	decode(t, resp, &created)
	if created.Session.ID == "" {
		t.Fatal("session id is empty")
	}
	if created.Session.Title != "我的对话" {
		t.Errorf("title = %q", created.Session.Title)
	}

	// List.
	var list struct {
		Sessions []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			MessageCount int    `json:"message_count"`
		} `json:"sessions"`
	}
	h.getJSON(t, "/api/chat/sessions", http.StatusOK, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.Session.ID {
		t.Fatalf("list = %+v, want the created session", list.Sessions)
	}

	// Fetch one.
	var got struct {
		Session  map[string]any   `json:"session"`
		Messages []map[string]any `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+created.Session.ID, http.StatusOK, &got)
	if got.Session["id"] != created.Session.ID {
		t.Errorf("session = %+v", got.Session)
	}
	if got.Messages == nil {
		t.Error("messages should be an empty array, not null")
	}

	// Rename.
	var renamed struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
	}
	resp = h.patchJSON(t, "/api/chat/sessions/"+created.Session.ID, map[string]string{"title": "改名了"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", resp.StatusCode)
	}
	decode(t, resp, &renamed)
	if renamed.Session.Title != "改名了" {
		t.Errorf("title after patch = %q", renamed.Session.Title)
	}

	// Delete.
	resp = h.deleteJSON(t, "/api/chat/sessions/"+created.Session.ID)
	requireStatus(t, resp, http.StatusOK)
	h.getJSON(t, "/api/chat/sessions/"+created.Session.ID, http.StatusNotFound, nil)
}

func TestChatSessions_MissingIsNotFound(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)

	h.getJSON(t, "/api/chat/sessions/nope", http.StatusNotFound, nil)
	requireStatus(t, h.patchJSON(t, "/api/chat/sessions/nope", map[string]string{"title": "x"}), http.StatusNotFound)
	requireStatus(t, h.deleteJSON(t, "/api/chat/sessions/nope"), http.StatusNotFound)
	requireStatus(t, h.postJSON(t, "/api/chat/sessions/nope/clear", nil), http.StatusNotFound)
}

func TestChatSessions_RequireAuth(t *testing.T) {
	h := newChatHarness(t, nil, nil) // not logged in
	for _, p := range []string{"/api/chat/sessions", "/api/chat/models"} {
		resp := h.get(t, p)
		requireStatus(t, resp, http.StatusUnauthorized)
	}
}

func TestChatModels_Listing(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)

	var got struct {
		Models       []map[string]any `json:"models"`
		Tools        []string         `json:"tools"`
		SystemPrompt string           `json:"system_prompt"`
		MaxSteps     int              `json:"max_steps"`
	}
	h.getJSON(t, "/api/chat/models", http.StatusOK, &got)
	if got.Models == nil || got.Tools == nil {
		t.Error("models and tools must be arrays, not null")
	}
	if got.SystemPrompt == "" {
		t.Error("system_prompt should carry the effective prompt")
	}
	if got.MaxSteps <= 0 {
		t.Errorf("max_steps = %d, want > 0", got.MaxSteps)
	}
}

func TestChatStream_PlainAnswer(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{
		{Role: schema.Assistant, Content: "你好，"},
		{Role: schema.Assistant, Content: "我是助手"},
	}}, nil)
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "你好"})

	types := eventTypes(events)
	if len(types) == 0 {
		t.Fatal("no events were streamed")
	}
	if types[0] != "step_start" {
		t.Errorf("first event = %q, want step_start", types[0])
	}
	if types[len(types)-1] != "stream_end" {
		t.Errorf("last event = %q, want stream_end (the client relies on it)", types[len(types)-1])
	}
	if _, ok := findEvent(events, "text_delta"); !ok {
		t.Error("no text_delta was streamed")
	}
	done, ok := findEvent(events, "done")
	if !ok {
		t.Fatal("no done event")
	}
	if done["text"] != "你好，我是助手" {
		t.Errorf("done text = %v, want the assembled answer", done["text"])
	}

	// The turn must be persisted so a reload shows it.
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("persisted %d messages, want 2 (user + assistant)", len(got.Messages))
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "你好" {
		t.Errorf("stored user message = %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "assistant" || got.Messages[1].Content != "你好，我是助手" {
		t.Errorf("stored assistant message = %+v", got.Messages[1])
	}
}

func TestChatStream_ReasoningIsSeparate(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{
		{Role: schema.Assistant, ReasoningContent: "先想想", Content: "答案"},
	}}, nil)
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "问题"})

	reasoning, ok := findEvent(events, "reasoning_delta")
	if !ok {
		t.Fatal("no reasoning_delta event; the UI cannot show the thinking process")
	}
	if reasoning["text"] != "先想想" {
		t.Errorf("reasoning text = %v", reasoning["text"])
	}

	// Reasoning must be persisted separately from the answer.
	var got struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	asst := got.Messages[1]
	if asst.Reasoning != "先想想" {
		t.Errorf("stored reasoning = %q, want 先想想", asst.Reasoning)
	}
	if strings.Contains(asst.Content, "先想想") {
		t.Errorf("reasoning leaked into the stored answer: %q", asst.Content)
	}
}

func TestChatStream_ToolCallAndResult(t *testing.T) {
	// The tool runs on the request goroutine while the test asserts from this
	// one, so the flag must be synchronised.
	var ran atomic.Bool
	tl := &stubTool{name: "clock", desc: "tells the time", run: func(context.Context, string) (string, error) {
		ran.Store(true)
		return "12:00", nil
	}}
	reg := tool.NewRegistry()
	if err := reg.Register(tl); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	h := newChatHarness(t, [][]*schema.Message{
		{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "clock", Arguments: `{"tz":"UTC"}`},
		}}}},
		{{Role: schema.Assistant, Content: "现在是 12:00"}},
	}, reg)
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "几点了"})

	if !ran.Load() {
		t.Error("the tool was never executed")
	}

	call, ok := findEvent(events, "tool_call")
	if !ok {
		t.Fatal("no tool_call event; the UI cannot show tool usage")
	}
	if call["tool_name"] != "clock" || call["tool_call_id"] != "c1" {
		t.Errorf("tool_call = %+v", call)
	}
	if call["tool_args"] != `{"tz":"UTC"}` {
		t.Errorf("tool args = %v", call["tool_args"])
	}

	result, ok := findEvent(events, "tool_result")
	if !ok {
		t.Fatal("no tool_result event")
	}
	if result["tool_result"] != "12:00" || result["tool_call_id"] != "c1" {
		t.Errorf("tool_result = %+v", result)
	}

	// Ordering matters for rendering.
	types := eventTypes(events)
	ci, ri := -1, -1
	for i, typ := range types {
		if typ == "tool_call" && ci < 0 {
			ci = i
		}
		if typ == "tool_result" && ri < 0 {
			ri = i
		}
	}
	if ci < 0 || ri < 0 || ci > ri {
		t.Errorf("tool_call at %d, tool_result at %d; the call must come first", ci, ri)
	}

	// The tool run must be recorded in the audit log.
	var audit struct {
		Rows []struct {
			ToolName   string `json:"tool_name"`
			UserID     string `json:"user_id"`
			Result     string `json:"result"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/audit", http.StatusOK, &audit)
	if len(audit.Rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.Rows))
	}
	if audit.Rows[0].ToolName != "clock" || audit.Rows[0].Result != "12:00" {
		t.Errorf("audit row = %+v", audit.Rows[0])
	}

	// And the tool runs must be persisted with the message so a reload shows them.
	var got struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls string `json:"tool_calls"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	stored := got.Messages[1].ToolCalls
	if !strings.Contains(stored, "clock") {
		t.Errorf("stored tool_calls = %q, want it to record the tool", stored)
	}
}

func TestChatStream_FailingToolIsReported(t *testing.T) {
	tl := &stubTool{name: "flaky", desc: "fails", run: func(context.Context, string) (string, error) {
		return "", errFake
	}}
	reg := tool.NewRegistry()
	if err := reg.Register(tl); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	h := newChatHarness(t, [][]*schema.Message{
		{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "flaky", Arguments: "{}"},
		}}}},
		{{Role: schema.Assistant, Content: "工具失败了"}},
	}, reg)
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "go"})

	result, ok := findEvent(events, "tool_result")
	if !ok {
		t.Fatal("no tool_result event")
	}
	if result["tool_error"] == "" {
		t.Error("a failing tool must carry tool_error so the UI can show the failure")
	}
	// The turn still completes.
	if done, ok := findEvent(events, "done"); !ok || done["text"] != "工具失败了" {
		t.Errorf("done = %+v, want the model's follow-up answer", done)
	}

	// The failure is recorded in the audit log too.
	var audit struct {
		Rows []struct {
			ToolName string `json:"tool_name"`
			Err      string `json:"err"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/audit", http.StatusOK, &audit)
	if len(audit.Rows) != 1 || audit.Rows[0].Err == "" {
		t.Errorf("audit row = %+v, want the error recorded", audit.Rows)
	}
}

func TestChatStream_UsageEvent(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{
		{Role: schema.Assistant, Content: "ok", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
		}},
	}}, nil)
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "x"})

	usage, ok := findEvent(events, "usage")
	if !ok {
		t.Fatal("no usage event; the UI cannot show token cost")
	}
	u, ok := usage["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage payload = %+v", usage["usage"])
	}
	if u["total_tokens"] != float64(15) {
		t.Errorf("total_tokens = %v, want 15", u["total_tokens"])
	}

	// Usage is persisted with the assistant message.
	var got struct {
		Messages []struct {
			Usage string `json:"usage"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 || !strings.Contains(got.Messages[1].Usage, "15") {
		t.Errorf("stored usage = %+v, want it to record 15 tokens", got.Messages)
	}
}

func TestChatStream_Validation(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)
	id := createSession(t, h)

	t.Run("empty content", func(t *testing.T) {
		resp := h.postJSON(t, "/api/chat/sessions/"+id+"/messages", map[string]string{"content": "   "})
		requireStatus(t, resp, http.StatusBadRequest)
	})
	t.Run("unknown session", func(t *testing.T) {
		resp := h.postJSON(t, "/api/chat/sessions/nope/messages", map[string]string{"content": "hi"})
		requireStatus(t, resp, http.StatusNotFound)
	})
}

func TestChatStream_HistoryCarriesSystemPrompt(t *testing.T) {
	// A second turn must replay the system prompt and the prior conversation,
	// otherwise the assistant forgets the exchange.
	mdl := &recordingModel{}
	runner, err := chat.New(chat.Config{Model: mdl, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	id := createSession(t, h)

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages", map[string]string{"content": "first"})
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages", map[string]string{"content": "second"})

	mdl.mu.Lock()
	defer mdl.mu.Unlock()
	if len(mdl.seen) < 2 {
		t.Fatalf("model called %d times, want 2", len(mdl.seen))
	}
	second := mdl.seen[len(mdl.seen)-1]
	if second[0].Role != schema.System {
		t.Errorf("first message role = %v, want system", second[0].Role)
	}
	var sawUser, sawPriorAssistant bool
	for _, m := range second {
		if m.Role == schema.User && m.Content == "first" {
			sawUser = true
		}
		if m.Role == schema.Assistant && strings.Contains(m.Content, "echo:") {
			sawPriorAssistant = true
		}
	}
	if !sawUser {
		t.Error("the previous user message was not replayed")
	}
	if !sawPriorAssistant {
		t.Error("the previous assistant answer was not replayed")
	}
}

func TestChatStream_ModelSwitchIsUsed(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)
	id := createSession(t, h)

	// Switching the model must be persisted, so the next turn uses it.
	resp := h.patchJSON(t, "/api/chat/sessions/"+id, map[string]string{"model": "other-model"})
	requireStatus(t, resp, http.StatusOK)

	var got struct {
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if got.Session.Model != "other-model" {
		t.Errorf("model = %q, want other-model", got.Session.Model)
	}
}

func TestChatClear_KeepsSession(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}, nil)
	h.login(t)
	id := createSession(t, h)
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages", map[string]string{"content": "x"})

	requireStatus(t, h.postJSON(t, "/api/chat/sessions/"+id+"/clear", nil), http.StatusOK)

	var got struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		Messages []any `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if got.Session.ID != id {
		t.Error("clearing must keep the session")
	}
	if len(got.Messages) != 0 {
		t.Errorf("messages = %d, want 0 after clearing", len(got.Messages))
	}
}

func TestChatSession_AutoTitle(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}, nil)
	h.login(t)

	// Create without a title, then send a message: the session should be named
	// after the first user message so the sidebar is useful.
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{})
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, resp, &created)

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+created.Session.ID+"/messages",
		map[string]string{"content": "帮我写一个 Go 程序"})

	var got struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
	}
	h.getJSON(t, "/api/chat/sessions/"+created.Session.ID, http.StatusOK, &got)
	if !strings.Contains(got.Session.Title, "帮我写") {
		t.Errorf("title = %q, want it derived from the first message", got.Session.Title)
	}
}

// createSession creates a session and returns its id.
func createSession(t *testing.T, h *harness) string {
	t.Helper()
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session status = %d", resp.StatusCode)
	}
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Session.ID == "" {
		t.Fatal("created session has no id")
	}
	return created.Session.ID
}

var errFake = errors.New("boom")

// recordingModel records the messages it is asked to complete.
type recordingModel struct {
	mu   sync.Mutex
	seen [][]*schema.Message
}

func (m *recordingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *recordingModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (m *recordingModel) Stream(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	m.seen = append(m.seen, append([]*schema.Message(nil), msgs...))
	n := len(m.seen)
	m.mu.Unlock()
	return schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: "echo:" + strconv.Itoa(n)},
	}), nil
}

func TestChatSession_UserTitleIsNeverOverwritten(t *testing.T) {
	// A user may deliberately name a session "新对话". That is the same text the
	// UI uses as a placeholder, so the server must not treat it as a sentinel and
	// clobber the user's choice on the next turn.
	h := newChatHarness(t, [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}, nil)
	h.login(t)
	id := createSession(t, h)

	const chosen = "新对话"
	resp := h.patchJSON(t, "/api/chat/sessions/"+id, map[string]string{"title": chosen})
	requireStatus(t, resp, http.StatusOK)

	// A full turn runs; the title must survive it.
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "这是一条很长的新消息应该被忽略掉"})

	var got struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if got.Session.Title != chosen {
		t.Errorf("title = %q, want the user's %q preserved", got.Session.Title, chosen)
	}
}

func TestChatSession_UntitledIsEmptyAndAutoNamed(t *testing.T) {
	h := newChatHarness(t, [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}, nil)
	h.login(t)

	// A brand-new session has an empty title (the UI supplies the placeholder).
	id := createSession(t, h)
	var created struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &created)
	if created.Session.Title != "" {
		t.Errorf("new session title = %q, want empty (the UI renders the placeholder)", created.Session.Title)
	}

	// The first turn names it from the message.
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "帮我排查一个 bug"})

	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &created)
	if !strings.Contains(created.Session.Title, "帮我排查") {
		t.Errorf("title = %q, want it derived from the first message", created.Session.Title)
	}
}

// recordingUsage captures what the chat reports to the usage recorder.
type recordingUsage struct {
	mu     sync.Mutex
	events []UsageEvent
	err    error
}

func (r *recordingUsage) Record(e UsageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func (r *recordingUsage) snapshot() []UsageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UsageEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestChatTurn_RecordsUsage(t *testing.T) {
	// The web chat is the surface almost every turn goes through, so a turn that
	// is not recorded leaves 统计监控 empty for the conversations users actually
	// have — and an empty dashboard reads as broken, not as unused.
	rec := &recordingUsage{}
	mdl := &scriptedModel{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "hi", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
		}},
	}}}
	runner, err := chat.New(chat.Config{Model: mdl, MaxSteps: 3, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, _ := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner, Usage: rec}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv}
	h.login(t)

	id := createSession(t, h)
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages", map[string]string{"content": "hi"})

	waitForCondition(t, 2*time.Second, "a recorded usage event", func() bool {
		return len(rec.snapshot()) > 0
	})
	got := rec.snapshot()[0]
	if got.PromptTokens != 30 || got.CompletionTokens != 12 || got.TotalTokens != 42 {
		t.Errorf("usage = %+v, want 30/12/42", got)
	}
	if got.SessionID != id {
		t.Errorf("session = %q, want %q", got.SessionID, id)
	}
	// Attribution matters: a row with no model cannot be priced or grouped.
	if got.Model == "" {
		t.Error("the recorded usage has no model, so it cannot be attributed or priced")
	}
}

func TestChatTurn_NoUsageReportedRecordsNothing(t *testing.T) {
	// A provider that reports no tokens must not produce a zero row: it would
	// inflate the call count while adding no information.
	rec := &recordingUsage{}
	mdl := &scriptedModel{turns: [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}}
	runner, err := chat.New(chat.Config{Model: mdl, MaxSteps: 3, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, _ := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner, Usage: rec}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv}
	h.login(t)

	id := createSession(t, h)
	readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages", map[string]string{"content": "hi"})
	time.Sleep(200 * time.Millisecond)
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("recorded %d events, want 0 when the provider reported no usage", n)
	}
}

func TestChatTurn_NilRecorderIsSafe(t *testing.T) {
	// A deployment without a recorder must still chat.
	mdl := &scriptedModel{turns: [][]*schema.Message{{{Role: schema.Assistant, Content: "hi"}}}}
	runner, err := chat.New(chat.Config{Model: mdl, MaxSteps: 3, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, _ := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv}
	h.login(t)

	id := createSession(t, h)
	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "hi"})
	if _, ok := findEvent(events, "done"); !ok {
		t.Error("the turn should still complete without a recorder")
	}
}
