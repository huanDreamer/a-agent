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

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/memory"
	"github.com/huan/huan-agent/internal/metrics"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/platform/feishu"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/usage"
	"github.com/huan/huan-agent/internal/workspaces"
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
	cfg        *config.Config
	logger     *zap.Logger
	cm         model.BaseChatModel
	provider   string
	model      string
	st         store.Store
	recorder   *usage.Recorder
	sender     feishu.Sender
	downloader feishu.ResourceDownloader
	// memStore is the process-wide long-term memory (the OpenViking mirror when
	// that is enabled). It is shared by every Feishu session because the mirror
	// batches turns across sessions.
	memStore memory.Store
	// metrics may be nil when observability is disabled.
	metrics *metrics.Metrics

	// wsMgr is the workspace layer (nil when it is unavailable or switched off).
	// The manager owns which workspace each sender is in, and the runner's
	// ToolsFor closure turns that into the tool set for one turn.
	wsMgr *workspaces.Manager
	// runner runs a turn through the tool-calling loop. Nil means the bot
	// answers with a plain chat model, which is what a deployment with
	// feishu.enable_tools = false gets — and what this bot did before workspaces
	// and tools existed.
	runner *chat.Runner

	mu       sync.Mutex
	sessions map[string]*botSession
}

// botSession is one user's conversation state.
type botSession struct {
	sync.Mutex
	sid string
	mem *sessionMemory
}

// newBotHandler builds the handler; sender is set by the caller after wiring.
func newBotHandler(cfg *config.Config, logger *zap.Logger, cm model.BaseChatModel,
	provider, modelName string, st store.Store, rec *usage.Recorder, memStore memory.Store) *botHandler {
	return &botHandler{
		cfg:      cfg,
		logger:   logger,
		cm:       cm,
		provider: provider,
		model:    modelName,
		st:       st,
		recorder: rec,
		memStore: memStore,
		sessions: make(map[string]*botSession),
	}
}

