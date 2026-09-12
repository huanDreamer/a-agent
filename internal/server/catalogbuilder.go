package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

// providerKindOpenAI is the only provider kind this build can call: every
// supported backend speaks the OpenAI HTTP protocol, so a provider declaring
// anything else is refused rather than silently treated as OpenAI-compatible.
const providerKindOpenAI = "openai"

// maxModelCache bounds the constructed-model cache. Providers x models is a small
// number in practice; the bound only stops a caller growing the map with
// arbitrary model strings.
const maxModelCache = 64

// ModelBuilderOptions configures a CatalogModelBuilder.
type ModelBuilderOptions struct {
	// TTL is how long a fetched model list stays fresh. Zero means "never
	// stale": nothing is ever refetched automatically and the console shows no
	// stale marker.
	TTL time.Duration
	// Logger receives the warnings a degraded catalog produces (a store read
	// that failed, a fallback to the config registry). Nil uses a no-op logger.
	Logger *zap.Logger
}

// CatalogModelBuilder is the ModelBuilder the console runs on: the database
// catalog is the source of truth, and the config-file registry is the fallback.
//
// # Why the store
//
// The 设置 panel writes providers, their fetched models and their capabilities
// into `llm_providers` / `llm_models` / `llm_bindings`. Before this builder the
// chat selector read the config registry instead, which knows exactly one model
// per provider: a provider added in 设置 was invisible to the chat, and even
// naming its model could not build a client. Reading the store for both surfaces
// is what makes a provider added in the console immediately usable in a
// conversation.
//
// # Why the config registry stays
//
// A deployment may have no catalog rows at all — a fresh database, or a config
// file that declares llm.providers and was never opened in the console — and it
// must keep working exactly as before. So a provider the store does not know is
// still resolved from the registry (Build), and an empty store still yields the
// registry's one-entry-per-provider catalog (Catalog). The config path is only
// an *addition* to what the store offers, never a second opinion about a
// provider the store does know: for a known provider the store's row decides
// identity, base_url, key and enabled flag.
type CatalogModelBuilder struct {
	st     store.Store
	reg    *llm.Registry
	ttl    time.Duration
	logger *zap.Logger

	mu    sync.Mutex
	cache map[string]cachedModel
}

// cachedModel is one constructed model plus the provider fields it was built
// from.
type cachedModel struct {
	built     model.BaseChatModel
	baseURL   string
	kind      string
	updatedAt time.Time
}

// NewCatalogModelBuilder returns a builder over the store, falling back to reg.
//
// Either may be nil: a nil store serves the config registry only (which is what
// the tests of the config path use), and a nil registry serves the store only.
func NewCatalogModelBuilder(st store.Store, reg *llm.Registry, opts ModelBuilderOptions) *CatalogModelBuilder {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CatalogModelBuilder{
		st:     st,
		reg:    reg,
		ttl:    opts.TTL,
		logger: logger,
	}
}

// compile-time check that the builder satisfies the interface.
var _ ModelBuilder = (*CatalogModelBuilder)(nil)

// Build returns a chat model for a provider and model name.
//
// Resolution order:
//
//  1. A provider the store knows is authoritative: it must be enabled, and the
//     model is built from the stored base_url and API key.
//  2. A provider the store does not know falls back to the config registry, so a
//     config-only setup is unaffected by the catalog being empty.
//
// An empty provider selects the catalog's default (see defaultTarget).
func (b *CatalogModelBuilder) Build(ctx context.Context, provider, modelName string) (any, error) {
	provider = strings.TrimSpace(provider)
	modelName = strings.TrimSpace(modelName)

	if provider == "" {
		// The catalog already implements the default rule; asking it keeps the
		// model a turn runs on and the model the picker highlights identical.
		provider, modelName = b.defaultTarget(ctx)
	}
	if provider == "" {
		return nil, errors.New("server: no LLM provider is configured: add one in 设置 → 模型管理, " +
			"or declare llm.providers in the config file")
	}

	if b.st != nil {
		p, err := b.st.GetProvider(ctx, provider)
		switch {
		case err == nil:
			return b.buildStored(ctx, p, modelName)
		case !errors.Is(err, store.ErrNotFound):
			return nil, fmt.Errorf("server: read provider %q: %w", provider, err)
		}
	}

	// The store does not have this provider. The config registry may still
	// declare it — an install that predates the catalog, or one whose provider
	// sync failed — and that setup must keep working.
	if b.reg == nil {
		return nil, fmt.Errorf("server: provider %q is not configured: add it in 设置 → 模型管理, "+
			"or declare it under llm.providers in the config file", provider)
	}
	built, err := b.reg.GetWithModel(provider, modelName)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	return built, nil
}

