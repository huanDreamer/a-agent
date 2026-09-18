package tool

import (
	"context"

	"github.com/cloudwego/eino/components/model"
)

// What a turn-scoped tool needs in order to run something of its own.
//
// The case this exists for is `spawn_agent`: it starts a nested conversation, and
// to do that it has to know **the registry and the model this turn is actually
// using**. Neither can be captured when the tool is built:
//
//   - The registry is resolved per turn (a conversation bound to a workspace gets
//     that workspace's tools), so a tool built at startup would hand its subagent
//     whatever workspace happened to be current then.
//   - The model is chosen per conversation too (the console lets a reader pick
//     one), so a captured model would run the subagent on the default model while
//     the parent ran on the chosen one.
//
// So both travel on the turn's context, which is the same mechanism the Asker,
// the Approver and the Planner use: a capability that belongs to one turn is
// published by whoever owns the turn.
type TurnResources struct {
	// Registry is the tool registry this turn is running with.
	Registry *Registry
	// Model drives this turn. It is nil for a turn with no model to hand on
	// (which is the same as "no subagents": a nested run needs one).
	Model model.BaseChatModel
	// SessionID and Scope keep anything the turn spawns attributable to it: a
	// subagent's cost belongs to the turn that asked for it, in the traces and in
	// the usage table alike.
	SessionID string
	Scope     string
	// SystemPrompt is the prompt this turn was given, so a nested run can inherit
	// it rather than starting from nothing.
	SystemPrompt string
}

// ModelResolver turns a model name into a model, so a tool can let the model ask
// for a cheaper model without knowing how this deployment chooses one.
type ModelResolver func(ctx context.Context, name string) (model.BaseChatModel, error)

type turnResourcesKey struct{}

// WithTurnResources publishes the turn's registry and model for the tools that
// run inside it.
//
// Both are required for the pair to be usable; a caller with only one of them
// publishes nothing rather than a half-answer a tool would have to defend against.
func WithTurnResources(ctx context.Context, res TurnResources) context.Context {
	if res.Registry == nil || res.Model == nil {
		return ctx
	}
	return context.WithValue(ctx, turnResourcesKey{}, res)
}

// TurnResourcesFrom reads what the current turn published, or reports that it
// published nothing.
//
// A tool that needs these and finds none must refuse with a readable reason
// rather than starting something with a guessed registry.
func TurnResourcesFrom(ctx context.Context) (TurnResources, bool) {
	res, ok := ctx.Value(turnResourcesKey{}).(TurnResources)
	return res, ok && res.Registry != nil && res.Model != nil
}

// The id of the tool call being invoked.
//
// It is published by the loop (internal/chat) and read by tools that need to report
// something about themselves — `spawn_agent` files each delegated run under the call
// that asked for it. The value is threaded through this package rather than imported
// from the loop, because the builtin tools must not depend on a specific loop.
type callIDKey struct{}

// WithToolCallID publishes the id of the current tool call.
func WithToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, callIDKey{}, id)
}

// ToolCallIDFrom reads the current tool call's id, or "" outside an invocation.
func ToolCallIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}