func (h *botHandler) Handle(ctx context.Context, in feishu.Inbound) error {
	if !in.Handled() {
		return nil
	}
	h.logger.Info("bot handler received",
		zap.String("open_id", in.OpenID),
		zap.String("chat_id", in.ChatID),
		zap.String("msg_type", in.MsgType),
		zap.Int("text_len", len(in.Text)),
		zap.Int("resources", len(in.Resources)),
	)

	// Slash commands are only meaningful for plain text messages.
	if in.IsText() {
		text := strings.TrimSpace(in.Text)
		lower := strings.ToLower(text)
		switch lower {
		case "/help":
			return h.reply(ctx, in, h.feishuHelpText())
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
		// The explicit commands first: they mean exactly one thing, and they
		// must not be at the mercy of prose interpretation.
		if handled, err := h.handleWorkspaceCommand(ctx, in, text); handled {
			return err
		}
		// Then the natural-language form, which is deliberately conservative:
		// see parseWorkspaceIntent.
		switch intent := h.parseWorkspaceIntent(ctx, in, text); intent.Kind {
		case workspaces.IntentList:
			return h.replyWorkspaceList(ctx, in)
		case workspaces.IntentCurrent:
			return h.replyCurrentWorkspace(ctx, in)
		case workspaces.IntentSwitch:
			return h.switchWorkspace(ctx, in, intent.Name)
		}
	}

	prompt := h.buildPrompt(ctx, in)
	if prompt == "" {
		h.logger.Info("feishu: nothing to answer", zap.String("msg_type", in.MsgType))
		return nil
	}
	return h.answer(ctx, in, prompt)
}

// buildPrompt renders the inbound message as the model prompt, downloading any
// attachments first and appending their local paths so the agent can reason
// about them. Download failures degrade to a note rather than an error: the
// user still gets an answer about the parts that worked.
func (h *botHandler) buildPrompt(ctx context.Context, in feishu.Inbound) string {
	base := strings.TrimSpace(in.PromptText())
	if !in.HasResources() || h.downloader == nil {
		if in.HasResources() && h.downloader == nil {
			h.logger.Warn("feishu: attachments present but no downloader configured")
		}
		return base
	}

	notes := make([]string, 0, len(in.Resources))
	for _, r := range in.Resources {
		path, err := h.downloader.Download(ctx, in.MessageID, r)
		if err != nil {
			h.logger.Warn("feishu: download attachment failed",
				zap.String("kind", string(r.Kind)), zap.Error(err))
			notes = append(notes, fmt.Sprintf("（%s 附件下载失败）", r.Kind))
			continue
		}
		h.logger.Info("feishu: attachment saved",
			zap.String("kind", string(r.Kind)), zap.String("path", path))
		if r.Name != "" {
			notes = append(notes, fmt.Sprintf("（附件 %s 已保存到 %s）", r.Name, path))
		} else {
			notes = append(notes, fmt.Sprintf("（附件已保存到 %s）", path))
		}
	}
	if len(notes) == 0 {
		return base
	}
	if base == "" {
		return strings.Join(notes, "\n")
	}
	return base + "\n" + strings.Join(notes, "\n")
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
	mem, err := newSessionMemory(h.cfg, h.cm, h.systemPrompt(), sid, h.logger, h.memStore)
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
	sess.mem.addUserMessage(text)
	history, err := sess.mem.history()
	sess.Unlock()
	if err != nil {
		return h.reply(ctx, in, "上下文错误: "+err.Error())
	}

	// Strip any leftover tool-call metadata from the assembled history before
	// sending to the model. memory can persist an assistant message that still
	// carries ToolCalls; without a paired tool result the OpenAI/DeepSeek
	// protocol rejects it with `missing field tool_call_id`. The tool-calling
	// runner below pairs them itself, but this history comes from memory, where
	// only the text was kept.
	history = sanitizeBotHistory(history)

	// Announce progress first: the model call can take many seconds and Feishu
	// has no streaming, so a placeholder card is replaced with the answer once
	// it is ready. Failure to send it is not fatal — we fall back to replying.
	placeholderID := h.announce(ctx, in)

	// With tools on, the turn runs through the same tool-calling loop the web
	// chat uses. That is what lets the bot actually work in a workspace, and it
	// is also what makes the earlier bypass unnecessary: the runner emits a
	// paired tool result for every assistant tool_call, so the OpenAI/DeepSeek
	// protocol is satisfied — the problem the plain-model path was avoiding.
	if h.runner != nil {
		return h.answerWithTools(ctx, in, sess, history, placeholderID)
	}

	var out *schema.Message
	started := time.Now()
	out, err = h.cm.Generate(ctx, history)
	h.observeLLM(started, out, err)
	if err != nil {
		h.logger.Error("generate", zap.Error(err))
		sess.Lock()
		sess.mem.buffer.Reset()
		sess.Unlock()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return h.finish(ctx, in, placeholderID, "抱歉，出错了: "+err.Error())
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return h.finish(ctx, in, placeholderID, "(empty response)")
	}
	sess.Lock()
	sess.mem.addAssistantMessage(out)
	sess.Unlock()
	return h.finish(ctx, in, placeholderID, out.Content)
}

// observeLLM records one model call in the metrics registry. Token counts come
// from the response when the provider reports them.
func (h *botHandler) observeLLM(started time.Time, out *schema.Message, err error) {
	if h.metrics == nil {
		return
	}
	var prompt, completion int
	if out != nil && out.ResponseMeta != nil && out.ResponseMeta.Usage != nil {
		prompt = out.ResponseMeta.Usage.PromptTokens
		completion = out.ResponseMeta.Usage.CompletionTokens
	}
	h.metrics.ObserveLLMCall(h.provider, h.model, time.Since(started), err, prompt, completion)
}

// announce sends the "thinking…" placeholder card and returns its id, or "" when
// there is nothing to replace later. A failure to send it is not fatal: the
// answer is delivered as a fresh message instead.
func (h *botHandler) announce(ctx context.Context, in feishu.Inbound) string {
	if !h.cfg.Feishu.Thinking || h.sender == nil {
		return ""
	}
	id, err := h.sender.SendCard(ctx, in.ChatID, "huan-agent", thinkingPlaceholder)
	if err != nil {
		h.logger.Warn("feishu: send thinking placeholder failed", zap.Error(err))
		return ""
	}
	return id
}

// answerWithTools runs one turn through the tool-calling loop, in the workspace
// the sender has selected.
//
// Feishu has no streaming, so the events are consumed rather than forwarded: the
// answer and the tool activity are collected and delivered as one card when the
// turn ends. Tool activity is summarised rather than omitted — a user watching a
// bot edit files on their machine deserves to see that it happened, and which
// workspace it happened in.
func (h *botHandler) answerWithTools(ctx context.Context, in feishu.Inbound, sess *botSession,
	history []*schema.Message, placeholderID string) error {

	scope := scopeFor(in)
	workspaceName := ""
	if h.wsMgr != nil {
		if spec, err := h.wsMgr.Active(ctx, scope); err == nil {
			workspaceName = spec.Name
		}
	}

	started := time.Now()
	res, err := h.runner.Run(ctx, chat.Request{
		Messages:  history,
		SessionID: sess.sid,
		UserID:    in.OpenID,
		Scope:     scope,
	}, func(chat.Event) {})
	h.observeLLMTurn(started, res, err)

	if err != nil {
		h.logger.Error("feishu: agent turn failed", zap.Error(err))
		sess.Lock()
		sess.mem.buffer.Reset()
		sess.Unlock()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return h.finish(ctx, in, placeholderID, "抱歉，出错了: "+err.Error())
	}
	if res == nil {
		return h.finish(ctx, in, placeholderID, "(empty response)")
	}

	// Audit the tool calls, the same way the web chat does, so 审计日志 shows
	// Feishu activity with the workspace it happened in.
	h.recordToolAudit(ctx, sess.sid, in.OpenID, workspaceName, res.Tools)

	// A turn that stopped on its budget is said out loud here too: the note is
	// already in the answer text the user sees, but the log is where an operator
	// notices that the bot keeps running out of steps on the same kind of task.
	if res.BudgetExhausted() {
		h.logger.Warn("feishu: turn stopped on its budget",
			zap.String("reason", res.StopReason),
			zap.Int("steps", res.Steps),
			zap.Int("tokens", res.Usage.TotalTokens),
			zap.Int("tools", len(res.Tools)),
			zap.String("session", sess.sid),
		)
	}

	if strings.TrimSpace(res.Text) == "" && len(res.Tools) == 0 {
		return h.finish(ctx, in, placeholderID, "(empty response)")
	}
	sess.Lock()
	sess.mem.addAssistantMessage(&schema.Message{Role: schema.Assistant, Content: res.Text})
	sess.Unlock()

	return h.finish(ctx, in, placeholderID, res.Text+toolSummary(res.Tools, workspaceName))
}

// recordToolAudit writes one row per tool call, with the workspace it ran in.
func (h *botHandler) recordToolAudit(ctx context.Context, sessionID, userID, workspaceName string,
	runs []chat.ToolRun) {
	if h.st == nil {
		return
	}
	for _, run := range runs {
		if err := h.st.RecordInvocation(ctx, store.InvocationEvent{
			SessionID:  sessionID,
			UserID:     userID,
			ToolName:   run.Name,
			Arguments:  run.Args,
			Result:     run.Result,
			Err:        run.Err,
			Workspace:  workspaceName,
			DurationMs: run.DurationMs,
		}); err != nil {
			h.logger.Warn("feishu: persist tool invocation failed", zap.Error(err))
		}
	}
}

// toolSummary renders the small footer that says what the agent did, in which
// workspace. It is empty when no tool ran, so an ordinary answer stays clean.
func toolSummary(runs []chat.ToolRun, workspaceName string) string {
	if len(runs) == 0 {
		return ""
	}
	counts := map[string]int{}
	var order []string
	failed := 0
	for _, run := range runs {
		if _, seen := counts[run.Name]; !seen {
			order = append(order, run.Name)
		}
		counts[run.Name]++
		if run.Err != "" {
			failed++
		}
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if counts[name] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
			continue
		}
		parts = append(parts, name)
	}
	line := fmt.Sprintf("\n\n---\n🛠 工具 %d 次（%s）", len(runs), strings.Join(parts, "、"))
	if failed > 0 {
		line += fmt.Sprintf("，其中 %d 次失败", failed)
	}
	if workspaceName != "" {
		line += " · 工作区 `" + workspaceName + "`"
	}
	return line
}

