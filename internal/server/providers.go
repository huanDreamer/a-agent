package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

// providerFetchTimeout bounds one outbound /models call made from a handler.
// The UI's "test" button and the model refresh both go through it, so a provider
// that hangs cannot hold a request (and its session) open indefinitely.
const providerFetchTimeout = 10 * time.Second

// maxProviderKeyLen caps an accepted API key. Real keys are well under this; the
// limit only stops a stray paste of a whole file from being stored.
const maxProviderKeyLen = 4096

// providerIDPattern keeps an id usable as a slug: it is embedded in URLs, used
// as the store's primary key and shown in a list, so a space or a slash would
// break routing and a very long id would break the layout.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// registerProviderRoutes wires model-provider management: the providers a user
// adds, the models fetched from them, and which model serves each capability.
func (s *Server) registerProviderRoutes(authed *route.RouterGroup) {
	authed.GET("/llm/providers", s.handleListProviders)
	authed.POST("/llm/providers", s.handleUpsertProvider)
	authed.DELETE("/llm/providers/:id", s.handleDeleteProvider)
	authed.PUT("/llm/providers/:id/key", s.handleSetProviderKey)
	authed.POST("/llm/providers/:id/test", s.handleTestProvider)
	authed.POST("/llm/providers/:id/models/refresh", s.handleRefreshProviderModels)

	authed.GET("/llm/models", s.handleListModels)
	authed.PUT("/llm/models", s.handleUpsertModel)
	authed.DELETE("/llm/models", s.handleDeleteModel)

	authed.GET("/llm/bindings", s.handleListBindings)
	authed.PUT("/llm/bindings", s.handleSetBinding)
}

// ------------------------------------------------------------------ providers --

// handleListProviders returns every provider. store.Provider's key field is
// unexported, so serialising one cannot leak the secret; has_api_key and
// api_key_hint tell the UI what it needs to know instead.
//
// The capability vocabulary is returned alongside, so the UI can render the
// binding form without hardcoding a list that this build may not share.
func (s *Server) handleListProviders(ctx context.Context, c *app.RequestContext) {
	providers, err := s.store.ListProviders(ctx)
	if err != nil {
		s.fail(c, "list providers", err)
		return
	}
	if providers == nil {
		providers = []store.Provider{}
	}
	c.JSON(http.StatusOK, map[string]any{
		"providers":    providers,
		"capabilities": store.AllCapabilities,
	})
}

// providerRequest is the body of POST /api/llm/providers. The pointer fields
// distinguish "absent" from "set to empty": api_key_env, kind and enabled are
// only overwritten when the caller actually sends them.
type providerRequest struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BaseURL   string  `json:"base_url"`
	APIKey    string  `json:"api_key"`
	APIKeyEnv *string `json:"api_key_env"`
	Kind      *string `json:"kind"`
	Enabled   *bool   `json:"enabled"`
}

