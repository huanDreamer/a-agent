package main

import (
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	gctx "github.com/huan/huan-agent/internal/context"
)

// longTurnSteps is the step count above which a turn is treated as long enough
// to need a bounded window. It is a heuristic for one warning, not a limit:
// twelve steps resend the history twelve times, which is affordable; forty do
// not.
const longTurnSteps = 24

// turnCondenser builds the in-loop condenser for one model.
//
// The window comes from the model, not from a constant: context.max_tokens in
// its default (auto) setting is resolved through gctx.WindowSpecFor into
// "window × ratio − reserve", so a conversation on a 200k model fills 130k and
// one on a 32k model fills 14k. A fixed 60000 was wrong for both, and wrong in
// opposite directions — it gave away most of the big window and overflowed the
// small one.
//
// It returns the *interface*, not *gctx.Manager, and that is load-bearing: a nil
// *gctx.Manager stored in a chat.Condenser is a non-nil interface holding a nil
// pointer, so a runner would call through it and crash every turn of a
// deployment that never configured compression. Returning the interface makes
// "off" a genuinely nil Condenser.
//
// A nil result is not a failure: compression is off only when it was explicitly
// turned off (context.max_tokens < 0), and a runner with no condenser sends the
// history whole, exactly as it did before this existed. When the step budget is
// large and compression is off it says so once at startup, because that
// combination is the one that turns a long task into a context-limit error
// partway through — and the person who can fix it is reading the logs, not the
// transcript.
//
// cm is the model used to summarize. It may be nil, in which case a rolled-up
// window gets a placeholder summary instead of a written one: still bounded,
// just less informative.
func turnCondenser(cfg *config.Config, cm model.BaseChatModel, logger *zap.Logger, modelName string) (chat.Condenser, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	spec := cfg.Context.WindowSpecFor()
	window := spec.Resolve(modelName)

	if window.Cap <= 0 {
		steps := cfg.Chat.MaxSteps
		if steps <= 0 {
			steps = chat.DefaultMaxSteps
		}
		if steps > longTurnSteps {
			logger.Warn("长任务未启用上下文压缩：每一轮都会把之前所有步骤重新发给模型，token 成本随步数近似平方增长",
				zap.Int("chat_max_steps", steps),
				zap.String("fix", "去掉 context.max_tokens 的负值（0 = 按模型窗口自动），或显式设成一个正数"),
			)
		}
		return nil, nil
	}

	logger.Info("上下文窗口预算已确定",
		zap.String("model", window.Model),
		zap.Int("context_window", window.Tokens),
		zap.Int("in_loop_cap", window.Cap),
		zap.String("source", window.Source),
		zap.String("derivation", window.Derivation),
	)

	var summarizer gctx.Summarizer
	if cfg.Context.Summarize && cm != nil {
		summarizer = gctx.LLMSummarizer{Model: cm}
	}
	mgr, err := gctx.NewManager(gctx.Budget{
		MaxTokens:  window.Cap,
		KeepRecent: cfg.Context.KeepRecent,
		Summarizer: summarizer,
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build the turn condenser: %w", err)
	}
	mgr.SetLog(func(s string) { logger.Info("context: " + s) })
	return mgr, nil
}

// turnCondenserFactory builds the condenser per conversation model.
//
// It exists because the window is a property of the model and a console serves
// several: the runner is already cached per (provider, model), so the condenser
// is built there too, and a conversation that switches model gets the window of
// the model it switched to. One condenser built at startup would be the fixed
// number this change removes.
func turnCondenserFactory(cfg *config.Config, logger *zap.Logger) func(model.BaseChatModel, string, string) (chat.Condenser, error) {
	return func(cm model.BaseChatModel, _ string, name string) (chat.Condenser, error) {
		return turnCondenser(cfg, cm, logger, name)
	}
}

// chatGuardFor translates the guard configuration for the runner.
func chatGuardFor(cfg *config.Config) chat.GuardConfig {
	g := cfg.Chat.Guard
	return chat.GuardConfig{
		Disable:        !g.Enable,
		RepeatNudge:    g.RepeatNudge,
		RepeatStop:     g.RepeatStop,
		IdleNudgeSteps: g.IdleNudgeSteps,
		IdleStopSteps:  g.IdleStopSteps,
		MaxSteers:      g.MaxSteers,
		RereadNudge:    g.RereadNudge,
	}
}

// toolResultCapFor is how many characters of one tool result the model may see.
//
// 0 in the config means "the built-in default" and a negative value means "no
// bound"; the runner resolves both, so this passes the configured number through
// unchanged and only guards against an unset struct field.
func toolResultCapFor(cfg *config.Config) int {
	return cfg.Context.ToolResultMaxChars
}

// contextBudgetForPanel resolves the window for the deployment's default model,
// for the 设置 → 对话预算 panel.
//
// The panel reports a number and says whether it is a fixed one or derived from
// the model: "60000" and "91750, derived from deepseek/deepseek-v4.1-flash's 128k
// window" are very different answers to "why does it compress so often", and
// only the second one is actionable.
func contextBudgetForPanel(cfg *config.Config, modelName string) (capTokens int, auto bool, resolvedFor string) {
	if cfg.Context.MaxTokens > 0 {
		return cfg.Context.MaxTokens, false, ""
	}
	window := cfg.Context.WindowSpecFor().Resolve(modelName)
	if window.Cap <= 0 {
		// Compression off (a negative max_tokens). Reporting it as "auto" would
		// tell the panel's reader that a window is being managed when none is.
		return 0, false, ""
	}
	return window.Cap, true, window.Model
}
