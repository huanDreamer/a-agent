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

// turnCondenser builds the in-loop condenser that keeps a long turn's window
// bounded, from the context.* settings.
//
// It returns the *interface*, not *gctx.Manager, and that is load-bearing: a nil
// *gctx.Manager stored in a chat.Condenser is a non-nil interface holding a nil
// pointer, so a runner would call through it and crash every turn of a
// deployment that never configured compression. Returning the interface makes
// "off" a genuinely nil Condenser.
//
// A nil result is not a failure: compression is off unless context.max_tokens is
// set, and a runner with no condenser sends the history whole, exactly as it did
// before this existed. When the step budget is large and compression is off it
// says so once at startup, because that combination is the one that turns a long
// task into a context-limit error partway through — and the person who can fix it
// is reading the logs, not the transcript.
//
// cm is the model used to summarize. It may be nil, in which case a rolled-up
// window gets a placeholder summary instead of a written one: still bounded,
// just less informative.
func turnCondenser(cfg *config.Config, cm model.BaseChatModel, logger *zap.Logger) (chat.Condenser, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	steps := cfg.Chat.MaxSteps
	if steps <= 0 {
		steps = chat.DefaultMaxSteps
	}
	if cfg.Context.MaxTokens <= 0 {
		if steps > longTurnSteps {
			logger.Warn("长任务未启用上下文压缩：每一轮都会把之前所有步骤重新发给模型，token 成本随步数近似平方增长",
				zap.Int("chat_max_steps", steps),
				zap.String("fix", "设置 context.max_tokens（例如模型窗口的 60%~70%）"),
			)
		}
		return nil, nil
	}
	var summarizer gctx.Summarizer
	if cfg.Context.Summarize && cm != nil {
		summarizer = gctx.LLMSummarizer{Model: cm}
	}
	mgr, err := gctx.NewManager(gctx.Budget{
		MaxTokens:  cfg.Context.MaxTokens,
		KeepRecent: cfg.Context.KeepRecent,
		Summarizer: summarizer,
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build the turn condenser: %w", err)
	}
	mgr.SetLog(func(s string) { logger.Info("context: " + s) })
	return mgr, nil
}
