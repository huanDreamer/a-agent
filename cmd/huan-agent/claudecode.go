package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/claudecode"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
)

// newClaudeCodeService builds ClaudeCode compatibility mode.
//
// It never fails, and that is the design: the mode is an addition to a working
// deployment, and a settings.json that cannot be read must leave every
// conversation running exactly as it did before rather than refusing to start
// the server. What went wrong is reported in 设置 → ClaudeCode, where the switch
// that depends on it is.
//
// The working directory handed to it is the deployment's default workspace, not
// the process's own directory: command hooks run there and see it as
// CLAUDE_PROJECT_DIR, which is how a hook script written for Claude Code finds
// the tree it is supposed to inspect.
//
// Only this surface (admin serve) gets the mode. `chat`, `run` and the Feishu
// bot are separate entry points with their own configuration, and a mode whose
// switch lives in the console should not silently change what those do.
func newClaudeCodeService(cfg *config.Config, st store.Store, logger *zap.Logger) *claudecode.Service {
	dir := ""
	if cfg != nil {
		if root, ok := cfg.Tools.WorkspaceOrDefault(); ok {
			dir = root
		}
	}
	svc := claudecode.NewService(context.Background(), claudecode.Options{
		Config: cfg.ClaudeCode,
		Store:  st,
		Retry:  cfg.LLM.RetryPolicy(),
		Logger: logger,
		Dir:    dir,
	})
	return svc
}
