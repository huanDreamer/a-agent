package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/agent"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/skill"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/usage"
	"github.com/huan/huan-agent/internal/viking"
	"github.com/huan/huan-agent/internal/workspace"
)

var (
	chatProvider string
	chatModel    string
	chatSystem   string
	chatTools    bool
	chatSkill    string
	chatCmd      = &cobra.Command{
		Use:   "chat",
		Short: "Interactive chat REPL with the configured LLM",
		Long: `Start an interactive REPL that streams responses from the configured LLM.

Examples:
  # Use the default provider/model from config
  huan-agent chat

  # Override provider and model
  huan-agent chat --provider qwen --model qwen-turbo

  # Inject a system prompt
  huan-agent chat --system "You are a helpful assistant. Answer in Chinese."

  # Enable the ReAct agent with built-in tools (time, calc, echo) and MCP tools
  huan-agent chat --tools

  # Load a skill (markdown + frontmatter) from configs/skills/<name>.md as the
  # system prompt and tool allow-list
  huan-agent chat --tools --skill daily-summary

Commands inside the REPL:
  /quit, /exit, Ctrl+D   exit the chat
  /reset                  clear conversation history
  /provider               print current provider/model
  /tools                  list tools available to the agent (when --tools is on)
`,
		RunE: runChat,
	}
)