// buildStored constructs a model from a stored provider.
func (b *CatalogModelBuilder) buildStored(ctx context.Context, p store.Provider, modelName string) (any, error) {
	// A disabled provider is refused, not silently used: the operator turned it
	// off, and a session that keeps calling it would contradict 设置.
	if !p.Enabled {
		return nil, fmt.Errorf("server: provider %q is disabled in 模型管理: enable it before using its models", p.ID)
	}
	if kind := strings.TrimSpace(p.Kind); kind != "" && kind != providerKindOpenAI {
		return nil, fmt.Errorf("server: provider %q declares kind %q, which this build cannot call: "+
			"only %q (an OpenAI-compatible endpoint) is supported", p.ID, kind, providerKindOpenAI)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, fmt.Errorf("server: provider %q has no base_url: set it in 模型管理", p.ID)
	}

	if modelName == "" {
		// A blank model means "whatever this provider offers": its first
		// enabled chat-capable model, by name. Deterministic, so two runs of the
		// same catalog pick the same model.
		first, ok := b.firstChatModel(ctx, p.ID)
		if !ok {
			// Nothing stored for it. A config-declared provider may still be
			// usable before its first fetch: the config names a model for it, or
			// llm.NewRegistry supplies the built-in default for a known name.
			if p.Source == store.SourceConfig {
				first = b.configModelName(p.ID)
			}
			if first == "" {
				return nil, fmt.Errorf("server: provider %q has no enabled model: refresh its model list in 模型管理", p.ID)
			}
		}
		modelName = first
	}

	// The cache is keyed by provider AND model, and is only trusted while the
	// provider row it was built from is unchanged. Invalidation is by
	// comparison, not by a TTL: the row is read on every Build, so an edit to
	// base_url, the key, the kind or the enabled flag is picked up on the next
	// turn. SetProviderError also bumps updated_at, so a failed refresh merely
	// costs one rebuild.
	//
	// base_url and kind are compared as well as updated_at because SQLite's
	// CURRENT_TIMESTAMP has one-second resolution: an edit made in the same
	// second as the previous build would otherwise look like no change. Only a
	// key swapped within that same second can keep the old client for one turn.
	cacheKey := p.ID + "\x00" + modelName
	if built, ok := b.cached(cacheKey, p); ok {
		return built, nil
	}

	client := llm.Provider{
		Name:    p.ID,
		BaseURL: p.BaseURL,
		// api_key_env wins over the stored key, so a key in the environment
		// overrides a stale copy in the database.
		APIKey: p.ResolveAPIKey(os.Getenv),
		Model:  modelName,
	}
	// A config-declared provider keeps the protocol quirks the config file
	// carries: the store has columns for identity and credentials, but none for
	// disable_usage_request, and losing it would make every streamed call fail
	// against a provider that rejects that OpenAI extension.
	if p.Source == store.SourceConfig && b.reg != nil {
		if cp, ok := b.reg.Provider(p.ID); ok {
			client.DisableUsageRequest = cp.DisableUsageRequest
		}
	}

	// No key is a warning, not a refusal: a local endpoint (ollama and friends)
	// needs none, and the catalog already marks the provider so the UI can say
	// the key is missing.
	if client.APIKey == "" {
		b.logger.Warn("model provider has no API key; calls will likely be rejected",
			zapString("provider", p.ID))
	}

	built, err := llm.New(client)
	if err != nil {
		return nil, fmt.Errorf("server: build %s/%s: %w", p.ID, modelName, err)
	}
	b.remember(cacheKey, built, p.BaseURL, p.Kind, p.UpdatedAt)
	return built, nil
}

// Catalog returns every selectable model with the provider summaries behind it.
func (b *CatalogModelBuilder) Catalog(ctx context.Context) ModelCatalog {
	cat := b.readCatalog(ctx)
	b.markDefault(ctx, &cat)
	return cat
}

