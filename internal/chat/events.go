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

import "context"

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
	// EventStepEnd marks the end of one iteration's model call, before the tools
	// it asked for run.
	//
	// It is what lets a reader tell the model's *process* from its *answer*: a
	// step that went on to call a tool was narrating what it was about to do, so
	// the text of such a step belongs inside that step rather than in the answer.
	// A client that only concatenates text deltas cannot make that distinction,
	// because the text arrives before the model has said whether it wants a tool.
	//
	// Text carries what the step said. The turn's last step never emits it: that
	// step is the answer, which arrives as EventDone.
	EventStepEnd EventType = "step_end"
	// EventReasoningDelta is a chunk of the model's reasoning/thinking. It is
	// populated by reasoning models (e.g. deepseek-reasoner) and is shown in a
	// collapsible panel rather than as the answer.
	EventReasoningDelta EventType = "reasoning_delta"
	// EventToolCall announces a tool the model wants to run, with its arguments.
	EventToolCall EventType = "tool_call"
	// EventToolResult carries a finished tool's output (or its error).
	EventToolResult EventType = "tool_result"
	// EventApproval announces a write or exec that needs a decision, and settles
	// it. Like EventAsk it carries two shapes: Approval is present when the
	// request is announced, ApprovalID when it is decided.
	//
	// Unlike a question, an unanswered approval is a refusal. The client shows
	// the same countdown, but what happens at zero is the opposite, and the
	// event says so by carrying ApprovalSource=timeout with DecisionDeny.
	EventApproval EventType = "approval"
	// EventAsk is one update about a question the model put to the person at the
	// other end (the ask_user tool). Two shapes share it, and a consumer tells
	// them apart by which fields are set: Ask is present when a question is
	// announced (AskStatus is AskPending), and AskID is present when it settles.
	// Both carry the status, so a client that missed the announcement can still
	// tell that a question it does not know about has ended.
	EventAsk EventType = "ask_user"
	// EventUsage reports token usage for one model call.
	EventUsage EventType = "usage"
	// EventPlan reports the plan the model maintains for this turn: the task
	// list behind the console's 任务看板. It is emitted every time the plan
	// changes (a plan_* tool ran), so the board follows the work while it
	// happens rather than only after the turn is stored.
	EventPlan EventType = "plan"
	// EventStepRetry reports that one step's model call failed and is about to be
	// run again, with the delay that is being waited out first.
	//
	// It exists because a retried step may have already streamed part of an
	// answer: the client has to know to throw that text away, or the retry's
	// output lands on top of the failed attempt's. A client that ignores this
	// event shows a spliced answer, which is worse than showing the failure.
	EventStepRetry EventType = "step_retry"
	// EventContextCompressed reports that the in-loop history was condensed to
	// stay inside the token budget. It is a notice, not a failure: the turn
	// carries on with a bounded window.
	//
	// The web chat page renders nothing for it on purpose — the reader cannot
	// act on it, and the runner logs it against the session instead (see
	// Runner.condense). The event stays in the stream for the callers that do
	// show it, the CLI included.
	EventContextCompressed EventType = "context_compressed"
	// EventBudgetStop reports that the turn ended short of an answer — on a
	// budget, or because the loop guard stopped a turn that was going nowhere.
	// It is emitted before EventDone, and Result.StopReason carries the same
	// reason for callers that never see events.
	EventBudgetStop EventType = "budget_stop"
	// EventSteer reports that the harness told the model how it was running the
	// turn: it is repeating a call, rereading one file, or looking without ever
	// acting. It is a notice to the reader, not a failure — the model gets the
	// same sentence as a message and usually changes course — and it is the one
	// place a user can see that a turn was steered rather than merely slow.
	EventSteer EventType = "steer"
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

	// ParentToolCallID marks an event that came from something a tool spawned
	// rather than from the turn itself — a subagent, today.
	//
	// It is what keeps a nested run from being indistinguishable from the parent:
	// its text must not be appended to the parent's answer (the whole point of a
	// subagent is that its intermediate work does not enter the parent's context,
	// and a console that showed it as the answer would be telling the reader
	// something false), and its steps belong under the card of the call that
	// spawned it.
	ParentToolCallID string `json:"parent_tool_call_id,omitempty"`

	// Usage is set on usage events.
	Usage *Usage `json:"usage,omitempty"`
	// Plan is set on plan events: the plan as it stands after the change.
	Plan *tool.Plan `json:"plan,omitempty"`
	// SteerKind, SteerStop and Text are set on steer events: what the model was
	// told, and the stop reason it will carry if it ignores the message.
	SteerKind string `json:"steer_kind,omitempty"`
	SteerStop string `json:"steer_stop,omitempty"`
	// Attempt, MaxAttempts, DelayMs and Error are set on step_retry: which
	// attempt is about to run, how many the runner will make in total, how long
	// it waits first, and what went wrong. Attempt counts the run that is about
	// to happen, so the first retry is attempt 2.
	Attempt     int   `json:"attempt,omitempty"`
	MaxAttempts int   `json:"max_attempts,omitempty"`
	DelayMs     int64 `json:"delay_ms,omitempty"`
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
	// Approval is the request being put to the user, set on the approval event
	// that announces it. Its ID is what the client decides with.
	Approval *tool.Request `json:"approval,omitempty"`
	// ApprovalID identifies the request an approval event settles.
	ApprovalID string `json:"approval_id,omitempty"`
	// ApprovalDecision is a request's outcome on an approval event, once it has
	// one: one of tool.DecisionAllowOnce, tool.DecisionAllowTurn or
	// tool.DecisionDeny.
	ApprovalDecision string `json:"approval_decision,omitempty"`
	// ApprovalReason is what the person said when they refused, and
	// ApprovalSource is where the decision came from: "human", "policy" or
	// "timeout". Both are kept for the audit: "someone approved this" and "nobody
	// was there" are different facts about the same action.
	ApprovalReason string `json:"approval_reason,omitempty"`
	ApprovalSource string `json:"approval_source,omitempty"`
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
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
	// Step is the 1-based iteration this call belongs to. It is what lets a
	// reader put a tool call back next to the reasoning that asked for it: the
	// turn's tool runs are a flat list, and without this the pairing is lost the
	// moment the turn is stored.
	//
	// Zero means "unknown" — a run recorded before steps were kept, or by a
	// caller that does not track them.
	Step       int    `json:"step,omitempty"`
	Result     string `json:"result,omitempty"`
	Err        string `json:"err,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`

	// Nested is what this call spawned: a subagent's own steps, kept under the call
	// rather than mixed into the turn's.
	//
	// It is stored with the turn, so a reloaded conversation still shows what the
	// subagent did — which is the difference between "the model spawned something
	// and waited" and "the model spawned something, and here is what it found".
	Nested []NestedCall `json:"nested,omitempty"`
}

