package main

// Workspace-bound tools, and the per-turn binding that makes a workspace
// switchable without rebuilding anything else.
//
// The problem this file solves: every filesystem and media tool is constructed
// with the workspace it may touch (that is what keeps internal/workspace the one
// place confinement is enforced). A registry built once at start-up can
// therefore only ever point at one directory — which was fine while there was
// one workspace, and is exactly what has to change now that a conversation can
// be in any of several.
//
// The shape here is deliberately a CLONE rather than a rebuild:
//
//  1. the process-wide base registry holds everything that does not depend on a
//     workspace — time/calc/echo, save_document, the skill tool, and every MCP
//     tool an operator added at runtime;
//  2. for one turn, the base is cloned and the workspace-bound tools are
//     replaced with instances built for that turn's workspace, or dropped when
//     the workspace's policy does not allow them (write tools in a read-only
//     workspace, bash when commands are off).
//
// Cloning is what keeps MCP and skill tools in every turn: they are registered
// into the base at runtime, so a clone taken per turn sees them, while a
// per-workspace registry built at start-up would not. It is also cheap: a clone
// is a map copy plus a handful of small structs.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/viking"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"

	agenttool "github.com/huan/huan-agent/internal/tool"
)

// workspaceToolSet builds the tools that operate inside one workspace.
//
// It is stateful only for the media capability targets: resolving them reads the
// model catalog, and doing that per turn would put extra queries on every
// message for an answer that only changes when the operator edits 模型管理.
type workspaceToolSet struct {
	cfg    *config.Config
	st     store.Store
	logger *zap.Logger
	// jobs supervises background processes. It is process-wide and deliberately
	// shared by every workspace: one manager per agent process is what makes
	// "nothing outlives the agent" a property of the process rather than of a
	// workspace. Nil withholds the background tools.
	jobs *jobs.Manager
	// surface names where these tools are used (web, cli, feishu). It is recorded
	// on every background job, so the console can say who started a process.
	surface string

	mu       sync.Mutex
	resolved map[store.Capability]mediaTarget
}

// mediaTarget is a resolved capability binding, or its recorded absence.
type mediaTarget struct {
	target media.Target
	ok     bool
}

// toolSetOptions is what a tool set needs from the process it lives in.
type toolSetOptions struct {
	// Jobs is the background-process supervisor. Nil withholds the background
	// tools rather than registering tools that cannot work.
	Jobs *jobs.Manager
	// Surface names the caller: web, cli or feishu.
	//
	// It is not only a label: a tool that needs something only one surface has is
	// registered off it. ask_user is the case it exists for — it parks the turn
	// until a person answers on the console's card, so the surfaces with no card
	// never see the tool at all.
	Surface string
}

// newWorkspaceToolSet builds a tool set for one process.
func newWorkspaceToolSet(cfg *config.Config, st store.Store, logger *zap.Logger, opts toolSetOptions) *workspaceToolSet {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &workspaceToolSet{
		cfg:      cfg,
		st:       st,
		logger:   logger,
		jobs:     opts.Jobs,
		surface:  opts.Surface,
		resolved: map[store.Capability]mediaTarget{},
	}
}

