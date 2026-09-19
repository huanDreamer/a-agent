package main

// The artifact feature at the command layer: building the store, registering the
// save_artifact tool, and publishing a store for the surfaces that are not the
// web console (the CLI REPL, a one-shot run, the Feishu bot).
//
// The console does the same three things from internal/server, because there the
// store is per turn and owned by the server object. A command is a single
// process with a single owner, so it can hold one store for its own lifetime —
// but the filing rules are shared, not copied: internal/artifact.Saver is the one
// implementation both layers publish (see internal/tool.ArtifactSaver).

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/artifact"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// registerArtifactTool registers save_artifact when the deployment has an
// artifact store.
//
// Without a store the tool is not registered at all, which is the same rule
// save_document follows: a tool the model can see but that cannot work is worse
// than no tool, because it will be called. The tool itself is built with no
// arguments — where an artifact goes is a property of the turn, published on the
// context — so this function only decides whether the process has the feature.
func registerArtifactTool(reg *tool.Registry, cfg *config.Config) error {
	if cfg == nil || !cfg.Tools.Artifacts.Enable {
		return nil
	}
	t, err := builtin.NewSaveArtifactTool()
	if err != nil {
		return fmt.Errorf("build save_artifact tool: %w", err)
	}
	// CapRead: an artifact is written to the deployment's own store, not to the
	// workspace. Saving one reads nothing, runs nothing, and cannot touch a file
	// the user cares about — it cannot even choose a path, only a name whose
	// extension has to be one the store accepts. It is therefore not withheld from
	// a read-only workspace, which is exactly where a long analysis produces a
	// report and has nowhere else to put it.
	//
	// Serial, and here the reason is a real collision rather than only the write
	// itself being a write: two artifacts saved in the same second with the same
	// title produce the same file name, and the second would overwrite the first
	// and leave two rows pointing at one file.
	if err := reg.Register(tool.WithConcurrency(
		tool.WithCapability(t, tool.CapRead), tool.Serial)); err != nil {
		return fmt.Errorf("register save_artifact: %w", err)
	}
	return nil
}

// newCommandArtifactStore builds the artifact store for a command, or returns nil
// when the feature is off or no root could be resolved.
//
// A nil store is a supported state everywhere below: save_artifact is not
// registered, so nothing can ask for one.
func newCommandArtifactStore(cfg *config.Config, logger *zap.Logger) *artifact.Store {
	if cfg == nil || !cfg.Tools.Artifacts.Enable {
		return nil
	}
	root, ok := cfg.Tools.ArtifactsRootOrDefault(cfg.Database.Path)
	if !ok || strings.TrimSpace(root) == "" {
		return nil
	}
	st, err := artifact.New(root, artifact.Options{MaxBytes: cfg.Tools.Artifacts.MaxBytes})
	if err != nil {
		if logger != nil {
			// A warning rather than a failure: the store is one feature of many,
			// and a command that cannot write artifacts can still answer.
			logger.Warn("产物存储不可用，save_artifact 已停用", zap.Error(err))
		}
		return nil
	}
	if logger != nil {
		logger.Info("产物存储已启用", zap.String("root", st.Root()))
	}
	return st
}

// withCommandArtifacts publishes the artifact store on a command surface's turn
// context.
//
// There is deliberately no URL: these surfaces have no HTTP server, so nothing is
// serving the bytes. Reporting an address that nothing answers is worse than
// reporting none — the tool says the file was stored and where, and the console
// will list it if the deployment runs one.
func withCommandArtifacts(ctx context.Context, files *artifact.Store, st store.Store,
	owner string, logger *zap.Logger) context.Context {
	if files == nil || st == nil {
		return ctx
	}
	saver, err := artifact.NewSaver(files, st, artifact.SaverOptions{
		Owner:  owner,
		Logger: commandArtifactLogger{logger},
	})
	if err != nil {
		if logger != nil {
			logger.Warn("save_artifact 本轮不可用", zap.Error(err))
		}
		return ctx
	}
	return tool.WithArtifacts(ctx, saver)
}

// commandArtifactLogger adapts *zap.Logger to the artifact package's Logger.
type commandArtifactLogger struct{ l *zap.Logger }

func (c commandArtifactLogger) Warn(msg string, kv ...any) {
	if c.l != nil {
		c.l.Sugar().Warnw(msg, kv...)
	}
}

// artifactRoot resolves where the console's artifact store lives, with "" when
// the feature is off. It mirrors checkpointDir: the path is settled here rather
// than inside server.New so a deployment that turned the feature off never has a
// directory resolved for it.
func artifactRoot(cfg *config.Config) string {
	if cfg == nil || !cfg.Tools.Artifacts.Enable {
		return ""
	}
	root, ok := cfg.Tools.ArtifactsRootOrDefault(cfg.Database.Path)
	if !ok {
		return ""
	}
	return root
}
