package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/langfuse"
	"github.com/huan/huan-agent/internal/store"
)

// newStore opens a real store: this package's whole job is the shape it writes
// and reads back, so a fake would only test the fake.
func newStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// recordingTurn drives the Recorder the way chat.Runner does: one trace, one
// model call and one tool call, all closed.
func recordingTurn(t *testing.T, r *Recorder, session, user string) string {
	t.Helper()
	ctx := context.Background()

	traceID := r.StartTrace(ctx, chat.TraceInfo{
		Name: "chat.turn", SessionID: session, UserID: user, Input: "what is 2+2?",
	})
	if traceID == "" {
		t.Fatal("StartTrace returned no id")
	}
	genID := r.StartGeneration(ctx, chat.GenInfo{
		TraceID: traceID, Name: "chat.step", Model: "m1", Step: 1,
		Input: map[string]any{"messages": 3},
	})
	r.EndGeneration(ctx, genID, "let me check", chat.Usage{
		PromptTokens: 120, CompletionTokens: 8, TotalTokens: 128,
	}, "")

	spanID := r.StartSpan(ctx, chat.SpanInfo{
		TraceID: traceID, Name: "calc", Input: map[string]any{"expression": "2+2"},
	})
	r.EndSpan(ctx, spanID, "4", "")

	r.EndTrace(ctx, traceID, "4")
	return traceID
}

func TestRecorderRecordsAReadableTurn(t *testing.T) {
	rec := NewRecorder(newStore(t), nil)
	if !rec.Enabled() {
		t.Fatal("a recorder over a real store must be enabled")
	}
	traceID := recordingTurn(t, rec, "s1", "u1")

	detail, err := rec.GetTrace(context.Background(), traceID)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if detail.Name != "chat.turn" || detail.SessionID != "s1" || detail.UserID != "u1" {
		t.Errorf("trace = %+v", detail.TraceSummary)
	}
	if detail.Latency <= 0 {
		t.Errorf("latency = %v, want a positive duration", detail.Latency)
	}
	// The input was recorded as a bare string; it must come back as a valid JSON
	// document the panel can render.
	var question string
	if err := json.Unmarshal(detail.Input, &question); err != nil || question != "what is 2+2?" {
		t.Errorf("input = %s (err %v)", detail.Input, err)
	}

	if len(detail.Observations) != 2 {
		t.Fatalf("got %d observations, want 2", len(detail.Observations))
	}
	for _, o := range detail.Observations {
		// Every node must nest under the trace, or the waterfall drops it.
		if o.ParentID != traceID {
			t.Errorf("observation %s parent = %q, want the trace id", o.ID, o.ParentID)
		}
		if o.EndTime == "" || o.Latency <= 0 {
			t.Errorf("observation %s is not closed: end=%q latency=%v", o.ID, o.EndTime, o.Latency)
		}
	}

	var gen, span *langfuse.Observation
	for i := range detail.Observations {
		switch detail.Observations[i].Type {
		case "GENERATION":
			gen = &detail.Observations[i]
		case "SPAN":
			span = &detail.Observations[i]
		}
	}
	if gen == nil || span == nil {
		t.Fatalf("want one GENERATION and one SPAN, got %+v", detail.Observations)
	}
	if gen.Model != "m1" {
		t.Errorf("generation model = %q, want m1", gen.Model)
	}
	if gen.Usage == nil || gen.Usage.Input != 120 || gen.Usage.Output != 8 || gen.Usage.Total != 128 {
		t.Errorf("generation usage = %+v, want the token counts", gen.Usage)
	}
	// A span carries no tokens, and reporting a usage object of zeros would draw
	// a meaningless token badge on a tool call.
	if span.Usage != nil {
		t.Errorf("span usage = %+v, want none", span.Usage)
	}
}

// The whole point of the trace id on a message is that it resolves. If it stops
// resolving, the console must be able to say why.
func TestGetTraceMissingIsNotFound(t *testing.T) {
	rec := NewRecorder(newStore(t), nil)

	if _, err := rec.GetTrace(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTrace on an unknown id = %v, want store.ErrNotFound", err)
	}
	if _, err := rec.GetTrace(context.Background(), "  "); err == nil {
		t.Error("GetTrace with a blank id = nil, want an error")
	}
}

