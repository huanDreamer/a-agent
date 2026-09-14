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

	"github.com/huan/huan-agent/internal/tool"
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
	// EventAsk is one update about a question the model put to the person at the
	// other end (the ask_user tool). Two shapes share it, and a consumer tells
	// them apart by which fields are set: Ask is present when a question is
	// announced (AskStatus is AskPending), and AskID is present when it settles.
	// Both carry the status, so a client that missed the announcement can still
	// tell that a question it does not know about has ended.
	EventAsk EventType = "ask_user"
	// EventUsage reports token usage for one model call.
	EventUsage EventType = "usage"
	// EventContextCompressed reports that the in-loop history was condensed to
	// stay inside the token budget. It is a notice, not a failure: the turn
	// carries on with a bounded window.
	EventContextCompressed EventType = "context_compressed"
	// EventBudgetStop reports that the turn ended on a budget rather than on an
	// answer. It is emitted before EventDone, and Result.StopReason carries the
	// same reason for callers that never see events.
	EventBudgetStop EventType = "budget_stop"
	// EventDone ends the run successfully.
	EventDone EventType = "done"
	// EventError ends the run with a failure.
	EventError EventType = "error"
)

// AskPending is the AskStatus of a question that has been put to the user and is
// waiting for an answer. The other statuses are tool.AnswerStatus values, so the
// vocabulary of "how a question ended" has exactly one definition.
const AskPending = "pending"

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
	// Ask is the question being put to the user, set on the ask_user event that
	// announces it. Its ID is what the client submits an answer with.
	Ask *tool.Question `json:"ask,omitempty"`
	// AskID identifies the question an ask_user event settles.
	AskID string `json:"ask_id,omitempty"`
	// AskStatus is a question's state on an ask_user event: AskPending while it
	// waits for an answer, then one of tool.AnswerAnswered, tool.AnswerTimeout or
	// tool.AnswerCancelled.
	AskStatus string `json:"ask_status,omitempty"`
	// AskAnswer is what the person submitted, set when AskStatus is answered. A
	// timeout or a cancellation carries the status alone: there is no answer to
	// report, and inventing an empty one would be indistinguishable from "the
	// user submitted nothing".
	AskAnswer *tool.Answer `json:"ask_answer,omitempty"`
	// Error is set on error events.
	Error string `json:"error,omitempty"`
	// Reason is set on budget_stop: "steps", "tokens" or "deadline".
	Reason string `json:"reason,omitempty"`
	// Tokens is what the turn has spent so far, set on budget_stop and
	// context_compressed.
	Tokens int `json:"tokens,omitempty"`
	// ElapsedMs is how long the turn has been running, set on budget_stop.
	ElapsedMs int64 `json:"elapsed_ms,omitempty"`
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
	// TraceID identifies the trace recorded for this turn; empty when tracing
	// is off. The caller persists it with the answer, because it is what lets a
	// conversation link to the trace that explains it — and the runner is the
	// only place that ever knows it.
	TraceID string
	// StopReason is why the loop ended when the model did not answer: StopSteps,
	// StopTokens or StopDeadline. Empty means the model produced its own answer,
	// which is the only case where Text is complete by construction.
	StopReason string
}

// BudgetExhausted reports whether the turn ended on a budget rather than on the
// model's own answer. Callers use it to render a notice, and to decide whether
// offering "continue" makes sense.
func (r *Result) BudgetExhausted() bool {
	return r != nil && r.StopReason != ""
}

// Emitter receives events as the run progresses. It must not block for long:
// it is called from the run goroutine, and a slow consumer (e.g. a browser on a
// slow link) would otherwise stall the model loop. Implementations that write
// to a network should bound their own writes.
type Emitter func(Event)

// nowFunc is an indirection so tests can freeze time.
var nowFunc = time.Now
