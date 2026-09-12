package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func newCatalogStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestCapabilities_RoundTrip(t *testing.T) {
	in := Capabilities{CapVision, CapChat, CapAudioTranscribe}
	// Stored order follows AllCapabilities, so the value is stable across saves
	// and a diff of the row is readable.
	if got := in.String(); got != "chat,vision,audio_transcribe" {
		t.Errorf("String = %q, want chat,vision,audio_transcribe", got)
	}
	got := ParseCapabilities("chat,vision,audio_transcribe")
	if len(got) != 3 || !got.Has(CapChat) || !got.Has(CapVision) || !got.Has(CapAudioTranscribe) {
		t.Errorf("parsed = %v", got)
	}
}

func TestCapabilities_ParsingIsForgiving(t *testing.T) {
	// Unknown or duplicated entries are dropped rather than stored, so a typo
	// cannot silently become a capability nothing understands.
	got := ParseCapabilities(" chat ,, vision ,nonsense,chat")
	if len(got) != 2 || !got.Has(CapChat) || !got.Has(CapVision) {
		t.Errorf("parsed = %v, want chat+vision", got)
	}
	if len(ParseCapabilities("")) != 0 {
		t.Error("an empty string should parse to nothing")
	}
	if ValidCapability("nonsense") {
		t.Error("an unknown capability must not validate")
	}
	if !ValidCapability(CapVision) {
		t.Error("a known capability must validate")
	}
}

