package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/huan/huan-agent/internal/metrics"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/store"
)

// DefaultLimit is the page size used when a request does not specify one.
const DefaultLimit = 50

// maxLimit bounds a client-supplied page size.
const maxLimit = 500

// handleLogin exchanges the admin password for a session cookie.
func (s *Server) handleLogin(_ context.Context, c *app.RequestContext) {
	var body struct {
		Password string `json:"password"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	token, err := s.auth.login(body.Password)
	if err != nil {
		s.logger.Warn("admin login failed", zapError(err), zapRemote(c))
		c.JSON(http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	setSessionCookie(c, token, s.auth.ttl)
	s.logger.Info("admin login ok", zapRemote(c))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "username": s.auth.username})
}

// handleLogout revokes the session and expires the cookie.
func (s *Server) handleLogout(_ context.Context, c *app.RequestContext) {
	s.auth.logout(string(c.Cookie(SessionCookieName)))
	clearSessionCookie(c)
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// handleMe reports whether the caller holds a valid session.
func (s *Server) handleMe(_ context.Context, c *app.RequestContext) {
	if !s.auth.valid(string(c.Cookie(SessionCookieName))) {
		c.JSON(http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      s.auth.username,
	})
}

// handleHealth is an unauthenticated liveness probe.
func (s *Server) handleHealth(_ context.Context, c *app.RequestContext) {
	c.JSON(http.StatusOK, map[string]any{
		"ok":       true,
		"version":  s.cfg.Version,
		"uptime_s": int64(time.Since(s.startT).Seconds()),
	})
}

// handleMeta describes the running configuration for the UI header.
func (s *Server) handleMeta(_ context.Context, c *app.RequestContext) {
	c.JSON(http.StatusOK, map[string]any{
		"provider":        s.cfg.Provider,
		"model":           s.cfg.Model,
		"version":         s.cfg.Version,
		"metrics_enabled": s.cfg.MetricsEnable,
		"feishu_enabled":  s.cfg.FeishuEnabled,
	})
}

// handleMetrics serves the Prometheus exposition.
func (s *Server) handleMetrics(_ context.Context, c *app.RequestContext) {
	if s.cfg.Metrics == nil {
		c.String(http.StatusOK, "# metrics disabled\n")
		return
	}
	var buf strings.Builder
	if err := metrics.WriteText(s.cfg.Metrics.Gatherer(), &buf); err != nil {
		s.logger.Error("encode metrics", zapError(err))
		c.String(http.StatusInternalServerError, "cannot encode metrics\n")
		return
	}
	c.Header("Content-Type", metrics.ContentType)
	c.String(http.StatusOK, buf.String())
}

// windowFromQuery builds a store.UsageWindow from the request's query string.
func windowFromQuery(c *app.RequestContext) (store.UsageWindow, error) {
	var w store.UsageWindow
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return w, err
		}
		w.Since = t
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return w, err
		}
		w.Until = t
	}
	w.UserID = c.Query("user")
	w.Provider = c.Query("provider")
	w.Model = c.Query("model")
	return w, nil
}

// limitFromQuery reads ?limit=, clamped to [1, maxLimit].
func limitFromQuery(c *app.RequestContext, def int) int {
	if def <= 0 {
		def = DefaultLimit
	}
	v := c.Query("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// totalsWithCost decorates an aggregate with its estimated cost.
type totalsWithCost struct {
	store.UsageTotals
	Cost pricing.Cost `json:"cost"`
}

// groupRowWithCost is a grouped bucket plus its cost.
type groupRowWithCost struct {
	Key string `json:"key"`
	store.UsageTotals
	Cost pricing.Cost `json:"cost"`
}

// dayRowWithCost is a trend bucket plus its cost.
type dayRowWithCost struct {
	Day string `json:"day"`
	store.UsageTotals
	Cost pricing.Cost `json:"cost"`
}

// costFor prices one bucket using its dominant provider/model when known.
// The by-model/by-provider endpoints know the key, so they price precisely;
// whole-table aggregates fall back to the configured default target.
func (s *Server) costFor(provider, model string, t store.UsageTotals) pricing.Cost {
	return s.pricing.CostOf(provider, model, int(t.PromptTokens), int(t.CompletionTokens))
}

// handleUsageSummary returns the totals for the window.
func (s *Server) handleUsageSummary(ctx context.Context, c *app.RequestContext) {
	w, err := windowFromQuery(c)
	if err != nil {
		badQuery(c, err)
		return
	}
	totals, err := s.store.QueryUsageTotals(ctx, w)
	if err != nil {
		s.fail(c, "query usage totals", err)
		return
	}
	cost, err := s.totalCost(ctx, w, totals)
	if err != nil {
		s.fail(c, "price usage totals", err)
		return
	}
	c.JSON(http.StatusOK, totalsWithCost{UsageTotals: totals, Cost: cost})
}

// totalCost prices every (provider, model) pair in the window and sums them.
//
// Pricing the whole aggregate with a single provider/model would be wrong: a
// window typically mixes models, and the same model can cost different amounts
// through different providers. Summing per pair gives the real spend. When no
// pair matched the price table the total is flagged unpriced rather than
// reported as a misleading zero.
func (s *Server) totalCost(ctx context.Context, w store.UsageWindow, totals store.UsageTotals) (pricing.Cost, error) {
	groups, err := s.store.QueryUsageByProviderModel(ctx, w, maxLimit)
	if err != nil {
		return pricing.Cost{}, err
	}
	if len(groups) == 0 {
		// No rows at all: nothing to price, and the cost of nothing is zero.
		return pricing.Cost{Total: 0, Priced: true}, nil
	}

	var out pricing.Cost
	anyPriced := false
	for _, g := range groups {
		c := s.pricing.CostOf(g.Provider, g.Model, int(g.PromptTokens), int(g.CompletionTokens))
		out.PromptCost += c.PromptCost
		out.CompletionCost += c.CompletionCost
		out.Total += c.Total
		anyPriced = anyPriced || c.Priced
	}
	out.Priced = anyPriced
	return out, nil
}

// groupHandler builds a handler for one grouped aggregation. keyIsModel selects
// how the bucket key maps onto the price table.
func (s *Server) groupHandler(fetch func(context.Context, store.UsageWindow, int) ([]store.UsageGroupRow, error), keyIsModel bool) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		w, err := windowFromQuery(c)
		if err != nil {
			badQuery(c, err)
			return
		}
		rows, err := fetch(ctx, w, limitFromQuery(c, DefaultLimit))
		if err != nil {
			s.fail(c, "query usage group", err)
			return
		}
		out := make([]groupRowWithCost, 0, len(rows))
		for _, r := range rows {
			provider, model := s.cfg.Provider, s.cfg.Model
			if keyIsModel {
				model = r.Key
			} else {
				provider = r.Key
			}
			out = append(out, groupRowWithCost{
				Key:         r.Key,
				UsageTotals: r.UsageTotals,
				Cost:        s.costFor(provider, model, r.UsageTotals),
			})
		}
		c.JSON(http.StatusOK, map[string]any{"rows": out})
	}
}

// handleUsageByModel returns usage grouped by model.
func (s *Server) handleUsageByModel(ctx context.Context, c *app.RequestContext) {
	s.groupHandler(s.store.QueryUsageByModel, true)(ctx, c)
}

// handleUsageByProvider returns usage grouped by provider.
func (s *Server) handleUsageByProvider(ctx context.Context, c *app.RequestContext) {
	s.groupHandler(s.store.QueryUsageByProvider, false)(ctx, c)
}

// handleUsageByUser returns usage grouped by end user.
func (s *Server) handleUsageByUser(ctx context.Context, c *app.RequestContext) {
	// A user key is not a model or provider, so price with the default target.
	s.groupHandler(s.store.QueryUsageByUser, false)(ctx, c)
}

// handleUsageByDay returns the daily trend.
func (s *Server) handleUsageByDay(ctx context.Context, c *app.RequestContext) {
	w, err := windowFromQuery(c)
	if err != nil {
		badQuery(c, err)
		return
	}
	days := 30
	if v := c.Query("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	rows, err := s.store.QueryUsageByDay(ctx, w, days)
	if err != nil {
		s.fail(c, "query usage by day", err)
		return
	}
	out := make([]dayRowWithCost, 0, len(rows))
	for _, r := range rows {
		out = append(out, dayRowWithCost{
			Day:         r.Day,
			UsageTotals: r.UsageTotals,
			Cost:        s.costFor(s.cfg.Provider, s.cfg.Model, r.UsageTotals),
		})
	}
	c.JSON(http.StatusOK, map[string]any{"rows": out})
}

// handleUsageRecent returns the most recent individual calls.
func (s *Server) handleUsageRecent(ctx context.Context, c *app.RequestContext) {
	w, err := windowFromQuery(c)
	if err != nil {
		badQuery(c, err)
		return
	}
	rows, err := s.store.QueryUsageRecent(ctx, w, limitFromQuery(c, 20))
	if err != nil {
		s.fail(c, "query recent usage", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"rows": rows})
}

// handleAudit returns the tool invocation audit log.
func (s *Server) handleAudit(ctx context.Context, c *app.RequestContext) {
	f := store.InvocationFilter{
		UserID:   c.Query("user"),
		ToolName: c.Query("tool"),
		Limit:    limitFromQuery(c, DefaultLimit),
	}
	if v := c.Query("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t
		}
	}
	if v := c.Query("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = t
		}
	}
	rows, err := s.store.QueryInvocations(ctx, f)
	if err != nil {
		s.fail(c, "query invocations", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"rows": rows})
}

// handleSkills lists the registered skills and their enabled state.
func (s *Server) handleSkills(_ context.Context, c *app.RequestContext) {
	c.JSON(http.StatusOK, map[string]any{"skills": s.skills()})
}

// handleSetSkill enables or disables a skill.
func (s *Server) handleSetSkill(_ context.Context, c *app.RequestContext) {
	name := c.Param("name")
	if strings.TrimSpace(name) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "skill name is required"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.BindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "body must be {\"enabled\": true|false}"})
		return
	}
	if err := s.setSkillEnabled(name, *body.Enabled); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// badQuery reports a malformed query string.
func badQuery(c *app.RequestContext, err error) {
	c.JSON(http.StatusBadRequest, map[string]string{
		"error": "invalid time format, want RFC3339: " + err.Error(),
	})
}

// fail logs and reports an internal error without leaking details.
func (s *Server) fail(c *app.RequestContext, what string, err error) {
	s.logger.Error("admin api error", zapString("op", what), zapError(err))
	c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
