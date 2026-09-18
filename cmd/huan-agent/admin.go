package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/huan/huan-agent/internal/usage"
	"github.com/huan/huan-agent/internal/viking"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/term"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/langfuse"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/metrics"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/server"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tracing"
	"github.com/huan/huan-agent/internal/version"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

// The built-in recorder is both halves of the seam: the conversation reports to
// it, and the console reads traces back from it.
var _ server.TraceReader = (*tracing.Recorder)(nil)

// buildChatDeps assembles the web-chat wiring: the tool set, a runner bound to
// the default model, and the model builder that resolves a per-session choice.
//
// The builder is backed by the store catalog, with the config registry as its
// fallback, so a provider added in 设置 → 模型管理 is usable in a conversation and
// a config-only deployment keeps working unchanged. The runner itself is only the
// fallback for a server whose per-session builder is absent; every session still
// resolves its model through the builder.
//
// It also returns the resolved default (provider, model) so /api/meta reports the
// target the chat actually uses — which is not necessarily the one named in the
// config file, because the default may come from the database.
func buildChatDeps(cfg *config.Config, tracer chat.Tracer, st store.Store,
	rec *usage.Recorder, logger *zap.Logger, vikingSvc *viking.Service,
	jobMgr *jobs.Manager) (server.ChatDeps, string, string) {

	if !cfg.Chat.Enable {
		logger.Info("web chat disabled (chat.enable = false)")
		return server.ChatDeps{}, "", ""
	}

	providers := make(map[string]llm.Provider, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		providers[name] = llm.Provider{
			Name: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model,
			DisableUsageRequest: p.DisableUsageRequest,
		}
	}
	reg := llm.NewRegistry(providers, cfg.LLM.DefaultProvider).
		WithOptions(llm.WithRetry(cfg.LLM.RetryPolicy(), logger))
	builder := server.NewCatalogModelBuilder(st, reg, server.ModelBuilderOptions{
		TTL: cfg.LLM.ModelsCacheTTL(),
		// Every model a conversation can pick retries a transient call failure,
		// so a 429 or a dropped socket costs a wait rather than the turn. The
		// builder passes it to llm.New; the registry is given the same option so
		// the config-only fallback path behaves identically.
		Retry:  cfg.LLM.RetryPolicy(),
		Logger: logger,
	})

	// The default model is resolved through the builder, not the registry: with
	// an empty config a provider added in the console is the only one there is,
	// and refusing to start the chat would leave it unusable exactly as before.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defProvider, defModel := builder.DefaultTarget(ctx)
	cancel()
	if defProvider == "" {
		// The catalog names nothing usable. The config may still declare a
		// default the registry can serve — a provider present in the config file
		// but with no catalog row, which is exactly the config-only install this
		// change must keep working — so it is tried before giving up.
		defProvider, defModel = cfg.LLM.DefaultProvider, adminDisplayModel(cfg)
	}
	if defProvider == "" {
		logger.Warn("web chat disabled: no usable model provider " +
			"(set llm.default_provider, or add a provider in 设置 → 模型管理)")
		return server.ChatDeps{}, "", ""
	}
	cm, err := builder.Build(context.Background(), defProvider, defModel)
	if err != nil {
		logger.Warn("web chat disabled: cannot build the default model", zap.Error(err))
		return server.ChatDeps{}, "", ""
	}

	// Subagents: one spawner per process, because the concurrency gate inside it
	// is what bounds how many nested runs happen at once. Built before the
	// registry so the tool can be registered into it below.
	spawner, spawnErr := spawnerFor(cfg, tracer, logger)
	if spawnErr != nil {
		logger.Warn("web chat: subagents disabled", zap.Error(spawnErr))
	}

	// Tools are shared with the chat REPL so the web UI can do what the CLI can.
	registry := tool.NewRegistry()
	if err := registerBuiltinTools(registry, cfg, st, logger, vikingSvc,
		toolSetOptions{
			Jobs:         jobMgr,
			Surface:      "web",
			SpawnAgent:   spawner,
			ResolveModel: modelResolverFor(builder, cfg),
		}); err != nil {
		logger.Warn("web chat: builtin tools unavailable", zap.Error(err))
	}

	// The workspace layer: what turns "the one directory the agent may touch"
	// into a set of named ones, each conversation remembering its own. Built
	// after the registry because it derives a per-turn registry from it — the
	// clone carries the MCP and skill tools that register into this one at
	// runtime, which is why the base has to be the same object.
	//
	// A failure here disables 设置 → 工作区 and the per-conversation switcher but
	// not the chat: a conversation then runs in whatever single workspace the
	// config names, which is how the product behaved before this existed.
	var bindings *workspaceBindings
	var wsManager *workspaces.Manager
	if mgr, err := newWorkspaceManager(cfg, st, logger); err != nil {
		logger.Warn("web chat: workspace layer disabled", zap.Error(err))
	} else {
		wsManager = mgr
		bindings = &workspaceBindings{
			logger: logger,
			mgr:    mgr,
			set:    newWorkspaceToolSet(cfg, st, logger, toolSetOptions{Jobs: jobMgr, Surface: "web"}),
			base:   registry,
		}
		// Seed before serving: it registers the configured directory when nothing
		// is registered yet and binds any conversation that has no workspace, so
		// "every conversation belongs to a workspace" holds from the first request.
		seedCtx, cancelSeed := context.WithTimeout(context.Background(), 10*time.Second)
		seed, seedErr := mgr.EnsureSeed(seedCtx)
		cancelSeed()
		if seedErr != nil {
			logger.Warn("web chat: workspace seed failed", zap.Error(seedErr))
		} else {
			logger.Info("workspaces ready",
				zap.String("default_workspace", seed.Name),
				zap.String("root", seed.Root),
				zap.Int("tools", len(registry.Names())),
			)
		}
	}

	maxSteps := cfg.Chat.MaxSteps
	if maxSteps <= 0 {
		maxSteps = chat.DefaultMaxSteps
	}
	chatModel, ok := cm.(einomodel.BaseChatModel)
	if !ok {
		logger.Warn("web chat disabled: the default model is not a chat model")
		return server.ChatDeps{}, "", ""
	}
	condenser, err := turnCondenser(cfg, chatModel, logger, defModel)
	if err != nil {
		// Not fatal: a turn with an unbounded window still works, it just costs
		// more the longer it runs.
		logger.Warn("web chat: in-turn context compression disabled", zap.Error(err))
	}
	runner, err := chat.New(chat.Config{
		Model:              chatModel,
		Tools:              registry,
		Tracer:             tracer,
		MaxSteps:           maxSteps,
		MaxParallel:        cfg.Tools.MaxParallelOr(),
		MaxTokens:          cfg.Chat.TurnMaxTokens,
		Deadline:           cfg.Chat.TurnDeadline(),
		StepRetry:          cfg.Chat.StepRetryPolicy(),
		Condenser:          condenser,
		Guard:              chatGuardFor(cfg),
		ToolResultMaxChars: toolResultCapFor(cfg),
		Logger:             logger,
	})
	if err != nil {
		logger.Warn("web chat disabled: cannot build the runner", zap.Error(err))
		return server.ChatDeps{}, "", ""
	}

	logger.Info("web chat enabled",
		zap.String("provider", defProvider),
		zap.String("model", defModel),
		zap.Int("tools", len(registry.Names())),
		zap.Int("max_steps", maxSteps),
		zap.Int("turn_max_tokens", cfg.Chat.TurnMaxTokens),
		zap.Duration("turn_deadline", cfg.Chat.TurnDeadline()),
		zap.Bool("in_turn_compression", condenser != nil),
		zap.Duration("ask_user_timeout", cfg.Chat.AskUserTimeout()),
		zap.String("approval_mode", cfg.Tools.Approval.ModeOr()),
		zap.Duration("approval_timeout", cfg.Tools.Approval.Timeout()),
	)
	return server.ChatDeps{
		Runner:     runner,
		Builder:    builder,
		Tools:      registry,
		ToolsFor:   func(ctx context.Context) (*tool.Registry, error) { return bindings.forScope(ctx) },
		Workspaces: wsManager,
		Condenser:  condenser,
		// Per-model: the condenser follows the conversation's model, so a session
		// on a big-window model is not compressed as if it were on a small one.
		CondenserFor: turnCondenserFactory(cfg, logger),
		// The in-turn loop guard and the bound on one tool result as the model
		// sees it. Both are what keep a long turn from spending its whole budget
		// exploring: see internal/chat/progress.go.
		Guard:              chatGuardFor(cfg),
		ToolResultMaxChars: toolResultCapFor(cfg),
		SystemPrompt:       cfg.Chat.SystemPrompt,
		// The same confinement the file tools use, so an uploaded attachment
		// lands somewhere the agent can read it and the workspace's
		// read-only mode and write limit apply to uploads too.
		Workspace: chatWorkspace(cfg, logger),
		Usage:     usageSink{rec: rec},
		// How long an ask_user card waits for an answer.
		AskTimeout: cfg.Chat.AskUserTimeout(),
		// The subagents this conversation delegated, for the header chip. Nil when
		// the feature is off, which is what keeps the endpoint (and the chip) absent
		// rather than permanently empty.
		Subagents:             subagentTrackerFor(cfg),
		SubagentMaxConcurrent: cfg.Subagent.MaxConcurrentOr(),
		// How long a write or exec waits for a person's decision. It is shorter
		// than the ask timeout on purpose: an approval blocks an action, and what
		// happens at zero differs — a question that times out is an answer, an
		// approval that times out is a refusal.
		ApprovalTimeout: cfg.Tools.Approval.Timeout(),
		// How one step of a turn is retried when its model call dies after it had
		// already started answering. It travels on the deps rather than being
		// baked into the runner above because every conversation gets a runner of
		// its own (one per model choice), and they must all retry the same way.
		StepRetry: cfg.Chat.StepRetryPolicy(),
		// The task plan behind 任务看板: the plan_* tools write to the per-turn
		// planner, the console renders it above the composer, and a resumed turn
		// reads it so it does not start the work over.
		PlanEnable:   cfg.Chat.Plan.Enable,
		PlanMaxTasks: cfg.Chat.Plan.MaxTasksOr(),
	}, defProvider, defModel
}

