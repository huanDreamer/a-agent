package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/store"
)

// stubStore implements store.Store by embedding the interface: only the catalog
// methods the resolver reads are overridden, so the stub stays small and the
// compiler still guarantees it satisfies the interface.
//
// It is deliberately a map-and-slice fake rather than a SQLite fixture: the
// resolver's job is to decide from rows, and a fake makes the "exactly one
// candidate" and "disabled provider" cases explicit instead of accidental.
type stubStore struct {
	store.Store

	providers map[string]store.Provider
	models    []store.Model
	bindings  []store.Binding

	listModelsErr   error
	listBindingsErr error
	getProviderErr  error
	getModelErr     error
}

func (s *stubStore) ListBindings(context.Context) ([]store.Binding, error) {
	if s.listBindingsErr != nil {
		return nil, s.listBindingsErr
	}
	return s.bindings, nil
}

func (s *stubStore) ListModels(_ context.Context, providerID string) ([]store.Model, error) {
	if s.listModelsErr != nil {
		return nil, s.listModelsErr
	}
	if providerID == "" {
		return s.models, nil
	}
	var out []store.Model
	for _, m := range s.models {
		if m.ProviderID == providerID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *stubStore) GetModel(_ context.Context, providerID, modelID string) (store.Model, error) {
	if s.getModelErr != nil {
		return store.Model{}, s.getModelErr
	}
	for _, m := range s.models {
		if m.ProviderID == providerID && m.ModelID == modelID {
			return m, nil
		}
	}
	return store.Model{}, store.ErrNotFound
}

func (s *stubStore) GetProvider(_ context.Context, id string) (store.Provider, error) {
	if s.getProviderErr != nil {
		return store.Provider{}, s.getProviderErr
	}
	p, ok := s.providers[id]
	if !ok {
		return store.Provider{}, store.ErrNotFound
	}
	return p, nil
}

// enabledProvider is a usable provider row.
func enabledProvider(id string) store.Provider {
	return store.Provider{
		ID:      id,
		Name:    id,
		BaseURL: "https://" + id + ".example.com/v1",
		Kind:    "openai",
		Enabled: true,
	}
}

func visionModel(providerID, modelID string) store.Model {
	return store.Model{
		ProviderID:   providerID,
		ModelID:      modelID,
		Capabilities: store.Capabilities{store.CapVision},
		Enabled:      true,
	}
}

func TestStoreResolver_ExplicitBindingWins(t *testing.T) {
	// Two enabled vision models and no way to choose between them: only the
	// binding makes the choice deterministic.
	st := &stubStore{
		providers: map[string]store.Provider{
			"alpha": enabledProvider("alpha"),
			"beta":  enabledProvider("beta"),
		},
		models: []store.Model{
			visionModel("alpha", "alpha-vision"),
			visionModel("beta", "beta-vision"),
		},
		bindings: []store.Binding{
			{Capability: store.CapVision, ProviderID: "beta", ModelID: "beta-vision"},
		},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("binding should resolve")
	}
	if got.ProviderID != "beta" || got.ModelID != "beta-vision" {
		t.Errorf("resolved %s/%s, want beta/beta-vision", got.ProviderID, got.ModelID)
	}
	if got.BaseURL != "https://beta.example.com/v1" {
		t.Errorf("base url = %q", got.BaseURL)
	}
	if got.Kind != KindOpenAI {
		t.Errorf("kind = %q, want %q", got.Kind, KindOpenAI)
	}
}

func TestStoreResolver_SingleCandidateFallback(t *testing.T) {
	st := &stubStore{
		providers: map[string]store.Provider{
			"alpha": enabledProvider("alpha"),
			"beta":  enabledProvider("beta"),
		},
		models: []store.Model{
			visionModel("alpha", "alpha-vision"),
			// A chat and a speech model must not make the vision model look
			// ambiguous: only models declaring the capability count.
			{ProviderID: "beta", ModelID: "beta-chat", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			{ProviderID: "beta", ModelID: "beta-tts", Capabilities: store.Capabilities{store.CapAudioSpeech}, Enabled: true},
		},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("a lone candidate should resolve")
	}
	if got.ProviderID != "alpha" || got.ModelID != "alpha-vision" {
		t.Errorf("resolved %s/%s, want alpha/alpha-vision", got.ProviderID, got.ModelID)
	}
}

func TestStoreResolver_TwoCandidatesWithoutBindingIsAmbiguous(t *testing.T) {
	st := &stubStore{
		providers: map[string]store.Provider{
			"alpha": enabledProvider("alpha"),
			"beta":  enabledProvider("beta"),
		},
		models: []store.Model{
			visionModel("alpha", "alpha-vision"),
			visionModel("beta", "beta-vision"),
		},
	}
	if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
		t.Fatalf("two candidates must not resolve, got %+v", got)
	}
}

func TestStoreResolver_DisabledModelIsNotUsable(t *testing.T) {
	disabled := visionModel("alpha", "alpha-vision")
	disabled.Enabled = false
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
		models:    []store.Model{disabled},
		bindings: []store.Binding{
			{Capability: store.CapVision, ProviderID: "alpha", ModelID: "alpha-vision"},
		},
	}
	if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
		t.Fatalf("a disabled model must not resolve, got %+v", got)
	}
}