// observeLLMTurn records one agent turn in the metrics registry. A turn is
// several model calls, but the metric is about the turn the user asked for.
func (h *botHandler) observeLLMTurn(started time.Time, res *chat.Result, err error) {
	if h.metrics == nil {
		return
	}
	var prompt, completion int
	if res != nil {
		prompt, completion = res.Usage.PromptTokens, res.Usage.CompletionTokens
	}
	h.metrics.ObserveLLMCall(h.provider, h.model, time.Since(started), err, prompt, completion)
}

// thinkingPlaceholder is shown while the model is generating.
const thinkingPlaceholder = "🤔 正在思考…"

// finish delivers the final answer. When a placeholder card was sent it is
// updated in place (so the chat does not accumulate two messages); otherwise a
// fresh reply is sent. A failed update also falls back to a new message so the
// user always receives the answer.
func (h *botHandler) finish(ctx context.Context, in feishu.Inbound, placeholderID, text string) error {
	// Render first: an answer containing a table too large for one card becomes
	// several messages, each carrying a native table that the platform can
	// actually render.
	msgs := feishu.MarkdownToCardMessages(text, "", feishu.DefaultTableLimits())
	if err := h.deliver(ctx, in, placeholderID, msgs); err == nil {
		return nil
	} else {
		h.logger.Warn("feishu: delivering the answer card failed; retrying without native tables",
			zap.Error(err))
	}

	// The platform may reject a native table component. Retry with the
	// maximum-compatibility rendering: tables become markdown text inside a
	// single card built only from long-established card features.
	compat := feishu.MarkdownToCardMessagesCompat(text, "")
	if err := h.deliver(ctx, in, placeholderID, compat); err == nil {
		return nil
	}

	// Last resort: a plain text reply still delivers the answer.
	h.logger.Warn("feishu: compatibility card also failed; falling back to plain text")
	return h.reply(ctx, in, text)
}