// build returns the tools bound to one workspace, each already tagged with the
// capability that describes what it does.
//
// What is withheld is process-wide policy, not a workspace property: with
// `tools.read_only` the write and command tools are not registered at all, and
// with `tools.enable_bash: false` the shell is not either. Withholding rather
// than refusing is deliberate — a model that can see a tool will call it, and
// "the call was refused" is a worse answer than a tool that was never on the
// menu.
func (s *workspaceToolSet) build(ws *workspace.Workspace, label string) ([]agenttool.Tool, error) {
	if ws == nil {
		return nil, fmt.Errorf("workspace tool set: 缺少工作区沙箱")
	}

	out := make([]agenttool.Tool, 0, 12)

	// Reads and searches, always available.
	readers := []struct {
		name string
		make func(*workspace.Workspace) (tool.InvokableTool, error)
	}{
		{"read_file", builtin.NewReadFileTool},
		{"list_dir", builtin.NewListDirTool},
		{"glob", builtin.NewGlobTool},
		{"grep", builtin.NewGrepTool},
	}
	for _, r := range readers {
		t, err := r.make(ws)
		if err != nil {
			return nil, fmt.Errorf("build %s tool: %w", r.name, err)
		}
		out = append(out, agenttool.WithCapability(t, agenttool.CapRead))
	}

	// The media tools read (and, for image generation, write) files in the same
	// workspace, so they follow the same process-wide policy.
	out = append(out, s.mediaTools(ws, s.cfg.Tools.ReadOnly)...)

	if s.cfg.Tools.ReadOnly {
		return out, nil
	}
	writers := []struct {
		name string
		make func(*workspace.Workspace) (tool.InvokableTool, error)
	}{
		{"write_file", builtin.NewWriteFileTool},
		{"edit_file", builtin.NewEditFileTool},
	}
	for _, w := range writers {
		t, err := w.make(ws)
		if err != nil {
			return nil, fmt.Errorf("build %s tool: %w", w.name, err)
		}
		out = append(out, agenttool.WithCapability(t, agenttool.CapWrite))
	}

	if !s.cfg.Tools.EnableBash {
		return out, nil
	}
	bashTool, err := builtin.NewBashTool(ws, bashPolicy(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("build bash tool: %w", err)
	}
	out = append(out, agenttool.WithCapability(bashTool, agenttool.CapExec))

	// Background processes are a separate, explicit capability rather than a mode
	// of bash. That split is the point: bash keeps its guarantee that a call
	// leaves nothing running, and only these tools may leave something behind.
	// They are withheld — not registered and refused — when the operator turned
	// them off or when this process has no manager to supervise them with.
	if !s.cfg.Tools.EnableBackground || s.jobs == nil {
		return out, nil
	}
	backgroundTools, err := builtin.NewBackgroundTools(ws, s.jobs, builtin.BackgroundPolicy{
		Enabled:      true,
		DenyPatterns: s.cfg.Tools.DenyOrDefault(),
		Workspace:    label,
		Surface:      s.surface,
		// The conversation the call belongs to, so a job can be shown on the
		// session that started it rather than only in a workspace-wide list.
		// chat.ScopeFrom is the reader the runner publishes for every turn.
		Scope: chat.ScopeFrom,
	})
	if err != nil {
		return nil, fmt.Errorf("build background tools: %w", err)
	}
	for _, t := range backgroundTools {
		out = append(out, agenttool.WithCapability(t, agenttool.CapExec))
	}
	return out, nil
}

// bashPolicy converts the command settings into the tool's policy. A workspace
// scopes where a command runs; the timeout and the deny patterns are process
// settings and stay shared.
func bashPolicy(cfg *config.Config) builtin.BashPolicy {
	return builtin.BashPolicy{
		Enabled:      true,
		Timeout:      cfg.Tools.BashTimeout(),
		MaxTimeout:   cfg.Tools.BashMaxTimeout(),
		DenyPatterns: cfg.Tools.DenyOrDefault(),
	}
}

// mediaTools returns the media tools usable in ws under the given policy.
//
// A tool is built only when 设置 → 模型 binds its capability to an enabled model:
// a tool the model can see but that cannot work is worse than no tool, because
// it will be called.
func (s *workspaceToolSet) mediaTools(ws *workspace.Workspace, readOnly bool) []agenttool.Tool {
	if s.st == nil {
		s.logger.Info("media tools skipped: no model catalog configured",
			zap.Strings("tools", mediaToolNames()))
		return nil
	}
	out := make([]agenttool.Tool, 0, len(mediaToolCandidates))
	for _, c := range mediaToolCandidates {
		// generate_image writes into the workspace, so a read-only workspace
		// withholds it entirely — the same rule the file tools follow.
		if c.access == agenttool.CapWrite && readOnly {
			continue
		}
		target, ok := s.mediaTarget(c.capability)
		if !ok {
			continue
		}
		t, err := c.build(ws, target)
		if err != nil {
			s.logger.Warn("media tool unavailable",
				zap.String("tool", c.name),
				zap.String("capability", string(c.capability)), zap.Error(err))
			continue
		}
		out = append(out, agenttool.WithCapability(t, c.access))
	}
	return out
}

// mediaTarget resolves one capability to the model bound to it, caching both
// answers, so a missing binding is reported once rather than on every turn.
func (s *workspaceToolSet) mediaTarget(cap store.Capability) (media.Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.resolved[cap]; ok {
		return got.target, got.ok
	}

	resolver := media.NewResolver(s.st, os.Getenv)
	// Resolution reads local catalog rows; there is no request to carry a
	// deadline from, and it must not be cancelled by one that ends.
	target, ok := resolver.For(context.Background(), cap)
	if !ok {
		s.logger.Info("media tool skipped: capability is bound to no usable model",
			zap.String("capability", string(cap)))
	}
	s.resolved[cap] = mediaTarget{target: target, ok: ok}
	return target, ok
}

// registerStaticTools adds the tools that do not depend on a workspace.
//
// These are the ones a workspace clone carries by reference: they have no path
// to resolve, so every turn gets the same instance.
func (s *workspaceToolSet) registerStaticTools(reg *agenttool.Registry, svc *viking.Service) error {
	basics := []func() (agenttool.Tool, error){
		func() (agenttool.Tool, error) { return builtin.NewTimeTool() },
		func() (agenttool.Tool, error) { return builtin.NewCalcTool() },
		func() (agenttool.Tool, error) { return builtin.NewEchoTool() },
	}
	for _, make := range basics {
		t, err := make()
		if err != nil {
			return err
		}
		if err := reg.Register(agenttool.WithCapability(t, agenttool.CapRead)); err != nil {
			return err
		}
	}
	return registerDocumentTool(reg, s.cfg, svc)
}

// workspaceBindings turns a scope into the tool registry a turn should run with.
type workspaceBindings struct {
	logger *zap.Logger
	mgr    *workspaces.Manager
	set    *workspaceToolSet
	// base is the process-wide registry: everything not bound to a workspace,
	// plus whatever MCP servers and skills registered as the process ran.
	base *agenttool.Registry
}

// forScope returns the registry for one turn.
//
// A failure is returned rather than worked around. Falling back to the base
// registry would hand the model tools bound to a *different* workspace, which is
// the one outcome this design exists to prevent: a write intended for the
// workspace the user chose landing in the process default.
func (b *workspaceBindings) forScope(ctx context.Context) (*agenttool.Registry, error) {
	if b == nil || b.mgr == nil {
		return nil, nil
	}
	scope := chat.ScopeFrom(ctx)
	ws, spec, err := b.mgr.Resolve(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("解析工作区失败: %w", err)
	}
	return b.bind(ws, spec)
}

// bind returns a registry for one workspace: the base, with its workspace tools
// replaced by instances built for this workspace.
//
// Withheld names are unregistered rather than left in place, so a clone can never
// carry a tool the base built for another directory into a turn that runs
// somewhere else — a write landing in the wrong project is the failure this whole
// design exists to prevent.
func (b *workspaceBindings) bind(ws *workspace.Workspace, spec workspaces.Spec) (*agenttool.Registry, error) {
	reg := b.base.Clone()
	built, err := b.set.build(ws, spec.Name)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(built))
	for _, t := range built {
		// Info reads no I/O for these tools, and binding is not part of a
		// request's work, so it runs on a background context.
		info, ierr := t.Info(context.Background())
		if ierr != nil {
			return nil, fmt.Errorf("workspace tool info: %w", ierr)
		}
		keep[info.Name] = true
		if err := reg.Replace(t); err != nil {
			return nil, fmt.Errorf("bind %s: %w", info.Name, err)
		}
	}
	for _, name := range workspaceBoundToolNames() {
		if keep[name] {
			continue
		}
		// Withheld by policy (or not configured in this deployment): dropping it
		// keeps the model from being offered something it may not use.
		reg.Unregister(name)
	}
	if b.logger != nil {
		b.logger.Debug("workspace tools bound",
			zap.String("workspace", spec.Name), zap.String("root", spec.Root),
			zap.Int("bound", len(built)))
	}
	return reg, nil
}

