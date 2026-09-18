package server

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/huan/huan-agent/internal/subagent"
)

// The console's view of the subagents a conversation delegated.
//
// It is the same shape as the background-jobs surface, deliberately: the header
// answers the same question ("what is this conversation running, and what did it
// run") for both, and a reader who has learned one chip has learned the other.
//
// The difference is where the truth lives. A job is a process the server owns, so
// the jobs endpoint reads the supervisor. A subagent is a nested run, and the list
// of them is kept by whoever spawned it — so the tracker is handed in, and this
// file only projects it.

// SubagentTracker is the run list as the console reads it.
type SubagentTracker interface {
	// List returns a conversation's runs, newest first.
	List(session string) []subagent.Run
	// Running counts a conversation's live runs.
	Running(session string) int
	// RunningAll counts live runs process-wide, which is what the gate bounds.
	RunningAll() int
}

// registerSubagentRoutes wires the endpoint. It is absent when the deployment has
// no subagents, so a header never shows a chip that can only ever say "none".
func (s *Server) registerSubagentRoutes(authed *route.RouterGroup) {
	if s.chat.Subagents == nil {
		return
	}
	authed.GET("/chat/sessions/:id/subagents", s.handleListSubagents)
}

// handleListSubagents reports what a conversation has delegated.
func (s *Server) handleListSubagents(ctx context.Context, c *app.RequestContext) {
	tracker := s.chat.Subagents
	if tracker == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "子 agent 未启用"})
		return
	}
	sessionID := c.Param("id")
	if _, err := s.store.GetChatSession(ctx, sessionID); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	runs := tracker.List(sessionID)
	tokens := 0
	for _, r := range runs {
		tokens += r.Tokens
	}
	c.JSON(http.StatusOK, map[string]any{
		"subagents": runs,
		"running":   tracker.Running(sessionID),
		"total":     len(runs),
		// The process-wide gate is what bounds parallelism, so the header can say
		// "2 of 3 running" rather than offering an unexplained wait.
		"max_concurrent": s.chat.SubagentMaxConcurrent,
		"running_all":    tracker.RunningAll(),
		"tokens":         tokens,
	})
}