// deliver sends the rendered messages: the first replaces the placeholder (or
// is sent fresh) and any continuations follow as new messages. It returns an
// error only when the first message could not be delivered at all.
func (h *botHandler) deliver(ctx context.Context, in feishu.Inbound, placeholderID string, msgs []feishu.CardMessage) error {
	if len(msgs) == 0 {
		return errors.New("feishu: no card to deliver")
	}
	first := msgs[0]

	if placeholderID != "" {
		if err := h.sender.UpdateCardElements(ctx, placeholderID, first.Title, first.Elements); err == nil {
			h.sendContinuations(ctx, in, msgs[1:])
			return nil
		} else {
			h.logger.Warn("feishu: update placeholder card failed, sending a new message",
				zap.Error(err))
		}
	}
	if _, err := h.sender.SendCardElements(ctx, in.ChatID, first.Title, first.Elements); err != nil {
		return err
	}
	h.sendContinuations(ctx, in, msgs[1:])
	return nil
}

// sendContinuations delivers the extra messages produced when a table had to be
// split. A failure stops the sequence: if one chunk cannot be sent, the
// remaining chunks would be out of context.
func (h *botHandler) sendContinuations(ctx context.Context, in feishu.Inbound, rest []feishu.CardMessage) error {
	for i, m := range rest {
		if _, err := h.sender.SendCardElements(ctx, in.ChatID, m.Title, m.Elements); err != nil {
			h.logger.Warn("feishu: sending a table continuation failed",
				zap.Int("index", i+1), zap.Error(err))
			return nil // the answer is already partly delivered; do not error
		}
	}
	return nil
}

// systemPrompt returns the default system prompt.
func (h *botHandler) systemPrompt() string {
	return "You are huan-agent, a helpful personal AI assistant. Answer concisely. " +
		"Commands run with no terminal and with standard input at /dev/null, so use the " +
		"non-interactive flag a tool offers (-y, --yes, --no-input, CI=1) instead of a " +
		"command that waits for input."
}

// reply sends text back to the user.
func (h *botHandler) reply(ctx context.Context, in feishu.Inbound, text string) error {
	if h.sender == nil {
		return errors.New("feishu: sender not set")
	}
	return h.sender.ReplyText(ctx, in, text)
}

