package main

// The apply_patch wiring: the batch applier bound to a workspace, a checkpoint
// recorder and this deployment's limits.

import (
	"context"

	"go.uber.org/zap"

	"github.com/cloudwego/eino/components/tool"
	"github.com/huan/huan-agent/internal/checkpoint"
	"github.com/huan/huan-agent/internal/config"

	"github.com/huan/huan-agent/internal/edit"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/workspace"
)

// newApplyPatchTool builds the tool for one workspace, or returns nil when this
// deployment does not have it.
//
// It is nil for a read-only workspace: the tool could only ever refuse, and a tool
// on the menu that always fails is worse than one that is absent.
// newApplyPatchBatch binds the batch applier to a workspace: its sandbox, this
// deployment's limits, and the checkpoint recorder of whichever turn calls it.
//
// It is built once per workspace and shared by apply_patch and rename_symbol, which
// is what makes a patch and a rename the same atomicity guarantee rather than two
// implementations of one.
func newApplyPatchBatch(set *workspaceToolSet, ws *workspace.Workspace, cp *checkpoint.Checkpointer) (*edit.Workspace, error) {
	if ws == nil {
		return nil, nil
	}
	return edit.NewWorkspace(ws,
		edit.Options{
			MaxBytes: cfgMaxWriteBytes(set.cfg),
			ReadOnly: ws.ReadOnly(),
			Prefix:   builtin.ApplyPatchToolName,
		},
		edit.ApplyOptions{
			Capturer: checkpointCapturer{cp},
			// The checkpoint turn travels on the context, so a tool built once
			// still files its captures under the turn that called it.
			TurnFrom: turnFromContext,
			Logger:   editLogger{set.logger},
			Prefix:   builtin.ApplyPatchToolName,
		}), nil
}

// newApplyPatchTool builds the apply_patch tool, or returns nil for a read-only
// workspace (where it could only ever refuse).
func newApplyPatchTool(set *workspaceToolSet, ws *workspace.Workspace, batch *edit.Workspace) (tool.InvokableTool, error) {
	if ws == nil || ws.ReadOnly() || batch == nil {
		return nil, nil
	}
	return builtin.NewApplyPatchTool(batch, builtin.ApplyPatchOptions{
		ReadOnly: ws.ReadOnly(),
		MaxBytes: cfgMaxWriteBytes(set.cfg),
	})
}

// checkpointCapturer records a file's pre-image through the checkpoint package.
type checkpointCapturer struct{ cp *checkpoint.Checkpointer }

func (c checkpointCapturer) Capture(ctx context.Context, session string, turn int, path string) error {
	if c.cp == nil {
		return nil
	}
	return c.cp.Capture(ctx, session, turn, path)
}

// turnFromContext reads the checkpoint turn the current turn published.
func turnFromContext(ctx context.Context) (string, int, bool) {
	turn, ok := checkpoint.TurnFrom(ctx)
	if !ok {
		return "", 0, false
	}
	return turn.Session, turn.Index, true
}

// editLogger adapts *zap.Logger to the edit package's interface.
type editLogger struct{ l *zap.Logger }

func (e editLogger) Warn(msg string, kv ...any) {
	if e.l != nil {
		e.l.Sugar().Warnw(msg, kv...)
	}
}

// cfgMaxWriteBytes is the per-file write limit, in bytes.
func cfgMaxWriteBytes(cfg *config.Config) int64 {
	if cfg == nil {
		return workspace.DefaultMaxWriteBytes
	}
	_, maxWrite, _ := cfg.Tools.Limits()
	return maxWrite
}