func TestRecorderListTracesFiltersAndPages(t *testing.T) {
	rec := NewRecorder(newStore(t), nil)
	ctx := context.Background()

	for _, s := range []struct{ session, user string }{
		{"s1", "u1"}, {"s1", "u1"}, {"s2", "u2"},
	} {
		recordingTurn(t, rec, s.session, s.user)
	}

	all, err := rec.ListTraces(ctx, langfuse.TraceFilter{})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d traces, want 3", len(all))
	}
	if all[0].Timestamp == "" {
		t.Error("a listed trace must carry a timestamp for the list to sort and render")
	}

	bySession, err := rec.ListTraces(ctx, langfuse.TraceFilter{SessionID: "s1"})
	if err != nil {
		t.Fatalf("ListTraces session: %v", err)
	}
	if len(bySession) != 2 {
		t.Errorf("session filter returned %d, want 2", len(bySession))
	}

	// Page 2 of 2 asks for the second trace; paging is what the 上一页/下一页
	// buttons do, and an off-by-one here shows the same row twice.
	page2, err := rec.ListTraces(ctx, langfuse.TraceFilter{Limit: 2, Page: 2})
	if err != nil {
		t.Fatalf("ListTraces page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Errorf("page 2 returned %d traces, want 1", len(page2))
	}
}

// A failed tool or model call must be visible as a failure: a waterfall that
// renders an error as a success is worse than one that shows nothing.
func TestRecorderMarksFailures(t *testing.T) {
	rec := NewRecorder(newStore(t), nil)
	ctx := context.Background()

	traceID := rec.StartTrace(ctx, chat.TraceInfo{Name: "chat.turn"})
	spanID := rec.StartSpan(ctx, chat.SpanInfo{TraceID: traceID, Name: "shell"})
	rec.EndSpan(ctx, spanID, nil, "exit status 1")
	genID := rec.StartGeneration(ctx, chat.GenInfo{TraceID: traceID, Name: "chat.step", Model: "m"})
	rec.EndGeneration(ctx, genID, nil, chat.Usage{}, "provider refused the request")
	rec.EndTrace(ctx, traceID, "")

	detail, err := rec.GetTrace(ctx, traceID)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(detail.Observations) != 2 {
		t.Fatalf("got %d observations, want 2", len(detail.Observations))
	}
	for _, o := range detail.Observations {
		if o.Level != "ERROR" {
			t.Errorf("observation %s level = %q, want ERROR", o.ID, o.Level)
		}
		if strings.TrimSpace(o.StatusMessage) == "" {
			t.Errorf("observation %s has no status message", o.ID)
		}
	}
}

// A failing store must never surface as a failure of the conversation, and the
// trace id must still be handed back so the turn is not silently untraced.
func TestRecorderFailsSoft(t *testing.T) {
	broken := &failingStore{err: errors.New("database is locked")}
	rec := NewRecorder(broken, nil)
	ctx := context.Background()

	traceID := rec.StartTrace(ctx, chat.TraceInfo{Name: "chat.turn"})
	if traceID == "" {
		t.Fatal("StartTrace must still return an id when the write failed")
	}
	spanID := rec.StartSpan(ctx, chat.SpanInfo{TraceID: traceID, Name: "tool"})
	// None of these may panic, block, or return an error to the caller.
	rec.EndSpan(ctx, spanID, "out", "")
	rec.EndGeneration(ctx, traceID, "out", chat.Usage{TotalTokens: 1}, "")
	rec.EndTrace(ctx, traceID, "out")

	stats := rec.Stats()
	if stats.Failed == 0 {
		t.Error("failed writes must be counted so an operator can see them")
	}
	if !stats.Enabled {
		t.Error("the recorder is wired to a store, so it must report enabled")
	}
}

