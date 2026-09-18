package main

// Checkpoints at the command layer: building the checkpointer for a workspace and
// the CLI commands that inspect and restore one.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/checkpoint"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/workspace"
)

// newCheckpointer builds the checkpointer for one workspace, or returns nil when
// the capability is off.
//
// It is called once per workspace, from the place that builds that workspace's
// tools: the console serves several workspaces from one process, so a checkpointer
// created once at startup would resolve every path through whichever workspace
// happened to be current then.
//
// A nil checkpointer is the "no checkpoints" state every caller already handles:
// no guard is installed, nothing is captured, and the file tools behave exactly as
// they did before this feature existed.
func newCheckpointer(cfg *config.Config, ws *workspace.Workspace, logger *zap.Logger) (*checkpoint.Checkpointer, error) {
	if cfg == nil || ws == nil || !cfg.Tools.Checkpoint.Enable {
		return nil, nil
	}
	dir, ok := cfg.Tools.CheckpointDirOrDefault(cfg.Database.Path)
	if !ok {
		return nil, nil
	}
	cp, err := checkpoint.New(checkpoint.Options{
		Dir:        dir,
		Workspace:  ws,
		KeepTurns:  cfg.Tools.Checkpoint.KeepTurnsOr(),
		MaxTotalMB: cfg.Tools.Checkpoint.MaxTotalMBOr(),
		Enable:     true,
		Logger:     checkpointLogger{logger},
	})
	if err != nil {
		return nil, err
	}
	return cp, nil
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

// beginCheckpointTurn opens a checkpoint for a new turn of a session, returning
// the turn to put on the context and a function to close it.
//
// It is one function rather than two calls so the two halves cannot drift: a turn
// whose end is never recorded is a turn whose git reading is a "before" picture
// with nothing to compare it to.
func beginCheckpointTurn(ctx context.Context, cp *checkpoint.Checkpointer, session string, logger *zap.Logger) (context.Context, func()) {
	if cp == nil {
		return ctx, func() {}
	}
	index, err := cp.NextTurn(session)
	if err != nil {
		if logger != nil {
			logger.Warn("checkpoint: 无法确定轮次编号，本轮不做检查点", zap.Error(err))
		}
		return ctx, func() {}
	}
	turn := checkpoint.Turn{Session: session, Index: index}
	if err := cp.BeginTurn(ctx, turn.Session, turn.Index); err != nil {
		if logger != nil {
			logger.Warn("checkpoint: 开始记录失败", zap.Error(err))
		}
	}
	return checkpoint.WithTurn(ctx, turn), func() {
		if err := cp.EndTurn(context.WithoutCancel(ctx), turn.Session, turn.Index); err != nil && logger != nil {
			logger.Warn("checkpoint: 结束记录失败", zap.Error(err))
		}
		// Pruning runs at the end of a turn rather than on a timer: that is the
		// moment the directory grew, and a deployment that never finishes a turn
		// has nothing to prune.
		if _, perr := cp.Prune(); perr != nil && logger != nil {
			logger.Warn("checkpoint: 清理旧检查点失败", zap.Error(perr))
		}
	}
}

// runCheckpointCommand handles the REPL's /checkpoints and /rollback commands.
//
// It returns true when it handled the line, so the REPL's switch stays a list of
// one-line cases.
func runCheckpointCommand(line string, cp *checkpoint.Checkpointer, session string) bool {
	lower := strings.ToLower(line)
	switch {
	case lower == "/checkpoints":
		describeCheckpoints(cp, session)
		return true
	case strings.HasPrefix(lower, "/rollback"):
		arg := strings.TrimSpace(line[len("/rollback"):])
		rollbackCheckpoint(cp, session, arg)
		return true
	default:
		return false
	}
}

func describeCheckpoints(cp *checkpoint.Checkpointer, session string) {
	if cp == nil {
		fmt.Println("（检查点功能已关闭：tools.checkpoint.enable=false）")
		return
	}
	turns, err := cp.ListTurns(session)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取检查点失败:", err)
		return
	}
	if len(turns) == 0 {
		fmt.Println("（本次会话还没有检查点：还没有文件被写入工具改动过）")
		return
	}
	fmt.Printf("本次会话有 %d 个检查点：\n", len(turns))
	for _, t := range turns {
		m, err := cp.Manifest(session, t)
		if err != nil {
			fmt.Printf("  第 %d 轮：%v\n", t, err)
			continue
		}
		fmt.Printf("  第 %d 轮：%d 个文件（%s）\n", t, len(m.Files),
			m.CreatedAt.Format(time.RFC3339))
	}
	fmt.Println("用 /rollback <轮次> 回退一轮；加 --force 覆盖检查点之后被改过的文件。")
}