// handleUpsertProvider creates or updates a provider.
//
// A blank or absent api_key NEVER clears the stored one: the UI is never given
// the secret, so it cannot send it back, and treating the absence as "clear"
// would delete the key whenever a user changed the base URL. Clearing is a
// deliberate act with its own endpoint (PUT .../key with an empty value).
func (s *Server) handleUpsertProvider(ctx context.Context, c *app.RequestContext) {
	var body providerRequest
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	id := strings.TrimSpace(body.ID)
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}

	existing, err := s.store.GetProvider(ctx, id)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(c, "get provider", err)
		return
	}

	// A config-declared provider is re-synced from the config file at every
	// start, so only its enabled flag can be changed here.
	if found && existing.Source == store.SourceConfig {
		s.updateConfigProvider(ctx, c, existing, body)
		return
	}

	if !providerIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": "id must be a slug of at most 64 characters: lowercase letters, digits, dot, dash or underscore, starting with a letter or digit",
		})
		return
	}
	base := strings.TrimSpace(body.BaseURL)
	if base == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "base_url is required"})
		return
	}
	if err := validBaseURL(base); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if len(key) > maxProviderKeyLen {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("api_key is longer than %d characters; that looks like a paste accident", maxProviderKeyLen),
		})
		return
	}

	// An absent name falls back to the existing one, then to the id, so a
	// provider is always listed as something a human can read.
	name := strings.TrimSpace(body.Name)
	if name == "" && found {
		name = existing.Name
	}
	if name == "" {
		name = id
	}

	kind := "openai"
	enabled := true
	env := ""
	if found {
		kind, enabled, env = existing.Kind, existing.Enabled, existing.APIKeyEnv
	}
	if body.Kind != nil {
		if v := strings.TrimSpace(*body.Kind); v != "" {
			kind = v
		}
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if body.APIKeyEnv != nil {
		env = strings.TrimSpace(*body.APIKeyEnv)
	}

	if err := s.store.UpsertProvider(ctx, store.Provider{
		ID:        id,
		Name:      name,
		BaseURL:   base,
		Kind:      kind,
		Source:    store.SourceUser,
		Enabled:   enabled,
		APIKeyEnv: env,
	}); err != nil {
		s.fail(c, "upsert provider", err)
		return
	}
	if key != "" {
		if err := s.store.SetProviderKey(ctx, id, key); err != nil {
			s.fail(c, "set provider key", err)
			return
		}
	}

	saved, err := s.store.GetProvider(ctx, id)
	if err != nil {
		s.fail(c, "read back provider", err)
		return
	}
	s.logger.Info("provider saved", zapString("provider", id))
	c.JSON(http.StatusOK, map[string]any{"provider": saved})
}

// updateConfigProvider is the whole of what POST may do to a config-sourced
// provider: toggle enabled.
//
// name, base_url, api_key and api_key_env come from the config file, which is
// re-read and re-applied at every start. Accepting those edits would report a
// success that the next restart silently discards, so they are refused with a
// message naming the config file.
func (s *Server) updateConfigProvider(ctx context.Context, c *app.RequestContext, cur store.Provider, body providerRequest) {
	refuse := func(field string) {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf(
				"provider %q is declared in the config file (llm.providers) and is re-synced at every start, "+
					"so only \"enabled\" can be changed here; edit %s in the config file instead",
				cur.ID, field),
		})
	}
	if v := strings.TrimSpace(body.BaseURL); v != "" && v != cur.BaseURL {
		refuse("base_url")
		return
	}
	if v := strings.TrimSpace(body.Name); v != "" && v != cur.Name {
		refuse("name")
		return
	}
	if strings.TrimSpace(body.APIKey) != "" {
		refuse("api_key")
		return
	}
	if body.APIKeyEnv != nil && strings.TrimSpace(*body.APIKeyEnv) != cur.APIKeyEnv {
		refuse("api_key_env")
		return
	}

	if body.Enabled != nil && *body.Enabled != cur.Enabled {
		cur.Enabled = *body.Enabled
		if err := s.store.UpsertProvider(ctx, cur); err != nil {
			s.fail(c, "update config provider", err)
			return
		}
		saved, err := s.store.GetProvider(ctx, cur.ID)
		if err != nil {
			s.fail(c, "read back provider", err)
			return
		}
		cur = saved
	}
	c.JSON(http.StatusOK, map[string]any{"provider": cur})
}

// handleDeleteProvider removes a provider with its models and bindings.
func (s *Server) handleDeleteProvider(ctx context.Context, c *app.RequestContext) {
	p, ok := s.providerFromPath(ctx, c)
	if !ok {
		return
	}
	if p.Source == store.SourceConfig {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf(
				"provider %q comes from the config file (llm.providers); remove it there instead of deleting it here, "+
					"or the next start re-creates it", p.ID),
		})
		return
	}
	if err := s.store.DeleteProvider(ctx, p.ID); err != nil {
		s.fail(c, "delete provider", err)
		return
	}
	s.logger.Info("provider deleted", zapString("provider", p.ID))
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// handleSetProviderKey stores or clears a provider's API key. An empty api_key
// clears it, which is the only way to remove a secret the client was never
// shown.
func (s *Server) handleSetProviderKey(ctx context.Context, c *app.RequestContext) {
	p, ok := s.providerFromPath(ctx, c)
	if !ok {
		return
	}
	if p.Source == store.SourceConfig {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf(
				"provider %q takes its key from the config file (llm.providers), which is re-applied at every start; "+
					"edit the key there instead", p.ID),
		})
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if len(key) > maxProviderKeyLen {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("api_key is longer than %d characters; that looks like a paste accident", maxProviderKeyLen),
		})
		return
	}
	if err := s.store.SetProviderKey(ctx, p.ID, key); err != nil {
		s.fail(c, "set provider key", err)
		return
	}
	saved, err := s.store.GetProvider(ctx, p.ID)
	if err != nil {
		s.fail(c, "read back provider", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"provider": saved})
}

