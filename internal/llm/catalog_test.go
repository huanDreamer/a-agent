package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
)

// modelsServer serves an OpenAI-compatible /models endpoint, asserting the
// Authorization header the client is expected to send. An empty wantKey means
// no Authorization header is expected at all.
func modelsServer(t *testing.T, wantKey, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/models" {
			t.Errorf("path = %s, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "application/json") {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if wantKey == "" {
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want it absent when no key is configured", got)
			}
		} else if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("Authorization = %q, want Bearer <key>", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchModels_Shapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "openai data envelope",
			body: `{"object":"list","data":[{"id":"deepseek-chat","owned_by":"deepseek"},{"id":"deepseek-reasoner"}]}`,
			want: []string{"deepseek-chat", "deepseek-reasoner"},
		},
		{
			name: "bare array",
			body: `[{"id":"qwen-plus"},{"id":"qwen-vl-max"}]`,
			want: []string{"qwen-plus", "qwen-vl-max"},
		},
		{
			name: "models envelope",
			body: `{"models":[{"id":"glm-4"},{"id":"glm-4v"}]}`,
			want: []string{"glm-4", "glm-4v"},
		},
		{
			name: "empty data array",
			body: `{"data":[]}`,
			want: []string{},
		},
		{
			name: "blank and duplicate ids are dropped",
			body: `{"data":[{"id":"a"},{"id":"  "},{"id":"a"},{"id":" a "}]}`,
			want: []string{"a"},
		},
		{
			name: "ids are trimmed",
			body: `{"data":[{"id":"  spaced-model  "}]}`,
			want: []string{"spaced-model"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, "sk-test", tc.body, http.StatusOK)
			got, err := FetchModels(context.Background(), srv.URL, "sk-test", time.Second)
			if err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestFetchModels_OwnedByIsKept(t *testing.T) {
	srv := modelsServer(t, "", `{"data":[{"id":"m1","owned_by":"someone"}]}`, http.StatusOK)
	got, err := FetchModels(context.Background(), srv.URL, "", 0)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 1 || got[0].OwnedBy != "someone" {
		t.Errorf("got %+v, want one entry owned by someone", got)
	}
}

func TestFetchModels_TrailingSlashAndKeylessProvider(t *testing.T) {
	// A base URL pasted with a trailing slash must not produce "//models", and a
	// local provider with no key must send no Authorization header.
	srv := modelsServer(t, "", `{"data":[{"id":"llama3"}]}`, http.StatusOK)
	got, err := FetchModels(context.Background(), srv.URL+"/", "", time.Second)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "llama3" {
		t.Errorf("got %+v", got)
	}
}

func TestFetchModels_UnauthorizedQuotesTheReason(t *testing.T) {
	srv := modelsServer(t, "sk-test",
		`{"error":{"message":"Invalid API key provided: sk-****abcd","type":"invalid_request_error"}}`,
		http.StatusUnauthorized)

	_, err := FetchModels(context.Background(), srv.URL, "sk-test", time.Second)
	if err == nil {
		t.Fatal("want an error for a 401 response")
	}
	// The status and the provider's own reason are what makes the message
	// actionable; the request's key must not be echoed back.
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error %q should name the status", msg)
	}
	if !strings.Contains(msg, "Invalid API key") {
		t.Errorf("error %q should quote the provider's reason", msg)
	}
	if strings.Contains(msg, "sk-test") {
		t.Errorf("error %q must not contain the API key", msg)
	}
}

func TestFetchModels_ErrorBodyIsBounded(t *testing.T) {
	huge := `<html><body>` + strings.Repeat("x", 200_000) + `</body></html>`
	srv := modelsServer(t, "", huge, http.StatusBadGateway)
	_, err := FetchModels(context.Background(), srv.URL, "", time.Second)
	if err == nil {
		t.Fatal("want an error for a 5xx response")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("error is %d bytes; a whole HTML page must not be dumped into it", len(err.Error()))
	}
}

