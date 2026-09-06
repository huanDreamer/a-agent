package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/agent"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/platform/feishu"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/usage"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the Feishu (IM) bot agent service",
	Long: `Start the Feishu long-connection bot and answer IM messages with the
configured agent + memory. Requires feishu.app_id / app_secret:

  HUAN_FEISHU_APP_ID=cli_xxx HUAN_FEISHU_APP_SECRET=xxx huan-agent serve

Each private user (open_id) gets an independent session wired to the same
memory + context machinery as ` + "`huan-agent chat`" + `.
`,
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

// botHandler answers Feishu IM messages using the session memory + optional
// ReAct agent, mirroring the chat REPL loop.
type botHandler struct {
	cfg      *config.Config
	logger   *zap.Logger
	cm       model.BaseChatModel
	provider string
	model    string
	st       store.Store
	recorder *usage.Recorder
	sender   feishu.Sender

	mu       sync.Mutex
	sessions map[string]*botSession
}

// botSession is one user's conversation state.
type botSession struct {
	sync.Mutex
	sid string
	mem *sessionMemory
	ag  *agent.Agent
}

// newBotHandler builds the handler; sender is set by the caller after wiring.
func newBotHandler(cfg *config.Config, logger *zap.Logger, cm model.BaseChatModel,
	provider, modelName string, st store.Store, rec *usage.Recorder) *botHandler {
	return &botHandler{
		cfg:      cfg,
		logger:   logger,
		cm:       cm,
		provider: provider,
		model:    modelName,
		st:       st,
		recorder: rec,
		sessions: make(map[string]*botSession),
	}
}

func (h *botHandler) Handle(ctx context.Context, in feishu.Inbound) error {
	if !in.IsText() {
		return nil
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	switch lower {
	case "/help":
		return h.reply(ctx, in, "Commands:\n/reset 清空会话\n/remember key: value 记住事实\n/recall query 检索记忆\n/provider 查看模型")
	case "/provider":
		return h.reply(ctx, in, fmt.Sprintf("provider=%s model=%s", h.provider, h.model))
	case "/reset":
		h.reset(in.OpenID)
		return h.reply(ctx, in, "(会话已重置)")
	}
	if strings.HasPrefix(lower, "/remember ") {
		return h.remember(ctx, in, text[len("/remember "):])
	}
	if strings.HasPrefix(lower, "/recall ") {
		return h.recall(ctx, in, text[len("/recall "):])
	}
	return h.answer(ctx, in, text)
}

func (h *botHandler) reset(uid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[uid]; s != nil {
		s.mem.buffer.Reset()
	}
}

func (h *botHandler) remember(ctx context.Context, in feishu.Inbound, kv string) error {
	sess := h.session(in.OpenID)
	key, val, ok := strings.Cut(kv, ": ")
	if !ok {
		key, val = kv, kv
	}
	sess.Lock()
	defer sess.Unlock()
	if sess.mem == nil {
		return h.reply(ctx, in, "记忆未启用")
	}
	if err := sess.mem.addFact(key, val); err != nil {
		return h.reply(ctx, in, "无法记住: "+err.Error())
	}
	return h.reply(ctx, in, "(remembered "+key+")")
}

func (h *botHandler) recall(ctx context.Context, in feishu.Inbound, query string) error {
	sess := h.session(in.OpenID)
	sess.Lock()
	defer sess.Unlock()
	if sess.mem == nil {
		return h.reply(ctx, in, "记忆未启用")
	}
	facts, err := sess.mem.recallFacts(query, 5)
	if err != nil || len(facts) == 0 {
		return h.reply(ctx, in, "(no matching memories)")
	}
	var b strings.Builder
	for _, f := range facts {
		b.WriteString("- " + f.Content() + "\n")
	}
	return h.reply(ctx, in, strings.TrimRight(b.String(), "\n"))
}

// session returns (creating if needed) the user's session.
func (h *botHandler) session(uid string) *botSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[uid]; ok {
		return s
	}
	sid := uuid.NewString()
	mem, err := newSessionMemory(h.cfg, h.cm, h.systemPrompt(), sid, h.logger)
	if err != nil {
		h.logger.Error("new session memory", zap.Error(err))
		mem = nil
	}
	s := &botSession{sid: sid, mem: mem}
	h.sessions[uid] = s
	h.logger.Info("new feishu session", zap.String("open_id", uid), zap.String("session_id", sid))
	return s
}