func rollbackCheckpoint(cp *checkpoint.Checkpointer, session, arg string) {
	if cp == nil {
		fmt.Println("（检查点功能已关闭：tools.checkpoint.enable=false）")
		return
	}
	force := false
	fields := strings.Fields(arg)
	keep := fields[:0]
	for _, f := range fields {
		if f == "--force" || f == "-f" {
			force = true
			continue
		}
		keep = append(keep, f)
	}
	if len(keep) != 1 {
		fmt.Println("用法：/rollback <轮次> [--force]；用 /checkpoints 查看有哪些轮次")
		return
	}
	turn, err := strconv.Atoi(keep[0])
	if err != nil {
		fmt.Println("轮次必须是一个数字；用 /checkpoints 查看有哪些轮次")
		return
	}

	report, err := cp.Restore(session, turn, checkpoint.RestoreOptions{
		Force:   force,
		Session: session,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "回退失败:", err)
		return
	}
	fmt.Println(report.Summary())
	if len(report.Conflicts) > 0 {
		fmt.Println("以下文件在检查点之后被改过，默认没有覆盖：")
		for _, c := range report.Conflicts {
			fmt.Printf("  %s：%s\n", c.Path, c.Reason)
		}
	}
	if len(report.Skipped) > 0 {
		fmt.Println("以下条目不在检查点覆盖范围内：")
		for _, s := range report.Skipped {
			fmt.Printf("  %s：%s\n", s.Path, s.Reason)
		}
	}
	if report.Undo != nil {
		fmt.Printf("这次回退本身也可以撤销：/rollback %d\n", report.Undo.Turn)
	}
}

// workspaceForCheckpoints resolves the workspace a CLI session works in.
//
// The checkpointer needs a sandbox to resolve and confine paths through, and the
// CLI has exactly one: the configured directory. A deployment with no workspace
// gets no checkpoints, which is correct — there is nowhere for them to point.
func workspaceForCheckpoints(cfg *config.Config) *workspace.Workspace {
	root, ok := cfg.Tools.WorkspaceOrDefault()
	if !ok {
		return nil
	}
	ws, err := workspace.New(root, workspace.Options{
		ReadOnly: cfg.Tools.ReadOnly,
		Limits:   workspaceLimits(cfg),
	})
	if err != nil {
		return nil
	}
	return ws
}

// workspaceLimits builds the sandbox limits from the tool configuration.
func workspaceLimits(cfg *config.Config) workspace.Limits {
	maxRead, maxWrite, maxList := cfg.Tools.Limits()
	return workspace.Limits{
		MaxReadBytes:   maxRead,
		MaxWriteBytes:  maxWrite,
		MaxListEntries: maxList,
	}
}

// checkpointDir resolves where checkpoints live for this deployment, with "" when
// the feature is off or no directory could be determined.
func checkpointDir(cfg *config.Config) string {
	if cfg == nil || !cfg.Tools.Checkpoint.Enable {
		return ""
	}
	dir, ok := cfg.Tools.CheckpointDirOrDefault(cfg.Database.Path)
	if !ok {
		return ""
	}
	return dir
}
