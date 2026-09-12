package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/store"
)

// testProviderKey is the key the mock provider expects and the console stores.
const testProviderKey = "sk-test-provider-key-abcd"

// newProviderHarness starts a logged-in server with no seeded data.
func newProviderHarness(t *testing.T) *harness {
	t.Helper()
	return newProviderHarnessWith(t, nil)
}

// newProviderHarnessWith starts a logged-in server with an optional fixture.
func newProviderHarnessWith(t *testing.T, seed func(store.Store)) *harness {
	t.Helper()
	srv, st := buildServerWith(t, buildOpts{seed: seed})
	startHarness(t, srv)
	h := &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}
	// RequireLogin is on in the harness, so every provider call needs a session.
	h.login(t)
	return h
}

// putJSON issues a PUT with a JSON body.
func (h *harness) putJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPut, h.base+path, &buf)
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	return resp
}

// deleteJSONBody issues a DELETE with a JSON body, for the endpoints that
// identify their target in the body rather than in the path.
func (h *harness) deleteJSONBody(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodDelete, h.base+path, &buf)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

// getRaw issues a GET and returns the status with the body as text, so a test
// can assert on the exact bytes a client would receive.
func (h *harness) getRaw(t *testing.T, path string) (int, string) {
	t.Helper()
	resp := h.get(t, path)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(b)
}

// rawBody reads and closes a response body as text.
func rawBody(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// modelsBackend is a minimal OpenAI-compatible /models endpoint. It answers 401
// unless the request carries the expected key, so a test that stores a key
// exercises the whole path from the database to the request header.
func modelsBackend(t *testing.T, key string, status int, raw string, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key provided","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if raw != "" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(raw))
			return
		}
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "owned_by": "vendor"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadURL returns a URL nothing listens on, for the failure paths.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// providerResponse is the body of the provider endpoints that return one.
type providerResponse struct {
	Provider store.Provider `json:"provider"`
}

// modelsResponse is the body of GET /api/llm/models and the refresh endpoint.
type modelsResponse struct {
	Models []store.Model `json:"models"`
	Count  int           `json:"count"`
}

// bindingsResponse is the body of the binding endpoints.
type bindingsResponse struct {
	Bindings []bindingView `json:"bindings"`
}

// createProvider posts a provider through the API and returns it.
func createProvider(t *testing.T, h *harness, body map[string]any) store.Provider {
	t.Helper()
	resp := h.postJSON(t, "/api/llm/providers", body)
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("create provider status = %d, want 200 (body: %s)", status, raw)
	}
	var out providerResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode provider: %v", err)
	}
	return out.Provider
}

// capabilitiesOf finds a model in a list and returns its capability string.
func capabilitiesOf(t *testing.T, models []store.Model, modelID string) string {
	t.Helper()
	for _, m := range models {
		if m.ModelID == modelID {
			return m.Capabilities.String()
		}
	}
	t.Fatalf("model %q not found in %+v", modelID, models)
	return ""
}