// chatWorkspace opens the workspace the web chat stores attachments in.
//
// A missing or unusable root returns nil rather than failing the whole server:
// chat still works, and only uploads are refused (with a message saying so).
func chatWorkspace(cfg *config.Config, logger *zap.Logger) *workspace.Workspace {
	root, ok := cfg.Tools.WorkspaceOrDefault()
	if !ok {
		logger.Warn("web chat: attachments disabled, no workspace root resolved")
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
		logger.Warn("web chat: attachments disabled, workspace unusable", zap.Error(err))
		return nil
	}
	if ws.ReadOnly() {
		// Worth saying out loud: a read-only workspace silently makes every
		// upload fail, and the reason is a config flag rather than a bug.
		logger.Info("web chat: workspace is read-only, attachment uploads will be refused")
	}
	return ws
}

// usageSink adapts the server's narrow recorder interface onto usage.Recorder,
// so internal/server does not have to depend on internal/usage.
type usageSink struct{ rec *usage.Recorder }

// Record forwards one turn's usage, ignoring a nil recorder so the chat works
// in a deployment that never started one.
func (u usageSink) Record(e server.UsageEvent) error {
	if u.rec == nil {
		return nil
	}
	return u.rec.Record(usage.Event{
		SessionID:        e.SessionID,
		Provider:         e.Provider,
		Model:            e.Model,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		TotalTokens:      e.TotalTokens,
		DurationMs:       e.DurationMs,
	})
}