func init() {
	chatCmd.Flags().StringVar(&chatProvider, "provider", "", "override default provider")
	chatCmd.Flags().StringVar(&chatModel, "model", "", "override default model")
	chatCmd.Flags().StringVar(&chatSystem, "system", "", "system prompt")
	chatCmd.Flags().BoolVar(&chatTools, "tools", false, "enable the ReAct agent loop with tools (built-in + MCP)")
	chatCmd.Flags().StringVar(&chatSkill, "skill", "", "load named skill from configs/skills/ as system prompt + tool allow-list (implies --tools)")
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	// Open SQLite + start the recorder.
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir database dir: %w", err)
	}
	st, err := store.Open(cmd.Context(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	recorder := usage.NewRecorder(st, logger, 256)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = recorder.Close(ctx)
	}()

	// Build the registry and resolve the model.
	providers := make(map[string]llm.Provider, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		providers[name] = llm.Provider{
			Name:                name,
			BaseURL:             p.BaseURL,
			APIKey:              p.APIKey,
			Model:               p.Model,
			DisableUsageRequest: p.DisableUsageRequest,
		}
	}
	registry := llm.NewRegistry(providers, cfg.LLM.DefaultProvider)

	chosen := chatProvider
	if chosen == "" {
		chosen = cfg.LLM.DefaultProvider
	}
	if chosen == "" {
		return fmt.Errorf("no provider configured (set llm.default_provider in config or use --provider)")
	}

	cm, err := registry.Get(chosen)
	if err != nil {
		return err
	}
	if chatModel != "" {
		prov, _ := registry.Provider(chosen)
		prov.Model = chatModel
		cm, err = llm.New(prov)
		if err != nil {
			return err
		}
	}

	// Resolve skill (loads from cfg.Skills.Dir, errors only if --skill is set).
	var chosenSkill *skill.Skill
	if chatSkill != "" {
		loader := skill.NewLoader(cfg.Skills.Dir)
		if _, lErr := loader.LoadAll(); lErr != nil {
			logger.Warn("skill load reported errors", zap.Error(lErr))
		}
		s, ok := loader.Get(chatSkill)
		if !ok {
			return fmt.Errorf("skill %q not found in %s", chatSkill, cfg.Skills.Dir)
		}
		chosenSkill = s
		chatTools = true // skills imply tool support
	}

	// Auto-enable tools when MCP servers are configured but the user did not
	// explicitly opt out.
	if !chatTools && len(cfg.MCP.Servers) > 0 {
		chatTools = true
	}

	sessionID := uuid.NewString()
	logger.Info("chat session starting",
		zap.String("session_id", sessionID),
		zap.String("provider", chosen),
		zap.String("model", effectiveModel(registry, chosen, chatModel)),
		zap.Bool("tools", chatTools),
		zap.String("skill", chatSkill),
	)

	fmt.Println(banner(chosen, effectiveModel(registry, chosen, chatModel), chatTools, chatSkill))
	fmt.Println()

	// Build the system prompt (skill or --system) and wire memory + context.
	systemPrompt := chatSystem
	if chosenSkill != nil {
		systemPrompt = chosenSkill.SystemPrompt()
	}
	// OpenViking (memory mirror + document sync), when configured.
	vikingSvc, err := newVikingService(cfg, logger)
	if err != nil {
		return err
	}
	if vikingSvc != nil {
		// Deferred after Close so it runs before it: the workspace is published
		// first, then buffered memory is flushed.
		defer vikingSvc.Close()
		if cfg.OpenViking.Documents.SyncOnExit {
			defer syncWorkspaceOnExit(vikingSvc, logger)
		}
	}
	// Long-term memory is process-scoped: with OpenViking enabled it is the
	// mirror, whose batching spans sessions.
	memStore, closeMemory, err := buildMemoryStore(cfg, vikingSvc, logger)
	if err != nil {
		return err
	}
	defer closeMemory()

	mem, err := newSessionMemory(cfg, cm, systemPrompt, sessionID, logger, memStore)
	if err != nil {
		return fmt.Errorf("init memory: %w", err)
	}
	defer mem.close()

	// Signal handler: cancels the in-flight stream when the user hits Ctrl+C.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background processes. One supervisor per process, closed when the REPL
	// exits: a dev server started in a session is meant to outlive the tool call
	// that started it, not the agent that supervises it.
	jobMgr, jobErr := newJobManager(cfg, logger)
	if jobErr != nil {
		logger.Warn("background jobs disabled", zap.Error(jobErr))
	}
	defer jobMgr.Close()

	// Build the agent if tools are enabled. Tool registry + MCP clients live
	// for the duration of the REPL.
	var (
		ag         *agent.Agent
		toolReg    *tool.Registry
		mcpClients []*mcp.Client
	)
	if chatTools {
		ag, toolReg, mcpClients, err = buildAgent(ctx, cm, cfg, chosenSkill, st, logger, sessionID, vikingSvc, jobMgr)
		if err != nil {
			return fmt.Errorf("build agent: %w", err)
		}
		defer func() {
			for _, c := range mcpClients {
				_ = c.Close()
			}
		}()
		if names := toolReg.AllowList(); len(names) > 0 {
			fmt.Println("tools available:", strings.Join(names, ", "))
			fmt.Println()
		}
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 1024*1024)

	for {
		fmt.Print("you> ")
		if !in.Scan() {
			if err := in.Err(); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "read error:", err)
			}
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case lower == "/quit" || lower == "/exit":
			return nil
		case lower == "/reset":
			mem.buffer.Reset()
			fmt.Println("(conversation reset)")
			continue
		case lower == "/provider":
			fmt.Printf("provider=%s model=%s tools=%v skill=%s memory=%v\n",
				chosen, effectiveModel(registry, chosen, chatModel), chatTools, chatSkill, cfg.Memory.Enable)
			continue
		case lower == "/tools":
			if toolReg == nil {
				fmt.Println("(tools disabled — restart with --tools to enable)")
			} else {
				fmt.Println("tools:", strings.Join(toolReg.AllowList(), ", "))
			}
			continue
		case strings.HasPrefix(lower, "/remember "):
			keyVal := strings.TrimSpace(line[len("/remember "):])
			if keyVal == "" {
				fmt.Println("usage: /remember key: value")
				continue
			}
			key, val, ok := strings.Cut(keyVal, ": ")
			if !ok {
				key, val = keyVal, keyVal
			}
			if err := mem.addFact(key, val); err != nil {
				fmt.Fprintln(os.Stderr, "remember failed:", err)
			} else {
				fmt.Printf("(remembered %q)\n", key)
			}
			continue
		case strings.HasPrefix(lower, "/recall "):
			facts, _ := mem.recallFacts(strings.TrimSpace(line[len("/recall "):]), 5)
			if len(facts) == 0 {
				fmt.Println("(no matching memories)")
			} else {
				for _, f := range facts {
					fmt.Println("- " + f.Content())
				}
			}
			continue
		}

		// Record the user message and assemble (and possibly compress) the window.
		mem.addUserMessage(line)
		history, err := mem.history()
		if err != nil {
			fmt.Fprintln(os.Stderr, "history error:", err)
			continue
		}

		var runErr error
		if ag != nil {
			runErr = agentOnce(ctx, ag, &history)
		} else {
			runErr = streamOnce(ctx, cm, registry, chosen, chatModel, &history, sessionID, recorder, logger)
		}
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "error:", runErr)
			mem.buffer.Reset() // drop the failed user turn so context stays sane
			if errors.Is(runErr, context.Canceled) {
				return nil
			}
		} else if len(history) > 0 {
			// The model/agent appended an assistant reply at the end of the
			// assembled window; sync it into short + long-term memory.
			last := history[len(history)-1]
			if last.Role == schema.Assistant {
				mem.addAssistantMessage(last)
			}
		}
		fmt.Println()
	}
}

