package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry holds a set of named Tools plus an optional allow-list.
//
// Concurrency: every method takes the lock, so the registry may be mutated
// while another goroutine lists it. That matters for the MCP runtime, which
// registers and unregisters tools (a server added, edited or deleted in
// 设置 → MCP) while a chat turn may be assembling its tool list.
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

// Empty reports whether the registry permits no tools at all.
//
// It is what a caller checks before running something that needs a tool set: an
// empty registry is not a smaller agent, it is an agent that can only talk.
func (r *Registry) Empty() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name := range r.tools {
		// Respect the allow-list: a registry whose tools are all excluded permits
		// nothing, whatever it holds.
		if !r.allowListSet {
			return false
		}
		if _, ok := r.allowList[name]; ok {
			return false
		}
	}
	return true
}

// Get returns the tool registered under name and whether it exists.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Unregister removes the named tool and returns whether it was there.
//
// It exists for tools whose backing resource can go away at runtime: an MCP
// server the operator deleted (or switched off) must take its tools with it,
// otherwise the model keeps being offered a tool that can only fail. The
// allow-list entry is dropped too, so a later Register of the same name starts
// from the same state as a first registration.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	delete(r.allowList, name)
	return true
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

// Clone returns an independent copy of the registry: the same tools under the
// same names, the same allow-list state, and no aliasing between the two.
//
// It exists for callers that need a slightly different tool set than everyone
// else's without disturbing the shared registry — the case it was written for is
// a conversation bound to a workspace, whose filesystem tools must be rebuilt
// against that workspace and whose write tools must be absent when it is
// read-only. The clone carries the tools registered at runtime (MCP servers, the
// skill tool) because it copies whatever the registry holds at the moment it is
// called, which is why it is taken per turn rather than cached.
//
// A nil registry clones to an empty one rather than panicking: a caller that had
// no tools still ends up with no tools.
func (r *Registry) Clone() *Registry {
	out := NewRegistry()
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, t := range r.tools {
		out.tools[name] = t
	}
	out.allowListSet = r.allowListSet
	for name := range r.allowList {
		out.allowList[name] = struct{}{}
	}
	return out
}

// Replace swaps the implementation registered under a tool's own name, keeping
// its allow-list membership exactly as it was.
//
// It is the other half of Clone: the tools that are not workspace-bound are
// carried over by reference, and the ones that are get rebuilt against the
// current workspace and swapped in here. Membership is preserved rather than
// recomputed so that replacing a tool can never widen what the model may call —
// a tool the allow-list excluded stays excluded, and one the registry never had
// is simply added (there was no membership to preserve).
//
// The name is taken from the tool's own Info, so a factory cannot silently
// register under a different name than the tool it replaced.
func (r *Registry) Replace(t Tool) error {
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
	_, existed := r.tools[info.Name]
	r.tools[info.Name] = t
	if !r.allowListSet {
		// "Allow all" is represented by membership, so a new name has to join.
		r.allowList[info.Name] = struct{}{}
	} else if !existed {
		// An explicit allow-list that never named this tool must not gain it.
		delete(r.allowList, info.Name)
	}
	return nil
}

// List returns the LLM-facing specs for every tool currently
// permitted by the allow-list, in deterministic name order.
//
// The registry is snapshotted under the read lock and the (possibly blocking)
// Info calls happen outside it, so registering a tool never has to wait for a
// slow one — and a concurrent Register/Unregister cannot make the iteration
// below observe a half-updated map.
func (r *Registry) List(ctx context.Context) ([]Spec, error) {
	r.mu.RLock()
	names := r.permittedLocked()
	snapshot := make([]Tool, 0, len(names))
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			snapshot = append(snapshot, t)
		}
	}
	r.mu.RUnlock()

	out := make([]Spec, 0, len(snapshot))
	for _, t := range snapshot {
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
