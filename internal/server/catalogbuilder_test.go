package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/store"
)

/* ------------------------------------------------------------- mock provider -- */

// mockProvider is an OpenAI-compatible endpoint that records what reached it.
//
// It is deliberately a real HTTP server: "the constructed model points at the
// provider's base_url" is only proven by a request arriving here, and that is
// exactly the property this work exists for.
type mockProvider struct {
	srv *httptest.Server
	mu  sync.Mutex
	// requests holds the path of every request, in order.
	requests []string
	// bodies holds the decoded body of every chat completion request.
	bodies []map[string]any
	// models is what GET {base}/models lists.
	models []string
	// modelsStatus, when non-zero, is returned by /models instead of 200.
	modelsStatus int
	// failChat, when true, answers a chat request with 500.
	failChat bool
	// authHeaders records the Authorization header of every request.
	authHeaders []string
}

func newMockProvider(t *testing.T, models ...string) *mockProvider {
	t.Helper()
	m := &mockProvider{models: models}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests = append(m.requests, r.URL.Path)
		m.authHeaders = append(m.authHeaders, r.Header.Get("Authorization"))
		m.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if m.modelsStatus != 0 {
				w.WriteHeader(m.modelsStatus)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
				return
			}
			data := make([]map[string]string, 0, len(m.models))
			for _, id := range m.models {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return

		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.bodies = append(m.bodies, body)
			fail := m.failChat
			m.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": 1,
				"model":   body["model"],
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": "pong"},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// chatPaths counts the chat completion requests the mock received.
func (m *mockProvider) chatPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.requests {
		if strings.HasSuffix(p, "/chat/completions") {
			out = append(out, p)
		}
	}
	return out
}

// lastBody returns the most recent chat completion request body.
func (m *mockProvider) lastBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return nil
	}
	return m.bodies[len(m.bodies)-1]
}

func (m *mockProvider) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *mockProvider) auths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.authHeaders...)
}

/* ------------------------------------------------------------ test fixtures -- */

// catalogStore opens a store in a temp dir with the given providers and models.
func catalogStore(t *testing.T, providers []store.Provider, models []store.Model) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/catalog.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, p := range providers {
		if err := st.UpsertProvider(ctx, p); err != nil {
			t.Fatalf("upsert provider %s: %v", p.ID, err)
		}
	}
	for _, m := range models {
		if err := st.UpsertModel(ctx, m); err != nil {
			t.Fatalf("upsert model %s/%s: %v", m.ProviderID, m.ModelID, err)
		}
	}
	return st
}

// providerRow is a shorthand for a user provider. The key is set separately,
// through SetProviderKey, because it is never part of a serialised fixture.
func providerRow(id string, enabled bool, baseURL string) store.Provider {
	return store.Provider{
		ID: id, Name: id, BaseURL: baseURL, Kind: providerKindOpenAI,
		Source: store.SourceUser, Enabled: enabled,
	}
}

// withKey stores an API key on a provider.
func withKey(t *testing.T, st store.Store, id, key string) {
	t.Helper()
	if err := st.SetProviderKey(context.Background(), id, key); err != nil {
		t.Fatalf("set key on %s: %v", id, err)
	}
}

// chatModelRow is a shorthand for an enabled chat-capable model row.
func chatModelRow(providerID, modelID string) store.Model {
	return store.Model{
		ProviderID: providerID, ModelID: modelID, DisplayName: modelID,
		Capabilities: store.Capabilities{store.CapChat}, Enabled: true,
	}
}

// providerByID finds a provider summary in a catalog.
func providerByID(t *testing.T, cat ModelCatalog, id string) ProviderChoice {
	t.Helper()
	for _, p := range cat.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("provider %q is not in the catalog: %+v", id, cat.Providers)
	return ProviderChoice{}
}

/* ------------------------------------------------------------------ Build --- */

