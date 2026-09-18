package main

// Code intelligence at the command layer: the process-wide language-server
// manager, the adapter that binds it to one workspace, and the registration that
// puts the four tools on the menu.
//
// The manager is a process singleton for the same reason the background-job
// manager is: starting a language server indexes a whole module, so it is paid
// once and shared by every workspace, surface and turn in this process. Creating
// one per workspace would multiply that cost by the number of folders.

import (
	"context"
	"fmt"
	"sync"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/edit"
	"github.com/huan/huan-agent/internal/lsp"
	agenttool "github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/version"
	"github.com/huan/huan-agent/internal/workspace"
)

var (
	lspOnce    sync.Once
	lspManager *lsp.Manager
)

// languageServers returns the process-wide manager, building it on first use.
//
// Lazy rather than eager: a deployment that never asks a semantic question
// should never have a language server's memory footprint, and one that has none
// installed should not see a startup warning it can do nothing about.
func languageServers(cfg *config.Config, logger *zap.Logger) *lsp.Manager {
	lspOnce.Do(func() {
		servers := make([]lsp.ServerSpec, 0, len(cfg.Tools.LSP.Servers))
		for _, s := range cfg.Tools.LSP.Servers {
			servers = append(servers, lsp.ServerSpec{
				Name:        s.Name,
				Command:     s.Command,
				Args:        s.Args,
				Env:         s.Env,
				Languages:   s.Languages,
				RootMarkers: s.RootMarkers,
				InitOptions: s.InitOptions,
				Enabled:     s.Enabled,
			})
		}
		lspManager = lsp.NewManager(lsp.ManagerOptions{
			Servers:          servers,
			IdleTimeout:      cfg.Tools.LSP.IdleTimeout(),
			RequestTimeout:   lsp.DefaultRequestTimeout,
			HandshakeTimeout: 30 * time.Second,
			Version:          version.String(),
			Logger:           zapLogger{logger},
		})
	})
	return lspManager
}

// closeLanguageServers stops every language server this process started.
//
// main calls it after the command returns, so it runs for every subcommand
// without each one having to remember: a language server is a child process, and
// one left behind holds locks on the module cache.
func closeLanguageServers() {
	if lspManager != nil {
		_ = lspManager.Close()
	}
}

// workspaceLSP binds the process manager to one workspace.
//
// It exists because the two halves of the answer come from different places: the
// manager knows the servers, and the workspace knows the boundary a search may
// not climb above. The adapter is what carries the boundary to the manager — and
// it is what keeps a typed nil from leaking into an interface, which is the
// classic way "no server covers this file" turns into a panic.
type workspaceLSP struct {
	mgr *lsp.Manager
	ws  *workspace.Workspace
}

// ClientFor implements builtin.CodeIntelSource.
func (w workspaceLSP) ClientFor(ctx context.Context, path string) (builtin.CodeIntelClient, error) {
	client, err := w.mgr.ClientFor(ctx, path, w.ws.Root())
	if err != nil || client == nil {
		return nil, err
	}
	return client, nil
}

// ClientForRoot implements builtin.CodeIntelSource.
func (w workspaceLSP) ClientForRoot(ctx context.Context, root string) (builtin.CodeIntelClient, error) {
	client, err := w.mgr.ClientForRoot(ctx, root)
	if err != nil || client == nil {
		return nil, err
	}
	return client, nil
}

// Covers implements builtin.CodeIntelSource.
func (w workspaceLSP) Covers(path string) bool { return w.mgr.Covers(path) }

// FileDiagnostics implements lsp.Diagnoser for the edit-feedback decorator.
func (w workspaceLSP) FileDiagnostics(ctx context.Context, path, ceiling string, wait time.Duration) ([]lsp.Diagnostic, lsp.Diagnosed, error) {
	return w.mgr.FileDiagnostics(ctx, path, ceiling, wait)
}

// zapLogger adapts *zap.Logger to lsp.Logger.
//
// The lsp package declares a two-method interface rather than importing zap, so
// it can be tested without a logging framework; this is the one place the two
// meet.
type zapLogger struct{ l *zap.Logger }

func (z zapLogger) Debugf(format string, args ...any) {
	if z.l != nil {
		z.l.Sugar().Debugf(format, args...)
	}
}

func (z zapLogger) Warnf(format string, args ...any) {
	if z.l != nil {
		z.l.Sugar().Warnf(format, args...)
	}
}

