package tracing

import (
	"context"
	"testing"

	"github.com/huan/huan-agent/internal/chat"
)

// fakeTracer records every call it receives. `mints` controls whether it behaves
// like an enabled backend (returns a generated id) or a disabled one (returns
// nothing), which is the difference the fan-out has to cope with.
type fakeTracer struct {
	mints bool
	// traceIDs, spanIDs and genIDs are the ids this sink was asked to start, and
	// obsTraceIDs the parent trace each observation was filed under.
	traceIDs []string
	spanIDs  []string
	genIDs   []string
	// ended records the ids passed to the End* methods.
	ended []string

	obsTraceIDs []string
}

func (f *fakeTracer) Enabled() bool { return f.mints }

func (f *fakeTracer) StartTrace(_ context.Context, info chat.TraceInfo) string {
	f.traceIDs = append(f.traceIDs, info.ID)
	if !f.mints {
		return ""
	}
	if info.ID != "" {
		return info.ID
	}
	return "minted-trace"
}

func (f *fakeTracer) EndTrace(_ context.Context, traceID string, _ any) {
	f.ended = append(f.ended, traceID)
}

func (f *fakeTracer) StartSpan(_ context.Context, info chat.SpanInfo) string {
	f.obsTraceIDs = append(f.obsTraceIDs, info.TraceID)
	f.spanIDs = append(f.spanIDs, info.ID)
	if !f.mints {
		return ""
	}
	if info.ID != "" {
		return info.ID
	}
	return "minted-span"
}

func (f *fakeTracer) EndSpan(_ context.Context, spanID string, _ any, _ string) {
	f.ended = append(f.ended, spanID)
}

func (f *fakeTracer) StartGeneration(_ context.Context, info chat.GenInfo) string {
	f.obsTraceIDs = append(f.obsTraceIDs, info.TraceID)
	f.genIDs = append(f.genIDs, info.ID)
	if !f.mints {
		return ""
	}
	if info.ID != "" {
		return info.ID
	}
	return "minted-gen"
}

func (f *fakeTracer) EndGeneration(_ context.Context, genID string, _ any, _ chat.Usage, _ string) {
	f.ended = append(f.ended, genID)
}

// The fan-out exists so the built-in store is the source of truth while a mirror
// still receives the same turn. That only works if every backend agrees on the
// ids, because the conversation reports observations by the id the fan-out
// returned and knows nothing about which sink produced it.
func TestMultiSharesOneIDAcrossSinks(t *testing.T) {
	primary := &fakeTracer{mints: true}
	mirror := &fakeTracer{mints: true}
	multi := Multi{primary, mirror}
	ctx := context.Background()

	traceID := multi.StartTrace(ctx, chat.TraceInfo{Name: "chat.turn"})
	if traceID == "" {
		t.Fatal("a fan-out with an enabled sink must return an id")
	}

	genID := multi.StartGeneration(ctx, chat.GenInfo{TraceID: traceID, Name: "step"})
	spanID := multi.StartSpan(ctx, chat.SpanInfo{TraceID: traceID, Name: "tool"})
	multi.EndGeneration(ctx, genID, "out", chat.Usage{TotalTokens: 3}, "")
	multi.EndSpan(ctx, spanID, "out", "")
	multi.EndTrace(ctx, traceID, "out")

	// The first sink mints; the second must be told to use the same id rather
	// than inventing one, or its observations land under a trace nobody reads.
	if primary.traceIDs[0] != "" {
		t.Errorf("the first sink was given %q, want nothing so it can mint", primary.traceIDs[0])
	}
	if mirror.traceIDs[0] != traceID {
		t.Errorf("the mirror was given trace %q, want %q", mirror.traceIDs[0], traceID)
	}
	if mirror.genIDs[0] != genID {
		t.Errorf("the mirror was given generation %q, want %q", mirror.genIDs[0], genID)
	}
	if mirror.spanIDs[0] != spanID {
		t.Errorf("the mirror was given span %q, want %q", mirror.spanIDs[0], spanID)
	}
	// Both sinks must have been told the same parent trace.
	for i, got := range mirror.obsTraceIDs {
		if got != traceID {
			t.Errorf("mirror observation %d trace = %q, want %q", i, got, traceID)
		}
	}
	// And both must have been closed with the shared ids.
	for _, want := range []string{traceID, genID, spanID} {
		found := false
		for _, got := range mirror.ended {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the mirror was never told to close %q (ended: %v)", want, mirror.ended)
		}
	}
}

// A disabled first sink must not leave the whole fan-out id-less: the next sink
// that actually records mints the id instead.
func TestMultiMintsFromTheFirstLiveSink(t *testing.T) {
	dead := &fakeTracer{mints: false}
	live := &fakeTracer{mints: true}
	multi := Multi{dead, live}

	traceID := multi.StartTrace(context.Background(), chat.TraceInfo{Name: "chat.turn"})
	if traceID != "minted-trace" {
		t.Errorf("trace id = %q, want the live sink's id", traceID)
	}
	if len(live.traceIDs) != 1 {
		t.Fatalf("the live sink saw %d starts, want 1", len(live.traceIDs))
	}
}

func TestMultiIsInertWhenNothingRecords(t *testing.T) {
	multi := Multi{&fakeTracer{mints: false}, nil}
	ctx := context.Background()

	if multi.Enabled() {
		t.Error("a fan-out of disabled sinks must not report enabled")
	}
	if id := multi.StartTrace(ctx, chat.TraceInfo{Name: "x"}); id != "" {
		t.Errorf("StartTrace = %q, want empty", id)
	}
	// Nil sinks and empty ids must be survivable: the conversation calls through
	// without checking.
	multi.EndTrace(ctx, "", nil)
	multi.EndSpan(ctx, "", nil, "")
	multi.EndGeneration(ctx, "", nil, chat.Usage{}, "")

	var empty Multi
	if empty.Enabled() {
		t.Error("an empty fan-out must not report enabled")
	}
	if id := empty.StartTrace(ctx, chat.TraceInfo{Name: "x"}); id != "" {
		t.Errorf("empty fan-out StartTrace = %q, want empty", id)
	}
}

func TestMultiEnabledReflectsAnyLiveSink(t *testing.T) {
	if !(Multi{&fakeTracer{mints: false}, &fakeTracer{mints: true}}).Enabled() {
		t.Error("one live sink is enough for the fan-out to be enabled")
	}
	if (Multi{nil, &fakeTracer{mints: false}}).Enabled() {
		t.Error("no live sink means the fan-out is not enabled")
	}
}