// handleTestProvider calls the provider's /models endpoint and records the
// outcome, so the UI can show why a provider is broken.
//
// A failed test is a normal result, not an API failure: the endpoint answers 200
// with ok=false and the reason, because "your key is wrong" is information the
// caller asked for rather than an error in the request.
func (s *Server) handleTestProvider(ctx context.Context, c *app.RequestContext) {
	p, ok := s.providerFromPath(ctx, c)
	if !ok {
		return
	}
	models, err := s.fetchProviderModels(ctx, p)
	out := map[string]any{
		"ok":           err == nil,
		"models_count": len(models),
	}
	if err != nil {
		msg := err.Error()
		out["error"] = msg
		s.recordProviderError(ctx, p.ID, msg)
		s.logger.Warn("provider model test failed", zapString("provider", p.ID), zapError(err))
	} else {
		s.recordProviderError(ctx, p.ID, "")
	}
	// Return the refreshed row so the UI can display last_error without a second
	// request.
	if cur, getErr := s.store.GetProvider(ctx, p.ID); getErr == nil {
		out["provider"] = cur
	}
	c.JSON(http.StatusOK, out)
}

// handleRefreshProviderModels fetches the provider's model list, infers each
// model's capabilities and stores the result.
//
// A failed fetch returns 502 and leaves the stored models untouched: empty is a
// worse answer than stale, because a stale list still lets a user bind a
// capability and a network blip must not wipe their configuration.
func (s *Server) handleRefreshProviderModels(ctx context.Context, c *app.RequestContext) {
	p, ok := s.providerFromPath(ctx, c)
	if !ok {
		return
	}
	models, err := s.fetchProviderModels(ctx, p)
	if err != nil {
		msg := err.Error()
		s.recordProviderError(ctx, p.ID, msg)
		s.logger.Warn("provider model refresh failed", zapString("provider", p.ID), zapError(err))
		c.JSON(http.StatusBadGateway, map[string]string{"error": msg})
		return
	}

	// A refresh learns ids, not names and not capabilities. The stored
	// capabilities are preserved by the store (it never overwrites them on a
	// fetch), and the display name is carried over here so a name a user typed
	// survives the refresh.
	existing, err := s.store.ListModels(ctx, p.ID)
	if err != nil {
		s.fail(c, "list provider models", err)
		return
	}
	prev := make(map[string]store.Model, len(existing))
	for _, m := range existing {
		prev[m.ModelID] = m
	}

	rows := make([]store.Model, 0, len(models))
	for _, info := range models {
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
		rows = append(rows, row)
	}
	if err := s.store.ReplaceFetchedModels(ctx, p.ID, rows); err != nil {
		s.fail(c, "replace fetched models", err)
		return
	}
	s.recordProviderError(ctx, p.ID, "")

	saved, err := s.store.ListModels(ctx, p.ID)
	if err != nil {
		s.fail(c, "list provider models", err)
		return
	}
	if saved == nil {
		saved = []store.Model{}
	}
	s.logger.Info("provider models refreshed",
		zapString("provider", p.ID), zap.Int("models", len(saved)))
	c.JSON(http.StatusOK, map[string]any{
		"models": saved,
		"count":  len(saved),
	})
}

// fetchProviderModels resolves the provider's key and calls its /models
// endpoint under a bounded context.
//
// The key is resolved here and passed straight into the request header: it is
// never logged and never returned to the client.
func (s *Server) fetchProviderModels(ctx context.Context, p store.Provider) ([]llm.ModelInfo, error) {
	key := p.ResolveAPIKey(os.Getenv)
	fetchCtx, cancel := context.WithTimeout(ctx, providerFetchTimeout)
	defer cancel()
	return llm.FetchModels(fetchCtx, p.BaseURL, key, providerFetchTimeout)
}

