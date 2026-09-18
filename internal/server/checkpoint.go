package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/checkpoint"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

// The console half of the checkpoints.
//
// The listener's half is a decorator on the write tools, which captures a file's
// pre-image before it changes. What only the console can do is the framing around
// a turn — deciding which turn number a capture belongs to, recording git's view at
// both ends, and offering the rollback — because a console conversation outlives
// the process and has to be able to undo a turn from a previous session.
//
// That is also why a checkpointer is built per workspace here (see checkpointFor)
// rather than once: this process serves several workspaces, and a single
// checkpointer made at startup would resolve every path through whichever one
// happened to be current then.

// checkpointFor builds a checkpointer bound to one workspace.
//
// It returns nil when the feature is off, which every caller treats as "no
// checkpoints" rather than as an error: a deployment that turned the feature off
// must not have turns that fail to start because of it.
func (s *Server) checkpointFor(ws *workspaceSandbox) *checkpoint.Checkpointer {
	if !s.checkpointSettings.Enable || ws == nil {
		return nil
	}
	cp, err := checkpoint.New(checkpoint.Options{
		Dir:        s.checkpointSettings.Dir,
		Workspace:  ws.sandbox,
		KeepTurns:  s.checkpointSettings.KeepTurns,
		MaxTotalMB: s.checkpointSettings.MaxTotalMB,
		Enable:     true,
		Logger:     checkpointLogger{s.logger},
	})
	if err != nil {
		s.logger.Warn("chat: checkpoints unavailable for this workspace", zapError(err))
		return nil
	}
	return cp
}

// checkpointLogger adapts *zap.Logger to the checkpoint package's interface.
type checkpointLogger struct{ l *zap.Logger }

func (c checkpointLogger) Info(msg string, kv ...any) {
	if c.l != nil {
		c.l.Sugar().Infow(msg, kv...)
	}
}

func (c checkpointLogger) Warn(msg string, kv ...any) {
	if c.l != nil {
		c.l.Sugar().Warnw(msg, kv...)
	}
}

// workspaceSandbox pairs the two things every checkpoint call needs: the sandbox
// paths are resolved through, and the name the manifest is filed under.
type workspaceSandbox struct {
	sandbox *workspace.Workspace
	name    string
}

// checkpointWorkspaceFor resolves the workspace a conversation runs in, or nil
// when there is no workspace layer to ask (a single-workspace deployment still has
// one: the bindings resolve the default).
func (s *Server) checkpointWorkspaceFor(ctx context.Context, sess store.ChatSession) *workspaceSandbox {
	if s.chat.Workspaces == nil {
		return nil
	}
	ws, spec, err := s.chat.Workspaces.Resolve(ctx, workspaces.WebScope(sess.ID))
	if err != nil || ws == nil {
		if err != nil {
			s.logger.Warn("chat: resolve workspace for checkpoints failed",
				zapString("session", sess.ID), zapError(err))
		}
		return nil
	}
	return &workspaceSandbox{sandbox: ws, name: spec.Name}
}

// beginCheckpointTurn opens a checkpoint for a console turn and returns the context
// to run it with, plus the call to close it.
//
// It mirrors the CLI's helper on purpose — the same three steps in the same order —
// because the two surfaces must agree about what a turn is: the number a capture is
// filed under, the git reading taken before anything happens, and the pruning that
// runs once the turn has grown the directory.
func (s *Server) beginCheckpointTurn(ctx context.Context, sess store.ChatSession) (context.Context, func()) {
	ws := s.checkpointWorkspaceFor(ctx, sess)
	cp := s.checkpointFor(ws)
	if cp == nil {
		return ctx, func() {}
	}

	index, err := cp.NextTurn(sess.ID)
	if err != nil {
		s.logger.Warn("chat: could not allocate a checkpoint turn", zapError(err))
		return ctx, func() {}
	}
	turn := checkpoint.Turn{Session: sess.ID, Index: index}
	if err := cp.BeginTurn(ctx, turn.Session, turn.Index); err != nil {
		s.logger.Warn("chat: checkpoint begin failed",
			zapString("session", sess.ID), zapError(err))
	}
	return checkpoint.WithTurn(ctx, turn), func() {
		// The turn is over, so a cancelled context is expected here: the readings
		// and the pruning still have to happen.
		done := context.WithoutCancel(ctx)
		if err := cp.EndTurn(done, turn.Session, turn.Index); err != nil {
			s.logger.Warn("chat: checkpoint end failed",
				zapString("session", sess.ID), zapError(err))
		}
		if _, err := cp.Prune(); err != nil {
			s.logger.Warn("chat: checkpoint prune failed", zapError(err))
		}
	}
}