func TestProviders_CreateListAndKeyNeverLeaks(t *testing.T) {
	h := newProviderHarness(t)

	created := createProvider(t, h, map[string]any{
		"id":       "my-vendor",
		"name":     "My Vendor",
		"base_url": "https://api.my-vendor.example/v1",
		"api_key":  testProviderKey,
	})
	if created.ID != "my-vendor" || created.Name != "My Vendor" {
		t.Errorf("created = %+v", created)
	}
	if !created.Enabled || created.Source != store.SourceUser || created.Kind != "openai" {
		t.Errorf("created = %+v, want an enabled user provider of kind openai", created)
	}
	if !created.HasAPIKey {
		t.Error("has_api_key should be true after storing a key")
	}
	if want := "…" + testProviderKey[len(testProviderKey)-4:]; created.APIKeyHint != want {
		t.Errorf("api_key_hint = %q, want %q", created.APIKeyHint, want)
	}

	// The key must reach the store, since that is what signs requests.
	stored, err := h.store.GetProvider(context.Background(), "my-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := stored.ResolveAPIKey(os.Getenv); got != testProviderKey {
		t.Errorf("resolved key = %q, want the stored key", got)
	}

	// No read endpoint may echo the secret. Each is checked on its raw bytes,
	// because that is what a browser receives.
	for _, path := range []string{"/api/llm/providers", "/api/llm/models", "/api/llm/bindings"} {
		status, raw := h.getRaw(t, path)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, status)
		}
		if strings.Contains(raw, testProviderKey) {
			t.Errorf("GET %s leaked the API key: %s", path, raw)
		}
	}

	// An update without api_key keeps the stored key: the UI is never given the
	// secret, so it cannot send it back.
	updated := createProvider(t, h, map[string]any{
		"id":       "my-vendor",
		"name":     "Renamed Vendor",
		"base_url": "https://api.my-vendor.example/v2",
	})
	if updated.Name != "Renamed Vendor" || updated.BaseURL != "https://api.my-vendor.example/v2" {
		t.Errorf("updated = %+v", updated)
	}
	if !updated.HasAPIKey {
		t.Error("a blank api_key must not clear the stored key")
	}
	stored, err = h.store.GetProvider(context.Background(), "my-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := stored.ResolveAPIKey(os.Getenv); got != testProviderKey {
		t.Errorf("resolved key = %q, want it preserved across an update", got)
	}

	// Clearing is deliberate and has its own endpoint.
	resp := h.putJSON(t, "/api/llm/providers/my-vendor/key", map[string]string{"api_key": ""})
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("clear key status = %d, want 200 (body: %s)", status, raw)
	}
	if strings.Contains(raw, testProviderKey) {
		t.Errorf("the key endpoint echoed the key: %s", raw)
	}
	stored, err = h.store.GetProvider(context.Background(), "my-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if stored.HasAPIKey || stored.ResolveAPIKey(os.Getenv) != "" {
		t.Errorf("provider = %+v, want the key cleared", stored)
	}

	// Setting it again through the key endpoint works.
	resp = h.putJSON(t, "/api/llm/providers/my-vendor/key", map[string]string{"api_key": "sk-second-key-9876"})
	status, raw = rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("set key status = %d, want 200 (body: %s)", status, raw)
	}
	stored, err = h.store.GetProvider(context.Background(), "my-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := stored.ResolveAPIKey(os.Getenv); got != "sk-second-key-9876" {
		t.Errorf("resolved key = %q, want the new key", got)
	}

	// Delete removes it, and the list agrees.
	status, raw = rawBody(t, h.deleteJSON(t, "/api/llm/providers/my-vendor"))
	if status != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body: %s)", status, raw)
	}
	var list struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &list)
	if len(list.Providers) != 0 {
		t.Errorf("providers = %+v, want none", list.Providers)
	}
	if _, err := h.store.GetProvider(context.Background(), "my-vendor"); err == nil {
		t.Error("the provider row should be gone")
	}
}

func TestProviders_APIKeyEnvIsPreferred(t *testing.T) {
	h := newProviderHarness(t)
	createProvider(t, h, map[string]any{
		"id":          "env-vendor",
		"base_url":    "https://api.env.example/v1",
		"api_key":     "sk-stored",
		"api_key_env": "HUAN_TEST_VENDOR_KEY",
	})

	// The environment wins over the stored copy, so a rotation is a restart
	// away and the secret can stay out of the database entirely.
	t.Setenv("HUAN_TEST_VENDOR_KEY", "sk-from-env")
	stored, err := h.store.GetProvider(context.Background(), "env-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := stored.ResolveAPIKey(os.Getenv); got != "sk-from-env" {
		t.Errorf("resolved key = %q, want the environment value", got)
	}

	// An explicit empty api_key_env clears the reference, which is how a user
	// switches back to a literal key.
	updated := createProvider(t, h, map[string]any{
		"id":          "env-vendor",
		"base_url":    "https://api.env.example/v1",
		"api_key_env": "",
	})
	if updated.APIKeyEnv != "" {
		t.Errorf("api_key_env = %q, want it cleared", updated.APIKeyEnv)
	}
	stored, err = h.store.GetProvider(context.Background(), "env-vendor")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got := stored.ResolveAPIKey(os.Getenv); got != "sk-stored" {
		t.Errorf("resolved key = %q, want the stored key once the env reference is gone", got)
	}
}