// recordProviderError stores the outcome of the last connection attempt.
// Failing to record it is logged, not returned: it must not turn a usable
// response into an error.
func (s *Server) recordProviderError(ctx context.Context, id, msg string) {
	if err := s.store.SetProviderError(ctx, id, msg); err != nil {
		s.logger.Warn("record provider error failed", zapString("provider", id), zapError(err))
	}
}

// providerFromPath loads the provider named by the :id path parameter. It
// writes the error response itself and reports whether the caller may continue.
func (s *Server) providerFromPath(ctx context.Context, c *app.RequestContext) (store.Provider, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "provider id is required"})
		return store.Provider{}, false
	}
	p, err := s.store.GetProvider(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, map[string]string{"error": "provider not found"})
		return store.Provider{}, false
	}
	if err != nil {
		s.fail(c, "get provider", err)
		return store.Provider{}, false
	}
	return p, true
}

// validBaseURL requires an absolute http(s) URL. Anything else cannot be
// fetched from, and catching it here turns a failed test into a validation
// message.
func validBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url does not parse as a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("base_url must be an absolute URL starting with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("base_url must include a host")
	}
	return nil
}

// -------------------------------------------------------------------- models --

// handleListModels returns the stored models with their capabilities. An empty
// ?provider= lists every provider's models.
func (s *Server) handleListModels(ctx context.Context, c *app.RequestContext) {
	models, err := s.store.ListModels(ctx, strings.TrimSpace(c.Query("provider")))
	if err != nil {
		s.fail(c, "list models", err)
		return
	}
	if models == nil {
		models = []store.Model{}
	}
	c.JSON(http.StatusOK, map[string]any{"models": models})
}

