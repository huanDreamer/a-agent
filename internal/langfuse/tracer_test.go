package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/chat"
)

// ingestRecorder captures ingestion payloads from a fake Langfuse.
type ingestRecorder struct {
	batches []map[string]any
}

// server starts a fake Langfuse returning 207 (as the real API does).
func (r *ingestRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != ingestPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.batches = append(r.batches, payload)
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// events flattens every captured event across batches.
func (r *ingestRecorder) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, b := range r.batches {
		batch, ok := b["batch"].([]any)
		if !ok {
			t.Fatalf("payload has no batch array: %+v", b)
		}
		for _, e := range batch {
			ev, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("batch entry is not an object: %+v", e)
			}
			out = append(out, ev)
		}
	}
	return out
}

// eventBody returns the body of the first event of a type.
func eventBody(t *testing.T, events []map[string]any, typ string) map[string]any {
	t.Helper()
	for _, e := range events {
		if e["type"] != typ {
			continue
		}
		body, ok := e["body"].(map[string]any)
		if !ok {
			t.Fatalf("event %s has no body object: %+v", typ, e)
		}
		return body
	}
	t.Fatalf("no %s event was ingested (got %d events)", typ, len(events))
	return nil
}

// newTestTracer wires a Tracer to a fake Langfuse.
func newTestTracer(t *testing.T, rec *ingestRecorder) (*Tracer, *Client) {
	t.Helper()
	srv := rec.server(t)
	client := New(Config{
		Host:        srv.URL,
		PublicKey:   "pk",
		SecretKey:   "sk",
		Environment: "test",
	}, nil)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return NewTracer(client), client
}

func TestTracer_DisabledIsInert(t *testing.T) {
	// A zero Tracer must not panic and must report itself disabled, because the
	// chat runner always calls through without checking.
	var tr Tracer
	ctx := context.Background()
	if tr.Enabled() {
		t.Error("a zero Tracer must be disabled")
	}
	if id := tr.StartTrace(ctx, chat.TraceInfo{}); id != "" {
		t.Errorf("StartTrace = %q, want empty", id)
	}
	if id := tr.StartSpan(ctx, chat.SpanInfo{}); id != "" {
		t.Errorf("StartSpan = %q, want empty", id)
	}
	if id := tr.StartGeneration(ctx, chat.GenInfo{}); id != "" {
		t.Errorf("StartGeneration = %q, want empty", id)
	}
	tr.EndTrace(ctx, "", nil)
	tr.EndSpan(ctx, "", nil, "")
	tr.EndGeneration(ctx, "", nil, chat.Usage{}, "")
	if got := tr.Stats(); got.Enabled {
		t.Errorf("Stats = %+v, want disabled", got)
	}
	if got := tr.Host(); got != "" {
		t.Errorf("Host = %q, want empty", got)
	}
	if _, err := tr.ListTraces(ctx, TraceFilter{}); err == nil {
		t.Error("ListTraces on a zero Tracer should report disabled")
	}
	if _, err := tr.GetTrace(ctx, "x"); err == nil {
		t.Error("GetTrace on a zero Tracer should report disabled")
	}
}

func TestTracer_ReportsTraceGenerationsAndSpans(t *testing.T) {
	rec := &ingestRecorder{}
	tr, client := newTestTracer(t, rec)
	if !tr.Enabled() {
		t.Fatal("the tracer should be enabled")
	}

	ctx := context.Background()
	traceID := tr.StartTrace(ctx, chat.TraceInfo{
		Name: "chat.turn", SessionID: "sess-1", UserID: "admin", Input: "hello",
	})
	if traceID == "" {
		t.Fatal("StartTrace returned an empty id")
	}

	genID := tr.StartGeneration(ctx, chat.GenInfo{
		TraceID: traceID, Name: "step-1", Model: "deepseek-chat", Input: "hello", Step: 1,
	})
	if genID == "" {
		t.Fatal("StartGeneration returned an empty id")
	}
	tr.EndGeneration(ctx, genID, "the answer", chat.Usage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}, "")

	spanID := tr.StartSpan(ctx, chat.SpanInfo{
		TraceID: traceID, Name: "tool.time", Input: map[string]any{"a": 1},
	})
	if spanID == "" {
		t.Fatal("StartSpan returned an empty id")
	}
	tr.EndSpan(ctx, spanID, "12:00", "")

	tr.EndTrace(ctx, traceID, "the answer")

	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := rec.events(t)
	if len(events) != 6 {
		t.Fatalf("ingested %d events, want 6 (trace, gen create/update, span create/update, trace end)", len(events))
	}

	// The trace must carry the session and user, so Langfuse can group a
	// conversation and attribute usage.
	trace := eventBody(t, events, eventTraceCreate)
	if trace["id"] != traceID {
		t.Errorf("trace id = %v, want %v", trace["id"], traceID)
	}
	if trace["sessionId"] != "sess-1" {
		t.Errorf("sessionId = %v, want sess-1", trace["sessionId"])
	}
	if trace["userId"] != "admin" {
		t.Errorf("userId = %v, want admin", trace["userId"])
	}

	// The model is declared on creation; the update carries the result.
	genCreate := eventBody(t, events, eventGenerationCreate)
	if genCreate["model"] != "deepseek-chat" {
		t.Errorf("generation model = %v, want deepseek-chat", genCreate["model"])
	}
	if genCreate["traceId"] != traceID {
		t.Errorf("generation traceId = %v, want %v", genCreate["traceId"], traceID)
	}

	gen := eventBody(t, events, eventGenerationUpdate)
	usage, ok := gen["usage"].(map[string]any)
	if !ok {
		t.Fatalf("generation update has no usage: %+v", gen)
	}
	if usage["input"] != float64(10) || usage["output"] != float64(5) || usage["total"] != float64(15) {
		t.Errorf("usage = %+v, want 10/5/15", usage)
	}

	span := eventBody(t, events, eventSpanUpdate)
	if span["output"] != "12:00" {
		t.Errorf("span output = %v, want the tool result", span["output"])
	}
}