func TestProviders_Validation(t *testing.T) {
	h := newProviderHarness(t)

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "missing id", body: map[string]any{"base_url": "https://api.example.com/v1"}},
		{name: "blank id", body: map[string]any{"id": "   ", "base_url": "https://api.example.com/v1"}},
		{name: "id with spaces", body: map[string]any{"id": "My Vendor", "base_url": "https://api.example.com/v1"}},
		{name: "id with a slash", body: map[string]any{"id": "vendor/one", "base_url": "https://api.example.com/v1"}},
		{name: "id starting with a dash", body: map[string]any{"id": "-vendor", "base_url": "https://api.example.com/v1"}},
		{name: "missing base_url", body: map[string]any{"id": "vendor"}},
		{name: "base_url without a scheme", body: map[string]any{"id": "vendor", "base_url": "api.example.com/v1"}},
		{name: "base_url with a bad scheme", body: map[string]any{"id": "vendor", "base_url": "ftp://api.example.com/v1"}},
		{name: "base_url without a host", body: map[string]any{"id": "vendor", "base_url": "http://"}},
		{name: "api_key is a pasted file", body: map[string]any{
			"id": "vendor", "base_url": "https://api.example.com/v1",
			"api_key": strings.Repeat("k", 5000),
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers", tc.body))
			if status < 400 || status >= 500 {
				t.Fatalf("status = %d, want a 4xx (body: %s)", status, raw)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatalf("decode error body %q: %v", raw, err)
			}
			if strings.TrimSpace(body["error"]) == "" {
				t.Errorf("body %q should explain the refusal", raw)
			}
		})
	}

	// Nothing was written by any of the refused requests.
	var list struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &list)
	if len(list.Providers) != 0 {
		t.Errorf("providers = %+v, want none", list.Providers)
	}
}

func TestProviders_TestEndpoint(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, testProviderKey, 0, "", "deepseek-chat", "deepseek-reasoner")
	createProvider(t, h, map[string]any{
		"id": "testable", "name": "Testable", "base_url": backend.URL, "api_key": testProviderKey,
	})

	var ok struct {
		OK          bool           `json:"ok"`
		ModelsCount int            `json:"models_count"`
		Error       string         `json:"error"`
		Provider    store.Provider `json:"provider"`
	}
	status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/testable/test", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("test status = %d, want 200 (body: %s)", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok.OK || ok.ModelsCount != 2 {
		t.Errorf("test result = %+v, want ok with 2 models", ok)
	}
	if ok.Provider.LastError != "" {
		t.Errorf("last_error = %q, want it cleared after a successful test", ok.Provider.LastError)
	}

	// A provider holding a wrong key fails, and the failure is recorded so the
	// UI can explain it without testing again.
	createProvider(t, h, map[string]any{
		"id": "wrong-key", "base_url": backend.URL, "api_key": "sk-not-the-key",
	})
	status, raw = rawBody(t, h.postJSON(t, "/api/llm/providers/wrong-key/test", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("test status = %d, want 200 even when the provider is unreachable (body: %s)", status, raw)
	}
	ok = struct {
		OK          bool           `json:"ok"`
		ModelsCount int            `json:"models_count"`
		Error       string         `json:"error"`
		Provider    store.Provider `json:"provider"`
	}{}
	if err := json.Unmarshal([]byte(raw), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok.OK {
		t.Error("ok should be false for a rejected key")
	}
	if !strings.Contains(ok.Error, "401") {
		t.Errorf("error = %q, want it to name the status", ok.Error)
	}
	if ok.Provider.LastError == "" {
		t.Error("last_error should record the failure")
	}

	var list struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &list)
	found := false
	for _, p := range list.Providers {
		if p.ID == "wrong-key" {
			found = true
			if p.LastError == "" {
				t.Error("the list should expose last_error")
			}
			if strings.Contains(p.LastError, "sk-not-the-key") {
				t.Errorf("last_error leaked the key: %q", p.LastError)
			}
		}
	}
	if !found {
		t.Error("the provider should still be listed after a failed test")
	}
}

