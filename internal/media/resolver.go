// Package media answers one question: which of the models the operator has
// configured serves this capability?
//
// It is the seam between the catalog (internal/store) and the media tools, so
// that each tool does not query the database itself and so that a tool is only
// ever constructed with a target it can actually use. A tool whose model is
// unbound must not be registered at all: a model that can see a tool it cannot
// serve will spend a turn calling it.
//
// Note the deliberate name collision with internal/tool.Capability. They mean
// different things: tool.Capability is what a tool does to the machine
// (read/write/exec), while store.Capability is what a model can do with an
// input (vision, image generation, transcription, ...). This package is about
// the second one.
package media

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/huan/huan-agent/internal/store"
)

const (
	// KindOpenAI is the only protocol family this build can speak: every
	// provider in the catalog is reached over the OpenAI-compatible HTTP API.
	KindOpenAI = "openai"
	// DefaultTimeout bounds one outbound media call, so a provider that accepts
	// the connection and then stalls cannot hold an agent turn open forever.
	DefaultTimeout = 120 * time.Second
)

// Target is a resolved provider+model, with everything needed to sign a
// request. It is a value, not a handle: a tool built from one is a pure
// function of its configuration.
type Target struct {
	ProviderID string
	ModelID    string
	BaseURL    string
	APIKey     string
	// Kind is the provider's protocol family ("openai" today).
	Kind string
	// Timeout bounds one outbound call. Zero means DefaultTimeout; a caller may
	// raise or lower it after resolution without touching the resolver.
	Timeout time.Duration
}

// CallTimeout returns the timeout one outbound call should use.
func (t Target) CallTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultTimeout
}

// Resolver answers "which model serves this capability?" and hands back a
// ready-to-use client configuration.
type Resolver interface {
	// For returns the provider+model bound to a capability, with everything
	// needed to sign a request. ok=false means nothing is bound, which is what
	// makes a tool unavailable.
	For(ctx context.Context, capability store.Capability) (Target, bool)
}

// StoreResolver resolves capabilities from the model catalog. Resolution reads
// the catalog at tool-construction time only, so it caches nothing: a tool
// registry is rebuilt on the next start, and a cache would only add a way for
// a stale binding to outlive the operator's change.
type StoreResolver struct {
	store store.Store
	// lookupEnv reads a provider's API key from the environment. It is a field
	// rather than a direct os.Getenv call so a test can supply its own answer,
	// and so nothing in this package reaches into the process environment
	// behind the caller's back.
	lookupEnv func(string) string
}

// NewResolver returns a Resolver over st. lookupEnv is used to read a
// provider's key from its APIKeyEnv; nil means os.Getenv.
func NewResolver(st store.Store, lookupEnv func(string) string) *StoreResolver {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	return &StoreResolver{store: st, lookupEnv: lookupEnv}
}

// compile-time check that the concrete resolver satisfies the interface.
var _ Resolver = (*StoreResolver)(nil)

// For resolves a capability.
//
// Order of preference:
//
//  1. an explicit binding for the capability, which is the operator saying
//     exactly which model to use;
//  2. otherwise, if exactly one enabled model anywhere declares the
//     capability, that one — convenient, and the common case for a single
//     provider;
//  3. otherwise nothing is bound.
//
// The rules that decide "usable":
//
//   - the model row must exist and be Enabled;
//   - its provider must exist and be Enabled. A disabled provider is not used
//     even when the capability is explicitly bound to one of its models:
//     disabling a provider is how an operator switches it off, and silently
//     falling through to a different model would be the agent second-guessing
//     that instruction, so a binding to a disabled model or provider yields
//     false rather than a fallback;
//   - the provider must have a base URL, and its Kind must be a protocol this
//     build speaks. A tool registered against an endpoint that cannot answer is
//     exactly the failure this resolver exists to prevent;
//   - the API key may be empty: a local provider (ollama, LM Studio) needs
//     none.
//
// Every failure path returns ok=false rather than an error. A half-configured
// or freshly migrated catalog must not stop the server from starting, and
// "this capability is not usable" is a normal state, not a fault.
func (r *StoreResolver) For(ctx context.Context, capability store.Capability) (Target, bool) {
	if r == nil || r.store == nil || !store.ValidCapability(capability) {
		return Target{}, false
	}

	// 1. An explicit binding wins.
	if providerID, modelID, bound := r.bindingFor(ctx, capability); bound {
		return r.targetFor(ctx, providerID, modelID)
	}

	// 2. Otherwise the sole candidate.
	return r.soleCandidateFor(ctx, capability)
}

// bindingFor returns the explicit binding for a capability, if there is one.
func (r *StoreResolver) bindingFor(ctx context.Context, capability store.Capability) (providerID, modelID string, ok bool) {
	bindings, err := r.store.ListBindings(ctx)
	if err != nil {
		return "", "", false
	}
	for _, b := range bindings {
		if b.Capability != capability {
			continue
		}
		if strings.TrimSpace(b.ProviderID) == "" || strings.TrimSpace(b.ModelID) == "" {
			// A half-written row is not a binding.
			return "", "", false
		}
		return b.ProviderID, b.ModelID, true
	}
	return "", "", false
}

// soleCandidateFor returns the only enabled model that declares the capability.
// Two candidates are ambiguous, and picking one of them arbitrarily would make
// the agent's behaviour depend on row order, so it yields false: the operator
// has to bind one.
func (r *StoreResolver) soleCandidateFor(ctx context.Context, capability store.Capability) (Target, bool) {
	models, err := r.store.ListModels(ctx, "")
	if err != nil {
		return Target{}, false
	}

	var (
		found Target
		count int
	)
	for _, m := range models {
		if !m.Enabled || !m.Has(capability) {
			continue
		}
		// A model under a disabled or unspeakable provider is not a candidate:
		// counting it would make one usable model look ambiguous.
		t, ok := r.targetFor(ctx, m.ProviderID, m.ModelID)
		if !ok {
			continue
		}
		count++
		if count > 1 {
			return Target{}, false
		}
		found = t
	}
	if count != 1 {
		return Target{}, false
	}
	return found, true
}

// targetFor loads a model and its provider and reduces them to a Target. Any
// missing row, disabled row or unusable provider yields ok=false.
func (r *StoreResolver) targetFor(ctx context.Context, providerID, modelID string) (Target, bool) {
	m, err := r.store.GetModel(ctx, providerID, modelID)
	if err != nil || !m.Enabled {
		return Target{}, false
	}
	p, err := r.store.GetProvider(ctx, providerID)
	if err != nil || !p.Enabled {
		return Target{}, false
	}

	baseURL := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if baseURL == "" {
		return Target{}, false
	}
	kind := strings.TrimSpace(p.Kind)
	if kind == "" {
		kind = KindOpenAI
	}
	if kind != KindOpenAI {
		return Target{}, false
	}

	return Target{
		ProviderID: p.ID,
		ModelID:    m.ModelID,
		BaseURL:    baseURL,
		// The key is resolved here and never logged: it travels to the tool
		// that signs the request and nowhere else.
		APIKey: p.ResolveAPIKey(r.lookupEnv),
		Kind:   kind,
	}, true
}