var (
	adminCmd = &cobra.Command{
		Use:   "admin",
		Short: "Admin server and credential management (Phase 5)",
		Long: `Manage the admin surface: run the usage/observability HTTP server, or set
the admin password used to sign in to it.

  huan-agent admin serve --config configs/config.yaml
  huan-agent admin set-password`,
	}

	adminServeCmd = &cobra.Command{
		Use:   "serve",
		Short: "Run the admin HTTP server (usage API, metrics, web UI)",
		RunE:  runAdminServe,
	}

	adminSetPasswordCmd = &cobra.Command{
		Use:   "set-password",
		Short: "Hash an admin password and print the config value for it",
		Long: `Read a password from the terminal (or --password / stdin) and print the
bcrypt hash to put in admin.password_hash.

The password is read without echo when the terminal supports it.`,
		RunE: runAdminSetPassword,
	}
)

// adminPasswordFlag allows non-interactive use (CI, scripts). The terminal
// prompt is preferred because a flag value is visible in the process list.
var adminPasswordFlag string

func init() {
	adminServeCmd.Flags().Bool("with-feishu", false, "also run the Feishu bot in the same process")
	adminSetPasswordCmd.Flags().StringVar(&adminPasswordFlag, "password", "", "password to hash (prefer the interactive prompt)")
	adminCmd.AddCommand(adminServeCmd, adminSetPasswordCmd)
	rootCmd.AddCommand(adminCmd)
}

