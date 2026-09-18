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
	"github.com/huan/huan-agent/internal/checkpoint"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/prompt"
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
	// Every model this process builds retries a transient call failure with
	// exponential backoff. It is applied here, where the providers are
	// constructed, rather than at each call site: a dropped connection interrupts
	// a long task whichever surface started it, and the CLI's ReAct loop has no
	// retry of its own.
	registry.WithOptions(llm.WithRetry(cfg.LLM.RetryPolicy(), logger))

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
		cm, err = llm.New(prov, llm.WithRetry(cfg.LLM.RetryPolicy(), logger))
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

	// Build the system prompt and wire memory + context. A skill's body is a
	// prompt of its own, so --skill wins over --system; either wins over the
	// general-purpose prompt every surface starts from, which is what keeps
	// `huan-agent chat` the same agent as the console instead of a model with no
	// instructions at all.
	systemPrompt := prompt.Effective(chatSystem, prompt.SurfaceCLI)
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

	// Checkpoints: the pre-image of every file a turn changes. The path is
	// resolved here rather than inside the tool set so a deployment that turned
	// the feature off pays nothing for it.
	checkpointer, cpErr := newCheckpointer(cfg, workspaceForCheckpoints(cfg), logger)
	if cpErr != nil {
		logger.Warn("checkpoints disabled", zap.Error(cpErr))
	}

	// Build the agent if tools are enabled. Tool registry + MCP clients live
	// for the duration of the REPL.
	var (
		ag         *agent.Agent
		toolReg    *tool.Registry
		mcpClients []*mcp.Client
	)
	if chatTools {
		ag, toolReg, mcpClients, err = buildAgent(ctx, cm, cfg, chosenSkill, st, logger, sessionID, vikingSvc, jobMgr, checkpointer, recorder)
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

	// The approval gate's terminal half. It reads through the scanner above
	// rather than opening os.Stdin, because two readers on one terminal would
	// fight over lines: whichever goroutine reached the file descriptor first
	// would take the line the other was waiting for.
	approver := newCLIApprover(
		func() (string, bool) {
			if !in.Scan() {
				return "", false
			}
			return in.Text(), true
		},
		os.Stderr,
		terminalInteractive(),
	)
	if gateMode, _ := approvalPolicyFor(cfg); gateMode.Mode != tool.ApprovalOff && !approver.interactive {
		// A piped session has nobody to ask. The gate withholds the write and
		// exec tools instead of refusing every call, and saying so once here is
		// what turns "why are there no write tools" into an answer.
		logger.Info("approval gate: stdin is not a terminal, so interactive approval is unavailable; " +
			"write and exec tools are withheld for this session (tools.approval.mode=off gives them back)")
	}

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
		case strings.HasPrefix(lower, "/checkpoints"), strings.HasPrefix(lower, "/rollback"):
			runCheckpointCommand(line, checkpointer, sessionID)
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
			// A fresh allowance set per turn: "allow for this turn" has to end
			// when the turn does, and the context is what enforces it. The
			// checkpoint turn is opened at the same moment, so the pre-image of
			// every file this turn writes is attributed to it.
			turnCtx, endTurn := beginCheckpointTurn(ctx, checkpointer, sessionID, logger)
			turnCtx = tool.WithApprover(tool.WithTurnAllowances(turnCtx), approver)
			runErr = agentOnce(turnCtx, ag, &history)
			endTurn()
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
	checkpointer *checkpoint.Checkpointer,
	rec *usage.Recorder,
) (*agent.Agent, *tool.Registry, []*mcp.Client, error) {
	tcm, ok := cm.(model.ToolCallingChatModel)
	if !ok {
		return nil, nil, nil, fmt.Errorf("provider does not support tool calling")
	}

	// The subagents this surface can spawn. The CLI drives Eino's ReAct loop, so it
	// needs the Eino runner; the spawner is the process's, so the concurrency gate
	// is shared with any other surface in the same process.
	spawner, spawnErr := spawnerForEino(cfg, st, sessionID, rec, logger)
	if spawnErr != nil {
		logger.Warn("subagents disabled", zap.Error(spawnErr))
	}

	reg := tool.NewRegistry()
	if err := registerBuiltinTools(reg, cfg, st, logger, svc,
		toolSetOptions{
			Jobs:       jobMgr,
			Surface:    "cli",
			SpawnAgent: spawner,
			// No model resolver on this surface: the model registry lives one
			// level up, and a subagent that asks for a specific model gets a clear
			// refusal from the tool rather than silently running on the parent's.
			ResolveModel: nil,
		}); err != nil {
		return nil, nil, nil, fmt.Errorf("register builtin tools: %w", err)
	}

	clients, err := connectConfiguredMCP(ctx, reg, cfg, logger, "cli")
	if err != nil {
		return nil, nil, nil, err
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

	// The approval gate is resolved once per surface and passed down, so the
	// workspace-bound tools this function registers on the way out are gated the
	// same way the base ones are. A failure to parse the mode is a configuration
	// error worth refusing: starting with a gate the operator did not ask for is
	// worse than not starting.
	policy, err := approvalPolicyFor(cfg)
	if err != nil {
		return err
	}
	gate := approvalGate{
		policy:  policy,
		canAsk:  approvalSurfaceCanAsk(opts.Surface),
		surface: opts.Surface,
		logger:  logger,
	}
	opts.Gate = gate

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
		// ParallelSafe: time, calc and echo are pure — no state, no I/O, nothing
		// another call could observe.
		if err := reg.Register(tool.WithConcurrency(
			tool.WithCapability(t, tool.CapRead), tool.ParallelSafe)); err != nil {
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
		// CapRead: asking reads nothing and changes nothing. Serial: it parks the
		// whole turn waiting for a person, so overlapping it with anything — or
		// with another card — is not a race, it is a conversation with two
		// questions in flight and one person reading them.
		if err := reg.Register(tool.WithConcurrency(
			tool.WithCapability(t, tool.CapRead), tool.Serial)); err != nil {
			return err
		}
	}

	// The plan tools are the web console's 任务看板: the model maintains a task
	// list that the console renders above the composer, and that a resumed turn
	// reads to find out what is already done. They are registered under the same
	// condition as ask_user and for the same reason: the plan store is installed
	// per turn by the server (see internal/server/turnPlanner), so on a surface
	// without one every call could only fail.
	if opts.Surface == "web" && cfg.Chat.Plan.Enable {
		planTools, err := builtin.NewPlanTools()
		if err != nil {
			return fmt.Errorf("build plan tools: %w", err)
		}
		for _, t := range planTools {
			// CapRead: a plan is the agent's own working state. It touches no
			// files and runs no commands, so it must not be withheld from a
			// read-only workspace — that is exactly where a long task needs to
			// explain itself.
			//
			// Serial, and this is the case that proves capability and concurrency
			// are different questions: a plan_update is CapRead and is a
			// read-modify-write on one shared plan. Two at once lose an update and
			// scramble the board the user is watching.
			if err := reg.Register(tool.WithConcurrency(
				tool.WithCapability(t, tool.CapRead), tool.Serial)); err != nil {
				return err
			}
		}
	}
	if err := registerDocumentTool(reg, cfg, svc); err != nil {
		return err
	}

	// fetch_url: reading pages from the internet. Registered here with the other
	// always-on tools, and declared ParallelSafe below because "read these three
	// pages and compare them" is its main use.
	if cfg.Tools.Web.Enable {
		if err := registerFetchURLTool(reg, cfg, logger); err != nil {
			return err
		}
	}

	// spawn_agent: the tool that hands a question to a nested run. It is built
	// here rather than per turn because the process-wide concurrency gate inside
	// the spawner is what bounds how many subagents run at once — a spawner per
	// surface would multiply that bound by the number of surfaces.
	//
	// The model it runs on is not captured here: it comes from the turn (see
	// tool.TurnResources), because a conversation can pick its own model and a
	// subagent should run on it unless the model asks for a cheaper one.
	if opts.SpawnAgent != nil {
		if err := registerSpawnAgentTool(reg, cfg, opts.SpawnAgent, opts.ResolveModel, logger); err != nil {
			return err
		}
	}

	// Everything registered so far goes through the gate as one pass. Doing it
	// here rather than at each Register call is what makes the coverage
	// structural: the always-on tools, the media tools, the document tool and the
	// skill tool are assembled in four different places, and a fifth added later
	// is covered without anyone remembering to.
	if err := gate.applyToRegistry(context.Background(), reg); err != nil {
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
	//
	// Serial for the same reason a plan update is: it is a write, whatever its
	// capability says, and two writes to one store overlapping is a question about
	// that store's idempotence that nobody has answered.
	if err := reg.Register(tool.WithConcurrency(
		tool.WithCapability(t, tool.CapRead), tool.Serial)); err != nil {
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