func TestStoreResolver_DisabledModelIsNotACandidate(t *testing.T) {
	// Same case without a binding: the disabled model must not count as one of
	// two candidates either.
	off := visionModel("beta", "beta-vision")
	off.Enabled = false
	st := &stubStore{
		providers: map[string]store.Provider{
			"alpha": enabledProvider("alpha"),
			"beta":  enabledProvider("beta"),
		},
		models: []store.Model{visionModel("alpha", "alpha-vision"), off},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok || got.ModelID != "alpha-vision" {
		t.Fatalf("got (%+v, %v), want the enabled model", got, ok)
	}
}

func TestStoreResolver_DisabledProviderIsNotUsable(t *testing.T) {
	off := enabledProvider("alpha")
	off.Enabled = false
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": off},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
		bindings: []store.Binding{
			{Capability: store.CapVision, ProviderID: "alpha", ModelID: "alpha-vision"},
		},
	}
	// A binding to a disabled provider is a hard no: the operator disabled the
	// provider, so the agent must not pick a different model behind their back.
	if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
		t.Fatalf("a disabled provider must not resolve, got %+v", got)
	}
}

func TestStoreResolver_DisabledProviderModelIsNotACandidate(t *testing.T) {
	alpha := enabledProvider("alpha")
	beta := enabledProvider("beta")
	beta.Enabled = false
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": alpha, "beta": beta},
		models: []store.Model{
			visionModel("alpha", "alpha-vision"),
			visionModel("beta", "beta-vision"),
		},
	}
	// The disabled provider's model is invisible, so the other one is the sole
	// candidate rather than a second half of an ambiguous pair.
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok || got.ModelID != "alpha-vision" {
		t.Fatalf("got (%+v, %v), want alpha-vision", got, ok)
	}
}

func TestStoreResolver_NothingConfigured(t *testing.T) {
	cases := map[string]*stubStore{
		"empty catalog": {},
		"no models":     {providers: map[string]store.Provider{"alpha": enabledProvider("alpha")}},
		"no such capability": {
			providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
			models:    []store.Model{{ProviderID: "alpha", ModelID: "alpha-chat", Capabilities: store.Capabilities{store.CapChat}, Enabled: true}},
		},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
				t.Fatalf("expected no resolution, got %+v", got)
			}
		})
	}
}

func TestStoreResolver_NilStoreAndUnknownCapability(t *testing.T) {
	if _, ok := NewResolver(nil, nil).For(context.Background(), store.CapVision); ok {
		t.Error("a nil store must not resolve")
	}
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
	}
	if _, ok := NewResolver(st, nil).For(context.Background(), store.Capability("telepathy")); ok {
		t.Error("an unknown capability must not resolve")
	}
	// A nil *StoreResolver must be safe to call: the tools build one from a
	// store that a caller may not have configured.
	var nilResolver *StoreResolver
	if _, ok := nilResolver.For(context.Background(), store.CapVision); ok {
		t.Error("a nil resolver must not resolve")
	}
}

func TestStoreResolver_BindingToMissingModelOrProvider(t *testing.T) {
	cases := map[string]*stubStore{
		"missing model row": {
			providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
			models:    []store.Model{visionModel("alpha", "alpha-vision")},
			bindings:  []store.Binding{{Capability: store.CapVision, ProviderID: "alpha", ModelID: "gone"}},
		},
		"missing provider row": {
			providers: map[string]store.Provider{},
			models:    []store.Model{visionModel("alpha", "alpha-vision")},
			bindings:  []store.Binding{{Capability: store.CapVision, ProviderID: "alpha", ModelID: "alpha-vision"}},
		},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
				t.Fatalf("expected no resolution, got %+v", got)
			}
		})
	}
}

func TestStoreResolver_UnusableProviderRows(t *testing.T) {
	cases := map[string]store.Provider{
		"blank base url": {ID: "alpha", Enabled: true, Kind: "openai"},
		"unknown kind":   {ID: "alpha", Enabled: true, Kind: "anthropic", BaseURL: "https://alpha.example.com"},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			st := &stubStore{
				providers: map[string]store.Provider{"alpha": p},
				models:    []store.Model{visionModel("alpha", "alpha-vision")},
			}
			if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
				t.Fatalf("expected no resolution, got %+v", got)
			}
		})
	}
}

func TestStoreResolver_BlankKindDefaultsToOpenAI(t *testing.T) {
	p := enabledProvider("alpha")
	p.Kind = ""
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": p},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("a blank kind should default to the OpenAI protocol")
	}
	if got.Kind != KindOpenAI {
		t.Errorf("kind = %q, want %q", got.Kind, KindOpenAI)
	}
}

