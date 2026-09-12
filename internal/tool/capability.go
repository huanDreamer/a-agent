package tool

import (
	"context"
	"fmt"
	"sort"
)

// Capability describes what a registered tool can do, so a caller can decide
// whether to expose it (for example: give a chat surface read-only tools and
// withhold writes).
type Capability string

const (
	// CapRead observes state without changing anything.
	CapRead Capability = "read"
	// CapWrite changes files.
	CapWrite Capability = "write"
	// CapExec runs commands, which can do anything the process user can.
	CapExec Capability = "exec"
)

// Capable is implemented by tools that declare their capability. Tools that do
// not implement it are treated as CapRead, because a tool that has not declared
// otherwise should not be assumed harmless — callers that need certainty should
// use an explicit allow-list instead.
type Capable interface {
	Capability() Capability
}

// CapabilityOf reports a tool's declared capability.
func CapabilityOf(t Tool) Capability {
	if c, ok := t.(Capable); ok {
		return c.Capability()
	}
	return CapRead
}

// CapabilityFunc adapts a function to the Capable interface, so a tool built
// with eino's utils.InferTool can be wrapped without redefining it.
type CapabilityFunc struct {
	Tool
	Cap Capability
}

// Capability implements Capable.
func (c CapabilityFunc) Capability() Capability { return c.Cap }

// WithCapability tags a tool with a capability. It returns the tool unchanged
// when cap is empty.
func WithCapability(t Tool, cap Capability) Tool {
	if t == nil || cap == "" {
		return t
	}
	return CapabilityFunc{Tool: t, Cap: cap}
}

// FilterByCapabilities returns a registry restricted to tools whose capability
// appears in allow. An empty allow list means "no restriction", which keeps the
// current behaviour for callers that do not care.
//
// This is the hook a surface uses to be safer than the agent as a whole: the web
// admin is authenticated and local, while an IM bot can be reached by anyone who
// can message it.
func FilterByCapabilities(src *Registry, allow ...Capability) (*Registry, error) {
	if src == nil {
		return NewRegistry(), nil
	}
	if len(allow) == 0 {
		return src, nil
	}
	permitted := make(map[Capability]bool, len(allow))
	for _, c := range allow {
		permitted[c] = true
	}

	// Iterate what the source actually permits, not everything it has
	// registered: Names() lists all registered tools, so filtering from it
	// would resurrect a tool the source's own allow-list had excluded — a
	// filtering step that widens access is worse than none.
	permittedSpecs, err := src.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("tool: list for filtering: %w", err)
	}
	sort.Slice(permittedSpecs, func(i, j int) bool { return permittedSpecs[i].Name < permittedSpecs[j].Name })

	out := NewRegistry()
	for _, spec := range permittedSpecs {
		t, ok := src.Get(spec.Name)
		if !ok {
			continue
		}
		if !permitted[CapabilityOf(t)] {
			continue
		}
		if err := out.Register(t); err != nil {
			return nil, fmt.Errorf("tool: filter %q: %w", spec.Name, err)
		}
	}
	return out, nil
}

// DescribeCapabilities returns a name→capability map, for logging and for the
// admin UI to show what the agent can do.
func DescribeCapabilities(ctx context.Context, r *Registry) map[string]Capability {
	out := map[string]Capability{}
	if r == nil {
		return out
	}
	// List returns the permitted specs, so this respects the allow-list.
	specs, err := r.List(ctx)
	if err != nil {
		return out
	}
	for _, s := range specs {
		t, ok := r.Get(s.Name)
		if !ok {
			continue
		}
		out[s.Name] = CapabilityOf(t)
	}
	return out
}