// buildAgent constructs the tool registry (builtin + MCP), applies the
// allow-list (skill takes precedence over config), and returns a ready
// agent.Agent plus the MCP clients to close on exit.
func buildAgent(
	ctx context.Context,
	cm model.BaseChatModel,
	cfg *config.Config,
	sk *skill.Skill,
	st store.Store,
	logger *zap.Logger,
	sessionID string,
	svc *viking.Service,
	jobMgr *jobs.Manager,
) (*agent.Agent, *tool.Registry, []*mcp.Client, error) {
	tcm, ok := cm.(model.ToolCallingChatModel)
	if !ok {
		return nil, nil, nil, fmt.Errorf("provider does not support tool calling")
	}

	reg := tool.NewRegistry()
	if err := registerBuiltinTools(reg, cfg, st, logger, svc,
		toolSetOptions{Jobs: jobMgr, Surface: "cli"}); err != nil {
		return nil, nil, nil, fmt.Errorf("register builtin tools: %w", err)
	}

	var clients []*mcp.Client
	for _, s := range cfg.MCP.Servers {
		c, err := mcp.Connect(ctx, mcp.ServerSpec{
			Name:    s.Name,
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		})
		if err != nil {
			// Roll back on failure: kill the servers we already started.
			for _, prev := range clients {
				_ = prev.Close()
			}
			return nil, nil, nil, fmt.Errorf("connect mcp %s: %w", s.Name, err)
		}
		clients = append(clients, c)
		n, rErr := mcp.RegisterMCPTools(ctx, reg, c, logger)
		if rErr != nil {
			for _, prev := range clients {
				_ = prev.Close()
			}
			return nil, nil, nil, fmt.Errorf("register mcp tools %s: %w", s.Name, rErr)
		}
		logger.Info("mcp server connected", zap.String("name", s.Name), zap.Int("tools", n))
	}

	// Apply allow-list: skill wins, then config, else "allow all".
	switch {
	case sk != nil && len(sk.Frontmatter.Tools) > 0:
		if err := reg.SetAllowList(sk.Frontmatter.Tools); err != nil {
			for _, c := range clients {
				_ = c.Close()
			}
			return nil, nil, nil, fmt.Errorf("apply skill allow-list: %w", err)
		}
	case len(cfg.Agent.AllowedTools) > 0:
		if err := reg.SetAllowList(cfg.Agent.AllowedTools); err != nil {
			for _, c := range clients {
				_ = c.Close()
			}
			return nil, nil, nil, fmt.Errorf("apply config allow-list: %w", err)
		}
	}

	ag, err := agent.New(ctx, agent.Config{
		Model:       tcm,
		Tools:       reg,
		MaxSteps:    cfg.Agent.MaxSteps,
		Audit:       st,
		SessionIDFn: func() string { return sessionID },
		Logger:      logger,
	})
	if err != nil {
		for _, c := range clients {
			_ = c.Close()
		}
		return nil, nil, nil, err
	}
	return ag, reg, clients, nil
}

// registerBuiltinTools populates a registry with the agent's tools.
//
// The always-on tools (time/calc/echo) are harmless. The workspace tools are
// what make the agent able to work on a codebase, and they are what makes it
// dangerous: they read, write and execute against a real directory. They are
// therefore confined by internal/workspace and tagged with a capability so a
// caller can expose read-only access to a less trusted surface.
func registerBuiltinTools(reg *tool.Registry, cfg *config.Config, st store.Store, logger *zap.Logger,
	svc *viking.Service, opts toolSetOptions) error {
	basics := []struct {
		make func() (tool.Tool, error)
	}{
		{func() (tool.Tool, error) { return builtin.NewTimeTool() }},
		{func() (tool.Tool, error) { return builtin.NewCalcTool() }},
		{func() (tool.Tool, error) { return builtin.NewEchoTool() }},
	}
	for _, b := range basics {
		t, err := b.make()
		if err != nil {
			return err
		}
		if err := reg.Register(tool.WithCapability(t, tool.CapRead)); err != nil {
			return err
		}
	}
	// ask_user is the web console's interactive card: it parks the turn until the
	// person answers. It is registered for the web surface only, because a surface
	// with nowhere to render the card (Feishu, the CLI REPL) would otherwise be
	// offered a tool whose every call can only fail — and a refused call is a
	// worse answer than a tool that was never on the menu.
	if opts.Surface == "web" {
		t, err := builtin.NewAskUserTool()
		if err != nil {
			return fmt.Errorf("build ask_user tool: %w", err)
		}
		// CapRead: asking reads nothing and changes nothing.
		if err := reg.Register(tool.WithCapability(t, tool.CapRead)); err != nil {
			return err
		}
	}
	if err := registerDocumentTool(reg, cfg, svc); err != nil {
		return err
	}
	return registerWorkspaceTools(reg, cfg, st, logger, opts)
}

