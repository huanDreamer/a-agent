package main

// The subagent's wiring: how this deployment runs a nested turn, and where the
// spawn_agent tool is registered.
//
// The policy — limits, narrowing, truncation, budget — lives in internal/subagent
// so it is written once. What this file supplies is the two things only the
// command layer knows: how to run a turn (the chat loop, with a fresh runner and
// no parent history) and how to turn a model name into a model.

import (
	"context"
	"fmt"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/server"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/subagent"
	agenttool "github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// nestedChatRunner runs a subagent on the same loop the console uses.
//
// A fresh Runner rather than the parent's: the nested run needs its own step
// counter, its own event stream (nobody is watching it, and its events must not
// land on the parent's) and its own history. Sharing the parent's runner would
// mean sharing all three.
type nestedChatRunner struct {
	tracer chatTracer
	logger *zap.Logger
	// guard and toolResult are the deployment's own policy. A nested run must
	// obey them for the same reason the parent does — and, until this was wired,
	// it silently used the built-in defaults instead: a deployment that turned
	// the guard off still had it inside every subagent.
	guard      chat.GuardConfig
	toolResult int
	// condenserFor builds the in-loop window bound for the model a nested run
	// ends up on. Without it a subagent with the parent's step budget would
	// resend its whole history on every step, which is the quadratic cost the
	// condenser exists to remove — and it is the shape that hits a provider's
	// context limit at the worst moment.
	condenserFor func(cm einomodel.BaseChatModel, provider, name string) (chat.Condenser, error)
}

// chatTracer is the tracer interface the chat package wants, named here so this
// file does not have to import chat for a type it only passes through.
type chatTracer = chat.Tracer

// RunNested implements subagent.Runner.
func (r nestedChatRunner) RunNested(ctx context.Context, in subagent.NestedRequest) (subagent.NestedResult, error) {
	var condenser chat.Condenser
	if r.condenserFor != nil {
		c, cerr := r.condenserFor(in.Model, "", in.ModelName)
		if cerr != nil {
			// Not fatal: a nested run with an unbounded window still answers, it
			// just costs more the longer it goes.
			r.logger.Warn("nested run: resolving the model's context window failed", zap.Error(cerr))
		} else {
			condenser = c
		}
	}
	runner, err := chat.New(chat.Config{
		Model:    in.Model,
		Tools:    in.Tools,
		Tracer:   r.tracer,
		MaxSteps: in.MaxSteps,
		// One at a time inside a subagent: a nested run is already a fan-out from
		// the parent's point of view, and letting it fan out again is how a
		// bounded cost becomes an unbounded one.
		MaxParallel: 1,
		// The same in-loop policy the parent runs under: a bounded window and the
		// loop guard, so a subagent that starts repeating itself or reading
		// without acting is steered and, if it keeps going, stopped — instead of
		// spending the whole budget it was given.
		Condenser:          condenser,
		Guard:              r.guard,
		ToolResultMaxChars: r.toolResult,
		ModelName:          in.ModelName,
		Logger:             r.logger,
	})
	if err != nil {
		return subagent.NestedResult{}, fmt.Errorf("nested runner: %w", err)
	}

	// The nested run reports through the parent turn's stream when there is one,
	// tagged with the call that spawned it. Without that the console shows a
	// spawn_agent card that sits silent for however long the subagent works, which
	// reads as a hang; with it, the card fills in as the subagent explores.
	sink, hasSink := chat.NestedFrom(ctx)
	var emit chat.Emitter
	if hasSink {
		emit = sink.Emit
	}

	res, err := runner.Run(ctx, chat.Request{
		Messages:  in.Messages,
		SessionID: in.SessionID,
		Scope:     in.Scope,
	}, emit)
	if err != nil {
		return subagent.NestedResult{}, err
	}

	nested := subagent.NestedResult{
		Text:       res.Text,
		Reasoning:  res.Reasoning,
		Steps:      res.Steps,
		StopReason: res.StopReason,
		Usage: subagent.Usage{
			PromptTokens:     res.Usage.PromptTokens,
			CompletionTokens: res.Usage.CompletionTokens,
			TotalTokens:      res.Usage.TotalTokens,
			DurationMs:       res.Usage.DurationMs,
		},
	}
	for _, run := range res.Tools {
		nested.Tools = append(nested.Tools, run.Name)
	}
	return nested, nil
}

// subagentAgent builds the spawner for this process.
//
// One per process, like the job manager and the language-server manager: the
// concurrency gate inside it is what bounds how many subagents run at once, and a
// gate per surface would multiply the bound by the number of surfaces.
var subagentAgent *subagent.Agent

// spawnTracker is the process's list of delegated runs.
//
// It is shared by both spawners on purpose: the console's header answers "what has
// this conversation delegated", and a per-surface list would show only the runs that
// came through one of them.
var spawnTracker = subagent.NewTracker(0)

// spawnerFor returns the process's subagent agent, building it on first use.
func spawnerFor(cfg *config.Config, tracer chatTracer, logger *zap.Logger) (*subagent.Agent, error) {
	if cfg == nil || !cfg.Subagent.Enable {
		return nil, nil
	}
	if subagentAgent != nil {
		return subagentAgent, nil
	}
	agent, err := subagent.NewWithTracker(
		nestedChatRunner{
			tracer: tracer,
			logger: logger,
			guard:  chatGuardFor(cfg),
			// The deployment's own bound, so a subagent's history is trimmed the
			// same way its parent's is.
			toolResult:   toolResultCapFor(cfg),
			condenserFor: turnCondenserFactory(cfg, logger, windowSpecFor(cfg, nil, logger)),
		},
		subagent.Limits{
			// 0 means "no limit of this package's own": the nested run then uses
			// the parent turn's budget, so a subagent has as much room as the
			// conversation it belongs to.
			// Inherited per spawn from the turn's own budget (see TurnResources);
			// the value here is only what a caller with no turn falls back to.
			MaxSteps:       cfg.Subagent.MaxStepsOr(cfg.Chat.MaxSteps),
			MaxConcurrent:  cfg.Subagent.MaxConcurrentOr(),
			MaxReportChars: cfg.Subagent.MaxReportCharsOr(),
		},
		subagentLogger{logger},
		spawnTracker,
	)
	if err != nil {
		return nil, err
	}
	subagentAgent = agent
	return agent, nil
}

// subagentLogger adapts *zap.Logger to the subagent package's interface.
type subagentLogger struct{ l *zap.Logger }

func (s subagentLogger) Debug(msg string, kv ...any) {
	if s.l != nil {
		s.l.Sugar().Debugw(msg, kv...)
	}
}

func (s subagentLogger) Warn(msg string, kv ...any) {
	if s.l != nil {
		s.l.Sugar().Warnw(msg, kv...)
	}
}

// registerSpawnAgentTool adds spawn_agent to a registry, when the deployment has
// subagents on and something to hand the tool.
//
// resolve turns a model name into a model; a nil resolve means "this deployment
// cannot choose a model for a subagent", which the tool reports if asked rather
// than silently using the parent's.
func registerSpawnAgentTool(reg *agenttool.Registry, cfg *config.Config, spawner *subagent.Agent,
	resolve agenttool.ModelResolver, logger *zap.Logger) error {

	if cfg == nil || !cfg.Subagent.Enable || spawner == nil {
		return nil
	}
	t, err := builtin.NewSpawnAgentTool(spawner, resolve)
	if err != nil {
		return fmt.Errorf("build spawn_agent tool: %w", err)
	}
	// CapRead: the tool itself touches nothing. The tools it may hand on are the
	// parent's own, already wrapped in the parent's approval gate, checkpoints and
	// sandbox — so a subagent cannot be a way around any of them.
	//
	// ParallelSafe: "explore three subsystems at once" is its main use. The
	// process-wide gate inside the agent is what keeps that from multiplying.
	if err := reg.Register(agenttool.WithConcurrency(
		agenttool.WithCapability(t, agenttool.CapRead), agenttool.ParallelSafe)); err != nil {
		return err
	}
	if logger != nil {
		logger.Info("spawn_agent registered",
			zap.Bool("model_override", resolve != nil),
			zap.Int("max_concurrent", cfg.Subagent.MaxConcurrentOr()))
	}
	return nil
}

// modelResolverFor turns a model name into a model, for a subagent the model asked
// to run on something cheaper.
//
// It goes through the same catalog builder the conversation's own model does, so a
// name that works for the console works here, and a name that does not is refused
// with the builder's reason rather than a silent fall back to the parent's model.
func modelResolverFor(builder *server.CatalogModelBuilder, cfg *config.Config) agenttool.ModelResolver {
	if builder == nil {
		return nil
	}
	provider := ""
	if cfg != nil {
		provider = cfg.LLM.DefaultProvider
	}
	return func(ctx context.Context, name string) (einomodel.BaseChatModel, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("subagent: 模型名不能为空")
		}
		built, err := builder.Build(ctx, provider, name)
		if err != nil {
			return nil, fmt.Errorf("subagent: 无法使用模型 %q：%w", name, err)
		}
		cm, ok := built.(einomodel.BaseChatModel)
		if !ok {
			return nil, fmt.Errorf("subagent: 模型 %q 不是对话模型", name)
		}
		return cm, nil
	}
}

