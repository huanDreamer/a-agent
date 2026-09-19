package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/hertz/pkg/app"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

const (
	// refreshProviderConcurrency is how many providers are contacted at once.
	// A handful: enough that a dozen providers do not take a dozen timeouts in
	// series, few enough not to hammer anything or open a burst of sockets.
	refreshProviderConcurrency = 3
	// refreshPassTimeout bounds one whole pass (startup or manual), so a pass
	// over many slow providers always terminates. Individual fetches are bounded
	// separately by providerFetchTimeout.
	refreshPassTimeout = 90 * time.Second
	// windowAskTimeout bounds one "what is your context window" probe.
	windowAskTimeout = llm.DefaultWindowAskTimeout
	// windowAskConcurrency is how many models are asked at once. Two: the probe
	// is a real chat completion on a real (paid) endpoint, and a burst of them
	// is a burst on the provider's rate limit for the sake of a number that
	// nobody is waiting on.
	windowAskConcurrency = 2
	// windowAskBudget caps how many models one pass asks. A catalog can hold
	// hundreds; the window of every one of them is not worth hundreds of model
	// calls, and a pass that never finishes is worse than a column that fills in
	// over a couple of refreshes.
	windowAskBudget = 12
)

// modelSourceFetched mirrors the source value store.ReplaceFetchedModels writes.
// Only a fetched row carries a meaningful fetched_at: a hand-added model, or the
// one llm.SyncConfigProviders records from the config, says nothing about when
// the provider's list was last read.
const modelSourceFetched = "fetched"

// ModelRefreshResult is one provider's outcome. It is the response element of
// POST /api/llm/models/refresh-all and the per-provider record the startup pass
// logs.
//
// A failed refresh is data, not an error: the endpoint answers 200 with ok=false
// and the reason, because "this provider's key is wrong" is what the caller
// asked for. One provider failing never stops the others.
type ModelRefreshResult struct {
	ProviderID  string `json:"provider_id"`
	OK          bool   `json:"ok"`
	ModelsCount int    `json:"models_count"`
	Error       string `json:"error"`
}

// ------------------------------------------------------------------ staleness --

// newestFetchedAt returns when the provider's model list was last read from
// {base_url}/models, and whether it ever was.
func newestFetchedAt(models []store.Model) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, m := range models {
		if m.Source != modelSourceFetched || m.FetchedAt == nil {
			continue
		}
		if !found || m.FetchedAt.After(newest) {
			newest = *m.FetchedAt
			found = true
		}
	}
	return newest, found
}

// providerModelsStale reports whether a provider's model list is worth
// refetching: it was never fetched, or the newest fetch is older than ttl.
//
// A provider whose list is empty is stale by definition — that is the state a
// freshly added provider is in, and the state a pass must fix without a click.
//
// A ttl of zero means "never stale": nothing is refetched automatically.
//
// Note the granularity this has to work with: fetched_at lives on the model
// rows, not on the provider, so a provider that lists no model at all always
// looks stale. That costs one request per start for such a provider, which is
// the intended behaviour anyway (its list is empty either way).
func providerModelsStale(models []store.Model, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	newest, found := newestFetchedAt(models)
	if !found {
		return true
	}
	return time.Since(newest) > ttl
}

// ------------------------------------------------------------------ fetching --

