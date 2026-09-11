// Package chat implements the streaming conversational loop used by the web
// admin UI: it drives a tool-calling chat model through a ReAct-style
// think → tool → observe cycle and emits structured events as it goes, so the
// browser can render assistant text, the model's reasoning, each tool call and
// its result as they happen.
//
// It is deliberately separate from internal/agent (which wraps eino's ReAct
// flow for the IM bot): the web UI needs per-step visibility into reasoning and
// tool calls, which a single final-message stream cannot provide.
package chat

import (
	"time"
)

// EventType identifies a streamed event.
type EventType string

const (
	// EventStepStart marks the beginning of one model→tool iteration.
	EventStepStart EventType = "step_start"
	// EventTextDelta is a chunk of assistant-visible text.
	EventTextDelta EventType = "text_delta"
	// EventReasoningDelta is a chunk of the model's reasoning/thinking. It is
	// populated by reasoning models (e.g. deepseek-reasoner) and is shown in a
	// collapsible panel rather than as the answer.
	EventReasoningDelta EventType = "reasoning_delta"
	// EventToolCall announces a tool the model wants to run, with its arguments.
	EventToolCall EventType = "tool_call"
	// EventToolResult carries a finished tool's output (or its error).
	EventToolResult EventType = "tool_result"
	// EventUsage reports token usage for one model call.
	EventUsage EventType = "usage"
	// EventDone ends the run successfully.
	EventDone EventType = "done"
	// EventError ends the run with a failure.
	EventError EventType = "error"
)

// Event is one streamed update. Fields are populated according to Type; a
// consumer must switch on Type rather than assume an event carries everything.
type Event struct {
	Type EventType `json:"type"`

	// Step is the 1-based iteration this event belongs to.
	Step int `json:"step,omitempty"`

	// Text carries the delta for text_delta / reasoning_delta, and the final
	// assembled answer for done.
	Text string `json:"text,omitempty"`

	// ToolCallID is a stable id for one tool invocation, used to pair a
	// tool_call with its tool_result.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the tool being invoked.
	ToolName string `json:"tool_name,omitempty"`
	// ToolArgs is the raw JSON arguments as produced by the model.
	ToolArgs string `json:"tool_args,omitempty"`
	// ToolResult is the tool's output. Empty when the tool failed.
	ToolResult string `json:"tool_result,omitempty"`
	// ToolError is the failure message; non-empty marks the call as failed.
	ToolError string `json:"tool_error,omitempty"`
	// DurationMs is how long the tool took.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Usage is set on usage events.
	Usage *Usage `json:"usage,omitempty"`
	// Error is set on error events.
	Error string `json:"error,omitempty"`
	// MessageID is the persisted assistant message id, set on done.
	MessageID string `json:"message_id,omitempty"`
}

// Usage is token accounting for one model call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// DurationMs is the model call's wall time.
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// ToolRun is a record of one finished tool invocation, kept on the Result so
// the caller can persist it to the audit log. The JSON tags define how it is
// stored with a message and served to the UI, so it matches the snake_case used
// everywhere else in the API.
type ToolRun struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Args       string `json:"args,omitempty"`
	Result     string `json:"result,omitempty"`
	Err        string `json:"err,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// Result summarises a completed run.
type Result struct {
	// Text is the final assistant answer.
	Text string
	// Reasoning is the accumulated reasoning content, if any.
	Reasoning string
	// Steps is how many model iterations ran.
	Steps int
	// Usage is the sum across every model call in the run.
	Usage Usage
	// Tools lists the tool invocations that ran, in order.
	Tools []ToolRun
}

// Emitter receives events as the run progresses. It must not block for long:
// it is called from the run goroutine, and a slow consumer (e.g. a browser on a
// slow link) would otherwise stall the model loop. Implementations that write
// to a network should bound their own writes.
type Emitter func(Event)

// nowFunc is an indirection so tests can freeze time.
var nowFunc = time.Now