// readCatalog reads the store, falling back to the config registry when the
// store has nothing to say (no providers yet, or a read that failed).
func (b *CatalogModelBuilder) readCatalog(ctx context.Context) ModelCatalog {
	if b.st == nil {
		return b.registryCatalog()
	}
	providers, err := b.st.ListProviders(ctx)
	if err != nil {
		b.logger.Warn("list providers failed; serving the config registry instead", zapError(err))
		return b.registryCatalog()
	}
	if len(providers) == 0 {
		return b.registryCatalog()
	}
	// One query for every provider's models, rather than one per provider: the
	// console reads this endpoint on every 设置 and 对话 render.
	models, err := b.st.ListModels(ctx, "")
	if err != nil {
		b.logger.Warn("list models failed; serving the config registry instead", zapError(err))
		return b.registryCatalog()
	}
	return b.storeCatalog(providers, models)
}

// storeCatalog assembles the catalog from stored rows.
//
// Which models are offered:
//
//   - only enabled providers contribute, and only their enabled models;
//   - a provider's chat-capable models are what the chat can actually run, so
//     they are what is offered;
//   - if a provider has NO chat-capable model, its models are offered anyway and
//     marked chat_capable=false. Capabilities are inferred from the model name
//     and corrected by the operator, so a wrong inference (a chat model whose id
//     contains "embed", say) must not make a whole provider disappear from the
//     chat selector — the operator needs to see it, pick it if the inference is
//     wrong, and fix the capability in 设置.
func (b *CatalogModelBuilder) storeCatalog(providers []store.Provider, models []store.Model) ModelCatalog {
	byProvider := make(map[string][]store.Model, len(providers))
	for _, m := range models {
		byProvider[m.ProviderID] = append(byProvider[m.ProviderID], m)
	}

	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		rows := byProvider[p.ID]

		enabled := make([]store.Model, 0, len(rows))
		chat := make([]store.Model, 0, len(rows))
		for _, m := range rows {
			if !m.Enabled {
				continue
			}
			enabled = append(enabled, m)
			if m.Has(store.CapChat) {
				chat = append(chat, m)
			}
		}
		offered := chat
		if len(offered) == 0 {
			offered = enabled
		}
		// A config-declared provider with no stored model still has one: the
		// model the config file names for it — or the built-in default for a
		// known provider name, which llm.NewRegistry fills in (a config that
		// only sets llm.providers.glm.api_key relies on it). Offering it keeps a
		// config-only install working exactly as it did before the catalog
		// existed, and such a provider is stale by definition, so the startup
		// pass replaces this with its real list.
		synthetic := false
		if len(offered) == 0 && p.Enabled && p.Source == store.SourceConfig {
			if name := b.configModelName(p.ID); name != "" {
				offered = []store.Model{{
					ProviderID:   p.ID,
					ModelID:      name,
					DisplayName:  name,
					Capabilities: store.Capabilities{store.CapChat},
					Enabled:      true,
					Source:       string(store.SourceConfig),
				}}
				synthetic = true
			}
		}
		// Models by name, deterministically (the id is unique per provider).
		sort.Slice(offered, func(i, j int) bool { return offered[i].ModelID < offered[j].ModelID })

		lastFetched, hasFetched := newestFetchedAt(rows)
		v := providerView{
			provider:    p,
			name:        providerDisplayName(p),
			offered:     offered,
			modelCount:  len(rows),
			enabled:     len(enabled),
			chatCount:   len(chat),
			stale:       providerModelsStale(rows, b.ttl),
			hasFetched:  hasFetched,
			lastFetched: lastFetched,
		}
		if synthetic {
			// The config's model is offered but has no row yet: the counts must
			// agree with what is listed, or the panel would read "0 个模型" beside
			// a model.
			v.modelCount, v.enabled, v.chatCount = 1, 1, 1
		}
		views = append(views, v)
	}

	// Providers by name; the id breaks a tie so the order never depends on map
	// iteration or on insertion order.
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].name != views[j].name {
			return views[i].name < views[j].name
		}
		return views[i].provider.ID < views[j].provider.ID
	})

	out := ModelCatalog{
		Models:    make([]ModelChoice, 0, len(models)),
		Providers: make([]ProviderChoice, 0, len(views)),
	}
	for _, v := range views {
		p := v.provider
		summary := ProviderChoice{
			ID:                p.ID,
			Name:              v.name,
			Source:            string(p.Source),
			Enabled:           p.Enabled,
			HasAPIKey:         p.HasAPIKey,
			ModelCount:        v.modelCount,
			EnabledModelCount: v.enabled,
			ChatModelCount:    v.chatCount,
			Stale:             v.stale,
			LastError:         p.LastError,
		}
		if v.hasFetched {
			at := v.lastFetched
			summary.LastFetchedAt = &at
		}
		out.Providers = append(out.Providers, summary)

		if !p.Enabled {
			continue
		}
		for _, m := range v.offered {
			out.Models = append(out.Models, ModelChoice{
				Provider:     p.ID,
				ProviderName: v.name,
				Model:        m.ModelID,
				DisplayName:  modelDisplayName(m),
				Capabilities: capabilityStrings(m.Capabilities),
				ChatCapable:  m.Has(store.CapChat),
				HasAPIKey:    p.HasAPIKey,
			})
		}
	}
	return out
}