func TestFetchModels_Timeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	_, err := FetchModels(context.Background(), srv.URL, "k", 50*time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("FetchModels took %s; the deadline was not honoured", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it to name the timeout", err)
	}
}

func TestFetchModels_MalformedBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not json at all", body: `<!doctype html><html>hello</html>`},
		{name: "data is not an array", body: `{"data":"nope"}`},
		{name: "unexpected object", body: `{"object":"list","error":"nope"}`},
		{name: "array of strings", body: `["model-a","model-b"]`},
		{name: "empty body", body: ``},
		{name: "truncated json", body: `{"data":[{"id":"a"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, "", tc.body, http.StatusOK)
			_, err := FetchModels(context.Background(), srv.URL, "", time.Second)
			if err == nil {
				t.Fatal("want an error rather than a silent empty list")
			}
			// The message must describe what was wrong, so a user can tell a
			// wrong base_url from a provider that answers a different protocol.
			if !strings.Contains(err.Error(), "fetch models") {
				t.Errorf("error = %q, want it to name the operation", err)
			}
		})
	}
}

func TestFetchModels_RejectsUnusableBaseURL(t *testing.T) {
	tests := []struct {
		name string
		base string
	}{
		{name: "empty", base: ""},
		{name: "blank", base: "   "},
		{name: "no scheme", base: "api.example.com/v1"},
		{name: "wrong scheme", base: "ftp://api.example.com/v1"},
		{name: "unparseable", base: "http://[::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FetchModels(context.Background(), tc.base, "k", time.Second); err == nil {
				t.Fatalf("base_url %q should be refused before any request is made", tc.base)
			}
		})
	}
}

func TestInferCapabilities(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		// Plain chat models: the default.
		{model: "deepseek-chat", want: "chat"},
		{model: "qwen-plus", want: "chat"},
		{model: "gpt-3.5-turbo", want: "chat"},
		{model: "my-self-hosted-model", want: "chat"},
		// A non-match still gets chat: every model is a chat model until the
		// user says otherwise.
		{model: "totally-unknown-xyz", want: "chat"},
		{model: "", want: "chat"},

		// Vision: chat plus image input.
		{model: "qwen-vl-max", want: "chat,vision"},
		{model: "Qwen2.5-VL-7B-Instruct", want: "chat,vision"},
		{model: "gpt-4o", want: "chat,vision"},
		{model: "gpt-4o-mini", want: "chat,vision"},
		{model: "gpt-4.1-mini", want: "chat,vision"},
		{model: "gpt-4-turbo-preview", want: "chat,vision"},
		{model: "claude-3-5-sonnet-20241022", want: "chat,vision"},
		{model: "claude-4-opus", want: "chat,vision"},
		{model: "gemini-1.5-pro", want: "chat,vision"},
		{model: "llava-1.6", want: "chat,vision"},
		{model: "pixtral-12b", want: "chat,vision"},
		{model: "internvl2-8b", want: "chat,vision"},
		{model: "minicpm-v-2.6", want: "chat,vision"},
		{model: "some-vision-model", want: "chat,vision"},
		{model: "vl-2-8b", want: "chat,vision"},

		// Image generation: exclusively, never chat.
		{model: "dall-e-3", want: "image_gen"},
		{model: "gpt-image-1", want: "image_gen"},
		{model: "stable-diffusion-xl", want: "image_gen"},
		{model: "sd3-medium", want: "image_gen"},
		{model: "flux-pro", want: "image_gen"},
		{model: "kolors-v1", want: "image_gen"},
		{model: "wanx-v1", want: "image_gen"},
		{model: "imagen-3.0", want: "image_gen"},
		{model: "seedream-3", want: "image_gen"},

		// Speech to text.
		{model: "whisper-1", want: "audio_transcribe"},
		{model: "gpt-4o-transcribe", want: "audio_transcribe"},
		{model: "paraformer-v2", want: "audio_transcribe"},
		{model: "sensevoice-small", want: "audio_transcribe"},
		{model: "asr-1", want: "audio_transcribe"},

		// Text to speech.
		{model: "tts-1-hd", want: "audio_speech"},
		{model: "cosyvoice-v1", want: "audio_speech"},
		{model: "sambert-zhichu", want: "audio_speech"},
		{model: "gpt-4o-mini-tts", want: "audio_speech"},

		// Embeddings.
		{model: "text-embedding-3-small", want: "embedding"},
		{model: "bge-large-zh-v1.5", want: "embedding"},
		{model: "gte-large", want: "embedding"},
		{model: "nomic-embed-text", want: "embedding"},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			got := InferCapabilities(tc.model)
			if got.String() != tc.want {
				t.Errorf("InferCapabilities(%q) = %q, want %q", tc.model, got.String(), tc.want)
			}
			// Whatever is returned must be storable and understood.
			for _, c := range got {
				if !store.ValidCapability(c) {
					t.Errorf("InferCapabilities(%q) returned unknown capability %q", tc.model, c)
				}
			}
		})
	}
}

func TestInferCapabilities_AudioAndImageAreNeverChat(t *testing.T) {
	// The distinction that matters most: offering a generator or a transcriber
	// as a chat model produces a call that cannot succeed.
	for _, id := range []string{"dall-e-3", "flux-schnell", "whisper-1", "tts-1", "bge-m3"} {
		if caps := InferCapabilities(id); caps.Has(store.CapChat) {
			t.Errorf("InferCapabilities(%q) = %q, must not include chat", id, caps.String())
		}
	}
}

// ------------------------------------------------------------ config sync --

// newCatalogStore opens a store for the catalog tests.
func newCatalogStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// catalogSnapshot is the part of the catalog the sync owns, in a form two runs
// can be compared with.
type catalogSnapshot struct {
	providers []string
	models    []string
}

func snapshotCatalog(t *testing.T, st store.Store) catalogSnapshot {
	t.Helper()
	ctx := context.Background()
	providers, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	models, err := st.ListModels(ctx, "")
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	snap := catalogSnapshot{}
	for _, p := range providers {
		snap.providers = append(snap.providers, fmt.Sprintf(
			"id=%s name=%s base=%s kind=%s source=%s enabled=%t has_key=%t hint=%s env=%s",
			p.ID, p.Name, p.BaseURL, p.Kind, p.Source, p.Enabled, p.HasAPIKey, p.APIKeyHint, p.APIKeyEnv))
	}
	for _, m := range models {
		snap.models = append(snap.models, fmt.Sprintf(
			"provider=%s model=%s display=%s caps=%s enabled=%t source=%s",
			m.ProviderID, m.ModelID, m.DisplayName, m.Capabilities.String(), m.Enabled, m.Source))
	}
	return snap
}

func TestSyncConfigProviders_IsIdempotentAndKeepsUserProviders(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()

	// A provider the user added through the console, which the sync must leave
	// completely alone.
	if err := st.UpsertProvider(ctx, store.Provider{
		ID: "mine", Name: "Mine", BaseURL: "https://mine.example/v1", Source: store.SourceUser, Enabled: true,
	}); err != nil {
		t.Fatalf("seed user provider: %v", err)
	}
	if err := st.SetProviderKey(ctx, "mine", "sk-user-key"); err != nil {
		t.Fatalf("seed user key: %v", err)
	}
	if err := st.UpsertModel(ctx, store.Model{
		ProviderID: "mine", ModelID: "my-model", DisplayName: "My Model",
		Capabilities: store.Capabilities{store.CapChat}, Enabled: true, Source: "user",
	}); err != nil {
		t.Fatalf("seed user model: %v", err)
	}

	providers := map[string]config.LLMProvider{
		"deepseek": {APIKey: "sk-config-secret", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
		"local":    {BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen2.5"},
	}
	if err := SyncConfigProviders(ctx, st, providers, "deepseek"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	first := snapshotCatalog(t, st)

	if err := SyncConfigProviders(ctx, st, providers, "deepseek"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second := snapshotCatalog(t, st)

	if strings.Join(first.providers, "|") != strings.Join(second.providers, "|") {
		t.Errorf("providers changed on a second sync:\n first: %v\nsecond: %v", first.providers, second.providers)
	}
	if strings.Join(first.models, "|") != strings.Join(second.models, "|") {
		t.Errorf("models changed on a second sync:\n first: %v\nsecond: %v", first.models, second.models)
	}

	// Both config providers are present, read-only and enabled.
	for _, want := range []string{
		"id=deepseek name=deepseek base=https://api.deepseek.com/v1 kind=openai source=config enabled=true has_key=true",
		"id=local name=local base=http://127.0.0.1:11434/v1 kind=openai source=config enabled=true has_key=false",
	} {
		if !containsPrefix(first.providers, want) {
			t.Errorf("provider snapshot is missing %q:\n%v", want, first.providers)
		}
	}

	// The config's literal key was stored and can be resolved.
	p, err := st.GetProvider(ctx, "deepseek")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := p.ResolveAPIKey(nil); got != "sk-config-secret" {
		t.Errorf("resolved key = %q, want the key from the config", got)
	}
	if p.APIKeyHint == "" || strings.Contains(p.APIKeyHint, "config-secret") {
		t.Errorf("api_key_hint = %q, want a masked tail", p.APIKeyHint)
	}

	// The default provider's model exists as a chat-capable config model.
	m, err := st.GetModel(ctx, "deepseek", "deepseek-chat")
	if err != nil {
		t.Fatalf("default model: %v", err)
	}
	if !m.Has(store.CapChat) || m.Source != "config" || !m.Enabled {
		t.Errorf("default model = %+v, want chat/config/enabled", m)
	}
	// The other provider's default model is not invented.
	if _, err := st.GetModel(ctx, "local", "qwen2.5"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("only the default provider's model is seeded, got %v", err)
	}

	// The user's own provider and model survive untouched.
	mine, err := st.GetProvider(ctx, "mine")
	if err != nil {
		t.Fatalf("user provider: %v", err)
	}
	if mine.Source != store.SourceUser || mine.HasAPIKey != true {
		t.Errorf("user provider = %+v, want it unchanged", mine)
	}
	if got := mine.ResolveAPIKey(nil); got != "sk-user-key" {
		t.Errorf("user key = %q, want it preserved", got)
	}
	if _, err := st.GetModel(ctx, "mine", "my-model"); err != nil {
		t.Errorf("user model was dropped: %v", err)
	}
}

func TestSyncConfigProviders_BlankKeyDoesNotClearAStoredOne(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()
	providers := map[string]config.LLMProvider{
		"deepseek": {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
	}
	if err := SyncConfigProviders(ctx, st, providers, "deepseek"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// The operator pasted a key through the console; a config file without one
	// must not wipe it on the next start.
	if err := st.SetProviderKey(ctx, "deepseek", "sk-pasted"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := SyncConfigProviders(ctx, st, providers, "deepseek"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	p, err := st.GetProvider(ctx, "deepseek")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := p.ResolveAPIKey(nil); got != "sk-pasted" {
		t.Errorf("key = %q, want the stored key to survive a keyless config entry", got)
	}
}

func TestSyncConfigProviders_EdgeCases(t *testing.T) {
	st := newCatalogStore(t)
	ctx := context.Background()

	// No providers and no default: nothing to do, and no error.
	if err := SyncConfigProviders(ctx, st, nil, ""); err != nil {
		t.Fatalf("sync with nothing configured: %v", err)
	}
	if rows, err := st.ListProviders(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("providers = %v (err %v), want none", rows, err)
	}

	// A default naming a provider that is not declared, and an entry with a
	// blank name, are skipped rather than failing the whole sync.
	if err := SyncConfigProviders(ctx, st, map[string]config.LLMProvider{
		"":        {Model: "ghost"},
		"missing": {BaseURL: "https://x.example/v1"},
	}, "not-there"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rows, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "missing" {
		t.Fatalf("providers = %+v, want just the named entry", rows)
	}
	if models, err := st.ListModels(ctx, ""); err != nil || len(models) != 0 {
		t.Fatalf("models = %v (err %v), want none", models, err)
	}

	// A store is required: passing nil is a programming error, not a panic.
	if err := SyncConfigProviders(ctx, nil, nil, ""); err == nil {
		t.Error("a nil store should be refused")
	}
}

// containsPrefix reports whether any element starts with prefix.
func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestFetchModels_JSONRoundTrip guards the wire shape: the endpoint is
// OpenAI-compatible, so a model entry is decoded from an object with an id.
func TestFetchModels_JSONRoundTrip(t *testing.T) {
	var m ModelInfo
	if err := json.Unmarshal([]byte(`{"id":"m","owned_by":"o"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ID != "m" || m.OwnedBy != "o" {
		t.Errorf("got %+v", m)
	}
}

func TestSyncConfigProviders_PreservesEnabledToggle(t *testing.T) {
	// The config file declares what a provider IS; whether it is switched on is
	// a runtime choice. Re-enabling at every start would silently undo a user
	// turning a provider off in the console.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	providers := map[string]config.LLMProvider{
		"cfg": {BaseURL: "https://api.example.com/v1", APIKey: "k", Model: "m1"},
	}

	if err := SyncConfigProviders(ctx, st, providers, "cfg"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	p, err := st.GetProvider(ctx, "cfg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Source != store.SourceConfig {
		t.Errorf("source = %q, want config", p.Source)
	}
	if !p.Enabled {
		t.Error("a fresh config provider should start enabled")
	}
	if !p.HasAPIKey {
		t.Error("the config's key should have been stored")
	}

	// Turn it off through the console, then sync again (as a restart would).
	if err := st.UpsertProvider(ctx, store.Provider{
		ID: "cfg", Name: "cfg", BaseURL: p.BaseURL,
		Kind: "openai", Source: store.SourceConfig, Enabled: false,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := SyncConfigProviders(ctx, st, providers, "cfg"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, _ := st.GetProvider(ctx, "cfg")
	if after.Enabled {
		t.Error("the sync re-enabled a provider the user had turned off")
	}

	// Idempotent: a third run changes nothing, and the config default model is
	// recorded with chat capability.
	if err := SyncConfigProviders(ctx, st, providers, "cfg"); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	m, err := st.GetModel(ctx, "cfg", "m1")
	if err != nil {
		t.Fatalf("config default model missing: %v", err)
	}
	if !m.Has(store.CapChat) {
		t.Errorf("config default model capabilities = %v, want chat", m.Capabilities)
	}
	models, _ := st.ListModels(ctx, "cfg")
	if len(models) != 1 {
		t.Errorf("got %d models, want 1 after repeated syncs", len(models))
	}

	// A nil store is refused rather than panicking.
	if err := SyncConfigProviders(ctx, nil, providers, "cfg"); err == nil {
		t.Error("a nil store must be refused")
	}
}

func TestSyncConfigProviders_DoesNotDeleteUserProviders(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "keep.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertProvider(ctx, store.Provider{
		ID: "mine", Name: "mine", BaseURL: "https://mine/v1", Source: store.SourceUser,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SyncConfigProviders(ctx, st, map[string]config.LLMProvider{
		"cfg": {BaseURL: "https://cfg/v1"},
	}, "cfg"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := st.GetProvider(ctx, "mine"); err != nil {
		t.Errorf("a user provider was removed by the config sync: %v", err)
	}
}