func TestStoreResolver_TrailingSlashOnBaseURLIsTrimmed(t *testing.T) {
	p := enabledProvider("alpha")
	p.BaseURL = "https://alpha.example.com/v1/"
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": p},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("expected a resolution")
	}
	// Otherwise every endpoint would be built as ".../v1//chat/completions".
	if got.BaseURL != "https://alpha.example.com/v1" {
		t.Errorf("base url = %q", got.BaseURL)
	}
}

func TestStoreResolver_APIKeyComesFromTheEnvironment(t *testing.T) {
	p := enabledProvider("alpha")
	p.APIKeyEnv = "HUAN_TEST_VISION_KEY"
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": p},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
	}
	env := map[string]string{"HUAN_TEST_VISION_KEY": "sk-secret"}
	got, ok := NewResolver(st, func(k string) string { return env[k] }).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("expected a resolution")
	}
	if got.APIKey != "sk-secret" {
		t.Errorf("api key = %q, want the environment value", got.APIKey)
	}

	// An unset variable is not an error: a local provider needs no key.
	got, ok = NewResolver(st, func(string) string { return "" }).For(context.Background(), store.CapVision)
	if !ok {
		t.Fatal("a provider without a key should still resolve")
	}
	if got.APIKey != "" {
		t.Errorf("api key = %q, want empty", got.APIKey)
	}

	// A nil lookup falls back to os.Getenv rather than panicking.
	t.Setenv("HUAN_TEST_VISION_KEY", "sk-from-env")
	got, ok = NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok || got.APIKey != "sk-from-env" {
		t.Errorf("got (%q, %v), want the process environment value", got.APIKey, ok)
	}
}

func TestStoreResolver_StoreErrorsYieldFalse(t *testing.T) {
	boom := errors.New("database is locked")
	cases := map[string]*stubStore{
		"list bindings fails": {listBindingsErr: boom},
		"list models fails":   {listModelsErr: boom},
		"get model fails": {
			providers:   map[string]store.Provider{"alpha": enabledProvider("alpha")},
			bindings:    []store.Binding{{Capability: store.CapVision, ProviderID: "alpha", ModelID: "alpha-vision"}},
			getModelErr: boom,
		},
		"get provider fails": {
			models:         []store.Model{visionModel("alpha", "alpha-vision")},
			bindings:       []store.Binding{{Capability: store.CapVision, ProviderID: "alpha", ModelID: "alpha-vision"}},
			getProviderErr: boom,
		},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision); ok {
				t.Fatalf("a store error must not resolve, got %+v", got)
			}
		})
	}
}

func TestStoreResolver_HalfWrittenBindingFallsBackToTheSoleCandidate(t *testing.T) {
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
		models:    []store.Model{visionModel("alpha", "alpha-vision")},
		bindings:  []store.Binding{{Capability: store.CapVision}},
	}
	got, ok := NewResolver(st, nil).For(context.Background(), store.CapVision)
	if !ok || got.ModelID != "alpha-vision" {
		t.Fatalf("got (%+v, %v), want the sole candidate", got, ok)
	}
}

func TestStoreResolver_ResolvesEveryMediaCapability(t *testing.T) {
	st := &stubStore{
		providers: map[string]store.Provider{"alpha": enabledProvider("alpha")},
		models: []store.Model{
			{ProviderID: "alpha", ModelID: "alpha-vision", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
			{ProviderID: "alpha", ModelID: "alpha-image", Capabilities: store.Capabilities{store.CapImageGen}, Enabled: true},
			{ProviderID: "alpha", ModelID: "alpha-whisper", Capabilities: store.Capabilities{store.CapAudioTranscribe}, Enabled: true},
		},
	}
	r := NewResolver(st, nil)
	for _, tc := range []struct {
		capability store.Capability
		wantModel  string
	}{
		{store.CapVision, "alpha-vision"},
		{store.CapImageGen, "alpha-image"},
		{store.CapAudioTranscribe, "alpha-whisper"},
	} {
		t.Run(string(tc.capability), func(t *testing.T) {
			got, ok := r.For(context.Background(), tc.capability)
			if !ok || got.ModelID != tc.wantModel {
				t.Fatalf("got (%+v, %v), want %s", got, ok, tc.wantModel)
			}
		})
	}
	// A capability no model declares is unbound even though other models exist.
	if _, ok := r.For(context.Background(), store.CapAudioSpeech); ok {
		t.Error("an undeclared capability must not resolve")
	}
}

func TestTarget_CallTimeout(t *testing.T) {
	if got := (Target{}).CallTimeout(); got != DefaultTimeout {
		t.Errorf("zero timeout = %v, want the default %v", got, DefaultTimeout)
	}
	if got := (Target{Timeout: 3 * time.Second}).CallTimeout(); got != 3*time.Second {
		t.Errorf("explicit timeout = %v, want 3s", got)
	}
}