// NestedCall is one thing a spawned agent did.
//
// The shape is deliberately flat and small: a reader sees a line per action, not a
// second conversation. A subagent's full transcript belongs in its own trace, which
// it has (the nested run is traced like any other turn), not in the parent's.
type NestedCall struct {
	// Kind is "text", "reasoning" or "tool".
	Kind string `json:"kind"`
	// Text accumulates a run of deltas of the same kind.
	Text string `json:"text,omitempty"`
	// Name is a tool's name; ID pairs its call with its result.
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
	// Result and Err are a nested tool call's outcome.
	Result string `json:"result,omitempty"`
	Err    string `json:"error,omitempty"`
}

// Step is one iteration of the ReAct loop, as it is shown to a reader and stored
// with the answer.
//
// The reasoning and the tool calls of one iteration belong together: a list of
// every tool call followed by one block of every thought is unreadable, because
// nothing says which thought asked for which call. Keeping the iteration whole
// is what makes the turn's process legible step by step.
type Step struct {
	// Index is the 1-based iteration number, matching Event.Step.
	Index int `json:"index"`
	// Reasoning is the model's thinking during this iteration. Empty for models
	// that do not report any.
	Reasoning string `json:"reasoning,omitempty"`
	// Text is what this iteration said before it acted. For the turn's last step
	// it is the answer itself (which Result.Text also carries); for every other
	// step it is process — "let me read the config first" — and is deliberately
	// kept out of the answer.
	Text string `json:"text,omitempty"`
	// Tools are the calls this iteration asked for, in the order it asked.
	Tools []ToolRun `json:"tools,omitempty"`
}

// StepsHaveTools reports whether any step in the list called a tool, which is
// what a UI uses to decide whether there is a process worth folding up at all.
func StepsHaveTools(steps []Step) bool {
	for _, step := range steps {
		if len(step.Tools) > 0 {
			return true
		}
	}
	return false
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
	// Plan is the turn broken down by iteration: each step's reasoning, what it
	// said, and the tool calls it asked for. It is what the console renders and
	// what is stored with the answer, so a reloaded conversation shows the same
	// step-by-step process the streaming one did.
	//
	// It is deliberately not named Steps: that field already exists and is the
	// *count* of iterations, which callers were reading long before this one.
	Plan []Step
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

// Nested is what a tool that spawns something needs in order to report it.
//
// It travels on the context because the tool cannot know it: the emitter and the
// call id belong to the turn, and a tool is built once at startup. The same
// mechanism the Asker and the Approver use.
type Nested struct {
	// ParentCallID is the id of the call that spawned the nested work.
	ParentCallID string
	// Emit publishes a nested event. It stamps the parent id itself, so a caller
	// cannot forget to.
	Emit func(Event)
}

// The id of the call a tool is running as.
//
// It is published alongside the nested emitter because they are the same fact seen
// twice: "what is running now" is needed both to tag what this call spawns and to
// file it under the right card.
type callKey struct{}

// WithToolCallID publishes the id of the tool call being invoked.
func WithToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, callKey{}, id)
}

// ToolCallIDFrom reads it, or "" when this is not a tool invocation.
func ToolCallIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(callKey{}).(string)
	return id
}

type nestedKey struct{}

// WithNested publishes the turn's nested emitter.
func WithNested(ctx context.Context, n Nested) context.Context {
	if n.Emit == nil || n.ParentCallID == "" {
		return ctx
	}
	return context.WithValue(ctx, nestedKey{}, n)
}

// NestedFrom reads it, or reports that this turn cannot show nested work.
//
// A surface with no answer means "run without reporting" rather than an error: a
// subagent that cannot be watched still produces its report, and refusing to run
// because nobody is watching would be the wrong trade.
func NestedFrom(ctx context.Context) (Nested, bool) {
	n, ok := ctx.Value(nestedKey{}).(Nested)
	return n, ok && n.Emit != nil
}

// nowFunc is an indirection so tests can freeze time.
var nowFunc = time.Now
