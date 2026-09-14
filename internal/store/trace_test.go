package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// base is a fixed instant so ordering assertions are not racing the clock.
var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func TestRecordAndGetTrace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{
		ID: "t1", Name: "chat.turn", SessionID: "s1", UserID: "u1",
		Input: `"hello"`, StartedAt: base,
	}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.RecordObservation(ctx, ObservationRow{
		ID: "o1", TraceID: "t1", Type: "GENERATION", Name: "chat.step",
		Model: "m1", Step: 1, Input: `{"q":"hi"}`, StartedAt: base.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("RecordObservation: %v", err)
	}
	if err := s.EndObservation(ctx, "o1", ObservationEnd{
		Output: `"answer"`, PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14,
		EndedAt: base.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("EndObservation: %v", err)
	}
	if err := s.EndTrace(ctx, "t1", `"answer"`, base.Add(time.Second)); err != nil {
		t.Fatalf("EndTrace: %v", err)
	}

	trace, obs, err := s.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if trace.Name != "chat.turn" || trace.SessionID != "s1" || trace.UserID != "u1" {
		t.Errorf("trace = %+v", trace)
	}
	if trace.Running() {
		t.Error("trace still reports running after EndTrace")
	}
	if got := trace.EndedAt.Sub(trace.StartedAt); got != time.Second {
		t.Errorf("latency = %v, want 1s", got)
	}
	if trace.Output != `"answer"` {
		t.Errorf("output = %s", trace.Output)
	}

	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1", len(obs))
	}
	o := obs[0]
	if o.Type != "GENERATION" || o.Model != "m1" || o.Step != 1 {
		t.Errorf("observation = %+v", o)
	}
	if o.PromptTokens != 10 || o.CompletionTokens != 4 || o.TotalTokens != 14 {
		t.Errorf("usage = %d/%d/%d", o.PromptTokens, o.CompletionTokens, o.TotalTokens)
	}
	if o.ParentID != "t1" {
		t.Errorf("parent = %q, want the trace id when none was given", o.ParentID)
	}
	if o.EndedAt.IsZero() {
		t.Error("observation has no end time after EndObservation")
	}
}

// A trace that is still running must be readable with its observations open,
// which is what lets the panel show a turn in progress rather than nothing.
func TestRunningTraceIsReadable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{ID: "t1", StartedAt: base}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.RecordObservation(ctx, ObservationRow{
		ID: "o1", TraceID: "t1", Type: "SPAN", Name: "search", StartedAt: base,
	}); err != nil {
		t.Fatalf("RecordObservation: %v", err)
	}

	trace, obs, err := s.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if !trace.Running() {
		t.Error("an unfinished trace must report running")
	}
	if len(obs) != 1 || !obs[0].EndedAt.IsZero() {
		t.Errorf("observations = %+v, want one with no end time", obs)
	}
}

func TestListTracesOrdersNewestFirstAndFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, row := range []TraceRow{
		{ID: "old", Name: "chat.turn", SessionID: "s1", UserID: "u1", StartedAt: base},
		{ID: "new", Name: "chat.turn", SessionID: "s2", UserID: "u2", StartedAt: base.Add(time.Hour)},
		{ID: "mid", Name: "tool.search", SessionID: "s1", UserID: "u1", StartedAt: base.Add(time.Minute)},
	} {
		if err := s.RecordTrace(ctx, row); err != nil {
			t.Fatalf("RecordTrace: %v", err)
		}
	}

	all, err := s.ListTraces(ctx, TraceFilter{})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d traces, want 3", len(all))
	}
	if all[0].ID != "new" || all[1].ID != "mid" || all[2].ID != "old" {
		t.Errorf("order = %s, %s, %s; want new, mid, old", all[0].ID, all[1].ID, all[2].ID)
	}

	bySession, err := s.ListTraces(ctx, TraceFilter{SessionID: "s1"})
	if err != nil {
		t.Fatalf("ListTraces session: %v", err)
	}
	if len(bySession) != 2 {
		t.Errorf("session filter returned %d, want 2", len(bySession))
	}

	byUser, err := s.ListTraces(ctx, TraceFilter{UserID: "u2"})
	if err != nil {
		t.Fatalf("ListTraces user: %v", err)
	}
	if len(byUser) != 1 || byUser[0].ID != "new" {
		t.Errorf("user filter = %+v", byUser)
	}

	// Name is a substring match: the panel's name box is a search field, not an
	// exact lookup.
	byName, err := s.ListTraces(ctx, TraceFilter{Name: "search"})
	if err != nil {
		t.Fatalf("ListTraces name: %v", err)
	}
	if len(byName) != 1 || byName[0].ID != "mid" {
		t.Errorf("name filter = %+v", byName)
	}

	paged, err := s.ListTraces(ctx, TraceFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("ListTraces paged: %v", err)
	}
	if len(paged) != 1 || paged[0].ID != "mid" {
		t.Errorf("second page = %+v, want mid", paged)
	}
}

func TestListTracesCapsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordTrace(ctx, TraceRow{ID: "t1", StartedAt: base}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	// An absurd limit is clamped rather than rejected, so a hand-written request
	// cannot ask for the whole table.
	if _, err := s.ListTraces(ctx, TraceFilter{Limit: 100000}); err != nil {
		t.Fatalf("ListTraces with a huge limit: %v", err)
	}
	if _, err := s.ListTraces(ctx, TraceFilter{Limit: -5, Offset: -3}); err != nil {
		t.Fatalf("ListTraces with negative paging: %v", err)
	}
}

func TestTraceWriteValidationAndMissingRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{StartedAt: base}); err == nil {
		t.Error("RecordTrace without an id = nil, want an error")
	}
	if err := s.RecordObservation(ctx, ObservationRow{ID: "o1", StartedAt: base}); err == nil {
		t.Error("RecordObservation without a trace id = nil, want an error")
	}
	if err := s.RecordObservation(ctx, ObservationRow{TraceID: "t1", StartedAt: base}); err == nil {
		t.Error("RecordObservation without an id = nil, want an error")
	}

	// Closing something that was never written must say so rather than look like
	// a success: the recorder counts it as a failure instead of silently
	// believing the trace is complete.
	if err := s.EndTrace(ctx, "missing", "out", base); !errors.Is(err, ErrNotFound) {
		t.Errorf("EndTrace on a missing row = %v, want ErrNotFound", err)
	}
	if err := s.EndObservation(ctx, "missing", ObservationEnd{EndedAt: base}); !errors.Is(err, ErrNotFound) {
		t.Errorf("EndObservation on a missing row = %v, want ErrNotFound", err)
	}

	if _, _, err := s.GetTrace(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTrace on a missing row = %v, want ErrNotFound", err)
	}
}

// Ending a trace with nothing to say must not blank what was recorded, and must
// never overwrite the start time — a bug this project has already hit once.
func TestEndTraceIsAdditive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{ID: "t1", Input: `"q"`, Output: "partial", StartedAt: base}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.EndTrace(ctx, "t1", "", base.Add(time.Second)); err != nil {
		t.Fatalf("EndTrace: %v", err)
	}

	trace, _, err := s.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if trace.Output != "partial" {
		t.Errorf("output = %q, want the recorded value kept", trace.Output)
	}
	if !trace.StartedAt.Equal(base) {
		t.Errorf("started_at = %v, want %v", trace.StartedAt, base)
	}
}

func TestObservationParentNests(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{ID: "t1", StartedAt: base}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.RecordObservation(ctx, ObservationRow{
		ID: "child", TraceID: "t1", ParentID: "parent", StartedAt: base,
	}); err != nil {
		t.Fatalf("RecordObservation: %v", err)
	}

	_, obs, err := s.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(obs) != 1 || obs[0].ParentID != "parent" {
		t.Errorf("observations = %+v, want the explicit parent preserved", obs)
	}
}

// Deleting a trace must take its observations with it, so pruning cannot leave
// orphaned nodes behind.
func TestDeletingTraceCascadesObservations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordTrace(ctx, TraceRow{ID: "t1", StartedAt: base}); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if err := s.RecordObservation(ctx, ObservationRow{ID: "o1", TraceID: "t1", StartedAt: base}); err != nil {
		t.Fatalf("RecordObservation: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, "DELETE FROM traces WHERE id = ?", "t1"); err != nil {
		t.Fatalf("delete trace: %v", err)
	}

	var count int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM observations").Scan(&count); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if count != 0 {
		t.Errorf("%d observations survived their trace", count)
	}
}

// A message written before this migration has no trace, and must say so with an
// empty id rather than a value that resolves to nothing.
func TestChatMessageCarriesTraceID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateChatSession(ctx, ChatSession{ID: "s1", Title: "t"}); err != nil {
		t.Fatalf("CreateChatSession: %v", err)
	}
	if _, err := s.AppendChatMessage(ctx, "s1", ChatMessage{
		Role: RoleAssistant, Content: "hi", TraceID: "trace-abc",
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if _, err := s.AppendChatMessage(ctx, "s1", ChatMessage{
		Role: RoleUser, Content: "q",
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}

	msgs, err := s.ListChatMessages(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].TraceID != "trace-abc" {
		t.Errorf("assistant trace id = %q, want trace-abc", msgs[0].TraceID)
	}
	if msgs[1].TraceID != "" {
		t.Errorf("user trace id = %q, want empty", msgs[1].TraceID)
	}
}