// runAdminServe starts the admin HTTP server.
func runAdminServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	if cfg.Database.Path == "" {
		return errors.New("database.path is empty; the admin server needs the usage store")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir database dir: %w", err)
	}
	st, err := store.Open(cmd.Context(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// Observability is best-effort: a metrics failure must not stop the server.
	m, err := metrics.New()
	if err != nil {
		logger.Warn("init metrics failed; continuing without them", zap.Error(err))
	}
	// Always publish build info so /metrics is never empty on a fresh process.
	m.SetBuildInfo(version.String())

	// Tracing is optional and must never stop the admin server from running.
	lf := langfuse.New(langfuse.Config{
		Host:        cfg.Langfuse.Host,
		PublicKey:   cfg.Langfuse.PublicKey,
		SecretKey:   cfg.Langfuse.SecretKey,
		Environment: cfg.Langfuse.Environment,
		Release:     cfg.Langfuse.Release,
	}, logger)
	if !cfg.Langfuse.Enable {
		lf = langfuse.New(langfuse.Config{}, logger) // explicitly disabled
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lf.Close(closeCtx)
	}()
	if lf.Enabled() {
		logger.Info("langfuse tracing enabled", zap.String("host", cfg.Langfuse.Host))
	} else if cfg.Langfuse.Enable {
		logger.Warn("langfuse.enable is set but the host or API keys are missing; tracing stays off")
	}
	// Tracing is built into the product: turns are recorded in the same SQLite
	// file as the conversations they belong to, so the 链路追踪 panel works on a
	// fresh install with no external service, no network and no account.
	//
	// A configured Langfuse still receives the same turns, as a mirror rather
	// than the source of truth. The local recorder is first in the fan-out so it
	// mints the ids every backend then shares.
	rec := tracing.NewRecorder(st, logger)
	// A nil *langfuse.Client is inert, so an unconfigured mirror costs nothing.
	mirror := langfuse.NewTracer(lf)
	tracer := tracing.Multi{rec, mirror}
	if rec.Enabled() {
		logger.Info("trace store enabled", zap.String("database", cfg.Database.Path),
			zap.Bool("langfuse_mirror", mirror.Enabled()))
	}

	// Seed the LLM catalog from the config file, so providers declared there
	// appear in the console's model management alongside any added at runtime.
	// A failure is not fatal: the config providers would be missing from the UI,
	// but chat still works from the config, and failing to start would be worse.
	if err := llm.SyncConfigProviders(cmd.Context(), st, cfg.LLM.Providers, cfg.LLM.DefaultProvider); err != nil {
		logger.Warn("sync config providers failed", zap.Error(err))
	}

	// Mirror mcp.servers into the store for the same reason: 设置 → MCP lists
	// what the process will connect, and a config-declared server must be
	// visible (and toggleable) there. A row is re-applied from the file at every
	// start, which is why the API refuses to edit one beyond its enabled flag.
	if n, err := server.SyncConfigServers(cmd.Context(), st, cfg.MCP.Servers, logger); err != nil {
		logger.Warn("sync config mcp servers failed", zap.Error(err))
	} else if n > 0 {
		logger.Info("config mcp servers synced", zap.Int("servers", n))
	}

	// Usage accounting for web-chat turns. Without it the analytics count only
	// CLI and Feishu traffic, and 统计监控 looks empty to a user who works in the
	// browser.
	recorder := usage.NewRecorder(st, logger, 256)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = recorder.Close(closeCtx)
	}()

	// OpenViking (context database). A failure to build it is fatal: an
	// operator who configured it wants to hear about a bad URL at startup
	// rather than discover later that memory only ever landed locally.
	vikingSvc, err := newVikingService(cfg, logger)
	if err != nil {
		return err
	}
	if vikingSvc != nil {
		defer vikingSvc.Close()
		if !cfg.OpenViking.MCP.Register {
			logger.Warn("openviking.mcp.register is false: the model will not get OpenViking's own tools " +
				"(the memory mirror and document sync still run)")
		}
	}

	// Background processes. A failure here is not fatal — the agent keeps working
	// with the background tools withheld — because a deployment that cannot write
	// a log directory should still be able to answer a question.
	jobMgr, jobErr := newJobManager(cfg, logger)
	if jobErr != nil {
		logger.Warn("background jobs disabled", zap.Error(jobErr))
	}
	// Jobs are terminated when this process exits: nothing is left running with
	// no one tracking it.
	defer jobMgr.Close()

	// The chat feature needs a model builder and a tool set.
	chatDeps, chatProvider, chatModel := buildChatDeps(cfg, tracer, st, recorder, logger, vikingSvc, jobMgr)

	// /api/meta reports where a conversation actually runs. The config file is
	// the answer when it names a provider; otherwise it is the model the catalog
	// resolved, so 服务信息 and the composer's picker agree.
	metaProvider, metaModel := cfg.LLM.DefaultProvider, adminDisplayModel(cfg)
	if metaProvider == "" {
		metaProvider, metaModel = chatProvider, chatModel
	}

	// What 设置 → 对话预算 reports about the in-turn window. It is resolved here
	// because the resolution needs the model table and the config, and the server
	// should not have to know either: it reports the number and where it came
	// from.
	contextCap, contextAuto, contextModel := contextBudgetForPanel(cfg, metaModel)

	srv, err := server.New(server.Config{
		Host:                    cfg.Server.Host,
		Port:                    cfg.Server.Port,
		SessionTTL:              cfg.Server.SessionTTL(),
		MetricsPath:             cfg.Server.MetricsPath,
		MetricsEnable:           cfg.Server.MetricsEnable,
		Metrics:                 m,
		Version:                 version.String(),
		Provider:                metaProvider,
		Model:                   metaModel,
		FeishuEnabled:           cfg.Feishu.Enabled(),
		SkillsDir:               cfg.Skills.Dir,
		StatePath:               server.StatePathFor(cfg.Database.Path),
		DefaultChatMaxSteps:     cfg.Chat.MaxSteps,
		DefaultChatMaxTokens:    cfg.Chat.TurnMaxTokens,
		DefaultChatTurnDeadline: cfg.Chat.TurnDeadline(),
		// The in-turn window the condenser resolves to, so 设置 → 对话预算 can say
		// what it is and where it came from — and can warn when compression is off
		// entirely, which is the combination that turns a long task into a
		// context-limit error partway through.
		ContextMaxTokens: contextCap,
		ContextAuto:      contextAuto,
		ContextModel:     contextModel,
		ChatHistoryLimit: cfg.Chat.HistoryLimitOr(),
		ChatEnable:       cfg.Chat.Enable,
		Chat:             chatDeps,
		Jobs:             jobMgr,
		OpenViking:       vikingConsole(vikingSvc),
		Tracer:           tracer,
		Traces:           rec,
		// Checkpoints: where they live, and how many turns to keep. The
		// per-workspace checkpointer is built by the server, because this process
		// serves several workspaces.
		Checkpoints: server.CheckpointSettings{
			Enable:     cfg.Tools.Checkpoint.Enable,
			Dir:        checkpointDir(cfg),
			KeepTurns:  cfg.Tools.Checkpoint.KeepTurnsOr(),
			MaxTotalMB: cfg.Tools.Checkpoint.MaxTotalMBOr(),
		},
		Logger: logger,
	}, st, pricingTable(cfg), cfg.Admin)
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	// Refresh stale model lists in the background: a provider's /models call can
	// take seconds or time out, and startup must not wait for it. The pass is
	// bounded, bounded in concurrency, and records each failure on the provider,
	// so a slow or broken provider delays nothing and stops nothing.
	//
	// It runs before the server starts listening on purpose — the goroutine is
	// launched, not awaited — so the first console render already sees a
	// refreshing provider rather than an empty list.
	if cfg.LLM.AutoRefreshModels {
		ttl := cfg.LLM.ModelsCacheTTL()
		go func() {
			results := server.RefreshStaleModels(ctx, st, logger, ttl)
			if len(results) == 0 {
				return
			}
			failed := 0
			for _, r := range results {
				if !r.OK {
					failed++
				}
			}
			logger.Info("model auto-refresh finished",
				zap.Int("providers", len(results)),
				zap.Int("failed", failed),
			)
		}()
	} else {
		logger.Info("model auto-refresh disabled (llm.auto_refresh_models = false)")
	}

	// Connect the configured MCP servers in the background. Spawning a stdio
	// server or dialing a remote one takes as long as it takes, and startup must
	// not wait for it; a server that is not ready when the console opens simply
	// shows as 未连接 with its error, and 重连 retries it.
	go func() {
		status := srv.SyncMCP(ctx)
		connected, failed := 0, 0
		for _, st := range status {
			if st.Connected {
				connected++
				continue
			}
			failed++
		}
		if len(status) > 0 {
			logger.Info("mcp servers synced",
				zap.Int("connected", connected), zap.Int("failed", failed))
		}
	}()

	logger.Info("admin server starting",
		zap.String("addr", srv.Addr()),
		zap.String("version", version.String()),
	)
	return srv.Start(ctx)
}