// providerView is one provider's catalog slice while it is being assembled.
type providerView struct {
	provider    store.Provider
	name        string
	offered     []store.Model
	modelCount  int
	enabled     int
	chatCount   int
	stale       bool
	hasFetched  bool
	lastFetched time.Time
}

// registryCatalog is the config-registry fallback: one entry per configured
// provider, naming the single model the config gives it.
//
// It exists so an install with an empty catalog still has a usable selector and
// a usable default model, which is exactly what it had before the catalog.
func (b *CatalogModelBuilder) registryCatalog() ModelCatalog {
	out := ModelCatalog{Models: []ModelChoice{}, Providers: []ProviderChoice{}}
	if b.reg == nil {
		return out
	}
	for _, entry := range b.reg.Catalog() {
		count := 1
		summary := ProviderChoice{
			ID:                entry.Name,
			Name:              entry.Name,
			Source:            string(store.SourceConfig),
			Enabled:           true,
			HasAPIKey:         entry.HasAPIKey,
			EnabledModelCount: 1,
			ChatModelCount:    1,
		}
		if strings.TrimSpace(entry.Model) == "" {
			// A config provider with no model cannot serve a chat at all: it is
			// listed so the console can show it, but it offers nothing.
			count = 0
			summary.EnabledModelCount, summary.ChatModelCount = 0, 0
		}
		summary.ModelCount = count
		// A registry list is the config itself, not a fetch: it cannot be
		// stale, and there is nothing to refetch it from.
		summary.Stale = false
		out.Providers = append(out.Providers, summary)

		if count == 0 {
			continue
		}
		out.Models = append(out.Models, ModelChoice{
			Provider:     entry.Name,
			ProviderName: entry.Name,
			Model:        entry.Model,
			DisplayName:  entry.Model,
			Capabilities: []string{string(store.CapChat)},
			ChatCapable:  true,
			Default:      entry.Default,
			HasAPIKey:    entry.HasAPIKey,
		})
	}
	return out
}

// markDefault marks the one model a new conversation starts on.
//
// The rule, in order:
//
//  1. llm.default_provider, when that provider is still on offer and has a
//     chat-capable model — preferring the model the config names for it. This
//     keeps an existing deployment behaving as it did.
//  2. the `chat` capability binding, which is the operator's explicit "this is
//     my conversation model" statement in 设置 → 能力绑定.
//  3. the first chat-capable model in catalog order (provider name, then model
//     name): deterministic, so an unconfigured install still has a default.
//
// When no model is chat-capable the catalog has no default at all — zero marks,
// never two, and never a model that cannot hold a conversation.
func (b *CatalogModelBuilder) markDefault(ctx context.Context, cat *ModelCatalog) {
	if len(cat.Models) == 0 {
		return
	}

	if want := b.configDefaultProvider(); want != "" {
		if idx := indexOfChatModel(cat.Models, want, ""); idx >= 0 {
			// The registry fallback already marks its own default; re-marking it
			// is idempotent, and when the store offers the provider by a
			// different model the config's choice is preferred while it is on
			// offer.
			if named := b.configModelName(want); named != "" {
				if i := indexOfChatModel(cat.Models, want, named); i >= 0 {
					idx = i
				}
			}
			cat.Models[idx].Default = true
			return
		}
		// The configured default provider is gone or disabled: fall through to
		// the next rule rather than marking something unusable.
	}

	if b.st != nil {
		bindings, err := b.st.ListBindings(ctx)
		if err != nil {
			b.logger.Warn("list capability bindings failed", zapError(err))
		} else {
			for _, binding := range bindings {
				if binding.Capability != store.CapChat {
					continue
				}
				if i := indexOfChatModel(cat.Models, binding.ProviderID, binding.ModelID); i >= 0 {
					cat.Models[i].Default = true
					return
				}
			}
		}
	}

	for i := range cat.Models {
		if cat.Models[i].ChatCapable {
			cat.Models[i].Default = true
			return
		}
	}
}

