package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
)

// DefaultModelsTimeout bounds a /models call when the caller does not pass one.
// It is generous enough for a slow provider but short enough that a UI "test"
// button answers before a user gives up on it.
const DefaultModelsTimeout = 10 * time.Second

// maxModelsBody caps how much of a /models response is read. Some providers
// list thousands of models, so the cap is high — but not unbounded, so a
// misconfigured base_url pointing at a file server cannot exhaust memory.
const maxModelsBody = 4 << 20

// maxErrorBody caps how much of a failing response is quoted back. Providers
// put the real reason in the body (an auth message, a quota notice), so it is
// worth showing — but not a whole HTML error page.
const maxErrorBody = 512

// ModelInfo is one entry from GET {base_url}/models.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// FetchModels calls GET {base_url}/models with the provider's key and returns
// the ids it lists. It must respect a context deadline and give a useful error.
//
// Three response shapes are seen in the wild and all are tolerated:
// {"data":[{"id":...}]} (the OpenAI shape), {"models":[...]} and a bare array.
// Anything else is reported as an error naming what was unexpected, rather than
// left to an unmarshal panic.
//
// The key is only ever written into the Authorization header. It never reaches
// an error message, a log line or a return value.
func FetchModels(ctx context.Context, baseURL, apiKey string, timeout time.Duration) ([]ModelInfo, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("llm: fetch models: base_url is required")
	}
	endpoint := base + "/models"
	// Validate before dialling so a typo in the settings page reads as a
	// configuration error instead of "unsupported protocol scheme".
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("llm: fetch models: invalid base_url %q: %w", baseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("llm: fetch models: base_url %q must be an absolute http(s) URL", baseURL)
	}

	if timeout <= 0 {
		timeout = DefaultModelsTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("llm: fetch models %s: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	if k := strings.TrimSpace(apiKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		// The error is a transport error: it carries the URL, never the headers,
		// so the key cannot leak through it.
		return nil, fmt.Errorf("llm: fetch models %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if readErr != nil {
		return nil, fmt.Errorf("llm: fetch models %s: read body: %w", endpoint, readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("llm: fetch models %s: provider returned %s: %s",
			endpoint, resp.Status, snippet(body, maxErrorBody))
	}

	models, err := parseModels(body)
	if err != nil {
		return nil, fmt.Errorf("llm: fetch models %s: %w", endpoint, err)
	}
	return models, nil
}

// parseModels decodes the response body, tolerating the known shapes.
func parseModels(body []byte) ([]ModelInfo, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty response body, want a JSON model list")
	}

	if trimmed[0] == '[' {
		var arr []ModelInfo
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("cannot parse the response as a bare array of {\"id\": ...} objects "+
				"(body starts with %q): %w", snippet(trimmed, 80), err)
		}
		return cleanModels(arr), nil
	}

	var envelope struct {
		Data   []ModelInfo `json:"data"`
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("cannot parse the response as {\"data\": [...]}, {\"models\": [...]} or a bare array "+
			"(body starts with %q): %w", snippet(trimmed, 80), err)
	}
	switch {
	case envelope.Data != nil:
		return cleanModels(envelope.Data), nil
	case envelope.Models != nil:
		return cleanModels(envelope.Models), nil
	}
	return nil, fmt.Errorf("unexpected response shape: expected an object with a \"data\" array, "+
		"an object with a \"models\" array, or a bare array (body starts with %q)", snippet(trimmed, 80))
}

// cleanModels drops entries without an id and duplicates, keeping the order the
// provider reported. A duplicate id would otherwise produce two rows for one
// model, and an empty id is not a model name.
func cleanModels(in []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m.ID = id
		out = append(out, m)
	}
	return out
}

