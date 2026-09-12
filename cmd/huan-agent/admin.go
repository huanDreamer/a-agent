package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/huan/huan-agent/internal/usage"
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
	"github.com/huan/huan-agent/internal/langfuse"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/metrics"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/server"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/version"
	"github.com/huan/huan-agent/internal/workspace"
)

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
func buildChatDeps(cfg *config.Config, tracer *langfuse.Tracer, st store.Store,
	rec *usage.Recorder, logger *zap.Logger) (server.ChatDeps, string, string) {

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
	reg := llm.NewRegistry(providers, cfg.LLM.DefaultProvider)
	builder := server.NewCatalogModelBuilder(st, reg, server.ModelBuilderOptions{
		TTL:    cfg.LLM.ModelsCacheTTL(),
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

	// Tools are shared with the chat REPL so the web UI can do what the CLI can.
	registry := tool.NewRegistry()
	if err := registerBuiltinTools(registry, cfg, st, logger); err != nil {
		logger.Warn("web chat: builtin tools unavailable", zap.Error(err))
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
	runner, err := chat.New(chat.Config{
		Model:    chatModel,
		Tools:    registry,
		Tracer:   tracer,
		MaxSteps: maxSteps,
		Logger:   logger,
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
	)
	return server.ChatDeps{
		Runner:       runner,
		Builder:      builder,
		Tools:        registry,
		SystemPrompt: cfg.Chat.SystemPrompt,
		// The same confinement the file tools use, so an uploaded attachment
		// lands somewhere the agent can read it and the workspace's
		// read-only mode and write limit apply to uploads too.
		Workspace: chatWorkspace(cfg, logger),
		Usage:     usageSink{rec: rec},
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
	tracer := langfuse.NewTracer(lf)

	// Seed the LLM catalog from the config file, so providers declared there
	// appear in the console's model management alongside any added at runtime.
	// A failure is not fatal: the config providers would be missing from the UI,
	// but chat still works from the config, and failing to start would be worse.
	if err := llm.SyncConfigProviders(cmd.Context(), st, cfg.LLM.Providers, cfg.LLM.DefaultProvider); err != nil {
		logger.Warn("sync config providers failed", zap.Error(err))
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

	// The chat feature needs a model builder and a tool set.
	chatDeps, chatProvider, chatModel := buildChatDeps(cfg, tracer, st, recorder, logger)

	// /api/meta reports where a conversation actually runs. The config file is
	// the answer when it names a provider; otherwise it is the model the catalog
	// resolved, so 服务信息 and the composer's picker agree.
	metaProvider, metaModel := cfg.LLM.DefaultProvider, adminDisplayModel(cfg)
	if metaProvider == "" {
		metaProvider, metaModel = chatProvider, chatModel
	}

	srv, err := server.New(server.Config{
		Host:             cfg.Server.Host,
		Port:             cfg.Server.Port,
		SessionTTL:       cfg.Server.SessionTTL(),
		MetricsPath:      cfg.Server.MetricsPath,
		MetricsEnable:    cfg.Server.MetricsEnable,
		Metrics:          m,
		Version:          version.String(),
		Provider:         metaProvider,
		Model:            metaModel,
		FeishuEnabled:    cfg.Feishu.Enabled(),
		SkillsDir:        cfg.Skills.Dir,
		StatePath:        server.StatePathFor(cfg.Database.Path),
		ChatMaxSteps:     cfg.Chat.MaxSteps,
		ChatHistoryLimit: cfg.Chat.HistoryLimitOr(),
		ChatEnable:       cfg.Chat.Enable,
		Chat:             chatDeps,
		Tracer:           tracer,
		Traces:           tracer,
		Logger:           logger,
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