// workspaceBoundToolNames is every tool name this file may build, and therefore
// every name a clone has to reconsider per workspace. A name left out of this
// list would leak the base registry's binding — built for the default
// workspace — into a turn running somewhere else.
func workspaceBoundToolNames() []string {
	names := []string{"read_file", "list_dir", "glob", "grep", "write_file", "edit_file", "bash"}
	// The background tools are bound here for the same reason bash is: their
	// policy (read-only workspace, deny patterns, cwd) and the jobs they may
	// manage both follow the workspace a turn runs in.
	names = append(names,
		builtin.BackgroundToolName,
		builtin.BackgroundListToolName,
		builtin.BackgroundOutputToolName,
		builtin.BackgroundStopToolName,
	)
	for _, c := range mediaToolCandidates {
		names = append(names, c.name)
	}
	return names
}

// registerWorkspaceTools registers the workspace-bound tools for one workspace
// into reg.
//
// It is the single-workspace path, used by surfaces that are not scope-aware
// (the interactive CLI). It goes through the same builder as the per-turn
// binding, so a read-only workspace withholds exactly the same tools whichever
// surface asks.
func registerWorkspaceTools(reg *agenttool.Registry, cfg *config.Config, st store.Store, logger *zap.Logger, opts toolSetOptions) error {
	root, ok := cfg.Tools.WorkspaceOrDefault()
	if !ok {
		return nil
	}
	maxRead, maxWrite, maxList := cfg.Tools.Limits()
	ws, err := workspace.New(root, workspace.Options{
		ReadOnly: cfg.Tools.ReadOnly,
		Limits: workspace.Limits{
			MaxReadBytes:   maxRead,
			MaxWriteBytes:  maxWrite,
			MaxListEntries: maxList,
		},
	})
	if err != nil {
		return fmt.Errorf("init workspace: %w", err)
	}

	set := newWorkspaceToolSet(cfg, st, logger, opts)
	// The single-workspace path has no label to carry: the tool set is built for
	// the configured root, so the directory's own name is the honest label.
	tools, err := set.build(ws, filepath.Base(root))
	if err != nil {
		return err
	}
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return fmt.Errorf("register %s: %w", nameOf(t), err)
		}
	}
	return nil
}