// TestCatalogModelBuilder_BuildUsesTheStoredBaseURL is the point of the whole
// change: a model picked from the database catalog must actually be sent to that
// provider's base_url.
func TestCatalogModelBuilder_BuildUsesTheStoredBaseURL(t *testing.T) {
	mock := newMockProvider(t, "stored-model", "stored-model-2")
	// The provider's base_url IS the mock: that is what makes the assertion
	// below meaningful.
	st := catalogStore(t,
		[]store.Provider{{
			ID: "stored", Name: "存储提供商", BaseURL: mock.srv.URL, Kind: providerKindOpenAI,
			Source: store.SourceUser, Enabled: true,
		}},
		[]store.Model{chatModelRow("stored", "stored-model")},
	)
	withKey(t, st, "stored", "sk-stored")

	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})

	built, err := b.Build(context.Background(), "stored", "stored-model")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cm, ok := built.(model.BaseChatModel)
	if !ok {
		t.Fatalf("Build returned %T, want a chat model", built)
	}

	resp, err := cm.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "ping"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Content != "pong" {
		t.Errorf("answer = %q, want pong", resp.Content)
	}

	// The request really reached the database provider's base_url...
	if got := mock.chatPaths(); len(got) != 1 {
		t.Fatalf("chat requests = %v, want exactly one", mock.paths())
	}
	// ...carrying the stored model id and the stored key.
	if body := mock.lastBody(); body == nil || body["model"] != "stored-model" {
		t.Errorf("request model = %v, want stored-model", body["model"])
	}
	if auths := mock.auths(); len(auths) == 0 || auths[0] != "Bearer sk-stored" {
		t.Errorf("authorization = %v, want the stored key", auths)
	}
}

// TestCatalogModelBuilder_BuildAcceptsADatabaseOnlyProvider covers a deployment
// with no config provider at all: the catalog alone must be enough.
func TestCatalogModelBuilder_BuildAcceptsADatabaseOnlyProvider(t *testing.T) {
	mock := newMockProvider(t, "m")
	st := catalogStore(t,
		[]store.Provider{providerRow("only", true, mock.srv.URL)},
		[]store.Model{chatModelRow("only", "m")},
	)
	withKey(t, st, "only", "sk-only")

	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})

	// A blank provider selects the catalog default, which is this provider.
	defProvider, defModel := b.DefaultTarget(context.Background())
	if defProvider != "only" || defModel != "m" {
		t.Fatalf("default = %s/%s, want only/m", defProvider, defModel)
	}
	if _, err := b.Build(context.Background(), "", ""); err != nil {
		t.Fatalf("Build with the default: %v", err)
	}
	// A blank model selects the provider's first enabled chat model.
	built, err := b.Build(context.Background(), "only", "")
	if err != nil {
		t.Fatalf("Build with a blank model: %v", err)
	}
	if _, err := built.(model.BaseChatModel).Generate(context.Background(),
		[]*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if body := mock.lastBody(); body == nil || body["model"] != "m" {
		t.Errorf("request model = %v, want m", body["model"])
	}
}

func TestCatalogModelBuilder_RefusesDisabledProvider(t *testing.T) {
	mock := newMockProvider(t, "m")
	st := catalogStore(t,
		[]store.Provider{providerRow("off", false, mock.srv.URL)},
		[]store.Model{chatModelRow("off", "m")},
	)
	withKey(t, st, "off", "sk-off")
	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})

	_, err := b.Build(context.Background(), "off", "m")
	if err == nil {
		t.Fatal("want an error for a disabled provider")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error = %q, want it to name the disabled provider", err)
	}
	if len(mock.paths()) != 0 {
		t.Errorf("a disabled provider must not be called: %v", mock.paths())
	}
}

func TestCatalogModelBuilder_RefusesUnsupportedKind(t *testing.T) {
	mock := newMockProvider(t, "m")
	st := catalogStore(t, nil, nil)
	if err := st.UpsertProvider(context.Background(), store.Provider{
		ID: "anthropic", Name: "anthropic", BaseURL: mock.srv.URL, Kind: "anthropic-messages",
		Source: store.SourceUser, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})

	_, err := b.Build(context.Background(), "anthropic", "m")
	if err == nil {
		t.Fatal("want an error for an unsupported provider kind")
	}
	if !strings.Contains(err.Error(), "unsupported") && !strings.Contains(err.Error(), "kind") {
		t.Errorf("error = %q, want it to name the unsupported kind", err)
	}
	if len(mock.paths()) != 0 {
		t.Errorf("an unsupported provider must not be called: %v", mock.paths())
	}
}

// TestCatalogModelBuilder_ConfigFallback keeps a config-only setup working: a
// provider the store does not know is still served by the registry.
func TestCatalogModelBuilder_ConfigFallback(t *testing.T) {
	st := catalogStore(t,
		[]store.Provider{providerRow("other", true, "http://127.0.0.1:1/v1")},
		[]store.Model{chatModelRow("other", "other-model")},
	)
	b := NewCatalogModelBuilder(st, testRegistry(), ModelBuilderOptions{Logger: zap.NewNop()})

	built, err := b.Build(context.Background(), "deepseek", "deepseek-reasoner")
	if err != nil {
		t.Fatalf("Build from the config registry: %v", err)
	}
	if _, ok := built.(model.BaseChatModel); !ok {
		t.Fatalf("Build returned %T, want a chat model", built)
	}

	// An unknown name is an error, and it says where to add the provider.
	_, err = b.Build(context.Background(), "nowhere", "m")
	if err == nil {
		t.Fatal("want an error for a provider nothing knows")
	}
	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("error = %q, want it to name the provider", err)
	}
}