// registerCheckpointRoutes wires the checkpoint endpoints.
//
// They are registered only when the feature is on: a console that offers a rollback
// button for a deployment with no checkpoints is offering a button that always
// fails.
func (s *Server) registerCheckpointRoutes(authed *route.RouterGroup) {
	if !s.checkpointSettings.Enable || s.checkpointSettings.Dir == "" {
		return
	}
	authed.GET("/chat/sessions/:id/turns/:tid/checkpoint", s.handleGetCheckpoint)
	authed.POST("/chat/sessions/:id/turns/:tid/rollback", s.handleRollbackCheckpoint)
}

// handleGetCheckpoint reports what a turn's checkpoint holds.
func (s *Server) handleGetCheckpoint(ctx context.Context, c *app.RequestContext) {
	sess, turn, ws, ok := s.checkpointTarget(ctx, c)
	if !ok {
		return
	}
	cp := s.checkpointFor(ws)
	if cp == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "检查点功能未启用"})
		return
	}
	m, err := cp.Manifest(sess.ID, turn)
	if err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session":        sess.ID,
		"turn":           turn,
		"created_at":     m.CreatedAt,
		"git_head":       m.GitHead,
		"git_status":     m.GitStatus,
		"git_status_end": m.GitStatusEnd,
		"files":          m.Files,
		"summary":        m.Describe(),
	})
}

// handleRollbackCheckpoint restores a turn.
func (s *Server) handleRollbackCheckpoint(ctx context.Context, c *app.RequestContext) {
	sess, turn, ws, ok := s.checkpointTarget(ctx, c)
	if !ok {
		return
	}
	cp := s.checkpointFor(ws)
	if cp == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "检查点功能未启用"})
		return
	}

	var body struct {
		Force bool `json:"force"`
	}
	// An empty body is the ordinary case (the button posts nothing), so a decode
	// failure is only reported when there was something to decode.
	if len(c.Request.Body()) > 0 {
		_ = c.BindJSON(&body)
	}

	report, err := cp.Restore(sess.ID, turn, checkpoint.RestoreOptions{
		Force:   body.Force,
		Session: sess.ID,
	})
	switch {
	case errors.Is(err, checkpoint.ErrNoCheckpoint):
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	case err != nil:
		s.fail(c, "rollback checkpoint", err)
		return
	}

	// A conflict is not a failure: the restore ran and deliberately left files it
	// did not write alone. It is 409 rather than 200 because the caller has a
	// decision to make — accept the partial rollback or ask for the force — and a
	// 200 would let a UI report a complete rollback that was not one.
	if len(report.Conflicts) > 0 {
		c.JSON(http.StatusConflict, map[string]any{
			"report":  report,
			"summary": report.Summary(),
		})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"report": report, "summary": report.Summary()})
}

// checkpointTarget resolves the request's session, turn number and workspace, and
// writes the error response itself when any of them is missing.
func (s *Server) checkpointTarget(ctx context.Context, c *app.RequestContext) (store.ChatSession, int, *workspaceSandbox, bool) {
	sess, err := s.store.GetChatSession(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return store.ChatSession{}, 0, nil, false
		}
		s.fail(c, "read chat session", err)
		return store.ChatSession{}, 0, nil, false
	}
	turn, err := strconv.Atoi(c.Param("tid"))
	if err != nil || turn <= 0 {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "轮次必须是一个正整数"})
		return store.ChatSession{}, 0, nil, false
	}
	return sess, turn, s.checkpointWorkspaceFor(ctx, sess), true
}