func TestTracer_MarksFailures(t *testing.T) {
	rec := &ingestRecorder{}
	tr, client := newTestTracer(t, rec)

	ctx := context.Background()
	traceID := tr.StartTrace(ctx, chat.TraceInfo{Name: "chat.turn"})

	// A failed tool call must be visible as an error in the trace.
	spanID := tr.StartSpan(ctx, chat.SpanInfo{TraceID: traceID, Name: "tool.bad"})
	tr.EndSpan(ctx, spanID, nil, "boom")

	genID := tr.StartGeneration(ctx, chat.GenInfo{TraceID: traceID, Name: "step-1", Model: "m"})
	tr.EndGeneration(ctx, genID, nil, chat.Usage{}, "model failed")

	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := rec.events(t)
	span := eventBody(t, events, eventSpanUpdate)
	if span["level"] != "ERROR" {
		t.Errorf("span level = %v, want ERROR", span["level"])
	}
	if span["statusMessage"] != "boom" {
		t.Errorf("span statusMessage = %v, want boom", span["statusMessage"])
	}
	gen := eventBody(t, events, eventGenerationUpdate)
	if gen["level"] != "ERROR" || gen["statusMessage"] != "model failed" {
		t.Errorf("generation error not reported: %+v", gen)
	}
}

func TestTracer_SkipsObservationsWithoutATrace(t *testing.T) {
	rec := &ingestRecorder{}
	tr, client := newTestTracer(t, rec)

	ctx := context.Background()
	// Without a trace id there is nothing to attach to, so nothing is sent.
	if id := tr.StartSpan(ctx, chat.SpanInfo{Name: "orphan"}); id != "" {
		t.Errorf("StartSpan without a trace = %q, want empty", id)
	}
	if id := tr.StartGeneration(ctx, chat.GenInfo{Name: "orphan"}); id != "" {
		t.Errorf("StartGeneration without a trace = %q, want empty", id)
	}
	// Ending an unknown observation is a no-op.
	tr.EndSpan(ctx, "", nil, "")
	tr.EndGeneration(ctx, "", nil, chat.Usage{}, "")
	tr.EndTrace(ctx, "", nil)

	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(rec.events(t)); n != 0 {
		t.Errorf("ingested %d events, want 0 for orphan observations", n)
	}
}

func TestTracer_HostAndStats(t *testing.T) {
	rec := &ingestRecorder{}
	tr, client := newTestTracer(t, rec)

	if tr.Host() == "" {
		t.Error("Host should expose the configured base URL for deep links")
	}
	tr.StartTrace(context.Background(), chat.TraceInfo{Name: "t"})
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stats := tr.Stats()
	if !stats.Enabled {
		t.Error("Stats should report enabled")
	}
	if stats.Sent == 0 {
		t.Errorf("Stats = %+v, want a non-zero sent count after a flush", stats)
	}
}

func TestTracer_ReadsProxyToTheClient(t *testing.T) {
	// A fake Langfuse that answers the read endpoints.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == tracesPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"t1","name":"chat.turn","userId":"admin"}]}`))
		case len(req.URL.Path) > len(tracesPath)+1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"t1","name":"chat.turn","observations":[{"id":"o1","type":"SPAN"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := New(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk"}, nil)
	tr := NewTracer(client)

	traces, err := tr.ListTraces(context.Background(), TraceFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(traces) != 1 || traces[0].ID != "t1" {
		t.Errorf("traces = %+v, want one entry", traces)
	}
	if traces[0].UserID != "admin" {
		t.Errorf("userId did not map onto UserID: %+v", traces[0])
	}

	detail, err := tr.GetTrace(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if detail == nil || len(detail.Observations) != 1 {
		t.Errorf("detail = %+v, want one observation", detail)
	}
}

func TestTracer_CloseFlushesRemaining(t *testing.T) {
	rec := &ingestRecorder{}
	tr, client := newTestTracer(t, rec)

	// Enqueue one event and close immediately: Close must flush it rather than
	// dropping it, or the last turn of a session would never be traced.
	tr.StartTrace(context.Background(), chat.TraceInfo{Name: "last"})

	done := make(chan error, 1)
	go func() { done <- client.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	if n := len(rec.events(t)); n != 1 {
		t.Errorf("ingested %d events, want 1 flushed by Close", n)
	}
}