// replyCard sends the answer as an interactive card, so markdown (code blocks,
// lists, tables) renders properly. The title is derived from the content.
func (h *botHandler) replyCard(ctx context.Context, in feishu.Inbound, markdown string) error {
	if h.sender == nil {
		return errors.New("feishu: sender not set")
	}
	return h.sender.ReplyCard(ctx, in, "", markdown)
}

// sanitizeBotHistory removes tool-only message fragments and clears stale
// tool-call metadata so a conversational model (DeepSeek/OpenAI protocol)
// does not reject the history with "missing field tool_call_id". It returns a
// new slice; the input is not mutated.
func sanitizeBotHistory(history []*schema.Message) []*schema.Message {
	out := history[:0:0] // fresh backing array
	for _, m := range history {
		if m == nil {
			continue
		}
		// Tool result messages have no conversational value here and, if the
		// matching assistant tool_call is not present, break the protocol.
		if m.Role == schema.Tool {
			continue
		}
		cp := *m
		// An assistant message that only announced tool calls carries no
		// content for this bot; drop it entirely (no paired tool result).
		if cp.Role == schema.Assistant && len(cp.ToolCalls) > 0 && cp.Content == "" {
			continue
		}
		// Clear leftover tool metadata regardless of role; only plaintext is sent.
		cp.ToolCalls = nil
		cp.ToolCallID = ""
		out = append(out, &cp)
	}
	return out
}

