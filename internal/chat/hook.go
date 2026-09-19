package chat

import "context"

// Hooks is the optional dispatcher a turn reports to around tool calls and at
// the end of the turn.
//
// It exists so the loop can honour a hook decision without knowing what a hook
// is: the runner says "about to run this call" / "this call returned this" /
// "this turn is about to answer", and a dispatcher it was configured with
// answers with what to do about it. The runner is the only place that knows all
// three moments, which is why the seam is here rather than in the tools.
//
// The interface is deliberately three methods and no more. Claude Code's own
// protocol has thirty-three events; the runner can only speak for the three
// moments it owns, and a wider interface would be a promise it cannot keep.
//
// A nil Hooks is the normal state for a deployment that did not turn the
// compatibility mode on, and it must cost nothing: every call site checks for
// nil rather than calling through a no-op implementation, so a turn with no
// hooks has no hook work at all.
type Hooks interface {
	// PreToolUse is asked before a tool runs. A Block decision skips the call
	// entirely and the block reason becomes the call's failure, which is what
	// the model reads; UpdatedArgs, when set, replaces the arguments the model
	// wrote.
	PreToolUse(ctx context.Context, call HookCall) HookDecision
	// PostToolUse is asked after a tool ran (or failed). HasUpdatedResult, when
	// set, replaces what the model is told the tool returned.
	PostToolUse(ctx context.Context, call HookCall) HookDecision
	// Stop is asked when the turn is about to answer. A Block decision means the
	// answer is not the end of the work: BlockReason is handed back to the model
	// as a system message and the loop continues, bounded by the step budget.
	Stop(ctx context.Context, stop HookStop) HookStopDecision
}

// HookCall is one tool invocation as a dispatcher sees it.
type HookCall struct {
	// SessionID is the conversation the call belongs to.
	SessionID string
	// ToolUseID is the model's id for this call, which is what a hook protocol
	// calls tool_use_id; empty when the provider did not supply one.
	ToolUseID string
	// ToolName is the tool as the model named it.
	ToolName string
	// Args is the raw JSON the model wrote.
	Args string
	// Result is the tool's output. PostToolUse only.
	Result string
	// Error is the tool's failure message, empty on success. PostToolUse only.
	Error string
	// DurationMs is how long the call took, the way the turn measured it. It
	// exists for the hook protocol's own duration_ms field (PostToolUse), which
	// is the number a handler logs when it wants to know what a tool costs.
	DurationMs int64
}

// HookDecision is what a dispatcher decided about one tool call.
type HookDecision struct {
	// Block refuses the call (PreToolUse) — see Hooks.
	Block bool
	// BlockReason is what the model is told instead of the tool's result.
	BlockReason string
	// UpdatedArgs replaces the arguments, when non-empty.
	UpdatedArgs string
	// UpdatedResult replaces the tool's output, when HasUpdatedResult is set.
	UpdatedResult    string
	HasUpdatedResult bool
	// Context is text to add to the model's context alongside the tool result,
	// which is how a PostToolUse hook warns without failing anything.
	Context []string
}

// HookStop is the end of a turn as a dispatcher sees it.
type HookStop struct {
	SessionID string
	// LastAssistantMessage is the answer the turn is about to return. It is what
	// a Stop hook inspects to decide whether the work is finished.
	LastAssistantMessage string
	// Step is the iteration the turn is ending on.
	Step int
	// AlreadyBlocked reports that a Stop hook already refused once in this turn.
	// It is the protocol's own loop guard: a hook that always blocks would
	// otherwise hold the turn until its step budget ran out, so a hook can check
	// this and let the second stop through.
	AlreadyBlocked bool
}

// HookStopDecision is what the Stop hooks decided.
type HookStopDecision struct {
	// Block means "not finished": the turn continues.
	Block bool
	// Reason is the message the model gets as the reason to continue.
	Reason string
	// Context is additional text for the model.
	Context []string
}
