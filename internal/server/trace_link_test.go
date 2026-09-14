package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tracing"
)

// newTracedChatHarness builds the chat endpoints over the built-in trace store,
// which is the wiring `huan-agent admin serve` uses: one store behind both the
// conversation and the trace panel.
func newTracedChatHarness(t *testing.T, turns [][]*schema.Message, tools *tool.Registry) (*harness, *tracing.Recorder) {
	t.Helper()

	// The store is opened here rather than by the harness, because the recorder
	// has to be built over it before the runner is: the runner is what reports
	// the turn, and it holds the tracer for its whole life.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "traced.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := tracing.NewRecorder(st, zap.NewNop())
	mdl := &scriptedModel{turns: turns}
	runner, err := chat.New(chat.Config{
		Model:    mdl,
		Tools:    tools,
		Tracer:   rec,
		MaxSteps: 4,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, _ := buildServerWith(t, buildOpts{
		st:     st,
		chat:   ChatDeps{Runner: runner, Tools: tools},
		tracer: rec,
		traces: rec,
	})
	startHarness(t, srv)
	return &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}, rec
}

// This is the chain the console's 链路 button depends on, end to end: a turn is
// answered, the answer stores the trace that produced it, and that id resolves
// through the trace API the panel reads. Any break in it turns the button into a
// link that leads nowhere.
func TestTurnAnswerLinksToItsTrace(t *testing.T) {
	turns := [][]*schema.Message{{{Role: schema.Assistant, Content: "四十二"}}}
	h, rec := newTracedChatHarness(t, turns, tool.NewRegistry())
	h.login(t)
	if !rec.Enabled() {
		t.Fatal("the recorder must be enabled: the built-in store needs no configuration")
	}

	// A conversation to run the turn in.
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "链路测试"})
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeBody(t, resp, &created)
	if created.Session.ID == "" {
		t.Fatal("no session id came back")
	}
	sessionID := created.Session.ID

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "六乘七等于几"})

	// 1. The persisted answer must carry the trace id, because that is what a
	//    reload reads and what the bubble renders its link from.
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			TraceID string `json:"trace_id"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+sessionID, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("persisted %d messages, want 2 (user + assistant)", len(got.Messages))
	}
	assistant := got.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("second message role = %q, want assistant", assistant.Role)
	}
	traceID := assistant.TraceID
	if traceID == "" {
		t.Fatal("the assistant message carries no trace id, so nothing can link to its trace")
	}
	if got.Messages[0].TraceID != "" {
		t.Errorf("the user message carries trace %q, want none", got.Messages[0].TraceID)
	}

	// 2. That id must resolve through the API the panel calls.
	var detail struct {
		Enabled bool `json:"enabled"`
		Trace   struct {
			ID           string `json:"id"`
			SessionID    string `json:"session_id"`
			Observations []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"observations"`
		} `json:"trace"`
	}
	h.getJSON(t, "/api/traces/"+traceID, http.StatusOK, &detail)
	if !detail.Enabled {
		t.Error("the trace endpoints report tracing as off")
	}
	if detail.Trace.ID != traceID {
		t.Errorf("trace id = %q, want %q", detail.Trace.ID, traceID)
	}
	if detail.Trace.SessionID != sessionID {
		t.Errorf("trace session = %q, want %q — the back-link needs it", detail.Trace.SessionID, sessionID)
	}
	if len(detail.Trace.Observations) == 0 {
		t.Fatal("the trace has no observations, so the waterfall would be empty")
	}
	var generation bool
	for _, o := range detail.Trace.Observations {
		if o.Type == "GENERATION" {
			generation = true
		}
		if o.ID == "" {
			t.Error("an observation has no id")
		}
	}
	if !generation {
		t.Error("the model call was not recorded as a GENERATION observation")
	}

	// 3. The session-level entry point: the conversation's traces come back when
	//    the panel is filtered by session.
	var list struct {
		Traces []struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		} `json:"traces"`
	}
	h.getJSON(t, "/api/traces?session="+sessionID, http.StatusOK, &list)
	if len(list.Traces) != 1 {
		t.Fatalf("session filter returned %d traces, want 1", len(list.Traces))
	}
	if list.Traces[0].ID != traceID {
		t.Errorf("listed trace = %q, want %q", list.Traces[0].ID, traceID)
	}
}

// A stale link is not an outage: the panel must be able to say the record is
// gone rather than reporting a broken backend.
func TestMissingTraceIsNotFoundNotOutage(t *testing.T) {
	h, _ := newTracedChatHarness(t, nil, tool.NewRegistry())
	h.login(t)

	// The status is the assertion: 404 (the record is gone) rather than 502
	// (the backend is broken). The body explains which.
	var body struct {
		Error string `json:"error"`
	}
	h.getJSON(t, "/api/traces/does-not-exist", http.StatusNotFound, &body)
	if body.Error == "" {
		t.Error("a missing trace must explain itself")
	}
}

// With tracing off the conversation still works; it just records no link.
func TestTurnWithoutTracerStoresNoLink(t *testing.T) {
	mdl := &scriptedModel{turns: [][]*schema.Message{{{Role: schema.Assistant, Content: "ok"}}}}
	runner, err := chat.New(chat.Config{Model: mdl, Tools: tool.NewRegistry(), MaxSteps: 2, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, _ := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv}
	h.login(t)

	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "无追踪"})
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeBody(t, resp, &created)

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+created.Session.ID+"/messages",
		map[string]string{"content": "hi"})

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			TraceID string `json:"trace_id"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+created.Session.ID, http.StatusOK, &got)
	for _, m := range got.Messages {
		if m.TraceID != "" {
			t.Errorf("message role %q carries trace %q with tracing off", m.Role, m.TraceID)
		}
	}
}

// decodeBody reads a JSON response body, failing the test on a decode error.
func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}