// runServe is the serve entrypoint.
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
		providers[name] = llm.Provider{
			Name: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model,
			DisableUsageRequest: p.DisableUsageRequest,
		}
	}
	reg := llm.NewRegistry(providers, cfg.LLM.DefaultProvider)
	cm, err := reg.Get(cfg.LLM.DefaultProvider)
	if err != nil {
		return err
	}

	// Build the outbound primitives, then wrap the create step with the
	// configured resilience layers (rate limit -> retry -> per-attempt
	// timeout) so a transient Feishu hiccup does not silently drop a reply.
	create, patch, recall, err := feishu.RealSenderFuncs(fsApp.AppID, fsApp.AppSecret, fsApp.Domain)
	if err != nil {
		return err
	}
	limiter := feishu.NewLimiter(cfg.Feishu.RateLimitPerSec, cfg.Feishu.RateLimitBurst)
	retryPolicy := feishu.RetryPolicy{
		MaxAttempts: cfg.Feishu.RetryAttempts,
		BaseDelay:   cfg.Feishu.RetryBaseDelay(),
		Jitter:      true,
	}
	create = feishu.WrapCreate(create, retryPolicy, limiter, cfg.Feishu.SendTimeout(), logger)
	sender := feishu.NewLarkSenderWithCreate(create,
		feishu.WithPatcher(patch), feishu.WithRecaller(recall))

	// Inbound attachments are downloaded to a local directory so the agent can
	// read them.
	downloadDir := cfg.Feishu.ResolveDownloadDir()
	downloader, err := feishu.RealDownloader(fsApp.AppID, fsApp.AppSecret, fsApp.Domain, downloadDir)
	if err != nil {
		return fmt.Errorf("init feishu downloader: %w", err)
	}

	// OpenViking integration: memory mirror + document sync. A failure to build
	// it is fatal on purpose — an operator who configured it wants to know at
	// startup, not to discover later that memory was silently local-only.
	vikingSvc, err := newVikingService(cfg, logger)
	if err != nil {
		return err
	}
	if vikingSvc != nil {
		defer vikingSvc.Close()
	}
	memStore, closeMemory, err := buildMemoryStore(cfg, vikingSvc, logger)
	if err != nil {
		return err
	}
	defer closeMemory()

	handler := newBotHandler(cfg, logger, cm, cfg.LLM.DefaultProvider, cfg.LLM.DefaultProvider, st, rec, memStore)
	handler.sender = sender
	handler.downloader = downloader

	// Tools and workspaces. Off means the bot answers with a plain chat model —
	// exactly what it did before either existed — so an operator who does not
	// want an IM-reachable shell can turn it off without losing the bot.
	// Background processes. A failure to build the supervisor is not fatal: the
	// bot keeps working with the background tools withheld.
	jobMgr, jobErr := newJobManager(cfg, logger)
	if jobErr != nil {
		logger.Warn("background jobs disabled", zap.Error(jobErr))
	}
	// The bot's jobs die with the bot: a dev server started over Feishu is not
	// meant to keep running once the process supervising it is gone.
	defer jobMgr.Close()

	if cfg.Feishu.EnableTools {
		tooling, terr := buildFeishuTooling(cmd.Context(), cfg, st, logger, vikingSvc, jobMgr)
		if terr != nil {
			// Fatal on purpose: an operator who asked for tools and got none
			// would otherwise discover it from a model that apologises for
			// being unable to read a file.
			return fmt.Errorf("feishu tools: %w", terr)
		}
		defer tooling.close()

		condenser, cErr := turnCondenser(cfg, cm, logger)
		if cErr != nil {
			// Not fatal: the bot keeps answering, with a window that grows with
			// the turn instead of being condensed.
			logger.Warn("feishu: in-turn context compression disabled", zap.Error(cErr))
		}
		runner, rerr := chat.New(chat.Config{
			Model:     cm,
			Tools:     tooling.base,
			ToolsFor:  tooling.bindings.forScope,
			MaxSteps:  cfg.Chat.MaxSteps,
			MaxTokens: cfg.Chat.TurnMaxTokens,
			Deadline:  cfg.Chat.TurnDeadline(),
			Condenser: condenser,
			Logger:    logger,
		})
		if rerr != nil {
			return fmt.Errorf("feishu agent runner: %w", rerr)
		}
		handler.runner = runner
		handler.wsMgr = tooling.manager

		// Seed before the bot answers: it registers the configured directory when
		// nothing is registered yet, so a message that arrives before anything was
		// set up in the console still has somewhere to run.
		if seed, seedErr := tooling.manager.EnsureSeed(cmd.Context()); seedErr != nil {
			logger.Warn("feishu: workspace seed failed", zap.Error(seedErr))
		} else {
			logger.Info("feishu workspaces ready",
				zap.String("default_workspace", seed.Name), zap.String("root", seed.Root))
		}
		logger.Warn("飞书已获得文件/命令工具：任何能给机器人发消息的人都可以在选定工作区内读写文件并执行命令（bash 仍可访问工作区外的绝对路径）。如需关闭请设置 feishu.enable_tools: false",
			zap.Int("tools", len(tooling.base.Names())),
		)
	} else {
		logger.Info("feishu tools disabled (feishu.enable_tools = false); the bot answers from the model alone")
	}

	// Observability is best-effort: a metrics failure must not stop the bot.
	// The registry is shared with the admin server when both run together.
	m, merr := metrics.New()
	if merr != nil {
		logger.Warn("init metrics failed; continuing without them", zap.Error(merr))
	}
	handler.metrics = m

	mode, err := feishu.ParseMode(fsApp.Transport)
	if err != nil {
		return err
	}
	app, err := feishu.NewApp(feishu.Config{
		AppID:             fsApp.AppID,
		AppSecret:         fsApp.AppSecret,
		Domain:            fsApp.Domain,
		VerificationToken: fsApp.VerificationToken,
		EncryptKey:        fsApp.EncryptKey,
		Mode:              mode,
		CallbackAddr:      fsApp.CallbackAddr,
		CallbackPath:      fsApp.CallbackPath,
		Sender:            sender,
		Logger:            logger,
	}, handler)
	if err != nil {
		return fmt.Errorf("init feishu app: %w", err)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Periodic workspace sync, when configured. Off by default: a long-running
	// process re-crawling a workspace on a timer is a surprising amount of
	// embedding traffic for a feature nobody switched on.
	if vikingSvc != nil {
		if interval := cfg.OpenViking.Documents.SyncInterval(); interval > 0 {
			startWorkspaceSyncLoop(ctx, vikingSvc, interval, logger)
		}
	}
	logger.Info("feishu bot starting",
		zap.String("provider", cfg.LLM.DefaultProvider),
		zap.String("transport", string(mode)),
		zap.Bool("memory", cfg.Memory.Enable),
		zap.Bool("thinking", cfg.Feishu.Thinking),
		zap.String("download_dir", downloadDir),
		zap.Int("retry_attempts", cfg.Feishu.RetryAttempts),
		zap.Float64("rate_limit_per_sec", cfg.Feishu.RateLimitPerSec),
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
