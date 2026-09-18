package tool

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// What a tool needs in order to ask a person to approve what it is about to do.
//
// The vocabulary lives here, next to the Tool interface, for the same reason the
// Asker's does: three packages have to agree on it and none of them owns the
// others — a tool is gated, the runtime carries the approver on the turn's
// context, and a surface answers. Nothing here knows about HTTP, SSE, cards or
// timeouts.
//
// The one place this deliberately differs from Ask: **a timeout is a refusal.**
// An unanswered question leaves the model to decide what to do next, which is
// fine because asking is how it gathers information. An unanswered approval must
// not let the action through, because the moment nobody is watching is exactly
// the moment the gate has to hold.

// PreviewKind labels one line of a request's preview, so a surface can style it
// without parsing the text.
type PreviewKind string

const (
	// PreviewContext is unchanged context around a change.
	PreviewContext PreviewKind = "context"
	// PreviewAdd is a line being added.
	PreviewAdd PreviewKind = "add"
	// PreviewDel is a line being removed.
	PreviewDel PreviewKind = "del"
	// PreviewMeta is anything else worth showing: a working directory, a byte
	// count, the reason a preview was truncated.
	PreviewMeta PreviewKind = "meta"
)

// PreviewLine is one line of the detail shown with a request.
type PreviewLine struct {
	Kind PreviewKind `json:"kind"`
	Text string      `json:"text"`
}

// Request is what a gated tool asks before it acts.
//
// Summary and Preview are what make the decision possible. A gate that shows only
// a tool name asks a person to approve something they cannot see, which trains
// them to approve without reading — worse than no gate, because it looks like one.
type Request struct {
	// ID is the handle the decision comes back with. It is assigned by the
	// Approver, so a caller leaves it empty.
	ID string `json:"id"`
	// Tool is the name of the tool that wants to act.
	Tool string `json:"tool"`
	// Capability is what it would do: write or exec. It is what the policy is
	// expressed in terms of, so a new tool needs no new policy.
	Capability Capability `json:"capability"`
	// Summary is one line a person can read at a glance:
	// "写入 internal/foo/bar.go", "执行: git push origin main".
	Summary string `json:"summary"`
	// Preview is the bounded detail behind the summary.
	Preview []PreviewLine `json:"preview,omitempty"`
	// Default is the decision to highlight as the safe one. It does not change
	// what a timeout means: a timeout is always a refusal.
	Default DecisionKind `json:"default"`
	// Timeout is how long the request waits. Zero means the surface's own limit.
	Timeout int64 `json:"timeout_ms,omitempty"`
}

// DecisionKind is what a person decided.
type DecisionKind string

const (
	// DecisionAllowOnce lets this one call through.
	DecisionAllowOnce DecisionKind = "allow_once"
	// DecisionAllowTurn lets the rest of this turn's calls to the same tool
	// through. It is scoped to one turn on purpose: "remember this forever"
	// turns one click into a rule nobody reviews again.
	DecisionAllowTurn DecisionKind = "allow_turn"
	// DecisionDeny refuses this call.
	DecisionDeny DecisionKind = "deny"
)

// Valid reports whether a decision kind is one this package defines. It is what
// an HTTP handler checks before believing a request body.
func (k DecisionKind) Valid() bool {
	switch k {
	case DecisionAllowOnce, DecisionAllowTurn, DecisionDeny:
		return true
	default:
		return false
	}
}

// Decision is what came back.
type Decision struct {
	Kind DecisionKind `json:"kind"`
	// Reason is the person's own words when they refused. It is handed to the
	// model as the tool's result, which is the whole point: a refusal that says
	// why lets the model choose a different approach instead of repeating the
	// same request.
	Reason string `json:"reason,omitempty"`
	// Source says where the decision came from: "human", "policy" (a configured
	// allow-list match) or "timeout". It is recorded so an audit can tell
	// "someone approved this" from "nobody was there".
	Source string `json:"source,omitempty"`
}

// Allowed reports whether the decision lets the call through.
func (d Decision) Allowed() bool {
	return d.Kind == DecisionAllowOnce || d.Kind == DecisionAllowTurn
}

// Decision sources, as they appear in the audit and in events.
const (
	SourceHuman   = "human"
	SourcePolicy  = "policy"
	SourceTimeout = "timeout"
)

// Approver puts a request to the person on the other end of a turn and waits for
// a decision.
//
// An implementation owns three things the gate does not: how the request is
// delivered, how long it may wait, and — the one that matters — that a failure to
// get an answer means "no". Returning an error is not a refusal the model should
// read as a person's decision; the gate treats it as a denial with the error as
// the reason.
type Approver interface {
	Approve(ctx context.Context, req Request) (Decision, error)
}

type approverKey struct{}

// WithApprover attaches an approver to a context, so a tool reached through this
// context can ask. It is the same mechanism Asker uses.
//
// A nil approver — including a typed nil, which is the trap — leaves the context
// alone. That matters more here than for the Asker: a non-nil interface holding a
// nil pointer passes every `if approver == nil` check and then panics on first use,
// which would surface as a tool call failing for no visible reason.
func WithApprover(ctx context.Context, a Approver) context.Context {
	if a == nil || isNilApprover(a) {
		return ctx
	}
	return context.WithValue(ctx, approverKey{}, a)
}

// isNilApprover reports whether an approver is a nil pointer behind a non-nil
// interface. The check is reflection because that is the only way to ask the
// question Go makes impossible to ask directly.
func isNilApprover(a Approver) bool {
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// ApproverFrom returns the approver on a context, or nil when there is none.
//
// A nil approver is the normal state of a surface that cannot ask (a one-shot run,
// a Feishu turn before its card exists). The gate refuses in that case rather than
// letting the call through, and the surface's registration is what keeps the tool
// off the menu in the first place.
func ApproverFrom(ctx context.Context) Approver {
	if a, ok := ctx.Value(approverKey{}).(Approver); ok {
		return a
	}
	return nil
}

// DenialResult renders a refusal as the tool's observation for the model.
//
// It is phrased as an instruction rather than an error because the model has to
// do something next: repeating the same call is the one response that wastes the
// person's attention twice.
func DenialResult(req Request, d Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "该操作已被拒绝，没有执行：%s。", req.Summary)
	if reason := strings.TrimSpace(d.Reason); reason != "" {
		fmt.Fprintf(&b, "\n拒绝理由：%s", reason)
	}
	if d.Source == SourceTimeout {
		b.WriteString("\n（等待超时，没有人回应；超时按拒绝处理。）")
	}
	b.WriteString("\n请改用在被拒绝的范围内可以完成的做法，或者把需要人确认的部分留在回答里说明；不要重复同一个请求。")
	return b.String()
}