func TestProviders_RefreshAppliesInferredCapabilities(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, testProviderKey, 0, "",
		"deepseek-chat", "gpt-4o", "dall-e-3", "whisper-1", "text-embedding-3-small")
	createProvider(t, h, map[string]any{
		"id": "inferred", "name": "Inferred", "base_url": backend.URL, "api_key": testProviderKey,
	})

	resp := h.postJSON(t, "/api/llm/providers/inferred/models/refresh", map[string]any{})
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body: %s)", status, raw)
	}
	var refreshed modelsResponse
	if err := json.Unmarshal([]byte(raw), &refreshed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if refreshed.Count != 5 || len(refreshed.Models) != 5 {
		t.Fatalf("refreshed %d models, want 5", refreshed.Count)
	}
	want := map[string]string{
		"deepseek-chat":          "chat",
		"gpt-4o":                 "chat,vision",
		"dall-e-3":               "image_gen",
		"whisper-1":              "audio_transcribe",
		"text-embedding-3-small": "embedding",
	}
	for model, caps := range want {
		if got := capabilitiesOf(t, refreshed.Models, model); got != caps {
			t.Errorf("%s capabilities = %q, want %q", model, got, caps)
		}
	}
	for _, m := range refreshed.Models {
		if m.Source != "fetched" || m.FetchedAt == nil {
			t.Errorf("%s = %+v, want a fetched row with a timestamp", m.ModelID, m)
		}
	}

	// The same list is readable from the models endpoint.
	var listed modelsResponse
	h.getJSON(t, "/api/llm/models?provider=inferred", http.StatusOK, &listed)
	if len(listed.Models) != 5 {
		t.Errorf("listed %d models, want 5", len(listed.Models))
	}
	if got := capabilitiesOf(t, listed.Models, "gpt-4o"); got != "chat,vision" {
		t.Errorf("gpt-4o = %q, want the inferred set to be stored", got)
	}
	// Filtering by another provider yields nothing.
	h.getJSON(t, "/api/llm/models?provider=other", http.StatusOK, &listed)
	if len(listed.Models) != 0 {
		t.Errorf("models = %+v, want none for an unknown provider", listed.Models)
	}
}

func TestProviders_RefreshFailureKeepsModels(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", 0, "", "deepseek-chat", "deepseek-reasoner")
	createProvider(t, h, map[string]any{"id": "flaky", "base_url": backend.URL})

	status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/flaky/models/refresh", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("first refresh = %d, want 200 (body: %s)", status, raw)
	}

	// Point the provider at a dead address: the refresh fails, and empty is not
	// an acceptable answer because the user's model list would vanish.
	createProvider(t, h, map[string]any{"id": "flaky", "base_url": deadURL(t)})
	status, raw = rawBody(t, h.postJSON(t, "/api/llm/providers/flaky/models/refresh", map[string]any{}))
	if status != http.StatusBadGateway {
		t.Fatalf("failing refresh = %d, want 502 (body: %s)", status, raw)
	}
	var errBody map[string]string
	if err := json.Unmarshal([]byte(raw), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if strings.TrimSpace(errBody["error"]) == "" {
		t.Errorf("body %q should explain the failure", raw)
	}

	var listed modelsResponse
	h.getJSON(t, "/api/llm/models?provider=flaky", http.StatusOK, &listed)
	if len(listed.Models) != 2 {
		t.Fatalf("models = %+v, want the previously fetched two to survive", listed.Models)
	}

	var providers struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &providers)
	for _, p := range providers.Providers {
		if p.ID == "flaky" && p.LastError == "" {
			t.Error("a failed refresh should record last_error")
		}
	}
}

func TestProviders_RefreshOnMalformedBody(t *testing.T) {
	// A base_url pointing at something that is not an OpenAI-compatible API is a
	// configuration mistake, and the error must say so rather than storing an
	// empty model list.
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", http.StatusOK, `<html><body>hello</body></html>`)
	createProvider(t, h, map[string]any{"id": "wrong-api", "base_url": backend.URL})

	status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/wrong-api/models/refresh", map[string]any{}))
	if status != http.StatusBadGateway {
		t.Fatalf("refresh = %d, want 502 (body: %s)", status, raw)
	}
	if strings.Contains(raw, "<html>") {
		t.Errorf("the upstream HTML body should be bounded and not echoed whole: %s", raw)
	}
}