// handleUpsertModel records what a model can do.
//
// This is the point of the whole screen: /models reports ids and nothing else,
// so the inferred set is only a starting guess and this endpoint is how the user
// makes it true.
func (s *Server) handleUpsertModel(ctx context.Context, c *app.RequestContext) {
	var body struct {
		ProviderID   string   `json:"provider_id"`
		ModelID      string   `json:"model_id"`
		DisplayName  *string  `json:"display_name"`
		Capabilities []string `json:"capabilities"`
		Enabled      *bool    `json:"enabled"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	providerID := strings.TrimSpace(body.ProviderID)
	modelID := strings.TrimSpace(body.ModelID)
	if providerID == "" || modelID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "provider_id and model_id are required"})
		return
	}
	if _, err := s.store.GetProvider(ctx, providerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "provider not found"})
			return
		}
		s.fail(c, "get provider", err)
		return
	}
	caps, err := capabilitySet(body.Capabilities)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Keep the existing row's bookkeeping: a hand-edited model that was fetched
	// from the provider stays "fetched", so the next refresh still recognises it.
	display := modelID
	source := "user"
	enabled := true
	prev, err := s.store.GetModel(ctx, providerID, modelID)
	switch {
	case err == nil:
		source = prev.Source
		enabled = prev.Enabled
		if prev.DisplayName != "" {
			display = prev.DisplayName
		}
	case errors.Is(err, store.ErrNotFound):
		// A model the provider's list omitted or does not report at all.
	default:
		s.fail(c, "get model", err)
		return
	}
	if body.DisplayName != nil {
		display = strings.TrimSpace(*body.DisplayName)
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	if err := s.store.UpsertModel(ctx, store.Model{
		ProviderID:   providerID,
		ModelID:      modelID,
		DisplayName:  display,
		Capabilities: caps,
		Enabled:      enabled,
		Source:       source,
	}); err != nil {
		s.fail(c, "upsert model", err)
		return
	}
	saved, err := s.store.GetModel(ctx, providerID, modelID)
	if err != nil {
		s.fail(c, "read back model", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"model": saved})
}

// handleDeleteModel removes one model and any binding that pointed at it.
func (s *Server) handleDeleteModel(ctx context.Context, c *app.RequestContext) {
	var body struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	providerID := strings.TrimSpace(body.ProviderID)
	modelID := strings.TrimSpace(body.ModelID)
	if providerID == "" || modelID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "provider_id and model_id are required"})
		return
	}
	if _, err := s.store.GetModel(ctx, providerID, modelID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "model not found"})
			return
		}
		s.fail(c, "get model", err)
		return
	}
	if err := s.store.DeleteModel(ctx, providerID, modelID); err != nil {
		s.fail(c, "delete model", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// capabilitySet validates a client-supplied capability list.
func capabilitySet(raw []string) (store.Capabilities, error) {
	var out store.Capabilities
	for _, name := range raw {
		if strings.TrimSpace(name) == "" {
			continue
		}
		c := store.Capability(strings.TrimSpace(name))
		if !store.ValidCapability(c) {
			return nil, fmt.Errorf("unknown capability %q; valid values are %s", c, capabilityNames())
		}
		if !out.Has(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one capability is required: a model nothing can be bound to is not selectable")
	}
	return out, nil
}

// capabilityNames renders the capability vocabulary for an error message.
func capabilityNames() string {
	names := make([]string, 0, len(store.AllCapabilities))
	for _, c := range store.AllCapabilities {
		names = append(names, string(c))
	}
	return strings.Join(names, ", ")
}

// ------------------------------------------------------------------ bindings --

// bindingView is a binding joined with the names the UI displays, so rendering
// the table needs no second request per row.
type bindingView struct {
	store.Binding
	ProviderName string `json:"provider_name"`
	ModelName    string `json:"model_name"`
}

// handleListBindings returns every capability binding, decorated with the
// provider and model names.
func (s *Server) handleListBindings(ctx context.Context, c *app.RequestContext) {
	views, err := s.bindingViews(ctx)
	if err != nil {
		s.fail(c, "list bindings", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"bindings": views})
}

// handleSetBinding points a capability at a model. A blank model clears the
// binding, which is how a capability is switched off without deleting anything.
func (s *Server) handleSetBinding(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Capability string `json:"capability"`
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	capability := store.Capability(strings.TrimSpace(body.Capability))
	if capability == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "capability is required"})
		return
	}
	if !store.ValidCapability(capability) {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown capability %q; valid values are %s", capability, capabilityNames()),
		})
		return
	}

	providerID := strings.TrimSpace(body.ProviderID)
	modelID := strings.TrimSpace(body.ModelID)
	if modelID != "" {
		// A binding to a model that is not stored would render as a broken row
		// and could not be resolved at run time.
		if providerID == "" {
			c.JSON(http.StatusBadRequest, map[string]string{"error": "provider_id is required when model_id is set"})
			return
		}
		if _, err := s.store.GetModel(ctx, providerID, modelID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				c.JSON(http.StatusBadRequest, map[string]string{"error": "unknown model: fetch or add it for this provider first"})
				return
			}
			s.fail(c, "get model", err)
			return
		}
	}

	if err := s.store.SetBinding(ctx, store.Binding{
		Capability: capability,
		ProviderID: providerID,
		ModelID:    modelID,
	}); err != nil {
		s.fail(c, "set binding", err)
		return
	}
	// Answer with the whole list: the caller's next question is always what the
	// bindings look like now.
	views, err := s.bindingViews(ctx)
	if err != nil {
		s.fail(c, "list bindings", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"bindings": views})
}

// bindingViews joins each binding with its provider and model names. A binding
// left behind by a deleted provider keeps its ids and shows a blank name rather
// than disappearing, so a broken row is visible instead of silent.
func (s *Server) bindingViews(ctx context.Context) ([]bindingView, error) {
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]bindingView, 0, len(bindings))
	for _, b := range bindings {
		v := bindingView{Binding: b, ProviderName: b.ProviderID, ModelName: b.ModelID}
		if p, err := s.store.GetProvider(ctx, b.ProviderID); err == nil {
			v.ProviderName = p.Name
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if m, err := s.store.GetModel(ctx, b.ProviderID, b.ModelID); err == nil {
			if strings.TrimSpace(m.DisplayName) != "" {
				v.ModelName = m.DisplayName
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