// registerDocumentTool exposes save_document when the OpenViking document store
// is configured. Without it the tool is not registered at all: a tool the model
// can see but that cannot work is worse than no tool, because it will be called.
func registerDocumentTool(reg *tool.Registry, cfg *config.Config, svc *viking.Service) error {
	if svc == nil || !cfg.OpenViking.Documents.Enable || svc.Documents() == nil {
		return nil
	}
	t, err := builtin.NewSaveDocumentTool(svc)
	if err != nil {
		return fmt.Errorf("build save_document tool: %w", err)
	}
	// CapRead: saving a document reads nothing and runs nothing. It is a write
	// to the agent's own document store, not to the workspace.
	if err := reg.Register(tool.WithCapability(t, tool.CapRead)); err != nil {
		return fmt.Errorf("register save_document: %w", err)
	}
	return nil
}

// mediaToolCandidate is one media tool together with the model capability that
// makes it usable and the access it needs.
type mediaToolCandidate struct {
	name string
	// capability is the store's model capability: what the model can do.
	capability store.Capability
	// access is the tool's own capability (read/write), a different vocabulary:
	// what the tool does to the machine.
	access tool.Capability
	build  func(*workspace.Workspace, media.Target) (einotool.InvokableTool, error)
}

// mediaToolCandidates is every media tool this build offers.
var mediaToolCandidates = []mediaToolCandidate{
	{name: "describe_image", capability: store.CapVision, access: tool.CapRead, build: builtin.NewDescribeImageTool},
	{name: "transcribe_audio", capability: store.CapAudioTranscribe, access: tool.CapRead, build: builtin.NewTranscribeAudioTool},
	// generate_image writes a file, so it is a write tool and is withheld from
	// a read-only workspace.
	{name: "generate_image", capability: store.CapImageGen, access: tool.CapWrite, build: builtin.NewGenerateImageTool},
}

// mediaToolNames lists the media tools, for the log line that explains what was
// skipped when there is nowhere for them to run.
func mediaToolNames() []string {
	out := make([]string, 0, len(mediaToolCandidates))
	for _, c := range mediaToolCandidates {
		out = append(out, c.name)
	}
	return out
}

func agentOnce(ctx context.Context, ag *agent.Agent, history *[]*schema.Message) error {
	fmt.Print("ai> ")
	out, err := ag.Generate(ctx, *history)
	if err != nil {
		return err
	}
	if out == nil {
		fmt.Println("(empty response)")
		return nil
	}
	fmt.Println(out.Content)
	*history = append(*history, out)
	return nil
}

func streamOnce(
	ctx context.Context,
	cm model.BaseChatModel,
	reg *llm.Registry,
	providerName string,
	modelOverride string,
	history *[]*schema.Message,
	sessionID string,
	rec *usage.Recorder,
	logger *zap.Logger,
) error {
	start := time.Now()
	stream, err := cm.Stream(ctx, *history)
	if err != nil {
		return err
	}
	defer stream.Close()

	var (
		assistant strings.Builder
		finish    string
	)
	fmt.Print("ai> ")
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return recvErr
		}
		if chunk == nil {
			continue
		}
		if chunk.Content != "" {
			assistant.WriteString(chunk.Content)
			fmt.Print(chunk.Content)
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.FinishReason != "" {
			finish = chunk.ResponseMeta.FinishReason
		}
	}
	fmt.Println()

	*history = append(*history, &schema.Message{
		Role:    schema.Assistant,
		Content: assistant.String(),
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: finish,
		},
	})

	dur := time.Since(start)
	recErr := rec.Record(usage.Event{
		SessionID:  sessionID,
		Provider:   providerName,
		Model:      effectiveModel(reg, providerName, modelOverride),
		DurationMs: dur.Milliseconds(),
	})
	if recErr != nil {
		logger.Warn("usage record failed", zap.Error(recErr))
	}
	return nil
}

func effectiveModel(reg *llm.Registry, name, override string) string {
	if override != "" {
		return override
	}
	if p, ok := reg.Provider(name); ok {
		return p.Model
	}
	return "<unknown>"
}

func banner(provider, modelName string, tools bool, skill string) string {
	mode := "chat"
	if tools {
		mode = "agent"
	}
	suffix := ""
	if skill != "" {
		suffix = fmt.Sprintf(" skill=%s", skill)
	}
	return fmt.Sprintf("huan-agent %s (provider=%s model=%s%s)\nType /quit to exit, /reset to clear, /provider /tools to inspect.", mode, provider, modelName, suffix)
}
