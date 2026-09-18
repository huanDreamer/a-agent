package tool

import (
	"context"
	"fmt"
	"sort"
)

// Whether a tool may run at the same time as another tool from the same model
// reply.
//
// This is a **separate axis from Capability**, and keeping them apart is the whole
// point. Capability answers "does this change the workspace"; concurrency answers
// "may this overlap another call". The two look similar and are not:
//
//   - `ask_user` is CapRead (it reads nothing and changes nothing) and must never
//     run beside anything — it parks the whole turn waiting for a person.
//   - `plan_update` is CapRead and does a read-modify-write on the plan: two of
//     them at once lose an update and scramble the board the user is watching.
//   - `save_document` is CapRead and writes to a document store.
//
// So inferring "parallel-safe" from "read-only" would be wrong for three of the
// tools this repository already has. The declaration is explicit, and the default
// is the safe one.
type Concurrency string

const (
	// Unspecified is what a tool that has not declared anything reports. It
	// behaves as Serial, but it is a distinct value so a test can assert that
	// every registered tool made an explicit choice: a new tool that silently
	// defaults is a new tool nobody thought about.
	Unspecified Concurrency = ""
	// Serial means this call is a barrier: everything before it finishes first,
	// and nothing after it starts until it is done.
	//
	// That is not a performance compromise, it is what the model's program order
	// means. "Write the config, then run the tests" is two calls with a dependency
	// the model expressed by ordering them; running them at once tests the old
	// config.
	Serial Concurrency = "serial"
	// ParallelSafe means this call may overlap other ParallelSafe calls in the
	// same reply. It must be true of the tool in general, not of one invocation:
	// two greps are fine, and so are two reads of the same file.
	ParallelSafe Concurrency = "parallel"
)

// Concurrent is implemented by tools that declare their concurrency.
type Concurrent interface {
	Concurrency() Concurrency
}

// ConcurrencyOf reports a tool's declared mode, or Unspecified when it has not
// declared one.
//
// Callers that are about to run something should use EffectiveConcurrency instead;
// this one exists so a test can tell "declared Serial" from "never thought about
// it".
func ConcurrencyOf(t Tool) Concurrency {
	if c, ok := t.(Concurrent); ok {
		if c.Concurrency() != Unspecified {
			return c.Concurrency()
		}
	}
	return Unspecified
}

// EffectiveConcurrency is ConcurrencyOf with the default applied: a tool that
// declares nothing is treated as Serial.
//
// The default is the conservative direction on purpose. An undeclared tool is one
// nobody considered, and running it beside something else is the choice that can
// corrupt data; running it alone is merely slower.
func EffectiveConcurrency(t Tool) Concurrency {
	if mode := ConcurrencyOf(t); mode != Unspecified {
		return mode
	}
	return Serial
}

// concurrencyFunc adapts a declaration onto a tool, the same way CapabilityFunc
// does for capabilities.
type concurrencyFunc struct {
	Tool
	mode Concurrency
}

// Concurrency implements Concurrent.
func (c concurrencyFunc) Concurrency() Concurrency { return c.mode }

// Capability delegates to the wrapped tool.
//
// It has to be stated explicitly: embedding the Tool interface promotes only that
// interface's methods, and Capability is not one of them. Without this line a
// decorated write tool would report the default (CapRead) and survive a read-only
// filter — which is exactly the bug the approval gate had.
func (c concurrencyFunc) Capability() Capability { return CapabilityOf(c.Tool) }

// WithConcurrency declares a tool's concurrency. It returns the tool unchanged
// when the mode is unspecified, so a decorator never appears where nothing was
// declared.
func WithConcurrency(t Tool, mode Concurrency) Tool {
	if t == nil || mode == Unspecified {
		return t
	}
	return concurrencyFunc{Tool: t, mode: mode}
}

// DescribeConcurrency returns a name → mode map, for logging and for the console,
// which shows the operator what the agent will and will not overlap.
func DescribeConcurrency(ctx context.Context, r *Registry) map[string]Concurrency {
	out := map[string]Concurrency{}
	if r == nil {
		return out
	}
	specs, err := r.List(ctx)
	if err != nil {
		return out
	}
	for _, s := range specs {
		t, ok := r.Get(s.Name)
		if !ok {
			continue
		}
		out[s.Name] = EffectiveConcurrency(t)
	}
	return out
}

// UndeclaredTools names every registered tool that has not declared its
// concurrency, in a deterministic order.
//
// It is what the "every tool made a choice" test asserts on, and what a startup
// check could log: a tool added without a declaration still works, and the fact
// that nobody decided is worth being able to see.
func UndeclaredTools(ctx context.Context, r *Registry) []string {
	if r == nil {
		return nil
	}
	specs, err := r.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range specs {
		t, ok := r.Get(s.Name)
		if !ok {
			continue
		}
		if ConcurrencyOf(t) == Unspecified {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Segment splits a program-order list of modes into the groups that may run
// together.
//
// The result is a partition of the indices, in order, where:
//
//   - a maximal run of consecutive ParallelSafe entries becomes one group, and
//   - every other entry becomes a group of its own, which is what makes it a
//     barrier.
//
// Indices rather than values because the caller has to put the results back in
// program order, and the whole point of the plan is that the *schedule* may
// reorder execution while the *results* may not be reordered.
func Segment(modes []Concurrency) [][]int {
	var groups [][]int
	var run []int
	flush := func() {
		if len(run) > 0 {
			groups = append(groups, run)
			run = nil
		}
	}
	for i, m := range modes {
		if m == ParallelSafe {
			run = append(run, i)
			continue
		}
		flush()
		groups = append(groups, []int{i})
	}
	flush()
	return groups
}

// ValidateConcurrency reports a declaration that cannot be honoured.
func ValidateConcurrency(name string, mode Concurrency) error {
	switch mode {
	case ParallelSafe, Serial:
		return nil
	case Unspecified:
		return fmt.Errorf("tool %q has not declared a concurrency mode; declare ParallelSafe or Serial", name)
	default:
		return fmt.Errorf("tool %q declares an unknown concurrency mode %q (want parallel or serial)", name, mode)
	}
}
