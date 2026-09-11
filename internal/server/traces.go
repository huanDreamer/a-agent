package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/huan/huan-agent/internal/langfuse"
)

// TraceReader reads traces back from the tracing backend. It is an interface so
// the server can expose the trace UI while remaining runnable (and testable)
// without a Langfuse instance.
type TraceReader interface {
	// Enabled reports whether tracing is configured.
	Enabled() bool
	// Stats returns delivery counters for the tracing client.
	Stats() langfuse.Stats
	// ListTraces returns recent traces.
	ListTraces(ctx context.Context, f langfuse.TraceFilter) ([]langfuse.TraceSummary, error)
	// GetTrace returns one trace with its observation tree.
	GetTrace(ctx context.Context, traceID string) (*langfuse.TraceDetail, error)
	// Host returns the configured Langfuse base URL, for deep links.
	Host() string
}

// registerTraceRoutes wires the trace endpoints. Reading traces is proxied
// through the server so the Langfuse secret key never reaches the browser.
//
// The routes are registered even when tracing is unconfigured: a UI that asks
// should be told "tracing is off, here is what to set" rather than getting a
// 404, which is indistinguishable from a broken deployment.
func (s *Server) registerTraceRoutes(authed *route.RouterGroup) {
	authed.GET("/traces/status", s.handleTraceStatus)
	authed.GET("/traces", s.handleListTraces)
	authed.GET("/traces/:id", s.handleGetTrace)
}

// handleTraceStatus reports whether tracing is on and how delivery is going.
func (s *Server) handleTraceStatus(_ context.Context, c *app.RequestContext) {
	if s.traces == nil {
		c.JSON(http.StatusOK, map[string]any{"enabled": false})
		return
	}
	stats := s.traces.Stats()
	c.JSON(http.StatusOK, map[string]any{
		"enabled": s.traces.Enabled(),
		"host":    s.traces.Host(),
		"stats":   stats,
	})
}

// handleListTraces proxies the recent-trace list.
func (s *Server) handleListTraces(ctx context.Context, c *app.RequestContext) {
	if s.traces == nil || !s.traces.Enabled() {
		c.JSON(http.StatusOK, map[string]any{
			"enabled": false,
			"traces":  []any{},
			"message": "链路追踪未启用：请配置 langfuse.enable 与 API 密钥",
		})
		return
	}
	page := 1
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	traces, err := s.traces.ListTraces(ctx, langfuse.TraceFilter{
		UserID:    c.Query("user"),
		SessionID: c.Query("session"),
		Name:      c.Query("name"),
		Page:      page,
		Limit:     limitFromQuery(c, 50),
	})
	if err != nil {
		s.traceError(c, err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"enabled": true,
		"host":    s.traces.Host(),
		"traces":  traces,
	})
}

// handleGetTrace proxies one trace and its observations.
func (s *Server) handleGetTrace(ctx context.Context, c *app.RequestContext) {
	if s.traces == nil || !s.traces.Enabled() {
		c.JSON(http.StatusOK, map[string]any{
			"enabled": false,
			"message": "链路追踪未启用",
		})
		return
	}
	id := c.Param("id")
	detail, err := s.traces.GetTrace(ctx, id)
	if err != nil {
		s.traceError(c, err)
		return
	}
	if detail == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "trace not found"})
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"enabled": true,
		"host":    s.traces.Host(),
		"trace":   detail,
	})
}

// traceError maps a tracing failure to a useful response.
//
// A Langfuse outage is not an application error: the UI must still work, so a
// disabled/unreachable backend answers 200 with an explanatory message rather
// than 500 (which would look like the admin server itself was broken).
func (s *Server) traceError(c *app.RequestContext, err error) {
	if errors.Is(err, langfuse.ErrDisabled) {
		c.JSON(http.StatusOK, map[string]any{
			"enabled": false,
			"message": "链路追踪未启用",
		})
		return
	}
	s.logger.Warn("langfuse read failed", zapError(err))
	c.JSON(http.StatusBadGateway, map[string]string{
		"error": "无法读取链路数据：" + err.Error(),
	})
}