// TestCatalogModelBuilder_StoreWinsOverConfigForAKnownProvider pins the
// precedence: once the store has a row, its base_url and enabled flag decide,
// even when the config registry also declares that name.
func TestCatalogModelBuilder_StoreWinsOverConfigForAKnownProvider(t *testing.T) {
	mock := newMockProvider(t, "deepseek-chat")
	st := catalogStore(t, nil, nil)
	if err := st.UpsertProvider(context.Background(), store.Provider{
		ID: "deepseek", Name: "deepseek", BaseURL: mock.srv.URL, Kind: "openai",
		Source: store.SourceConfig, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	withKey(t, st, "deepseek", "sk-store")
	b := NewCatalogModelBuilder(st, testRegistry(), ModelBuilderOptions{Logger: zap.NewNop()})

	built, err := b.Build(context.Background(), "deepseek", "deepseek-chat")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := built.(model.BaseChatModel).Generate(context.Background(),
		[]*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// The registry's deepseek points at api.deepseek.com; the store's points at
	// the mock, and the mock must have been the one called.
	if len(mock.chatPaths()) != 1 {
		t.Errorf("the stored base_url was not used: %v", mock.paths())
	}

	// Disabling the stored provider stops it working, config or not.
	if err := st.UpsertProvider(context.Background(), store.Provider{
		ID: "deepseek", Name: "deepseek", BaseURL: mock.srv.URL, Kind: "openai",
		Source: store.SourceConfig, Enabled: false,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	if _, err := b.Build(context.Background(), "deepseek", "deepseek-chat"); err == nil {
		t.Error("a disabled stored provider must be refused even when the config declares it")
	}
}

// TestCatalogModelBuilder_CacheInvalidatesOnProviderEdit: the cache must not
// ignore a provider edit.
func TestCatalogModelBuilder_CacheInvalidatesOnProviderEdit(t *testing.T) {
	first := newMockProvider(t, "m")
	second := newMockProvider(t, "m")
	st := catalogStore(t,
		[]store.Provider{providerRow("p", true, first.srv.URL)},
		[]store.Model{chatModelRow("p", "m")},
	)
	withKey(t, st, "p", "sk-p")
	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})
	ctx := context.Background()

	a, err := b.Build(ctx, "p", "m")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	again, err := b.Build(ctx, "p", "m")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if a != again {
		t.Error("an unchanged provider should reuse the cached client")
	}

	// Move the provider to the other mock. updated_at has one-second
	// resolution, so this also exercises the base_url comparison.
	if err := st.UpsertProvider(ctx, store.Provider{
		ID: "p", Name: "p", BaseURL: second.srv.URL, Kind: "openai",
		Source: store.SourceUser, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	moved, err := b.Build(ctx, "p", "m")
	if err != nil {
		t.Fatalf("Build after the edit: %v", err)
	}
	if moved == a {
		t.Fatal("the cached client survived a base_url change")
	}
	if _, err := moved.(model.BaseChatModel).Generate(ctx,
		[]*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(second.chatPaths()) != 1 {
		t.Errorf("the edited base_url was not used: second=%v first=%v",
			second.paths(), first.paths())
	}
}

// TestCatalogModelBuilder_CacheIsBounded keeps an arbitrary model string from
// growing the cache without limit.
func TestCatalogModelBuilder_CacheIsBounded(t *testing.T) {
	mock := newMockProvider(t)
	st := catalogStore(t,
		[]store.Provider{providerRow("p", true, mock.srv.URL)},
		[]store.Model{chatModelRow("p", "m")},
	)
	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop()})
	for i := 0; i < maxModelCache+10; i++ {
		if _, err := b.Build(context.Background(), "p", fmt.Sprintf("model-%d", i)); err != nil {
			t.Fatalf("Build model-%d: %v", i, err)
		}
	}
	b.mu.Lock()
	size := len(b.cache)
	b.mu.Unlock()
	if size > maxModelCache {
		t.Errorf("cache holds %d entries, want at most %d", size, maxModelCache)
	}
}

func TestCatalogModelBuilder_NoProviderConfigured(t *testing.T) {
	b := NewCatalogModelBuilder(nil, nil, ModelBuilderOptions{Logger: zap.NewNop()})
	_, err := b.Build(context.Background(), "", "")
	if err == nil {
		t.Fatal("want an error when nothing is configured anywhere")
	}
	if !strings.Contains(err.Error(), "no LLM provider") {
		t.Errorf("error = %q, want it to explain that no provider is configured", err)
	}
}

/* ---------------------------------------------------------- Catalog shape --- */

// TestCatalogModelBuilder_CatalogFiltersAndMarks covers the offer rules: only
// enabled providers contribute, only their enabled models, chat-capable first
// and everything marked.
func TestCatalogModelBuilder_CatalogFiltersAndMarks(t *testing.T) {
	st := catalogStore(t,
		[]store.Provider{
			{ID: "beta", Name: "Beta", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			{ID: "alpha", Name: "Alpha", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			{ID: "gone", Name: "Gone", BaseURL: "http://127.0.0.1:3/v1", Kind: "openai", Source: store.SourceUser, Enabled: false},
		},
		[]store.Model{
			{ProviderID: "beta", ModelID: "z-chat", DisplayName: "Z 对话", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			{ProviderID: "beta", ModelID: "a-chat", DisplayName: "A 对话", Capabilities: store.Capabilities{store.CapChat, store.CapVision}, Enabled: true},
			{ProviderID: "beta", ModelID: "off-chat", DisplayName: "停用", Capabilities: store.Capabilities{store.CapChat}, Enabled: false},
			{ProviderID: "beta", ModelID: "an-embedding", DisplayName: "向量", Capabilities: store.Capabilities{store.CapEmbedding}, Enabled: true},
			{ProviderID: "alpha", ModelID: "alpha-chat", DisplayName: "Alpha 对话", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			{ProviderID: "gone", ModelID: "gone-chat", DisplayName: "不该出现", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
		},
	)
	withKey(t, st, "beta", "sk-beta") // alpha has no key on purpose

	b := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop(), TTL: time.Hour})
	cat := b.Catalog(context.Background())

	// Providers are ordered by name; a disabled provider is listed but offers
	// nothing, so the console can still show why it is missing.
	if len(cat.Providers) != 3 {
		t.Fatalf("providers = %d, want 3 (enabled or not)", len(cat.Providers))
	}
	if cat.Providers[0].ID != "alpha" || cat.Providers[1].ID != "beta" || cat.Providers[2].ID != "gone" {
		t.Errorf("provider order = %s/%s/%s, want alpha/beta/gone",
			cat.Providers[0].ID, cat.Providers[1].ID, cat.Providers[2].ID)
	}

	// Models: alpha's first (by name), then beta's chat models by name. The
	// disabled model and beta's embedding model are not offered, because beta
	// has chat-capable models of its own.
	var got []string
	for _, m := range cat.Models {
		got = append(got, m.Provider+"/"+m.Model)
	}
	want := []string{"alpha/alpha-chat", "beta/a-chat", "beta/z-chat"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("catalog models = %v, want %v", got, want)
	}

	// Names, capabilities, the key flag and the chat mark all travel with the
	// entry, so both surfaces can label without a second request.
	first := cat.Models[0]
	if first.ProviderName != "Alpha" || first.DisplayName != "Alpha 对话" {
		t.Errorf("first entry = %+v, want the provider and display names", first)
	}
	if first.HasAPIKey {
		t.Error("alpha has no key and must say so")
	}
	if !first.ChatCapable {
		t.Error("alpha-chat declares chat and must be marked chat-capable")
	}
	beta := providerByID(t, cat, "beta")
	if !beta.HasAPIKey || beta.ModelCount != 4 || beta.EnabledModelCount != 3 || beta.ChatModelCount != 2 {
		t.Errorf("beta summary = %+v, want 4 stored / 3 enabled / 2 chat, with a key", beta)
	}
	if !beta.Stale {
		t.Error("a provider whose list was never fetched must be marked stale")
	}
	// No row here was ever produced by a fetch (they are hand-added), so the
	// list counts as never fetched: stale, with nothing to report as the time
	// of the last fetch.
	alpha := providerByID(t, cat, "alpha")
	if !alpha.Stale || alpha.LastFetchedAt != nil {
		t.Errorf("alpha = %+v, want stale with no fetch time", alpha)
	}
	gone := providerByID(t, cat, "gone")
	if gone.Enabled || gone.ModelCount != 1 {
		t.Errorf("gone = %+v, want it listed as disabled with its stored model counted", gone)
	}

	// Exactly one default.
	defaults := 0
	for _, m := range cat.Models {
		if m.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default marks = %d, want exactly 1", defaults)
	}
}

// TestCatalogModelBuilder_CatalogKeepsAProviderWithNoChatModel: a wrong
// capability inference must not make a provider vanish from the chat.
func TestCatalogModelBuilder_CatalogKeepsAProviderWithNoChatModel(t *testing.T) {
	st := catalogStore(t,
		[]store.Provider{
			{ID: "images", Name: "Images", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
		},
		[]store.Model{
			{ProviderID: "images", ModelID: "flux-pro", DisplayName: "Flux Pro", Capabilities: store.Capabilities{store.CapImageGen}, Enabled: true},
			{ProviderID: "images", ModelID: "bge-m3", DisplayName: "BGE", Capabilities: store.Capabilities{store.CapEmbedding}, Enabled: true},
		},
	)
	withKey(t, st, "images", "sk-images")

	cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Catalog(context.Background())
	if len(cat.Models) != 2 {
		t.Fatalf("models = %d, want both of them offered anyway", len(cat.Models))
	}
	for _, m := range cat.Models {
		if m.ChatCapable {
			t.Errorf("%s must not claim to be chat-capable", m.Model)
		}
		if m.Default {
			t.Errorf("%s must not be the default: nothing here can hold a conversation", m.Model)
		}
	}
	// Models by name, deterministically.
	if cat.Models[0].Model != "bge-m3" || cat.Models[1].Model != "flux-pro" {
		t.Errorf("order = %s/%s, want bge-m3/flux-pro", cat.Models[0].Model, cat.Models[1].Model)
	}
	summary := providerByID(t, cat, "images")
	if summary.ChatModelCount != 0 || summary.EnabledModelCount != 2 {
		t.Errorf("summary = %+v, want 2 enabled and 0 chat-capable", summary)
	}
}

// TestCatalogModelBuilder_EmptyStoreFallsBackToConfig covers a fresh install.
func TestCatalogModelBuilder_EmptyStoreFallsBackToConfig(t *testing.T) {
	st := catalogStore(t, nil, nil)
	cat := NewCatalogModelBuilder(st, testRegistry(), ModelBuilderOptions{}).Catalog(context.Background())
	if len(cat.Models) != 2 {
		t.Fatalf("models = %d, want the registry's two providers", len(cat.Models))
	}
	if cat.Models[0].Provider != "deepseek" || !cat.Models[0].Default {
		t.Errorf("first = %+v, want the registry default first", cat.Models[0])
	}
	if len(cat.Models[0].Capabilities) != 1 || cat.Models[0].Capabilities[0] != "chat" {
		t.Errorf("capabilities = %v, want [chat]", cat.Models[0].Capabilities)
	}
	if cat.Models[1].HasAPIKey {
		t.Error("the registry's keyless provider must report has_api_key=false")
	}
	for _, p := range cat.Providers {
		if p.Stale {
			t.Errorf("a config provider cannot be stale: %+v", p)
		}
	}
}

/* ----------------------------------------------------------- default rule --- */

func TestCatalogModelBuilder_DefaultRule(t *testing.T) {
	ctx := context.Background()

	t.Run("config default provider wins", func(t *testing.T) {
		mock := newMockProvider(t)
		st := catalogStore(t,
			[]store.Provider{
				{ID: "chosen", Name: "A", BaseURL: mock.srv.URL, Kind: "openai", Source: store.SourceConfig, Enabled: true},
				{ID: "extra", Name: "B", BaseURL: mock.srv.URL, Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "chosen", ModelID: "zz-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "chosen", ModelID: "aa-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "extra", ModelID: "extra-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			},
		)
		reg := llm.NewRegistry(map[string]llm.Provider{
			"chosen": {Name: "chosen", BaseURL: mock.srv.URL, APIKey: "sk", Model: "zz-model"},
		}, "chosen")
		cat := NewCatalogModelBuilder(st, reg, ModelBuilderOptions{}).Catalog(ctx)

		if got := defaultOf(t, cat); got != "chosen/zz-model" {
			t.Errorf("default = %s, want chosen/zz-model (the config's choice within that provider)", got)
		}
	})

	t.Run("config default falls back to the provider's first chat model", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "chosen", Name: "A", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceConfig, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "chosen", ModelID: "zz-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "chosen", ModelID: "aa-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			},
		)
		// The config names a model the store does not offer.
		reg := llm.NewRegistry(map[string]llm.Provider{
			"chosen": {Name: "chosen", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk", Model: "gone-model"},
		}, "chosen")
		cat := NewCatalogModelBuilder(st, reg, ModelBuilderOptions{}).Catalog(ctx)
		if got := defaultOf(t, cat); got != "chosen/aa-model" {
			t.Errorf("default = %s, want chosen/aa-model", got)
		}
	})

	t.Run("a disabled config default provider does not win", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "chosen", Name: "A", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceConfig, Enabled: false},
				{ID: "other", Name: "B", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "chosen", ModelID: "chosen-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "other", ModelID: "other-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			},
		)
		reg := llm.NewRegistry(map[string]llm.Provider{
			"chosen": {Name: "chosen", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk", Model: "chosen-model"},
		}, "chosen")
		cat := NewCatalogModelBuilder(st, reg, ModelBuilderOptions{}).Catalog(ctx)
		if got := defaultOf(t, cat); got != "other/other-model" {
			t.Errorf("default = %s, want other/other-model", got)
		}
	})

	t.Run("the chat binding wins when the config names nothing", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "aaa", Name: "A", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
				{ID: "bbb", Name: "B", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "aaa", ModelID: "aaa-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "bbb", ModelID: "bbb-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			},
		)
		if err := st.SetBinding(ctx, store.Binding{
			Capability: store.CapChat, ProviderID: "bbb", ModelID: "bbb-model",
		}); err != nil {
			t.Fatalf("SetBinding: %v", err)
		}
		cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Catalog(ctx)
		if got := defaultOf(t, cat); got != "bbb/bbb-model" {
			t.Errorf("default = %s, want the bound bbb/bbb-model", got)
		}
	})

	t.Run("a binding to a model that is gone is ignored", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "aaa", Name: "A", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
				{ID: "bbb", Name: "B", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "aaa", ModelID: "aaa-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "bbb", ModelID: "bbb-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: false},
			},
		)
		if err := st.SetBinding(ctx, store.Binding{
			Capability: store.CapChat, ProviderID: "bbb", ModelID: "bbb-model",
		}); err != nil {
			t.Fatalf("SetBinding: %v", err)
		}
		cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Catalog(ctx)
		if got := defaultOf(t, cat); got != "aaa/aaa-model" {
			t.Errorf("default = %s, want the first chat-capable model", got)
		}
	})

	t.Run("otherwise the first chat-capable model", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "zzz", Name: "Z", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
				{ID: "aaa", Name: "A", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "zzz", ModelID: "zzz-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
				{ProviderID: "aaa", ModelID: "bbb-model", Capabilities: store.Capabilities{store.CapEmbedding}, Enabled: true},
				{ProviderID: "aaa", ModelID: "aaa-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			},
		)
		cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Catalog(ctx)
		// Provider order is by name (A before Z), so A/aaa-model comes first —
		// not the embedding model, which cannot hold a conversation.
		if got := defaultOf(t, cat); got != "aaa/aaa-model" {
			t.Errorf("default = %s, want aaa/aaa-model", got)
		}
	})

	t.Run("nothing usable means no default at all", func(t *testing.T) {
		st := catalogStore(t,
			[]store.Provider{
				{ID: "images", Name: "I", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			},
			[]store.Model{
				{ProviderID: "images", ModelID: "flux", Capabilities: store.Capabilities{store.CapImageGen}, Enabled: true},
			},
		)
		cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Catalog(ctx)
		if len(cat.Models) != 1 {
			t.Fatalf("models = %d, want the provider kept anyway", len(cat.Models))
		}
		if got := defaultOf(t, cat); got != "" {
			t.Errorf("default = %s, want none", got)
		}
		// And Build cannot invent one.
		if _, err := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{}).Build(ctx, "", ""); err == nil {
			t.Error("Build must fail when nothing usable is configured")
		}
	})
}

// choiceFor finds one catalog entry.
func choiceFor(t *testing.T, models []ModelChoice, provider, model string) ModelChoice {
	t.Helper()
	for _, m := range models {
		if m.Provider == provider && m.Model == model {
			return m
		}
	}
	t.Fatalf("%s/%s is not in the catalog: %+v", provider, model, models)
	return ModelChoice{}
}

// defaultOf renders the single default entry of a catalog, or "".
func defaultOf(t *testing.T, cat ModelCatalog) string {
	t.Helper()
	found := ""
	for _, m := range cat.Models {
		if !m.Default {
			continue
		}
		if found != "" {
			t.Fatalf("more than one default: %s and %s/%s", found, m.Provider, m.Model)
		}
		found = m.Provider + "/" + m.Model
	}
	return found
}

/* ------------------------------------------------------- the /chat/models --- */

// TestHandleChatModels_ReportsTheDatabaseCatalog is the endpoint contract both
// surfaces read.
func TestHandleChatModels_ReportsTheDatabaseCatalog(t *testing.T) {
	seed := func(s store.Store) {
		ctx := context.Background()
		for _, p := range []store.Provider{
			{ID: "db", Name: "数据库提供商", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
			{ID: "nokey", Name: "NoKey", BaseURL: "http://127.0.0.1:2/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
		} {
			if err := s.UpsertProvider(ctx, p); err != nil {
				t.Fatalf("upsert provider: %v", err)
			}
		}
		if err := s.SetProviderKey(ctx, "db", "sk-db"); err != nil {
			t.Fatalf("set key: %v", err)
		}
		for _, m := range []store.Model{
			{ProviderID: "db", ModelID: "db-chat", DisplayName: "数据库模型", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
			{ProviderID: "db", ModelID: "db-vision", DisplayName: "看图", Capabilities: store.Capabilities{store.CapChat, store.CapVision}, Enabled: true},
			{ProviderID: "nokey", ModelID: "nokey-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
		} {
			if err := s.UpsertModel(ctx, m); err != nil {
				t.Fatalf("upsert model: %v", err)
			}
		}
	}

	h, _ := newCatalogHarness(t, seed, func(st store.Store) ModelBuilder {
		return NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Logger: zap.NewNop(), TTL: time.Hour})
	})

	var body struct {
		Models    []ModelChoice    `json:"models"`
		Providers []ProviderChoice `json:"providers"`
		Tools     []string         `json:"tools"`
		MaxSteps  int              `json:"max_steps"`
		System    string           `json:"system_prompt"`
	}
	h.getJSON(t, "/api/chat/models", http.StatusOK, &body)

	if len(body.Models) != 3 {
		t.Fatalf("models = %+v, want the three database models", body.Models)
	}
	// Providers are ordered by name — "NoKey" sorts before the CJK name — and
	// each provider's models follow in name order.
	if body.Providers[0].Name != "NoKey" || body.Providers[1].Name != "数据库提供商" {
		t.Errorf("providers = %+v, want them ordered by name", body.Providers)
	}
	wantOrder := []string{"nokey/nokey-model", "db/db-chat", "db/db-vision"}
	var gotOrder []string
	for _, m := range body.Models {
		gotOrder = append(gotOrder, m.Provider+"/"+m.Model)
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("model order = %v, want %v", gotOrder, wantOrder)
	}

	entry := choiceFor(t, body.Models, "db", "db-chat")
	if entry.ProviderName != "数据库提供商" {
		t.Errorf("provider_name = %q, want the display name", entry.ProviderName)
	}
	if entry.DisplayName != "数据库模型" {
		t.Errorf("display_name = %q", entry.DisplayName)
	}
	if !entry.ChatCapable || !entry.HasAPIKey {
		t.Errorf("entry = %+v, want it chat-capable with a key", entry)
	}
	vision := choiceFor(t, body.Models, "db", "db-vision")
	if !strings.Contains(strings.Join(vision.Capabilities, ","), "vision") {
		t.Errorf("capabilities = %v, want the stored ones", vision.Capabilities)
	}
	if choiceFor(t, body.Models, "nokey", "nokey-model").HasAPIKey {
		t.Error("a provider with no key must be reported as such so the UI can warn")
	}
	// Exactly one default, and it is a model that can hold a conversation.
	defaults := 0
	for _, m := range body.Models {
		if m.Default {
			defaults++
			if !m.ChatCapable {
				t.Errorf("%s/%s is the default but is not chat-capable", m.Provider, m.Model)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("default marks = %d, want exactly 1", defaults)
	}

	db := providerByID(t, ModelCatalog{Providers: body.Providers}, "db")
	if db.ChatModelCount != 2 || db.ModelCount != 2 || !db.HasAPIKey || !db.Stale {
		t.Errorf("db summary = %+v", db)
	}
	nokey := providerByID(t, ModelCatalog{Providers: body.Providers}, "nokey")
	if nokey.HasAPIKey {
		t.Error("a provider with no key must be reported as such so the UI can warn")
	}
	// The endpoint keeps everything it returned before.
	if body.System == "" || body.MaxSteps == 0 || body.Tools == nil {
		t.Errorf("system_prompt/max_steps/tools must be preserved: %+v", body)
	}
}

// TestHandleChatModels_NoBuilderIsStillAnEmptyArray keeps the shape stable when
// chat has no model builder at all.
func TestHandleChatModels_NoBuilderIsStillAnEmptyArray(t *testing.T) {
	h, _ := newCatalogHarness(t, nil, func(store.Store) ModelBuilder { return nil })

	status, raw := h.getRaw(t, "/api/chat/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(raw, `"models":[]`) || !strings.Contains(raw, `"providers":[]`) {
		t.Errorf("empty arrays expected, got %s", raw)
	}
}

// TestResolveModelChoice_UsesTheDatabaseCatalog checks that creating or patching
// a session persists a database provider/model pair.
func TestResolveModelChoice_UsesTheDatabaseCatalog(t *testing.T) {
	st := catalogStore(t,
		[]store.Provider{
			{ID: "db", Name: "DB", BaseURL: "http://127.0.0.1:1/v1", Kind: "openai", Source: store.SourceUser, Enabled: true},
		},
		[]store.Model{
			{ProviderID: "db", ModelID: "db-chat", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
		},
	)
	builder := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{})
	s := &Server{logger: zap.NewNop(), chat: ChatDeps{Builder: builder}}
	ctx := context.Background()

	if p, m := s.resolveModelChoice(ctx, "", ""); p != "db" || m != "db-chat" {
		t.Errorf("blank choice = %s/%s, want db/db-chat", p, m)
	}
	if p, m := s.resolveModelChoice(ctx, "db", ""); p != "db" || m != "db-chat" {
		t.Errorf("provider only = %s/%s, want db/db-chat", p, m)
	}
	if p, m := s.resolveModelChoice(ctx, "db", "another"); p != "db" || m != "another" {
		t.Errorf("explicit model = %s/%s, want it kept", p, m)
	}
}

/* ------------------------------------------------------------------ helper -- */

// newCatalogHarness starts a chat server whose ModelBuilder is built from the
// store the test seeded.
//
// The shared buildServerWith cannot be used here: the builder needs the store,
// and the store only exists once the harness has created it, while the chat
// routes are registered at construction time.
func newCatalogHarness(t *testing.T, seed func(store.Store), builder func(store.Store) ModelBuilder) (*harness, store.Store) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if seed != nil {
		seed(st)
	}

	hash, err := HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	srv, err := New(Config{
		Host:          "127.0.0.1",
		Port:          0,
		MetricsEnable: true,
		Version:       "test-version",
		ChatMaxSteps:  4,
		Chat:          ChatDeps{Runner: chatRunnerForTest(t), Builder: builder(st)},
		Logger:        zap.NewNop(),
	}, st, pricing.NewTable(nil, pricing.Rate{}), config.AdminConfig{
		Username:     "admin",
		PasswordHash: hash,
		RequireLogin: true,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	startHarness(t, srv)

	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, st
}

// chatRunnerForTest builds a runner over a scripted model, for tests that only
// need ChatDeps.Runner to be non-nil (the endpoints refuse to register without
// one).
func chatRunnerForTest(t *testing.T) *chat.Runner {
	t.Helper()
	r, err := chat.New(chat.Config{Model: &scriptedModel{}, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	return r
}

// TestCatalogModelBuilder_ConfigProviderWithoutAStoredModel covers the config
// shape that names a provider but no model: `llm.providers.glm.api_key` alone
// relies on the built-in known-provider table, and such a provider must keep
// working before its first model fetch.
func TestCatalogModelBuilder_ConfigProviderWithoutAStoredModel(t *testing.T) {
	ctx := context.Background()
	mock := newMockProvider(t, "glm-4-plus")
	st := catalogStore(t,
		[]store.Provider{{
			ID: "glm", Name: "glm", BaseURL: mock.srv.URL, Kind: providerKindOpenAI,
			Source: store.SourceConfig, Enabled: true,
		}},
		nil,
	)
	// The registry is what supplies the model name when the config names none.
	reg := llm.NewRegistry(map[string]llm.Provider{
		"glm": {Name: "glm", BaseURL: mock.srv.URL, Model: "glm-4-plus", APIKey: "sk-glm"},
	}, "glm")

	b := NewCatalogModelBuilder(st, reg, ModelBuilderOptions{TTL: time.Hour})
	cat := b.Catalog(ctx)
	if len(cat.Models) != 1 || cat.Models[0].Model != "glm-4-plus" {
		t.Fatalf("models = %+v, want the config's model offered", cat.Models)
	}
	if !cat.Models[0].Default || !cat.Models[0].ChatCapable {
		t.Errorf("entry = %+v, want it chat-capable and the default", cat.Models[0])
	}
	// Such a provider has never been fetched, so it is stale — the startup pass
	// will replace this with its real list.
	if summary := providerByID(t, cat, "glm"); !summary.Stale {
		t.Errorf("summary = %+v, want it stale", summary)
	}

	// And it can be built, with a blank model as well as an explicit one.
	for _, want := range []string{"", "glm-4-plus"} {
		built, err := b.Build(ctx, "glm", want)
		if err != nil {
			t.Fatalf("Build(glm, %q): %v", want, err)
		}
		cm, ok := built.(model.BaseChatModel)
		if !ok {
			t.Fatalf("Build(glm, %q) returned %T, want a chat model", want, built)
		}
		if _, err := cm.Generate(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	if len(mock.chatPaths()) != 2 {
		t.Errorf("chat requests = %v, want 2", mock.paths())
	}
	if body := mock.lastBody(); body["model"] != "glm-4-plus" {
		t.Errorf("model = %v, want glm-4-plus", body["model"])
	}
}
