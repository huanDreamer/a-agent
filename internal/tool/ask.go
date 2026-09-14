package tool

import "context"

// What a tool needs in order to ask the person at the other end a question.
//
// The vocabulary lives here, next to the Tool interface itself, because three
// packages have to agree on it and none of them owns the others: a tool asks
// (internal/tool/builtin), a runner carries the asker on the turn's context
// (internal/chat), and a surface answers (internal/server for the web console).
// Putting the types in any one of them would make the other two depend on it for
// no reason — and a tool that imported an HTTP package to ask a question could
// never be reused by a different surface.
//
// Nothing here knows about SSE, HTTP, cards or timeouts: an Asker decides how a
// question is delivered, how long it may wait, and what "no answer" means.

// Option is one choice offered with a question.
//
// Label is the answer — it is what comes back in Answer.Selected, what the model
// reads, and what is persisted — so it must stand on its own. Description is the
// optional one-line explanation shown under it.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is what a tool asks.
type Question struct {
	// ID is the handle an answer comes back with. It is assigned by the Asker
	// (a question with no id cannot be answered, and only the Asker knows how to
	// mint one), so a caller leaves it empty.
	ID string `json:"id"`
	// Header is the short card title, e.g. "数据库选型". Optional: a surface with
	// no room for one (or a model that did not bother) still renders the Text.
	Header string `json:"header,omitempty"`
	// Text is the question itself.
	Text string `json:"text"`
	// Options are the offered choices; empty means "free text only".
	Options []Option `json:"options,omitempty"`
	// MultiSelect allows more than one option to be selected.
	MultiSelect bool `json:"multi_select,omitempty"`
	// AllowCustom lets the person answer in their own words instead of — or, with
	// MultiSelect, in addition to — the options. It is stated explicitly rather
	// than inferred from len(Options) == 0 so a surface never has to guess whether
	// a missing input box was intended.
	AllowCustom bool `json:"allow_custom"`
}

// AnswerStatus says how a question ended.
type AnswerStatus string

const (
	// AnswerAnswered means a person submitted an answer.
	AnswerAnswered AnswerStatus = "answered"
	// AnswerTimeout means nobody answered within the surface's wait limit. It is
	// not an error: the model gets to decide what to do next.
	AnswerTimeout AnswerStatus = "timeout"
	// AnswerCancelled means the turn ended (the browser went away, the user
	// stopped it, the deadline fired) while the question was pending.
	AnswerCancelled AnswerStatus = "cancelled"
)

// Answer is what came back.
//
// Selected holds option labels — never indices — so an answer stays readable in
// a log or a persisted tool result without the question next to it. Text is the
// person's own words, present only when the question allowed them.
type Answer struct {
	Status   AnswerStatus `json:"status"`
	Selected []string     `json:"selected,omitempty"`
	Text     string       `json:"text,omitempty"`
}

// Answered reports whether a person actually answered. A timeout and a
// cancellation are both "nobody answered", and every caller that cares about the
// difference asks about the status instead.
func (a Answer) Answered() bool { return a.Status == AnswerAnswered }

// Asker puts a question to the person on the other end of a turn and waits for
// their answer.
//
// The contract is deliberately small — one blocking call — because everything
// interesting is surface-specific: the web console streams the question down an
// open SSE response and wakes this call from a second HTTP request, while a
// surface with no way to ask simply never installs an Asker.
//
// Implementations must respect ctx: returning is what lets the turn finish, so a
// call that ignored cancellation would pin a goroutine for the life of the
// process. They must also return a zero error for a timeout or a cancellation —
// "nobody answered" is an answer, and the model has to be told it plainly rather
// than as a failed tool call.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// askerKey is the context key carrying the turn's Asker. Unexported so the only
// way to install one is WithAsker.
type askerKey struct{}

// WithAsker returns ctx carrying a, the answerer for this turn.
//
// A nil Asker returns ctx unchanged: "nobody can answer here" is the absence of
// the value, not a value that every reader has to nil-check.
func WithAsker(ctx context.Context, a Asker) context.Context {
	if a == nil || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, askerKey{}, a)
}

// AskerFrom returns the Asker installed on ctx, and whether there is one.
//
// A tool that gets false must say so instead of asking into the void: the model
// can then answer directly, which is the right behaviour on a surface with no
// cards (Feishu, the CLI REPL, a test).
func AskerFrom(ctx context.Context) (Asker, bool) {
	if ctx == nil {
		return nil, false
	}
	a, ok := ctx.Value(askerKey{}).(Asker)
	if !ok || a == nil {
		return nil, false
	}
	return a, true
}