// answer replies to a text message via the model or agent.
func (h *botHandler) answer(ctx context.Context, in feishu.Inbound, text string) error {
	sess := h.session(in.OpenID)
	if sess.mem == nil {
		return h.reply(ctx, in, "会话初始化失败，请稍后再试")
	}

	sess.Lock()
	if sess.ag == nil {
		sess.ag = h.buildAgent(in.OpenID, sess.sid)
	}
	ag := sess.ag
	sess.mem.addUserMessage(text)
	history, err := sess.mem.history()
	sess.Unlock()
	if err != nil {
		return h.reply(ctx, in, "上下文错误: "+err.Error())
	}

	var out *schema.Message
	if ag != nil {
		out, err = ag.Generate(ctx, history)
	} else {
		out, err = h.cm.Generate(ctx, history)
	}
	if err != nil {
		h.logger.Error("generate", zap.Error(err))
		sess.Lock()
		sess.mem.buffer.Reset()
		sess.Unlock()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return h.reply(ctx, in, "抱歉，出错了: "+err.Error())
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return h.reply(ctx, in, "(empty response)")
	}
	sess.Lock()
	sess.mem.addAssistantMessage(out)
	sess.Unlock()
	return h.reply(ctx, in, out.Content)
}

// buildAgent constructs a per-user ReAct agent (once). If the model does not
// support tool calling, nil is returned (plain generate is used instead).
func (h *botHandler) buildAgent(uid, sid string) *agent.Agent {
	tcm, ok := h.cm.(model.ToolCallingChatModel)
	if !ok {
		return nil
	}
	reg := tool.NewRegistry()
	if err := registerBuiltinTools(reg); err != nil {
		h.logger.Error("register builtin tools", zap.Error(err))
		return nil
	}
	// MCP servers are optional for a session; connection failures are logged.
	for _, s := range h.cfg.MCP.Servers {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := mcp.Connect(ctx, mcp.ServerSpec{
			Name:    s.Name,
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		})
		if err != nil {
			cancel()
			h.logger.Error("mcp connect", zap.String("name", s.Name), zap.Error(err))
			continue
		}
		if _, err := mcp.RegisterMCPTools(ctx, reg, c, h.logger); err != nil {
			h.logger.Error("register mcp tools", zap.String("name", s.Name), zap.Error(err))
		}
		cancel()
	}
	ag, err := agent.New(context.Background(), agent.Config{
		Model:       tcm,
		Tools:       reg,
		MaxSteps:    h.cfg.Agent.MaxSteps,
		Recorder:    h.recorder,
		Audit:       h.st,
		SessionIDFn: func() string { return sid },
		Logger:      h.logger,
	})
	if err != nil {
		h.logger.Error("agent new", zap.Error(err))
		return nil
	}
	_ = uid
	return ag
}

// systemPrompt returns the default system prompt.
func (h *botHandler) systemPrompt() string {
	return "You are huan-agent, a helpful personal AI assistant. Answer concisely."
}

// reply sends text back to the user.
func (h *botHandler) reply(ctx context.Context, in feishu.Inbound, text string) error {
	if h.sender == nil {
		return errors.New("feishu: sender not set")
	}
	return h.sender.ReplyText(ctx, in, text)
}

// runServe is the serve command entrypoint.
func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	// Resolve the active Feishu app (multi-app via feishu.active, or legacy
	// single app_id/app_secret).
	fsApp := cfg.Feishu.Resolve()
	if !cfg.Feishu.Enabled() {
		logger.Warn("no enabled feishu app; serve has nothing to do. Set feishu.apps.<name> with app_id/app_secret (or feishu.app_id / env HUAN_FEISHU_*).")
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir database dir: %w", err)
	}
	st, err := store.Open(cmd.Context(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	rec := usage.NewRecorder(st, logger, 256)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = rec.Close(ctx)
	}()

	if cfg.LLM.DefaultProvider == "" {
		return fmt.Errorf("no provider configured (set llm.default_provider or HUAN_LLM_DEFAULT_PROVIDER)")
	}
	providers := make(map[string]llm.Provider, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		providers[name] = llm.Provider{Name: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model}
	}
	reg := llm.NewRegistry(providers, cfg.LLM.DefaultProvider)
	cm, err := reg.Get(cfg.LLM.DefaultProvider)
	if err != nil {
		return err
	}

	sender, err := feishu.RealSender(fsApp.AppID, fsApp.AppSecret, fsApp.Domain)
	if err != nil {
		return err
	}
	handler := newBotHandler(cfg, logger, cm, cfg.LLM.DefaultProvider, cfg.LLM.DefaultProvider, st, rec)
	handler.sender = sender

	app, err := feishu.NewApp(feishu.Config{
		AppID:     fsApp.AppID,
		AppSecret: fsApp.AppSecret,
		Domain:    fsApp.Domain,
		Sender:    sender,
	}, handler)
	if err != nil {
		return fmt.Errorf("init feishu app: %w", err)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("feishu bot starting",
		zap.String("provider", cfg.LLM.DefaultProvider),
		zap.Bool("memory", cfg.Memory.Enable),
	)
	if err := app.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("feishu bot stopped")
			return nil
		}
		return fmt.Errorf("feishu bot: %w", err)
	}
	return nil
}