// A disabled recorder is what an unwired deployment gets: it must be inert
// rather than a source of nil-pointer panics on the chat path.
func TestRecorderDisabledIsInert(t *testing.T) {
	var nilRec *Recorder
	if nilRec.Enabled() {
		t.Error("a nil recorder must not report enabled")
	}
	if id := nilRec.StartTrace(context.Background(), chat.TraceInfo{Name: "x"}); id != "" {
		t.Errorf("nil recorder StartTrace = %q, want empty", id)
	}
	nilRec.EndTrace(context.Background(), "t", "out")
	nilRec.EndSpan(context.Background(), "s", "out", "")
	if got := nilRec.Stats(); got.Enabled || got.Sent != 0 {
		t.Errorf("nil recorder Stats = %+v, want the zero value", got)
	}
	if _, err := nilRec.ListTraces(context.Background(), langfuse.TraceFilter{}); !errors.Is(err, langfuse.ErrDisabled) {
		t.Errorf("nil recorder ListTraces = %v, want ErrDisabled", err)
	}

	// Built over nothing, which is how a caller wires tracing optimistically.
	empty := NewRecorder(nil, nil)
	if empty.Enabled() {
		t.Error("a recorder with no store must not report enabled")
	}
	if id := empty.StartTrace(context.Background(), chat.TraceInfo{Name: "x"}); id != "" {
		t.Errorf("storess recorder StartTrace = %q, want empty", id)
	}
}

// A RawMessage is emitted verbatim, so a stored value that is not valid JSON
// would corrupt the entire trace response and leave the panel unable to parse
// anything. Whatever is in the column, the answer must be valid JSON.
func TestUnparseableStoredPayloadStaysValidJSON(t *testing.T) {
	s := newStore(t)
	rec := NewRecorder(s, nil)
	ctx := context.Background()

	// Written directly, the way a hand-edited or half-written row would be.
	if err := s.RecordTrace(ctx, store.TraceRow{
		ID: "t1", Name: "chat.turn", Input: "{not json", Output: "plain text",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.RecordObservation(ctx, store.ObservationRow{
		ID: "o1", TraceID: "t1", Type: "SPAN", Name: "t", Input: "also not json",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordObservation: %v", err)
	}

	detail, err := rec.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	// The whole response must serialise, which is the property that matters.
	blob, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if !json.Valid(blob) {
		t.Fatalf("trace detail serialised to invalid JSON: %s", blob)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("the response could not be read back: %v", err)
	}

	list, err := rec.ListTraces(ctx, langfuse.TraceFilter{})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d traces, want 1", len(list))
	}
	if _, err := json.Marshal(list); err != nil {
		t.Fatalf("the list could not be serialised: %v", err)
	}
}

func TestRecorderClipsLongLabels(t *testing.T) {
	rec := NewRecorder(newStore(t), nil)
	long := strings.Repeat("汉", maxNameLen+50)

	id := rec.StartTrace(context.Background(), chat.TraceInfo{Name: long})
	detail, err := rec.GetTrace(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if runes := []rune(detail.Name); len(runes) != maxNameLen {
		t.Errorf("clipped name has %d runes, want %d", len(runes), maxNameLen)
	}
	// The clip must not split a multi-byte character into an invalid string.
	if !strings.Contains(detail.Name, "汉") {
		t.Error("the clipped name is not valid UTF-8 text")
	}
}

func TestHostIsEmptyForTheBuiltInStore(t *testing.T) {
	// No external UI means no deep link; the console keys the link off this.
	if got := NewRecorder(newStore(t), nil).Host(); got != "" {
		t.Errorf("Host = %q, want empty", got)
	}
}

// failingStore rejects every write, standing in for a locked database.
type failingStore struct{ err error }

func (f *failingStore) RecordTrace(context.Context, store.TraceRow) error { return f.err }
func (f *failingStore) RecordObservation(context.Context, store.ObservationRow) error {
	return f.err
}
func (f *failingStore) EndTrace(context.Context, string, string, time.Time) error { return f.err }
func (f *failingStore) EndObservation(context.Context, string, store.ObservationEnd) error {
	return f.err
}
func (f *failingStore) ListTraces(context.Context, store.TraceFilter) ([]store.TraceRow, error) {
	return nil, f.err
}
func (f *failingStore) GetTrace(context.Context, string) (store.TraceRow, []store.ObservationRow, error) {
	return store.TraceRow{}, nil, f.err
}