func TestModels_ManualCapabilityEditSticks(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", 0, "", "deepseek-chat", "gpt-4o")
	createProvider(t, h, map[string]any{"id": "manual", "name": "Manual", "base_url": backend.URL})
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/manual/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("refresh = %d, want 200 (body: %s)", status, raw)
	}

	// gpt-4o is only a guess: the user decides it cannot do vision here.
	var one struct {
		Model store.Model `json:"model"`
	}
	resp := h.putJSON(t, "/api/llm/models", map[string]any{
		"provider_id": "manual", "model_id": "gpt-4o", "capabilities": []string{"chat"},
	})
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("edit status = %d, want 200 (body: %s)", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := one.Model.Capabilities.String(); got != "chat" {
		t.Errorf("capabilities = %q, want chat", got)
	}
	if one.Model.Source != "fetched" {
		t.Errorf("source = %q, want the fetched row to stay fetched", one.Model.Source)
	}

	// An edit survives a refresh: a refresh learns ids, not capabilities.
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/manual/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("second refresh = %d, want 200 (body: %s)", status, raw)
	}
	var listed modelsResponse
	h.getJSON(t, "/api/llm/models?provider=manual", http.StatusOK, &listed)
	if got := capabilitiesOf(t, listed.Models, "gpt-4o"); got != "chat" {
		t.Errorf("gpt-4o after refresh = %q, want the manual edit to survive", got)
	}

	// A hand-added model, with a name and a hand-picked capability set, also
	// survives a refresh that does not list it.
	resp = h.putJSON(t, "/api/llm/models", map[string]any{
		"provider_id": "manual", "model_id": "local-only-model",
		"display_name": "Local Only", "capabilities": []string{"chat", "vision"},
	})
	status, raw = rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("add status = %d, want 200 (body: %s)", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if one.Model.DisplayName != "Local Only" || one.Model.Source != "user" {
		t.Errorf("model = %+v, want a user model with the given display name", one.Model)
	}
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/manual/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("third refresh = %d, want 200 (body: %s)", status, raw)
	}
	h.getJSON(t, "/api/llm/models?provider=manual", http.StatusOK, &listed)
	if got := capabilitiesOf(t, listed.Models, "local-only-model"); got != "chat,vision" {
		t.Errorf("hand-added model = %q, want it preserved", got)
	}

	// A user's display name is not clobbered by a refresh either.
	resp = h.putJSON(t, "/api/llm/models", map[string]any{
		"provider_id": "manual", "model_id": "deepseek-chat",
		"display_name": "DeepSeek Chat (fast)", "capabilities": []string{"chat"},
	})
	if status, raw := rawBody(t, resp); status != http.StatusOK {
		t.Fatalf("rename = %d, want 200 (body: %s)", status, raw)
	}
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/manual/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("fourth refresh = %d, want 200 (body: %s)", status, raw)
	}
	h.getJSON(t, "/api/llm/models?provider=manual", http.StatusOK, &listed)
	for _, m := range listed.Models {
		if m.ModelID == "deepseek-chat" && m.DisplayName != "DeepSeek Chat (fast)" {
			t.Errorf("display_name = %q, want the user's name kept", m.DisplayName)
		}
	}
}