// nameOf returns a tool's name for a log line or an error, tolerating a tool
// whose Info fails.
func nameOf(t agenttool.Tool) string {
	if t == nil {
		return ""
	}
	info, err := t.Info(context.Background())
	if err != nil || info == nil {
		return fmt.Sprintf("%T", t)
	}
	return info.Name
}

// newWorkspaceManager builds the workspace layer from configuration.
//
// The seed directory is `tools.workspace` (or the process working directory): it
// becomes the first workspace on a database that has none, so an existing
// deployment keeps working in the directory it already used. Whether the agent
// may write files or run commands is process-wide (`tools.read_only` /
// `tools.enable_bash`) and is passed through to every sandbox, not stored per
// workspace.
func newWorkspaceManager(cfg *config.Config, st store.Store, logger *zap.Logger) (*workspaces.Manager, error) {
	if st == nil {
		return nil, fmt.Errorf("工作区需要数据库：未配置 database.path")
	}
	seedRoot, ok := cfg.Tools.WorkspaceOrDefault()
	if !ok {
		return nil, fmt.Errorf("无法确定初始工作区目录（tools.workspace）")
	}
	maxRead, maxWrite, maxList := cfg.Tools.Limits()
	return workspaces.New(workspaces.Options{
		Store:    st,
		SeedRoot: seedRoot,
		ReadOnly: cfg.Tools.ReadOnly,
		Limits: workspace.Limits{
			MaxReadBytes:   maxRead,
			MaxWriteBytes:  maxWrite,
			MaxListEntries: maxList,
		},
		Logger: logger,
	})
}

