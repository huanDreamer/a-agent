package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry holds a set of named Tools plus an optional allow-list.
//
// Concurrency: Registry is safe for concurrent reads after
// construction; Register/SetAllowList are expected to be called at
// startup and not concurrently with List/Get.
type Registry struct {
	mu           sync.RWMutex
	tools        map[string]Tool
	allowList    map[string]struct{} // empty + allowListSet=false => allow all
	allowListSet bool                // true once SetAllowList has been called
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:     map[string]Tool{},
		allowList: map[string]struct{}{},
	}
}

// allowListSet distinguishes "default = allow all" (no SetAllowList
// call yet) from "explicit empty list = allow nothing".
func (r *Registry) isAllowListSet() bool { return r.allowListSet }

// Register adds t to the registry. Duplicate names return an error
// and the registry is left unchanged.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool: nil tool")
	}
	info, err := t.Info(context.Background())
	if err != nil {
		return fmt.Errorf("tool: info: %w", err)
	}
	if info.Name == "" {
		return fmt.Errorf("tool: empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[info.Name]; exists {
		return fmt.Errorf("tool: %q already registered", info.Name)
	}
	r.tools[info.Name] = t
	// New tools join the allow-list implicitly if it is currently
	// "allow all" (i.e. no SetAllowList call has happened). This keeps
	// Register-then-List simple for callers that never use allow-lists.
	if !r.allowListSet {
		r.allowList[info.Name] = struct{}{}
	}
	return nil
}

// Get returns the tool registered under name and whether it exists.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns all registered tool names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// List returns the LLM-facing specs for every tool currently
// permitted by the allow-list, in deterministic name order.
func (r *Registry) List(ctx context.Context) ([]Spec, error) {
	r.mu.RLock()
	permitted := r.permittedLocked()
	r.mu.RUnlock()

	out := make([]Spec, 0, len(permitted))
	for _, name := range permitted {
		t, ok := r.tools[name]
		if !ok {
			continue
		}
		spec, err := SpecOf(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// SetAllowList restricts the registry to only the named tools. An
// empty / nil list means "allow everything" (the default).
// Every name MUST already be registered, otherwise the call fails
// and the allow-list is left unchanged.
func (r *Registry) SetAllowList(names []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Snapshot current tools.
	tools := make(map[string]struct{}, len(r.tools))
	for n := range r.tools {
		tools[n] = struct{}{}
	}

	if len(names) == 0 {
		r.allowList = map[string]struct{}{}
		for n := range r.tools {
			r.allowList[n] = struct{}{}
		}
		r.allowListSet = true
		return nil
	}

	next := make(map[string]struct{}, len(names))
	for _, n := range names {
		if _, ok := tools[n]; !ok {
			return fmt.Errorf("tool: allow-list references unknown tool %q (registered: %v)", n, sortedStringKeys(r.tools))
		}
		next[n] = struct{}{}
	}
	r.allowList = next
	r.allowListSet = true
	return nil
}

// AllowList returns the names currently permitted, in deterministic
// order. Empty result with no error means "all tools permitted".
func (r *Registry) AllowList() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.allowListSet {
		return append([]string(nil), sortedStringKeys(r.tools)...)
	}
	return sortedKeys(r.allowList)
}

// IsAllowed reports whether name is currently permitted for LLM use.
func (r *Registry) IsAllowed(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isAllowedLocked(name)
}

func (r *Registry) isAllowedLocked(name string) bool {
	if !r.allowListSet {
		_, ok := r.tools[name]
		return ok
	}
	_, ok := r.allowList[name]
	return ok
}

func (r *Registry) permittedLocked() []string {
	if !r.allowListSet {
		return sortedStringKeys(r.tools)
	}
	return sortedKeys(r.allowList)
}

// sortedStringKeys is the generic form of sortedKeys so callers can
// pass a map of either struct{} or Tool values.
func sortedStringKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]struct{}) []string { return sortedStringKeys(m) }