func TestModels_ValidationAndDelete(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", 0, "", "deepseek-chat")
	createProvider(t, h, map[string]any{"id": "v", "base_url": backend.URL})
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/v/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("refresh = %d (body: %s)", status, raw)
	}

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "missing ids", body: map[string]any{"capabilities": []string{"chat"}}},
		{name: "no capabilities", body: map[string]any{"provider_id": "v", "model_id": "deepseek-chat", "capabilities": []string{}}},
		{name: "unknown capability", body: map[string]any{
			"provider_id": "v", "model_id": "deepseek-chat", "capabilities": []string{"telepathy"},
		}},
		{name: "unknown provider", body: map[string]any{
			"provider_id": "nope", "model_id": "m", "capabilities": []string{"chat"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := rawBody(t, h.putJSON(t, "/api/llm/models", tc.body))
			if status < 400 || status >= 500 {
				t.Fatalf("status = %d, want 4xx (body: %s)", status, raw)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatalf("decode %q: %v", raw, err)
			}
			if strings.TrimSpace(body["error"]) == "" {
				t.Errorf("body %q should explain the refusal", raw)
			}
		})
	}

	// Deleting needs both ids, and an unknown model is a 404.
	status, raw := rawBody(t, h.deleteJSONBody(t, "/api/llm/models", map[string]any{"provider_id": "v"}))
	if status != http.StatusBadRequest {
		t.Errorf("delete without a model = %d, want 400 (body: %s)", status, raw)
	}
	status, raw = rawBody(t, h.deleteJSONBody(t, "/api/llm/models", map[string]any{
		"provider_id": "v", "model_id": "ghost",
	}))
	if status != http.StatusNotFound {
		t.Errorf("delete of an unknown model = %d, want 404 (body: %s)", status, raw)
	}
	status, raw = rawBody(t, h.deleteJSONBody(t, "/api/llm/models", map[string]any{
		"provider_id": "v", "model_id": "deepseek-chat",
	}))
	if status != http.StatusOK {
		t.Fatalf("delete = %d, want 200 (body: %s)", status, raw)
	}
	var listed modelsResponse
	h.getJSON(t, "/api/llm/models?provider=v", http.StatusOK, &listed)
	if len(listed.Models) != 0 {
		t.Errorf("models = %+v, want none", listed.Models)
	}
}

func TestBindings_SetListAndClear(t *testing.T) {
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", 0, "", "deepseek-chat", "gpt-4o")
	createProvider(t, h, map[string]any{"id": "b", "name": "B Vendor", "base_url": backend.URL})
	if status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/b/models/refresh", map[string]any{})); status != http.StatusOK {
		t.Fatalf("refresh = %d (body: %s)", status, raw)
	}

	// Bind chat and vision to two different models.
	for _, pair := range []struct{ capability, model string }{
		{"chat", "deepseek-chat"},
		{"vision", "gpt-4o"},
	} {
		resp := h.putJSON(t, "/api/llm/bindings", map[string]any{
			"capability": pair.capability, "provider_id": "b", "model_id": pair.model,
		})
		status, raw := rawBody(t, resp)
		if status != http.StatusOK {
			t.Fatalf("bind %s = %d, want 200 (body: %s)", pair.capability, status, raw)
		}
		var out bindingsResponse
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The response carries the whole list, joined with the names the UI
		// shows, so no follow-up request is needed.
		found := false
		for _, b := range out.Bindings {
			if string(b.Capability) == pair.capability {
				found = true
				if b.ProviderName != "B Vendor" {
					t.Errorf("provider_name = %q, want the display name", b.ProviderName)
				}
				if b.ModelName != pair.model {
					t.Errorf("model_name = %q, want %q", b.ModelName, pair.model)
				}
			}
		}
		if !found {
			t.Errorf("binding %s missing from %+v", pair.capability, out.Bindings)
		}
	}

	var listed bindingsResponse
	h.getJSON(t, "/api/llm/bindings", http.StatusOK, &listed)
	if len(listed.Bindings) != 2 {
		t.Fatalf("bindings = %+v, want two", listed.Bindings)
	}

	// A blank model clears the binding, which is how a capability is switched
	// off without deleting anything.
	resp := h.putJSON(t, "/api/llm/bindings", map[string]any{
		"capability": "vision", "provider_id": "", "model_id": "",
	})
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("clear = %d, want 200 (body: %s)", status, raw)
	}
	var cleared bindingsResponse
	if err := json.Unmarshal([]byte(raw), &cleared); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cleared.Bindings) != 1 || cleared.Bindings[0].Capability != "chat" {
		t.Errorf("bindings = %+v, want only chat", cleared.Bindings)
	}

	// An unknown capability is refused by name.
	status, raw = rawBody(t, h.putJSON(t, "/api/llm/bindings", map[string]any{
		"capability": "telepathy", "provider_id": "b", "model_id": "gpt-4o",
	}))
	if status != http.StatusBadRequest {
		t.Fatalf("unknown capability = %d, want 400 (body: %s)", status, raw)
	}
	if !strings.Contains(raw, "telepathy") {
		t.Errorf("body %q should name the offending capability", raw)
	}

	// Missing capability, a model without a provider, and an unknown model.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "no capability", body: map[string]any{"provider_id": "b", "model_id": "gpt-4o"}},
		{name: "model without provider", body: map[string]any{"capability": "vision", "model_id": "gpt-4o"}},
		{name: "unknown model", body: map[string]any{"capability": "vision", "provider_id": "b", "model_id": "ghost"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := rawBody(t, h.putJSON(t, "/api/llm/bindings", tc.body))
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, raw)
			}
		})
	}

	// Deleting the model clears the binding that pointed at it, so a capability
	// cannot be left bound to something that no longer exists.
	status, raw = rawBody(t, h.deleteJSONBody(t, "/api/llm/models", map[string]any{
		"provider_id": "b", "model_id": "deepseek-chat",
	}))
	if status != http.StatusOK {
		t.Fatalf("delete model = %d, want 200 (body: %s)", status, raw)
	}
	h.getJSON(t, "/api/llm/bindings", http.StatusOK, &listed)
	if len(listed.Bindings) != 0 {
		t.Errorf("bindings = %+v, want the binding dropped with its model", listed.Bindings)
	}
}