func TestProvider_KeyIsNeverSerialised(t *testing.T) {
	// The whole point of the unexported field: a handler that returns a
	// Provider cannot leak the secret by forgetting to strip it.
	st := newCatalogStore(t)
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, Provider{
		ID: "p1", Name: "P1", BaseURL: "https://api.example.com/v1", apiKey: "sk-secret-value",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	p, err := st.GetProvider(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if s := string(b); contains(s, "sk-secret-value") {
		t.Errorf("the API key leaked into JSON: %s", s)
	}
	if !contains(string(b), `"has_api_key":true`) {
		t.Errorf("JSON should report that a key exists: %s", b)
	}
	// The hint is a suffix only, never the whole key.
	if p.APIKeyHint != "…alue" {
		t.Errorf("hint = %q, want …alue", p.APIKeyHint)
	}
	if got := p.ResolveAPIKey(nil); got != "sk-secret-value" {
		t.Errorf("ResolveAPIKey = %q", got)
	}
}

func TestProvider_EnvReferenceWinsOverStoredKey(t *testing.T) {
	// A key in the environment is the better practice, so it must take
	// precedence over a stale copy in the database.
	st := newCatalogStore(t)
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, Provider{
		ID: "p1", BaseURL: "https://x/v1", apiKey: "stored-key", APIKeyEnv: "MY_KEY",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	p, _ := st.GetProvider(ctx, "p1")

	env := func(k string) string {
		if k == "MY_KEY" {
			return "env-key"
		}
		return ""
	}
	if got := p.ResolveAPIKey(env); got != "env-key" {
		t.Errorf("ResolveAPIKey = %q, want env-key", got)
	}
	// An empty env var falls back to the stored key rather than sending none.
	empty := func(string) string { return "" }
	if got := p.ResolveAPIKey(empty); got != "stored-key" {
		t.Errorf("ResolveAPIKey with a blank env = %q, want the stored key", got)
	}
	// A provider with only an env reference reports a key without storing one.
	if err := st.UpsertProvider(ctx, Provider{ID: "p2", APIKeyEnv: "OTHER_KEY"}); err != nil {
		t.Fatalf("upsert p2: %v", err)
	}
	p2, _ := st.GetProvider(ctx, "p2")
	if !p2.HasAPIKey {
		t.Error("an env reference should count as having a key")
	}
}

func TestProvider_UpsertKeepsKeyWhenBlank(t *testing.T) {
	// The UI never receives the key, so saving an unrelated field must not wipe
	// it — otherwise editing a base URL would silently break the provider.
	st := newCatalogStore(t)
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, Provider{ID: "p", BaseURL: "https://a/v1", apiKey: "keepme"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertProvider(ctx, Provider{ID: "p", BaseURL: "https://b/v1", Name: "renamed"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	p, _ := st.GetProvider(ctx, "p")
	if got := p.ResolveAPIKey(nil); got != "keepme" {
		t.Errorf("key = %q, want it preserved", got)
	}
	if p.BaseURL != "https://b/v1" || p.Name != "renamed" {
		t.Errorf("other fields were not updated: %+v", p)
	}
}

func TestProvider_SetKeyClearsAndReportsMissing(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "p", apiKey: "old"})

	if err := st.SetProviderKey(ctx, "p", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	p, _ := st.GetProvider(ctx, "p")
	if p.HasAPIKey {
		t.Error("clearing the key should leave no key")
	}
	// Setting a key on a provider that does not exist must fail rather than
	// report success for a write that went nowhere.
	if err := st.SetProviderKey(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetProvider(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
}

func TestProvider_DeleteCascades(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "p"})
	_ = st.UpsertModel(ctx, Model{ProviderID: "p", ModelID: "m", Capabilities: Capabilities{CapVision}})
	_ = st.SetBinding(ctx, Binding{Capability: CapVision, ProviderID: "p", ModelID: "m"})

	if err := st.DeleteProvider(ctx, "p"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	models, _ := st.ListModels(ctx, "p")
	if len(models) != 0 {
		t.Errorf("models survived the provider delete: %v", models)
	}
	// A binding left pointing at a deleted provider would make a tool look
	// configured when it cannot work.
	bindings, _ := st.ListBindings(ctx)
	if len(bindings) != 0 {
		t.Errorf("bindings survived: %v", bindings)
	}
}

func TestModels_ReplaceFetchedPreservesUserEdits(t *testing.T) {
	// A refresh must not discard capability flags the user set, nor a model the
	// provider's list happens to omit.
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "p"})
	_ = st.UpsertModel(ctx, Model{
		ProviderID: "p", ModelID: "gpt-4o", Capabilities: Capabilities{CapChat, CapVision}, Source: "user",
	})
	_ = st.UpsertModel(ctx, Model{
		ProviderID: "p", ModelID: "hand-added", Capabilities: Capabilities{CapChat}, Source: "user",
	})

	if err := st.ReplaceFetchedModels(ctx, "p", []Model{
		{ProviderID: "p", ModelID: "gpt-4o", DisplayName: "GPT-4o"},
		{ProviderID: "p", ModelID: "new-model"},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	models, err := st.ListModels(ctx, "p")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ModelID] = m
	}

	if len(models) != 3 {
		t.Fatalf("got %d models, want 3 (the hand-added one must survive): %v", len(models), byID)
	}
	// The user's capability flags must survive a refresh.
	if !byID["gpt-4o"].Has(CapVision) {
		t.Error("a refresh discarded the user's vision flag on gpt-4o")
	}
	if _, ok := byID["hand-added"]; !ok {
		t.Error("the hand-added model was dropped by a refresh")
	}
	// And the newly discovered one is recorded with its fetch time.
	if byID["new-model"].FetchedAt == nil {
		t.Error("a fetched model should carry fetched_at")
	}
}

func TestModels_UpsertAndDelete(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "p"})

	if err := st.UpsertModel(ctx, Model{ProviderID: "p", ModelID: "m1", Capabilities: Capabilities{CapChat}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Update the capabilities; that is the edit the UI exists for.
	if err := st.UpsertModel(ctx, Model{
		ProviderID: "p", ModelID: "m1", DisplayName: "M1",
		Capabilities: Capabilities{CapChat, CapImageGen}, Source: "user",
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	m, err := st.GetModel(ctx, "p", "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !m.Has(CapImageGen) || m.DisplayName != "M1" {
		t.Errorf("model = %+v", m)
	}
	if m.Source != "user" {
		t.Errorf("source = %q, want user (a hand edit must be marked as one)", m.Source)
	}

	// Missing rows are distinguishable.
	if _, err := st.GetModel(ctx, "p", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
	// Upsert requires both halves of the key.
	if err := st.UpsertModel(ctx, Model{ProviderID: "p"}); err == nil {
		t.Error("a model without an id must be refused")
	}

	_ = st.SetBinding(ctx, Binding{Capability: CapImageGen, ProviderID: "p", ModelID: "m1"})
	if err := st.DeleteModel(ctx, "p", "m1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	bindings, _ := st.ListBindings(ctx)
	if len(bindings) != 0 {
		t.Errorf("deleting a model left a binding pointing at it: %v", bindings)
	}
}

func TestBindings_SetClearAndValidate(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()

	if err := st.SetBinding(ctx, Binding{Capability: CapVision, ProviderID: "p", ModelID: "m"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := st.ListBindings(ctx)
	if len(got) != 1 || got[0].Capability != CapVision || got[0].ModelID != "m" {
		t.Fatalf("bindings = %v", got)
	}

	// Re-pointing the same capability replaces rather than adds.
	if err := st.SetBinding(ctx, Binding{Capability: CapVision, ProviderID: "p2", ModelID: "m2"}); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	got, _ = st.ListBindings(ctx)
	if len(got) != 1 || got[0].ModelID != "m2" {
		t.Errorf("bindings = %v, want one row pointing at m2", got)
	}

	// An unknown capability is refused rather than stored.
	if err := st.SetBinding(ctx, Binding{Capability: "nonsense", ProviderID: "p", ModelID: "m"}); err == nil {
		t.Error("an unknown capability must be refused")
	}
	// A blank model clears the binding, which is how a tool is switched off.
	if err := st.SetBinding(ctx, Binding{Capability: CapVision}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := st.ListBindings(ctx); len(got) != 0 {
		t.Errorf("bindings = %v, want none after clearing", got)
	}
}

func TestProviders_ListOrdersConfigFirst(t *testing.T) {
	// The UI groups by source, and config-sourced rows are read-only, so they
	// should come first and in a stable order.
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "zzz", Source: SourceUser})
	_ = st.UpsertProvider(ctx, Provider{ID: "aaa", Source: SourceConfig})
	_ = st.UpsertProvider(ctx, Provider{ID: "bbb", Source: SourceConfig})

	list, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d providers", len(list))
	}
	if list[0].ID != "aaa" || list[1].ID != "bbb" || list[2].ID != "zzz" {
		t.Errorf("order = %v, want the config ones first then the user one", []string{list[0].ID, list[1].ID, list[2].ID})
	}
	if list[0].Source != SourceConfig || list[2].Source != SourceUser {
		t.Errorf("sources = %q, %q", list[0].Source, list[2].Source)
	}
}

func TestProvider_LastErrorIsRecorded(t *testing.T) {
	// The UI shows why a provider is broken without re-testing it.
	st := newCatalogStore(t)
	ctx := context.Background()
	_ = st.UpsertProvider(ctx, Provider{ID: "p"})
	if err := st.SetProviderError(ctx, "p", "connection refused"); err != nil {
		t.Fatalf("set error: %v", err)
	}
	p, _ := st.GetProvider(ctx, "p")
	if p.LastError != "connection refused" {
		t.Errorf("last_error = %q", p.LastError)
	}
	_ = st.SetProviderError(ctx, "p", "")
	p, _ = st.GetProvider(ctx, "p")
	if p.LastError != "" {
		t.Errorf("last_error = %q, want it cleared", p.LastError)
	}
}

func TestProvider_RequiresID(t *testing.T) {
	st := newCatalogStore(t)
	if err := st.UpsertProvider(context.Background(), Provider{Name: "no id"}); err == nil {
		t.Error("a provider without an id must be refused")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