// refreshProviderModels fetches a provider's model list, infers each model's
// capabilities and stores the result, returning the provider's stored models.
//
// It is the one refresh path: the per-provider route, 刷新全部 and the startup
// pass all go through it, so they cannot diverge in what they store or in how
// they report failure.
//
// A fetch that fails returns an error and leaves the stored models untouched:
// empty is a worse answer than stale, because a stale list still lets the
// operator bind a capability while a network blip must not wipe their
// configuration.
func refreshProviderModels(ctx context.Context, st store.Store, logger *zap.Logger, p store.Provider, asker WindowAsker) ([]store.Model, error) {
	if st == nil {
		return nil, fmt.Errorf("server: refresh %q: no store configured", p.ID)
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	key := p.ResolveAPIKey(os.Getenv)
	fetchCtx, cancel := context.WithTimeout(ctx, providerFetchTimeout)
	defer cancel()
	fetched, err := llm.FetchModels(fetchCtx, p.BaseURL, key, providerFetchTimeout)
	if err != nil {
		return nil, err
	}

	// A refresh learns ids, not names and not capabilities. What the operator
	// typed and confirmed is carried over: the store preserves the stored
	// capabilities on a fetch, and the display name and enabled flag are read
	// here so a renamed or switched-off model survives the refresh.
	existing, err := st.ListModels(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("server: list models of %q: %w", p.ID, err)
	}
	prev := make(map[string]store.Model, len(existing))
	for _, m := range existing {
		prev[m.ModelID] = m
	}

	rows := make([]store.Model, 0, len(fetched))
	for _, info := range fetched {
		row := store.Model{
			ProviderID:   p.ID,
			ModelID:      info.ID,
			DisplayName:  info.ID,
			Capabilities: llm.InferCapabilities(info.ID),
			Enabled:      true,
		}
		if old, ok := prev[info.ID]; ok {
			row.DisplayName = old.DisplayName
			row.Enabled = old.Enabled
		}
		// A window the provider publishes is the number it will enforce, so it
		// is recorded here and never overwritten by a guess: the store keeps
		// whatever a listing does not mention.
		if info.ContextWindow > 0 {
			row.ContextWindow = info.ContextWindow
			row.ContextWindowSource = store.ModelWindowSourceAPI
		}
		rows = append(rows, row)
	}
	if err := st.ReplaceFetchedModels(ctx, p.ID, rows); err != nil {
		return nil, fmt.Errorf("server: store models of %q: %w", p.ID, err)
	}

	saved, err := st.ListModels(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("server: read back models of %q: %w", p.ID, err)
	}
	logger.Info("provider models refreshed",
		zapString("provider", p.ID), zap.Int("models", len(saved)))

	// The listing answered as far as it could; the rest is asked directly. A
	// provider that publishes context_length (any of the big gateways) needs no
	// probe at all, which is the common case and the reason this is a fallback.
	if asker != nil {
		fillModelWindows(ctx, st, logger, asker, saved, windowAskBudget)
	}

	// Read back again when something was learned, so the caller (and the
	// console) sees the windows without a second round trip.
	if asker != nil {
		if after, aerr := st.ListModels(ctx, p.ID); aerr == nil {
			return after, nil
		}
	}
	return saved, nil
}

// WindowAsker answers what a model's context window is by asking it. It is an
// interface rather than a *llm call so this package does not need a provider
// client: the server implements it on top of the model builder, and a caller
// with no builder passes nil and simply gets no probing.
type WindowAsker interface {
	AskContextWindow(ctx context.Context, provider, name string) (int, error)
}

// fillModelWindows asks the enabled models that have no recorded window.
//
// It is bounded three ways — how many models one pass asks, how many are in
// flight, and how long one answer may take — because it spends real money on a
// real endpoint for a number that is a hint. A model that does not answer (or
// answers something implausible) is simply left without one: the built-in table
// covers it, and the next refresh will ask again.
func fillModelWindows(ctx context.Context, st store.Store, logger *zap.Logger, asker WindowAsker, models []store.Model, budget int) (asked, filled int) {
	if asker == nil || st == nil {
		return 0, 0
	}
	if budget <= 0 {
		budget = windowAskBudget
	}
	pending := make([]store.Model, 0, budget)
	for _, m := range models {
		if !m.Enabled || m.ContextWindow > 0 {
			continue
		}
		pending = append(pending, m)
		if len(pending) == budget {
			break
		}
	}
	if len(pending) == 0 {
		return 0, 0
	}

	sem := make(chan struct{}, windowAskConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, m := range pending {
		sem <- struct{}{}
		wg.Add(1)
		go func(m store.Model) {
			defer wg.Done()
			defer func() { <-sem }()

			tokens, err := asker.AskContextWindow(ctx, m.ProviderID, m.ModelID)
			mu.Lock()
			asked++
			mu.Unlock()
			if err != nil {
				logger.Debug("asking a model for its context window failed",
					zapString("provider", m.ProviderID), zapString("model", m.ModelID), zapError(err))
				return
			}
			if tokens <= 0 {
				// The model said it does not know. Recorded as nothing rather
				// than as a wrong number.
				logger.Debug("a model did not know its context window",
					zapString("provider", m.ProviderID), zapString("model", m.ModelID))
				return
			}
			if serr := st.SetModelContextWindow(ctx, m.ProviderID, m.ModelID, tokens, store.ModelWindowSourceAsked); serr != nil {
				logger.Warn("recording a model's context window failed",
					zapString("provider", m.ProviderID), zapString("model", m.ModelID), zapError(serr))
				return
			}
			mu.Lock()
			filled++
			mu.Unlock()
			logger.Info("model context window recorded",
				zapString("provider", m.ProviderID), zapString("model", m.ModelID),
				zap.Int("context_window", tokens), zap.String("source", store.ModelWindowSourceAsked))
		}(m)
	}
	wg.Wait()
	return asked, filled
}

// setProviderError records the outcome of the last connection attempt. Failing
// to record it is logged, never returned: it must not turn a usable response
// into an error.
func setProviderError(ctx context.Context, st store.Store, logger *zap.Logger, id, msg string) {
	if st == nil {
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := st.SetProviderError(ctx, id, msg); err != nil {
		logger.Warn("record provider error failed", zapString("provider", id), zapError(err))
	}
}

// refreshProviderSet refreshes the given providers concurrently and returns one
// result per provider, in the order they were given.
//
// Every provider is isolated: one failure records its reason and continues, so a
// broken key never stops the rest of a pass.
func refreshProviderSet(ctx context.Context, st store.Store, logger *zap.Logger, targets []store.Provider, asker WindowAsker) []ModelRefreshResult {
	if logger == nil {
		logger = zap.NewNop()
	}
	results := make([]ModelRefreshResult, len(targets))
	if len(targets) == 0 {
		return results
	}

	sem := make(chan struct{}, refreshProviderConcurrency)
	var wg sync.WaitGroup
	for i, p := range targets {
		// Acquiring before spawning bounds the number of live goroutines, so a
		// hundred providers do not mean a hundred open connections.
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, p store.Provider) {
			defer wg.Done()
			defer func() { <-sem }()

			models, err := refreshProviderModels(ctx, st, logger, p, asker)
			res := ModelRefreshResult{ProviderID: p.ID, OK: err == nil, ModelsCount: len(models)}
			if err != nil {
				res.Error = err.Error()
				setProviderError(ctx, st, logger, p.ID, res.Error)
				logger.Warn("provider model refresh failed",
					zapString("provider", p.ID), zapError(err))
			} else {
				setProviderError(ctx, st, logger, p.ID, "")
			}
			results[i] = res
		}(i, p)
	}
	wg.Wait()
	return results
}

// ------------------------------------------------------------------- targets --

// refreshTargets lists the providers a refresh may contact: enabled, and with a
// key that could authenticate. Order is deterministic (by display name, then
// id), so a pass and its log read the same way twice.
//
// A provider with no key is skipped: the request would only come back 401 and
// overwrite last_error with a message the operator already knows from the
// "未配置密钥" marker.
func refreshTargets(ctx context.Context, st store.Store) ([]store.Provider, error) {
	if st == nil {
		return nil, nil
	}
	providers, err := st.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: list providers: %w", err)
	}
	out := make([]store.Provider, 0, len(providers))
	for _, p := range providers {
		if p.Enabled && p.HasAPIKey {
			out = append(out, p)
		}
	}
	sortProviders(out)
	return out, nil
}

// staleRefreshTargets narrows refreshTargets to the providers whose cached model
// list is stale.
func staleRefreshTargets(ctx context.Context, st store.Store, ttl time.Duration) ([]store.Provider, error) {
	if ttl <= 0 {
		return nil, nil
	}
	candidates, err := refreshTargets(ctx, st)
	if err != nil {
		return nil, err
	}
	out := make([]store.Provider, 0, len(candidates))
	for _, p := range candidates {
		models, err := st.ListModels(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("server: list models of %q: %w", p.ID, err)
		}
		if providerModelsStale(models, ttl) {
			out = append(out, p)
		}
	}
	return out, nil
}

// sortProviders orders providers by display name, then id.
func sortProviders(providers []store.Provider) {
	sort.SliceStable(providers, func(i, j int) bool {
		left, right := providerDisplayName(providers[i]), providerDisplayName(providers[j])
		if left != right {
			return left < right
		}
		return providers[i].ID < providers[j].ID
	})
}

// ----------------------------------------------------------------- the passes --

// RefreshStaleModels refreshes every enabled provider with a key whose cached
// model list is stale, and then fills whatever context windows are still
// missing.
//
// It is meant to be called from a goroutine at start-up (the caller must not
// wait on it): a slow or unreachable provider can delay nothing but its own
// result. The whole pass is bounded by refreshPassTimeout, and any failure is
// recorded on the provider and logged at warn without stopping the pass.
func (s *Server) RefreshStaleModels(ctx context.Context, ttl time.Duration) []ModelRefreshResult {
	logger := s.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	passCtx, cancel := context.WithTimeout(ctx, refreshPassTimeout)
	defer cancel()

	results := refreshStaleModels(passCtx, s.store, logger, ttl, s.windowAsker())

	// The window is a separate question from the model list: a provider whose
	// list is fresh can still have no windows recorded — a deployment that just
	// upgraded to this version, or a provider that publishes none. This is the
	// "初始化时问一遍" half, and it is skipped entirely when compression is off,
	// because then nothing reads the number.
	s.fillMissingWindows(passCtx, logger)
	return results
}

// refreshStaleModels is the pass itself, without a Server: the parts that need
// one are the asker (which needs a model builder) and the compression check, and
// both are optional.
func refreshStaleModels(ctx context.Context, st store.Store, logger *zap.Logger, ttl time.Duration, asker WindowAsker) []ModelRefreshResult {
	if st == nil || ttl <= 0 {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	targets, err := staleRefreshTargets(ctx, st, ttl)
	if err != nil {
		logger.Warn("model auto-refresh: cannot list stale providers", zapError(err))
		return nil
	}
	if len(targets) == 0 {
		logger.Debug("model auto-refresh: every provider's model list is fresh")
		return nil
	}
	logger.Info("model auto-refresh: refreshing stale providers", zap.Int("providers", len(targets)))
	return refreshProviderSet(ctx, st, logger, targets, asker)
}

// fillMissingWindows asks the models that have no recorded context window, one
// provider at a time, under a single pass-wide budget.
//
// The budget is shared rather than per provider so that adding providers does
// not multiply the number of paid calls a startup makes.
func (s *Server) fillMissingWindows(ctx context.Context, logger *zap.Logger) {
	if s.store == nil {
		return
	}
	asker := s.windowAsker()
	if asker == nil {
		return
	}
	// Compression off means nothing reads a window: no reason to spend a call.
	if s.cfg.ContextMaxTokens <= 0 {
		return
	}
	providers, err := refreshTargets(ctx, s.store)
	if err != nil {
		logger.Warn("model window fill: cannot list providers", zapError(err))
		return
	}
	remaining := windowAskBudget
	totalAsked, totalFilled := 0, 0
	for _, p := range providers {
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		models, lerr := s.store.ListModels(ctx, p.ID)
		if lerr != nil {
			logger.Warn("model window fill: cannot list models", zapString("provider", p.ID), zapError(lerr))
			continue
		}
		asked, filled := fillModelWindows(ctx, s.store, logger, asker, models, remaining)
		remaining -= asked
		totalAsked += asked
		totalFilled += filled
	}
	if totalAsked > 0 {
		logger.Info("model windows filled",
			zap.Int("asked", totalAsked), zap.Int("recorded", totalFilled))
	}
}

// windowAsker adapts the console's model builder into a WindowAsker.
//
// Nil when chat is disabled or no builder is wired: probing needs to construct a
// real client for a real provider, and a deployment without one simply keeps the
// built-in table.
func (s *Server) windowAsker() WindowAsker {
	if s.chat.Builder == nil {
		return nil
	}
	return builderWindowAsker{builder: s.chat.Builder}
}

// builderWindowAsker asks a model, built on demand, what its window is.
type builderWindowAsker struct{ builder ModelBuilder }

func (a builderWindowAsker) AskContextWindow(ctx context.Context, provider, name string) (int, error) {
	built, err := a.builder.Build(ctx, provider, name)
	if err != nil {
		return 0, err
	}
	cm, ok := built.(model.BaseChatModel)
	if !ok {
		return 0, fmt.Errorf("server: %q is not a chat model", name)
	}
	return llm.AskContextWindow(ctx, cm, windowAskTimeout)
}

// handleRefreshAllModels refreshes every enabled provider that has a key.
//
// It is the manual counterpart to the startup pass and what 设置's 刷新全部
// button calls. The answer is 200 even when every provider failed: the body is
// the per-provider report the UI renders, and an HTTP error would only hide it.
func (s *Server) handleRefreshAllModels(ctx context.Context, c *app.RequestContext) {
	passCtx, cancel := context.WithTimeout(ctx, refreshPassTimeout)
	defer cancel()

	targets, err := refreshTargets(passCtx, s.store)
	if err != nil {
		s.fail(c, "list providers to refresh", err)
		return
	}
	results := refreshProviderSet(passCtx, s.store, s.logger, targets, s.windowAsker())
	if results == nil {
		results = []ModelRefreshResult{}
	}
	c.JSON(http.StatusOK, map[string]any{"results": results})
}
