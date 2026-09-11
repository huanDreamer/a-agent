package chat

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// Tracer receives execution structure for observability. The runner depends on
// this narrow interface rather than on a specific backend, so the Langfuse
// client can be swapped for a no-op (or a test double) without touching the
// loop.
//
// Every method must be safe to call on a nil/disabled implementation, and must
// never block for long: tracing must not slow the conversation down.
type Tracer interface {
	// StartTrace begins a turn and returns its id ("" when disabled).
	StartTrace(ctx context.Context, info TraceInfo) string
	// EndTrace finishes a turn with its final output.
	EndTrace(ctx context.Context, traceID string, output any)
	// StartSpan begins a nested operation (a tool call) and returns its id.
	StartSpan(ctx context.Context, info SpanInfo) string
	// EndSpan finishes a span. errMsg != "" marks it failed.
	EndSpan(ctx context.Context, spanID string, output any, errMsg string)
	// StartGeneration begins a model call and returns its id.
	StartGeneration(ctx context.Context, info GenInfo) string
	// EndGeneration finishes a model call with its output and token usage.
	EndGeneration(ctx context.Context, genID string, output any, usage Usage, errMsg string)
}

// TraceInfo describes a trace being started.
type TraceInfo struct {
	Name      string
	SessionID string
	UserID    string
	Input     any
}

// SpanInfo describes a span being started.
type SpanInfo struct {
	TraceID string
	Name    string
	Input   any
}

// GenInfo describes a model call being started.
type GenInfo struct {
	TraceID string
	Name    string
	Model   string
	Input   any
	Step    int
}

// NopTracer discards everything. It is the default so a Runner never needs a
// nil check.
type NopTracer struct{}

// StartTrace implements Tracer.
func (NopTracer) StartTrace(context.Context, TraceInfo) string { return "" }

// EndTrace implements Tracer.
func (NopTracer) EndTrace(context.Context, string, any) {}

// StartSpan implements Tracer.
func (NopTracer) StartSpan(context.Context, SpanInfo) string { return "" }

// EndSpan implements Tracer.
func (NopTracer) EndSpan(context.Context, string, any, string) {}

// StartGeneration implements Tracer.
func (NopTracer) StartGeneration(context.Context, GenInfo) string { return "" }

// EndGeneration implements Tracer.
func (NopTracer) EndGeneration(context.Context, string, any, Usage, string) {}

// schemaToParams converts a tool's parameter JSON Schema into eino's
// ParamsOneOf. The raw document is unmarshalled directly, so any valid JSON
// Schema (draft 07 or 2020-12) is preserved exactly as the tool declared it —
// dropping the parameters would leave the model unable to call the tool with
// arguments at all.
func schemaToParams(raw string) (*schema.ParamsOneOf, error) {
	trimmed := strings.TrimSpace(raw)
	// A tool that declares nothing, or only an empty object, still needs a valid
	// object schema: providers reject a function whose parameter type is null
	// ("Invalid schema for function ... got 'type: null'").
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{
			Type:       "object",
			Properties: jsonschema.NewProperties(),
		}), nil
	}

	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
		return nil, err
	}
	// A schema without a type is rejected too, so default it to an object
	// rather than forwarding a null.
	if strings.TrimSpace(s.Type) == "" {
		s.Type = "object"
	}
	if s.Properties == nil {
		s.Properties = jsonschema.NewProperties()
	}
	return schema.NewParamsOneOfByJSONSchema(&s), nil
}

// modelName extracts a display name for tracing. The web chat model wrapper
// implements Name(); models that do not simply report an empty name, and
// tracing records no model rather than a wrong one.
func modelName(m any) string {
	if named, ok := m.(interface{ Name() string }); ok {
		return named.Name()
	}
	return ""
}
