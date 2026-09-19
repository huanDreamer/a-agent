package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

// Asking a model about itself, on demand.
//
// The automatic pass (a refresh, or startup) covers the catalog a piece at a
// time: it fills what is missing, asks twelve models per pass, and never asks
// again once a model has answered. That is the right shape for a background
// job — it costs money and nobody is waiting — and the wrong shape for an
// operator who has just added a provider, or who suspects a window is wrong.
//
// This is the interactive counterpart: 设置 → 模型 can ask one model, or one
// provider's worth, and get the answer in the response.

// probeTimeout bounds one interactive probe. It is longer than a batch probe
// (windowAskTimeout) because somebody is watching a spinner and a reasoning
// model that spends 15 seconds thinking is not a failure.
const probeTimeout = 40 * time.Second

// probeRequest is the body of POST /api/llm/models/probe.
//
// One of the two shapes: model_id asks about exactly that model, and no model_id
// asks about up to probeBatchLimit of the provider's models that have something
// open (or all of them with force).
type probeRequest struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	// Force re-asks models that have already answered, which is what "this number
	// looks wrong" needs. Without it only open questions are asked, so a click on
	// a settled catalog costs nothing.
	Force bool `json:"force"`
}

// probeBatchLimit caps how many models one interactive click asks. The batch pass
// uses windowAskBudget (12); this is smaller because the caller is waiting on an
// HTTP response and each probe can take tens of seconds.
const probeBatchLimit = 4

// handleProbeModelFacts asks a model — or a provider's models — about themselves
// and records what they say.
//
// It is synchronous and therefore bounded: one model, or at most
// probeBatchLimit of them, each under probeTimeout. A provider with more open
// models than that reports how many are left, and the operator can click again —
// which is honest about what one click did, rather than starting a background
// job the UI cannot show.
func (s *Server) handleProbeModelFacts(ctx context.Context, c *app.RequestContext) {
	var body probeRequest
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	providerID := strings.TrimSpace(body.ProviderID)
	if providerID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "provider_id is required"})
		return
	}
	asker := s.factsAsker()
	if asker == nil {
		c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "这个部署没有可用的模型客户端，无法询问模型（检查 provider 与 API Key）",
		})
		return
	}

	// Which models to ask: one named model, or the provider's enabled ones.
	models, err := s.store.ListModels(ctx, providerID)
	if err != nil {
		s.fail(c, "list models", err)
		return
	}
	targets := make([]store.Model, 0, probeBatchLimit)
	if id := strings.TrimSpace(body.ModelID); id != "" {
		for _, m := range models {
			if m.ModelID == id {
				targets = append(targets, m)
			}
		}
		if len(targets) == 0 {
			c.JSON(http.StatusNotFound, map[string]string{"error": "model not found"})
			return
		}
	} else {
		for _, m := range models {
			if !m.Enabled || !canAnswerAProbe(m) {
				continue
			}
			if !body.Force && m.ContextWindow > 0 && m.CapabilitiesCheckedAt != nil {
				continue
			}
			targets = append(targets, m)
			if len(targets) == probeBatchLimit {
				break
			}
		}
	}
	if len(targets) == 0 {
		c.JSON(http.StatusOK, map[string]any{
			"asked":     0,
			"remaining": 0,
			"models":    []store.Model{},
			"note":      "这个 provider 的模型都已经有窗口和能力记录了；要重新问一遍，勾选「强制重新询问」",
		})
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout*time.Duration(len(targets)))
	defer cancel()

	asked, answered := 0, 0
	for _, m := range targets {
		asked++
		changed, perr := s.probeOneModel(probeCtx, asker, m)
		if perr != nil {
			s.logger.Warn("asking a model about itself failed",
				zapString("provider", m.ProviderID), zapString("model", m.ModelID), zapError(perr))
			continue
		}
		if changed {
			answered++
		}
	}

	// A probe can change a window, and a window is read when a Runner is built —
	// so the cache has to go, or the change waits for a restart. See the same
	// reset in handleUpsertModel.
	if answered > 0 {
		s.runnerCache.reset()
	}

	// Read back so the caller can render the result without a second request.
	after, err := s.store.ListModels(ctx, providerID)
	if err != nil {
		s.fail(c, "read back models", err)
		return
	}
	open := 0
	for _, m := range after {
		if m.Enabled && canAnswerAProbe(m) && (m.ContextWindow <= 0 || m.CapabilitiesCheckedAt == nil) {
			open++
		}
	}
	s.logger.Info("model facts probed on demand",
		zapString("provider", providerID), zap.Int("asked", asked),
		zap.Int("recorded", answered), zap.Int("still_open", open))
	c.JSON(http.StatusOK, map[string]any{
		"asked":     asked,
		"recorded":  answered,
		"remaining": open,
		"models":    after,
	})
}

// probeOneModel asks one model about itself and records the answers.
//
// It reports whether the stored row actually changed, which is not the same as
// "the model answered": the store refuses writes that would take a value an
// operator or the provider set (see SetModelContextWindow and
// SetModelCapabilitiesFromProbe), and the answer here drives what the console
// says. Hence the read-back — the first version reported "recorded: 1" for a
// probe that changed nothing, because the guard it had just been refused by was
// invisible to it.
func (s *Server) probeOneModel(ctx context.Context, asker FactsAsker, m store.Model) (bool, error) {
	facts, err := asker.AskModelFacts(ctx, m.ProviderID, m.ModelID)
	if err != nil {
		return false, err
	}
	// The single-number fallback, for a model that hedged on the combined
	// question: the two prompts measurably get different answers out of the same
	// model.
	if facts.ContextWindow <= 0 && m.ContextWindow <= 0 {
		if tokens, werr := asker.AskContextWindow(ctx, m.ProviderID, m.ModelID); werr == nil && tokens > 0 {
			facts.ContextWindow = tokens
		}
	}

	if facts.ContextWindow > 0 {
		if serr := s.store.SetModelContextWindow(ctx, m.ProviderID, m.ModelID,
			facts.ContextWindow, store.ModelWindowSourceAsked); serr != nil {
			return false, serr
		}
	}
	if len(facts.Capabilities) > 0 {
		if serr := s.store.SetModelCapabilitiesFromProbe(ctx, m.ProviderID, m.ModelID,
			facts.Capabilities, store.ModelCapabilitySourceAsked); serr != nil {
			return false, serr
		}
	}

	after, gerr := s.store.GetModel(ctx, m.ProviderID, m.ModelID)
	if gerr != nil {
		return false, gerr
	}
	if after.ContextWindow != m.ContextWindow || after.ContextWindowSource != m.ContextWindowSource {
		return true, nil
	}
	if !sameCapabilities(after.Capabilities, m.Capabilities) || after.CapabilitiesSource != m.CapabilitiesSource {
		return true, nil
	}
	return false, nil
}

// sameCapabilities compares two sets as sets, whatever order they are in.
func sameCapabilities(a, b store.Capabilities) bool {
	if len(a) != len(b) {
		return false
	}
	for _, c := range a {
		if !b.Contains(c) {
			return false
		}
	}
	return true
}