// seedConfigProvider plants a config-sourced provider, as the start-up sync
// would.
func seedConfigProvider(st store.Store) {
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, store.Provider{
		ID: "from-config", Name: "from-config", BaseURL: "https://api.config.example/v1",
		Kind: "openai", Source: store.SourceConfig, Enabled: true,
	}); err != nil {
		panic(err)
	}
	if err := st.SetProviderKey(ctx, "from-config", "sk-from-the-config-file"); err != nil {
		panic(err)
	}
	if err := st.UpsertModel(ctx, store.Model{
		ProviderID: "from-config", ModelID: "deepseek-chat", DisplayName: "deepseek-chat",
		Capabilities: store.Capabilities{store.CapChat}, Enabled: true, Source: "config",
	}); err != nil {
		panic(err)
	}
}

func TestProviders_ConfigSourcedIsReadOnlyButTogglable(t *testing.T) {
	h := newProviderHarnessWith(t, seedConfigProvider)

	var list struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &list)
	if len(list.Providers) != 1 || list.Providers[0].Source != store.SourceConfig {
		t.Fatalf("providers = %+v, want the config provider", list.Providers)
	}
	if !list.Providers[0].HasAPIKey {
		t.Error("a config provider's key should be reported as present")
	}

	// Editing anything the config file owns is refused, with a message that
	// says where to edit it instead.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "base_url", body: map[string]any{"id": "from-config", "base_url": "https://elsewhere.example/v1"}},
		{name: "name", body: map[string]any{"id": "from-config", "name": "Renamed", "base_url": "https://api.config.example/v1"}},
		{name: "api_key", body: map[string]any{"id": "from-config", "base_url": "https://api.config.example/v1", "api_key": "sk-new"}},
		{name: "api_key_env", body: map[string]any{"id": "from-config", "base_url": "https://api.config.example/v1", "api_key_env": "SOME_VAR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers", tc.body))
			if status != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body: %s)", status, raw)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body["error"], "config") {
				t.Errorf("error %q should say the provider comes from the config file", body["error"])
			}
		})
	}

	// Toggling enabled is the one edit that sticks, so a user can switch a
	// config-declared provider off from the console.
	var one providerResponse
	resp := h.postJSON(t, "/api/llm/providers", map[string]any{"id": "from-config", "enabled": false})
	status, raw := rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("toggle = %d, want 200 (body: %s)", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if one.Provider.Enabled {
		t.Error("the provider should be disabled")
	}
	if one.Provider.BaseURL != "https://api.config.example/v1" {
		t.Errorf("base_url = %q, want it untouched", one.Provider.BaseURL)
	}

	// Its key is not settable here either, and it cannot be deleted.
	status, raw = rawBody(t, h.putJSON(t, "/api/llm/providers/from-config/key", map[string]string{"api_key": "sk-x"}))
	if status != http.StatusConflict {
		t.Errorf("key edit = %d, want 409 (body: %s)", status, raw)
	}
	status, raw = rawBody(t, h.deleteJSON(t, "/api/llm/providers/from-config"))
	if status != http.StatusConflict {
		t.Fatalf("delete = %d, want 409 (body: %s)", status, raw)
	}
	if !strings.Contains(raw, "config") {
		t.Errorf("body %q should explain that the provider comes from the config file", raw)
	}
	var after struct {
		Providers []store.Provider `json:"providers"`
	}
	h.getJSON(t, "/api/llm/providers", http.StatusOK, &after)
	if len(after.Providers) != 1 {
		t.Errorf("providers = %+v, want the config provider to still exist", after.Providers)
	}

	// The config-declared model is editable, though: its capabilities are a hint
	// the user confirms.
	resp = h.putJSON(t, "/api/llm/models", map[string]any{
		"provider_id": "from-config", "model_id": "deepseek-chat",
		"capabilities": []string{"chat", "vision"},
	})
	status, raw = rawBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("model edit = %d, want 200 (body: %s)", status, raw)
	}
	var edited struct {
		Model store.Model `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &edited); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := edited.Model.Capabilities.String(); got != "chat,vision" {
		t.Errorf("capabilities = %q, want the edit applied", got)
	}
}

func TestProviders_UnknownIDsAreNotFound(t *testing.T) {
	h := newProviderHarness(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "delete", method: http.MethodDelete, path: "/api/llm/providers/ghost"},
		{name: "key", method: http.MethodPut, path: "/api/llm/providers/ghost/key", body: map[string]string{"api_key": "k"}},
		{name: "test", method: http.MethodPost, path: "/api/llm/providers/ghost/test", body: map[string]any{}},
		{name: "refresh", method: http.MethodPost, path: "/api/llm/providers/ghost/models/refresh", body: map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			switch tc.method {
			case http.MethodDelete:
				resp = h.deleteJSON(t, tc.path)
			case http.MethodPut:
				resp = h.putJSON(t, tc.path, tc.body)
			default:
				resp = h.postJSON(t, tc.path, tc.body)
			}
			status, raw := rawBody(t, resp)
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body: %s)", status, raw)
			}
		})
	}

	// The unauthenticated case must not be an accidental way in.
	fresh := &harness{base: h.base, client: newJar(t), srv: h.srv, store: h.store}
	resp, err := fresh.client.Post(fresh.base+"/api/llm/providers", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	status, _ := rawBody(t, resp)
	if status != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d, want 401", status)
	}
}

func TestProviders_RefreshAgainstNoModels(t *testing.T) {
	// A provider that legitimately lists nothing leaves an empty list rather
	// than an error, so a local server still mid-load can be retried.
	h := newProviderHarness(t)
	backend := modelsBackend(t, "", 0, "")
	createProvider(t, h, map[string]any{"id": "empty", "base_url": backend.URL})

	status, raw := rawBody(t, h.postJSON(t, "/api/llm/providers/empty/models/refresh", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("refresh = %d, want 200 (body: %s)", status, raw)
	}
	var out modelsResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Models) != 0 || out.Count != 0 {
		t.Errorf("out = %+v, want an empty list", out)
	}
}

func TestProviders_CapabilityVocabularyIsReturned(t *testing.T) {
	// The UI renders the binding form from this list rather than hardcoding it.
	h := newProviderHarness(t)
	var out struct {
		Providers    []store.Provider `json:"providers"`
		Capabilities []string         `json:"capabilities"`
	}
	resp := h.get(t, "/api/llm/providers")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fmt.Sprint(out.Capabilities) != fmt.Sprint([]string{
		"chat", "vision", "image_gen", "audio_transcribe", "audio_speech", "embedding",
	}) {
		t.Errorf("capabilities = %v", out.Capabilities)
	}
	if out.Providers == nil {
		t.Error("providers should be an empty array, not null")
	}
}