// defaultTarget returns the provider and model a session with no explicit
// choice runs on, or ("", "") when nothing is usable.
func (b *CatalogModelBuilder) defaultTarget(ctx context.Context) (string, string) {
	for _, m := range b.Catalog(ctx).Models {
		if m.Default {
			return m.Provider, m.Model
		}
	}
	return "", ""
}

// DefaultTarget is defaultTarget for callers outside the server (the admin
// command reports the resolved default in /api/meta).
func (b *CatalogModelBuilder) DefaultTarget(ctx context.Context) (string, string) {
	return b.defaultTarget(ctx)
}

// firstChatModel returns a provider's first enabled chat-capable model.
func (b *CatalogModelBuilder) firstChatModel(ctx context.Context, providerID string) (string, bool) {
	if b.st == nil {
		return "", false
	}
	rows, err := b.st.ListModels(ctx, providerID)
	if err != nil {
		b.logger.Warn("list provider models failed",
			zapString("provider", providerID), zapError(err))
		return "", false
	}
	var best string
	for _, m := range rows {
		if !m.Enabled || !m.Has(store.CapChat) {
			continue
		}
		if best == "" || m.ModelID < best {
			best = m.ModelID
		}
	}
	return best, best != ""
}

// configDefaultProvider is the provider named by llm.default_provider.
func (b *CatalogModelBuilder) configDefaultProvider() string {
	if b.reg == nil {
		return ""
	}
	return strings.TrimSpace(b.reg.DefaultName())
}

// configModelName is the model the config names for a provider.
func (b *CatalogModelBuilder) configModelName(provider string) string {
	if b.reg == nil {
		return ""
	}
	p, ok := b.reg.Provider(provider)
	if !ok {
		return ""
	}
	return strings.TrimSpace(p.Model)
}

// indexOfChatModel finds a chat-capable entry; an empty model matches the
// provider's first offered chat model.
func indexOfChatModel(models []ModelChoice, provider, model string) int {
	for i := range models {
		if models[i].Provider != provider || !models[i].ChatCapable {
			continue
		}
		if model == "" || models[i].Model == model {
			return i
		}
	}
	return -1
}

// capabilityStrings renders a capability set for the wire.
func capabilityStrings(caps store.Capabilities) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

// providerDisplayName is the human-readable provider name, never empty.
func providerDisplayName(p store.Provider) string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	return p.ID
}

// modelDisplayName is the human-readable model name, never empty.
func modelDisplayName(m store.Model) string {
	if name := strings.TrimSpace(m.DisplayName); name != "" {
		return name
	}
	return m.ModelID
}

// cached returns a previously constructed model, but only while the provider row
// it was built from is unchanged.
func (b *CatalogModelBuilder) cached(key string, p store.Provider) (model.BaseChatModel, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.cache[key]
	if !ok {
		return nil, false
	}
	if entry.baseURL != p.BaseURL || entry.kind != p.Kind || !entry.updatedAt.Equal(p.UpdatedAt) {
		// The provider was edited (or a refresh recorded an error on it): the
		// built client may point at the old base URL or key, so it is dropped.
		delete(b.cache, key)
		return nil, false
	}
	return entry.built, true
}

// remember stores a constructed model, evicting an arbitrary entry when the
// cache is full.
func (b *CatalogModelBuilder) remember(key string, built model.BaseChatModel, baseURL, kind string, updatedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cache == nil {
		b.cache = make(map[string]cachedModel, 8)
	}
	if _, exists := b.cache[key]; !exists && len(b.cache) >= maxModelCache {
		for k := range b.cache {
			delete(b.cache, k)
			break
		}
	}
	b.cache[key] = cachedModel{built: built, baseURL: baseURL, kind: kind, updatedAt: updatedAt}
}