// pricingTable builds the price table from config.
func pricingTable(cfg *config.Config) *pricing.Table {
	models := make(map[string]pricing.Rate, len(cfg.Pricing.Models))
	for k, v := range cfg.Pricing.Models {
		models[k] = pricing.Rate{PromptPer1K: v.PromptPer1K, CompletionPer1K: v.CompletionPer1K}
	}
	return pricing.NewTable(models, pricing.Rate{
		PromptPer1K:     cfg.Pricing.Fallback.PromptPer1K,
		CompletionPer1K: cfg.Pricing.Fallback.CompletionPer1K,
	})
}

// adminDisplayModel returns the model configured for the default provider, for
// display in the admin UI. It is separate from the chat helpers because the
// admin server reports configuration, not a resolved client.
func adminDisplayModel(cfg *config.Config) string {
	p, ok := cfg.LLM.Providers[cfg.LLM.DefaultProvider]
	if !ok {
		return ""
	}
	return p.Model
}

// runAdminSetPassword hashes a password and prints the config snippet.
func runAdminSetPassword(cmd *cobra.Command, _ []string) error {
	pw := adminPasswordFlag
	if pw == "" {
		var err error
		pw, err = readPassword(cmd)
		if err != nil {
			return err
		}
	}
	hash, err := server.HashPassword(pw)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Add this to your config file:")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintf(cmd.OutOrStdout(), "admin:\n  username: %q\n  password_hash: %q\n",
		cfg.Admin.Username, hash)
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Or set it without editing the file:")
	fmt.Fprintf(cmd.OutOrStdout(), "  export HUAN_ADMIN_PASSWORD_HASH=%s\n", shellQuote(hash))
	return nil
}

// readPassword prompts twice and requires the entries to match.
func readPassword(cmd *cobra.Command) (string, error) {
	out := cmd.OutOrStdout()
	fd := int(os.Stdin.Fd())

	if !term.IsTerminal(fd) {
		// Non-interactive: read one line from stdin.
		var line string
		if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return "", errors.New("password must not be empty")
		}
		return line, nil
	}

	fmt.Fprint(out, "New admin password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(out, "Repeat password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	if strings.TrimSpace(string(first)) == "" {
		return "", errors.New("password must not be empty")
	}
	return string(first), nil
}

// shellQuote wraps a value in single quotes for safe pasting into a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// signalContext returns a context cancelled on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
