package langfuse

import (
	"context"
	"time"

	"github.com/huan/huan-agent/internal/chat"
)

// Tracer adapts a Client to the chat.Tracer interface, so the conversational
// loop can report traces, tool spans and model generations without importing
// Langfuse itself.
//
// A zero or disabled Tracer is inert: every method returns without doing work,
// which is what lets the chat runner always call through without checking.
type Tracer struct {
	client *Client
	// SessionName labels the trace with the chat session id so the Langfuse UI
	// can group a conversation's turns.
	SessionName string
}

// NewTracer wraps a client. A nil client yields a disabled tracer.
func NewTracer(c *Client) *Tracer { return &Tracer{client: c} }

// Enabled reports whether traces will actually be sent.
func (t *Tracer) Enabled() bool {
	return t != nil && t.client != nil && t.client.Enabled()
}

// StartTrace begins a turn.
func (t *Tracer) StartTrace(ctx context.Context, info chat.TraceInfo) string {
	if !t.Enabled() {
		return ""
	}
	id := NewID()
	_ = t.client.Trace(ctx, TraceEvent{
		ID:        id,
		Name:      info.Name,
		UserID:    info.UserID,
		SessionID: info.SessionID,
		Input:     info.Input,
		Metadata:  map[string]any{"source": "web-admin"},
		Tags:      []string{"huan-agent", "web"},
	})
	return id
}

// EndTrace finishes a turn.
func (t *Tracer) EndTrace(ctx context.Context, traceID string, output any) {
	if !t.Enabled() || traceID == "" {
		return
	}
	_ = t.client.EndTrace(ctx, traceID, output)
}

// StartSpan begins a tool call.
func (t *Tracer) StartSpan(ctx context.Context, info chat.SpanInfo) string {
	if !t.Enabled() || info.TraceID == "" {
		return ""
	}
	id := NewID()
	_ = t.client.Span(ctx, SpanEvent{
		ID:      id,
		TraceID: info.TraceID,
		Name:    info.Name,
		Input:   info.Input,
	})
	return id
}

// EndSpan finishes a tool call.
func (t *Tracer) EndSpan(ctx context.Context, spanID string, output any, errMsg string) {
	if !t.Enabled() || spanID == "" {
		return
	}
	_ = t.client.EndSpan(ctx, spanID, output, errMsg, time.Now().UTC())
}

// StartGeneration begins a model call.
func (t *Tracer) StartGeneration(ctx context.Context, info chat.GenInfo) string {
	if !t.Enabled() || info.TraceID == "" {
		return ""
	}
	id := NewID()
	_ = t.client.Generation(ctx, GenerationEvent{
		ID:      id,
		TraceID: info.TraceID,
		Name:    info.Name,
		Model:   info.Model,
		Input:   info.Input,
		Metadata: map[string]any{
			"step": info.Step,
		},
	})
	return id
}

// EndGeneration finishes a model call with its usage.
func (t *Tracer) EndGeneration(ctx context.Context, genID string, output any, usage chat.Usage, errMsg string) {
	if !t.Enabled() || genID == "" {
		return
	}
	_ = t.client.EndGeneration(ctx, genID, output, Usage{
		Input:  usage.PromptTokens,
		Output: usage.CompletionTokens,
		Total:  usage.TotalTokens,
	}, errMsg, time.Now().UTC())
}

// compile-time check that the adapter satisfies the chat tracer contract.
var _ chat.Tracer = (*Tracer)(nil)

// Stats reports the client's delivery counters, so the admin UI can show
// whether traces are actually reaching Langfuse.
func (t *Tracer) Stats() Stats {
	if t == nil || t.client == nil {
		return Stats{}
	}
	return t.client.Stats()
}

// Host returns the Langfuse base URL for deep links.
func (t *Tracer) Host() string {
	if t == nil || t.client == nil {
		return ""
	}
	return t.client.Host()
}

// ListTraces proxies the trace list.
func (t *Tracer) ListTraces(ctx context.Context, f TraceFilter) ([]TraceSummary, error) {
	if t == nil || t.client == nil {
		return nil, ErrDisabled
	}
	return t.client.ListTraces(ctx, f)
}

// GetTrace proxies one trace with its observations.
func (t *Tracer) GetTrace(ctx context.Context, traceID string) (*TraceDetail, error) {
	if t == nil || t.client == nil {
		return nil, ErrDisabled
	}
	return t.client.GetTrace(ctx, traceID)
}