// codeIntelligenceFor builds the four tools for one workspace, or returns
// nothing at all when the capability is off or no server is configured.
//
// Returning nothing is the design, not a shortcut: a code-intelligence tool in a
// deployment with no language server can only fail, and "the call was refused" is
// a worse answer than a tool that was never offered. The same rule the console
// follows for ask_user.
func codeIntelligenceFor(cfg *config.Config, ws *workspace.Workspace, logger *zap.Logger) ([]agenttool.Tool, lsp.Diagnoser) {
	if cfg == nil || ws == nil || !cfg.Tools.LSP.Enable {
		return nil, nil
	}
	mgr := languageServers(cfg, logger)
	if !mgr.HasEnabledServer() {
		logger.Info("code intelligence disabled: no language server is configured",
			zap.String("hint", "set tools.lsp.servers, or install gopls"))
		return nil, nil
	}
	// Configured is not the same as available. Four tools whose every call fails
	// with "not installed" are worse than no tools, so the check is whether the
	// binary is actually on PATH — and the log says which one to install.
	if _, ok := mgr.AvailableCommand(); !ok {
		spec, configured := mgr.FirstConfigured()
		fields := []zap.Field{zap.String("hint", "these tools stay off the menu until the server is installed")}
		if configured {
			fields = append(fields,
				zap.String("command", spec.Command),
				zap.String("install", lsp.InstallHint(spec.Command)))
		}
		logger.Info("code intelligence disabled: the configured language server is not installed", fields...)
		return nil, nil
	}

	source := workspaceLSP{mgr: mgr, ws: ws}
	opts := builtin.CodeIntelOptions{Workspace: ws, Source: source}

	// The constructors return eino's InvokableTool, which already satisfies the
	// internal Tool interface — the same shape every other builtin has.
	builders := []struct {
		name string
		make func(builtin.CodeIntelOptions) (einotool.InvokableTool, error)
	}{
		{builtin.DiagnosticsToolName, builtin.NewDiagnosticsTool},
		{builtin.GotoDefinitionToolName, builtin.NewGotoDefinitionTool},
		{builtin.FindReferencesToolName, builtin.NewFindReferencesTool},
		{builtin.WorkspaceSymbolsToolName, builtin.NewWorkspaceSymbolsTool},
	}

	out := make([]agenttool.Tool, 0, len(builders))
	for _, b := range builders {
		t, err := b.make(opts)
		if err != nil {
			logger.Warn("code intelligence tool not registered",
				zap.String("tool", b.name), zap.Error(err))
			continue
		}
		// Read-only, so the tools stay available on a read-only workspace:
		// observing is not what a read-only workspace forbids.
		out = append(out, agenttool.WithCapability(t, agenttool.CapRead))
	}

	var diagnoser lsp.Diagnoser
	if cfg.Tools.LSP.AttachDiagnostics {
		diagnoser = source
	}
	logger.Info("code intelligence enabled",
		zap.Int("tools", len(out)),
		zap.Bool("attach_diagnostics", diagnoser != nil))
	return out, diagnoser
}

// feedbackOptions builds the decorator's configuration for one workspace.
func feedbackOptions(cfg *config.Config, ws *workspace.Workspace) lsp.FeedbackOptions {
	return lsp.FeedbackOptions{
		Workspace: ws,
		Ceiling:   ws.Root(),
		Max:       cfg.Tools.LSP.MaxDiagnosticsOr(),
		Wait:      cfg.Tools.LSP.DiagnosticsWait(),
	}
}

// newRenameSymbolTool builds the rename tool for one workspace, or returns nil.
//
// It is nil in two cases, and both are the tool being absent rather than present
// and always failing:
//
//   - a read-only workspace, where it could only refuse;
//   - a workspace with no language server, where a rename would have to be faked
//     with a string search — which is exactly the thing this tool exists to replace.
func newRenameSymbolTool(set *workspaceToolSet, mgr *lsp.Manager, ws *workspace.Workspace, batch *edit.Workspace) (einotool.InvokableTool, error) {
	if ws == nil || ws.ReadOnly() || batch == nil {
		return nil, nil
	}
	if mgr == nil || !mgr.HasEnabledServer() {
		return nil, nil
	}
	renamer := &workspaceRenamer{mgr: mgr, ws: ws}
	return builtin.NewRenameSymbolTool(renamer, batch)
}

// workspaceRenamer adapts the language-server manager to what the tool needs.
type workspaceRenamer struct {
	mgr *lsp.Manager
	ws  *workspace.Workspace
}

// Rename implements builtin.Renamer.
//
// The client comes from ClientFor(ctx, path, ceiling): the file decides which
// server covers it (a Go file and a TypeScript file in one workspace are two
// servers), which is the same lookup the code-intelligence tools use.
func (r *workspaceRenamer) Rename(ctx context.Context, path string, at lsp.LineCol, newName string) (lsp.WorkspaceEdit, error) {
	// The tool speaks workspace-relative paths (that is what the model has, and
	// what the sandbox accepts) and the language server speaks absolute ones, so the
	// translation happens here. Resolve also confines the path: a language server
	// request must not be a way to read outside the workspace.
	abs, err := r.ws.Resolve(path)
	if err != nil {
		return lsp.WorkspaceEdit{}, fmt.Errorf("%s: %w", path, err)
	}
	client, err := r.mgr.ClientFor(ctx, abs, "")
	if err != nil {
		return lsp.WorkspaceEdit{}, err
	}
	return client.Rename(ctx, abs, at, newName)
}
