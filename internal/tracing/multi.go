package tracing

import (
	"context"

	"github.com/huan/huan-agent/internal/chat"
)

// Multi reports one conversation to several trace backends at once.
//
// It exists so the built-in store can be the source of truth while an external
// backend still receives the same turns: the console reads locally, and a
// configured Langfuse keeps collecting what its evaluations and prompt
// management need.
//
// The backends must agree on ids, and that is the whole subtlety here. The
// conversation reports an observation by the id StartTrace/StartSpan/
// StartGeneration returned, and it knows nothing about which backend produced
// it — so if each sink minted its own, every observation after the first would
// be filed under a trace that only one backend had created. This fan-out
// therefore pins the id from the first sink that actually records something and
// hands it to the rest (see chat.TraceInfo.ID, which each tracer must honour).
//
// A nil sink is skipped, so a caller can pass an unwired backend without a
// guard, and an empty Multi is inert.
type Multi []chat.Tracer

// compile-time check that the fan-out is a conversation tracer.
var _ chat.Tracer = (Multi)(nil)

// Enabled reports whether any sink is recording.
//
// chat.Tracer does not declare Enabled, so the sinks that have one are found by
// assertion. The server's own tracing contract asks for it, and "something will
// record this turn" is the honest answer for a fan-out: true as soon as one
// backend is configured, false when none is.
func (m Multi) Enabled() bool {
	for _, t := range m {
		if e, ok := t.(interface{ Enabled() bool }); ok && e.Enabled() {
			return true
		}
	}
	return false
}

// StartTrace opens a turn on every sink and returns the shared id.
func (m Multi) StartTrace(ctx context.Context, info chat.TraceInfo) string {
	id := ""
	for _, t := range m {
		if t == nil {
			continue
		}
		// Empty for the first sink that records (it mints the id), pinned for
		// every sink after it.
		info.ID = id
		if got := t.StartTrace(ctx, info); id == "" {
			id = got
		}
	}
	return id
}

// EndTrace closes the turn on every sink.
func (m Multi) EndTrace(ctx context.Context, traceID string, output any) {
	for _, t := range m {
		if t == nil {
			continue
		}
		t.EndTrace(ctx, traceID, output)
	}
}

// StartSpan opens a tool call on every sink and returns the shared id.
func (m Multi) StartSpan(ctx context.Context, info chat.SpanInfo) string {
	id := ""
	for _, t := range m {
		if t == nil {
			continue
		}
		info.ID = id
		if got := t.StartSpan(ctx, info); id == "" {
			id = got
		}
	}
	return id
}

// EndSpan closes the tool call on every sink.
func (m Multi) EndSpan(ctx context.Context, spanID string, output any, errMsg string) {
	for _, t := range m {
		if t == nil {
			continue
		}
		t.EndSpan(ctx, spanID, output, errMsg)
	}
}

// StartGeneration opens a model call on every sink and returns the shared id.
func (m Multi) StartGeneration(ctx context.Context, info chat.GenInfo) string {
	id := ""
	for _, t := range m {
		if t == nil {
			continue
		}
		info.ID = id
		if got := t.StartGeneration(ctx, info); id == "" {
			id = got
		}
	}
	return id
}

// EndGeneration closes the model call on every sink.
func (m Multi) EndGeneration(ctx context.Context, genID string, output any, usage chat.Usage, errMsg string) {
	for _, t := range m {
		if t == nil {
			continue
		}
		t.EndGeneration(ctx, genID, output, usage, errMsg)
	}
}
