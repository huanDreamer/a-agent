package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/documents"
	"github.com/huan/huan-agent/internal/viking"
)

// OpenVikingConsole is the OpenViking integration as the console uses it. It is
// an interface so the routes can be exercised with a stub, and so a server built
// without the integration (the zero value) simply reports "disabled" instead of
// failing.
type OpenVikingConsole interface {
	// Status gathers the connection state and the counters.
	Status(ctx context.Context) viking.Status
	// SyncWorkspace publishes the workspace; full ignores the state file.
	SyncWorkspace(ctx context.Context, full bool) (documents.Report, error)
	// SaveDocument stores one document and returns its viking:// URI.
	SaveDocument(ctx context.Context, doc documents.Document) (string, error)
	// SyncedDocuments lists what is already stored.
	SyncedDocuments() []documents.Entry
	// Flush submits buffered memory.
	Flush(ctx context.Context)
}

// registerOpenVikingRoutes wires the OpenViking endpoints.
func (s *Server) registerOpenVikingRoutes(authed *route.RouterGroup) {
	authed.GET("/openviking/status", s.handleOpenVikingStatus)
	authed.POST("/openviking/sync", s.handleOpenVikingSync)
	authed.POST("/openviking/save", s.handleOpenVikingSave)
	authed.GET("/openviking/documents", s.handleOpenVikingDocuments)
	authed.POST("/openviking/flush", s.handleOpenVikingFlush)
}

// openVikingDisabled is the answer every OpenViking route gives when the
// integration is off. Returning 200 with `enable:false` (rather than 404 or 500)
// is what lets the console render "not configured" as a state instead of as an
// error the operator cannot clear.
type openVikingDisabled struct {
	Enable  bool   `json:"enable"`
	Message string `json:"message"`
}

func (s *Server) openVikingUnavailable(c *app.RequestContext) bool {
	if s.cfg.OpenViking == nil {
		c.JSON(http.StatusOK, openVikingDisabled{
			Enable:  false,
			Message: "openviking is not enabled; set openviking.enable: true in the config",
		})
		return true
	}
	return false
}

// handleOpenVikingStatus reports the connection, memory counters and document
// counts.
func (s *Server) handleOpenVikingStatus(ctx context.Context, c *app.RequestContext) {
	if s.openVikingUnavailable(c) {
		return
	}
	c.JSON(http.StatusOK, s.cfg.OpenViking.Status(ctx))
}

// handleOpenVikingSync runs a workspace sync.
//
// A sync that cannot run (no workspace configured) answers 200 with ok=false and
// the reason, matching how MCP's connection test reports failures: the console
// shows the reason, and "misconfigured" is not a transport error.
func (s *Server) handleOpenVikingSync(ctx context.Context, c *app.RequestContext) {
	if s.openVikingUnavailable(c) {
		return
	}
	var body struct {
		Full bool `json:"full"`
	}
	// An empty body is the normal case (the button sends none), so a bind
	// failure is not an error the caller needs to hear about.
	_ = c.BindJSON(&body)

	rep, err := s.cfg.OpenViking.SyncWorkspace(ctx, body.Full)
	if err != nil {
		if errors.Is(err, documents.ErrSyncing) {
			c.JSON(http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "report": rep})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "report": rep})
}

// handleOpenVikingSave stores one document.
func (s *Server) handleOpenVikingSave(ctx context.Context, c *app.RequestContext) {
	if s.openVikingUnavailable(c) {
		return
	}
	var body struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
		Source  string   `json:"source"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Content) == "" {
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "title and content are required"})
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = "console"
	}
	uri, err := s.cfg.OpenViking.SaveDocument(ctx, documents.Document{
		Title:   body.Title,
		Content: body.Content,
		Tags:    body.Tags,
		Source:  source,
	})
	if err != nil {
		s.logger.Warn("openviking save document failed", zap.Error(err))
		c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "uri": uri})
}

// handleOpenVikingDocuments lists what has been stored.
func (s *Server) handleOpenVikingDocuments(_ context.Context, c *app.RequestContext) {
	if s.openVikingUnavailable(c) {
		return
	}
	docs := s.cfg.OpenViking.SyncedDocuments()
	if docs == nil {
		docs = []documents.Entry{}
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "documents": docs})
}

// handleOpenVikingFlush submits buffered memory immediately, which is what an
// operator wants before checking the other end for what was just written.
func (s *Server) handleOpenVikingFlush(ctx context.Context, c *app.RequestContext) {
	if s.openVikingUnavailable(c) {
		return
	}
	s.cfg.OpenViking.Flush(ctx)
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}