// newJobManager builds the background-process supervisor for one agent process.
//
// One manager per process is the design, not an implementation detail: a job
// belongs to the agent that started it and is terminated with it, so a manager
// that adopted another process's jobs would be claiming to stop processes it
// cannot reach. That is also why the console shows the jobs of the process that
// serves it rather than a fleet-wide view.
//
// A failure to build it is not fatal — the deployment keeps working with the
// background tools withheld — but it is logged, because silently losing half the
// shell capability is exactly the kind of thing an operator should hear about.
func newJobManager(cfg *config.Config, logger *zap.Logger) (*jobs.Manager, error) {
	dir, ok := cfg.Tools.JobsDirOrDefault(cfg.Database.Path)
	if !ok {
		return nil, fmt.Errorf("无法确定后台进程日志目录（tools.background_dir）")
	}
	mgr, err := jobs.New(jobs.Options{
		Dir:         dir,
		MaxJobs:     cfg.Tools.BackgroundMaxJobs,
		MaxLogBytes: int64(cfg.Tools.BackgroundLogMaxMB) << 20,
		WindowBytes: cfg.Tools.BackgroundWindowKB << 10,
		StopGrace:   cfg.Tools.BackgroundStopGrace(),
		Logger:      logger,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("background jobs ready",
		zap.String("dir", dir),
		zap.Int("max_jobs", mgr.Options().MaxJobs),
		zap.Int64("log_cap_bytes", mgr.Options().MaxLogBytes),
	)
	return mgr, nil
}

// feishuTooling is the bot's tool wiring: a base registry, the workspace layer,
// and the MCP clients to close when the bot stops.
type feishuTooling struct {
	base     *agenttool.Registry
	manager  *workspaces.Manager
	bindings *workspaceBindings
	clients  []*mcp.Client
}

// close disconnects every MCP server the bot started. A stdio server is a child
// process, so leaving one running after the bot exits would leak it.
func (t *feishuTooling) close() {
	for _, c := range t.clients {
		_ = c.Close()
	}
	t.clients = nil
}

// buildFeishuTooling assembles the tools the Feishu bot answers with.
//
// The registry holds everything that does not depend on a workspace, plus the
// workspace-bound tools built for the default workspace: that last part is what
// a per-turn clone replaces, and what 设置 → 服务与工具 lists. MCP servers come
// from the config file — the console's own MCP runtime belongs to the admin
// process, and the bot must not depend on that process running.
//
// Every failure here is reported rather than swallowed: a bot with tools that
// silently do nothing is worse than one that says so at startup.
func buildFeishuTooling(ctx context.Context, cfg *config.Config, st store.Store, logger *zap.Logger,
	svc *viking.Service, jobMgr *jobs.Manager) (*feishuTooling, error) {

	out := &feishuTooling{base: agenttool.NewRegistry()}
	if err := registerBuiltinTools(out.base, cfg, st, logger, svc, toolSetOptions{Jobs: jobMgr, Surface: "feishu"}); err != nil {
		return nil, fmt.Errorf("register builtin tools: %w", err)
	}

	for _, s := range cfg.MCP.Servers {
		if !s.IsEnabled() {
			continue
		}
		c, err := mcp.Connect(ctx, mcp.ServerSpec{
			Name:    s.Name,
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		})
		if err != nil {
			out.close()
			return nil, fmt.Errorf("connect mcp %s: %w", s.Name, err)
		}
		out.clients = append(out.clients, c)
		n, rerr := mcp.RegisterMCPTools(ctx, out.base, c, logger)
		if rerr != nil {
			out.close()
			return nil, fmt.Errorf("register mcp tools %s: %w", s.Name, rerr)
		}
		logger.Info("mcp server connected for the feishu bot",
			zap.String("server", s.Name), zap.Int("tools", n))
	}

	mgr, err := newWorkspaceManager(cfg, st, logger)
	if err != nil {
		out.close()
		return nil, err
	}
	out.manager = mgr
	out.bindings = &workspaceBindings{
		logger: logger,
		mgr:    mgr,
		set:    newWorkspaceToolSet(cfg, st, logger, toolSetOptions{Jobs: jobMgr, Surface: "feishu"}),
		base:   out.base,
	}
	return out, nil
}