// runModelResolver resolves a model name for a subagent on the `run` surface,
// where there is no console model builder: the name is looked up in the same
// provider registry the run itself was resolved from.
func runModelResolver(cfg *config.Config, st store.Store, logger *zap.Logger) agenttool.ModelResolver {
	if cfg == nil {
		return nil
	}
	providers := make(map[string]llm.Provider, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		providers[name] = llm.Provider{
			Name: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model,
			DisableUsageRequest: p.DisableUsageRequest,
		}
	}
	registry := llm.NewRegistry(providers, cfg.LLM.DefaultProvider).
		WithOptions(llm.WithRetry(cfg.LLM.RetryPolicy(), logger))

	return func(ctx context.Context, name string) (einomodel.BaseChatModel, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("subagent: 模型名不能为空")
		}
		// The name may be a provider or a model; the registry answers both.
		if cm, err := registry.Get(name); err == nil {
			return cm, nil
		}
		chosen := cfg.LLM.DefaultProvider
		cm, err := registry.GetWithModel(chosen, name)
		if err != nil {
			return nil, fmt.Errorf("subagent: 无法使用模型 %q：%w", name, err)
		}
		return cm, nil
	}
}

// subagentTrackerFor returns the process's tracker, or nil when this deployment has
// no subagents.
//
// Nil rather than an empty tracker: the console registers the endpoint only when it
// has one, so a deployment with the feature off shows no chip at all instead of a
// chip that always says "0".
func subagentTrackerFor(cfg *config.Config) server.SubagentTracker {
	if cfg == nil || !cfg.Subagent.Enable {
		return nil
	}
	return spawnTracker
}