// snippet renders a bounded, single-line view of an upstream body for an error
// message, so a multi-line HTML page cannot flood a log.
func snippet(b []byte, max int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if s == "" {
		return "(empty body)"
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Inference hints, matched case-insensitively as substrings of the model id.
// They are ordered by family; the exclusive families (image generation, audio,
// embeddings) come first because such a model must not be offered as a chat
// model, even when its id also mentions something chat-like.
var (
	// imageGenHints: a model that produces images rather than text.
	imageGenHints = []string{
		"dall-e", "gpt-image", "stable-diffusion", "sd3", "flux", "kolors", "wanx", "imagen", "seedream",
	}
	// transcribeHints: speech in, text out.
	transcribeHints = []string{"whisper", "transcribe", "asr", "paraformer", "sensevoice"}
	// speechHints: text in, speech out.
	speechHints = []string{"tts", "speech", "cosyvoice", "sambert"}
	// embeddingHints: vectors, not conversation.
	embeddingHints = []string{"embed", "bge", "text-embedding", "gte"}
	// visionHints: a chat model that also accepts images.
	visionHints = []string{
		"-vl", "vl-", "vision", "gpt-4o", "gpt-4.1", "gpt-4-turbo",
		"claude-3", "claude-4", "gemini", "llava", "pixtral", "qwen-vl", "internvl", "minicpm-v",
	}
)

// InferCapabilities guesses what a model can do from its id.
//
// The /models endpoint does NOT report capabilities — it returns ids only — so
// this is a HINT the user then confirms in the UI. It must never be presented
// as authoritative: a provider is free to name a chat model "embedding-pro" or a
// vision model anything at all, and the length of the hint lists below is not a
// guarantee that they cover a given vendor. The stored capabilities are always
// whatever was last written, and the UI lets the user correct them; nothing
// downstream may rely on this inference being right.
//
// The rules are:
//   - every model is a chat model by default;
//   - image-generation, audio and embedding ids get ONLY their own capability,
//     because offering an image generator or a transcriber as a chat model would
//     produce a call that cannot succeed;
//   - vision ids additionally get vision, on top of chat.
func InferCapabilities(modelID string) store.Capabilities {
	id := strings.ToLower(strings.TrimSpace(modelID))

	switch {
	case containsAny(id, imageGenHints):
		return store.Capabilities{store.CapImageGen}
	case containsAny(id, transcribeHints):
		return store.Capabilities{store.CapAudioTranscribe}
	case containsAny(id, speechHints):
		return store.Capabilities{store.CapAudioSpeech}
	case containsAny(id, embeddingHints):
		return store.Capabilities{store.CapEmbedding}
	}

	caps := store.Capabilities{store.CapChat}
	if containsAny(id, visionHints) {
		caps = append(caps, store.CapVision)
	}
	return caps
}

// containsAny reports whether s contains any of the hints.
func containsAny(s string, hints []string) bool {
	for _, h := range hints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// SyncConfigProviders upserts config-declared providers as source=config, so
// the UI can show them read-only and a runtime edit cannot be silently lost.
// It must NOT delete user-sourced providers.
//
// It is idempotent: running it twice leaves the same rows. A blank api_key in
// the config does not clear a key already stored, so an operator who pasted a
// key through the console keeps it while the config file stays key-free.
//
// The config-declared default provider's model is recorded as a chat-capable
// model row, so the UI has something to bind a capability to before anyone
// fetches a model list.
func SyncConfigProviders(ctx context.Context, st store.Store, providers map[string]config.LLMProvider, defaultProvider string) error {
	if st == nil {
		return errors.New("llm: sync config providers: store is required")
	}

	// Deterministic order keeps a two-run comparison (and the logs) stable.
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		id := strings.TrimSpace(name)
		if id == "" {
			continue
		}
		p := providers[name]

		// Preserve an existing row's Enabled flag. The config file declares
		// what a provider IS; whether it is switched on is a runtime choice the
		// console makes, and re-enabling it at every start would silently undo
		// a user turning a provider off.
		enabled := true
		if existing, err := st.GetProvider(ctx, id); err == nil {
			enabled = existing.Enabled
		}

		if err := st.UpsertProvider(ctx, store.Provider{
			ID:      id,
			Name:    id,
			BaseURL: strings.TrimSpace(p.BaseURL),
			Kind:    "openai",
			Source:  store.SourceConfig,
			Enabled: enabled,
		}); err != nil {
			return fmt.Errorf("llm: sync config provider %q: %w", id, err)
		}
		// The literal key from the config file is stored through its own setter,
		// because Provider's key field is unexported by design — which is what
		// keeps it out of every API response.
		if key := strings.TrimSpace(p.APIKey); key != "" {
			if err := st.SetProviderKey(ctx, id, key); err != nil {
				return fmt.Errorf("llm: sync config provider %q key: %w", id, err)
			}
		}
	}

	dp := strings.TrimSpace(defaultProvider)
	p, ok := providers[dp]
	if !ok {
		return nil
	}
	model := strings.TrimSpace(p.Model)
	if model == "" {
		return nil
	}
	if err := st.UpsertModel(ctx, store.Model{
		ProviderID:   dp,
		ModelID:      model,
		DisplayName:  model,
		Capabilities: store.Capabilities{store.CapChat},
		Enabled:      true,
		Source:       string(store.SourceConfig),
	}); err != nil {
		return fmt.Errorf("llm: sync config default model %s/%s: %w", dp, model, err)
	}
	return nil
}
