package main

// The subagent runner for Eino's ReAct loop, which is what the CLI and the Feishu
// bot use.
//
// The two loops need the same thing from a nested run — "answer this, in your own
// context, and give me the text" — so the policy in internal/subagent is written
// once and each loop supplies only how to run a turn. This is the second of those
// two implementations, and it exists so that `chat --tools` and the bot can spawn
// subagents at all: without it the tool is registered on those surfaces and would
// fail on every call, which is worse than it being absent.

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/agent"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/subagent"
	"github.com/huan/huan-agent/internal/usage"
)

// einoNestedRunner runs a subagent on Eino's ReAct loop.
type einoNestedRunner struct {
	recorder *usage.Recorder
	audit    agent.AuditSink
	session  string
	logger   *zap.Logger
}

// RunNested implements subagent.Runner.
//
// The nested agent gets its own registry (the narrowed one), its own step cap and
// its own Agent, so it shares nothing with the parent loop's state. That is the
// same isolation the chat-loop runner gets by building a fresh Runner, and it is
// what makes "the subagent's working-out stays in the subagent's context" true
// rather than aspirational.
func (r einoNestedRunner) RunNested(ctx context.Context, in subagent.NestedRequest) (subagent.NestedResult, error) {
	if in.Model == nil {
		return subagent.NestedResult{}, fmt.Errorf("nested eino runner: 没有可用的模型")
	}
	nested, err := agent.New(ctx, agent.Config{
		Model:    r.einoModel(in),
		Tools:    in.Tools,
		MaxSteps: in.MaxSteps,
		Recorder: r.recorder,
		Audit:    r.audit,
		SessionIDFn: func() string {
			// The nested run's usage and audit rows belong to the turn that asked
			// for it, so the session id is the parent's.
			return r.session
		},
		Logger: r.logger,
	})
	if err != nil {
		return subagent.NestedResult{}, fmt.Errorf("nested eino runner: %w", err)
	}

	msg, err := nested.Generate(ctx, in.Messages)
	if err != nil {
		return subagent.NestedResult{}, err
	}
	res := subagent.NestedResult{
		Text: msg.Content,
		// Eino's ReAct loop hands back only the final message, so this runner
		// cannot say how many iterations ran or which tools were used. Saying so is
		// the difference between a report a parent can weigh and one that claims the
		// subagent did nothing.
		StepsUnavailable: true,
	}
	if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
		res.Usage = subagent.Usage{
			PromptTokens:     msg.ResponseMeta.Usage.PromptTokens,
			CompletionTokens: msg.ResponseMeta.Usage.CompletionTokens,
			TotalTokens:      msg.ResponseMeta.Usage.TotalTokens,
		}
	}
	return res, nil
}

// einoModel narrows the turn's model to what agent.New wants.
//
// It is a separate method so the type assertion and its failure live in one place:
// a turn whose model is not a tool-calling model cannot run a subagent, and saying
// that once beats a panic deep inside the nested loop.
func (r einoNestedRunner) einoModel(in subagent.NestedRequest) model.ToolCallingChatModel {
	if tcm, ok := in.Model.(model.ToolCallingChatModel); ok {
		return tcm
	}
	return nil
}

// einoSpawner is the process's subagent agent for the Eino surfaces, kept apart
// from the chat-loop one because the two need different runners.
//
// One per process, like the chat-loop spawner: the concurrency gate inside is what
// bounds how many subagents run at once, and a gate per surface would multiply the
// bound by the number of surfaces.
var einoSpawner *subagent.Agent

// spawnerForEino builds the spawner the CLI and the Feishu bot use.
func spawnerForEino(cfg *config.Config, st store.Store, session string, rec *usage.Recorder, logger *zap.Logger) (*subagent.Agent, error) {
	if cfg == nil || !cfg.Subagent.Enable {
		return nil, nil
	}
	if einoSpawner != nil {
		return einoSpawner, nil
	}
	var audit agent.AuditSink
	if st != nil {
		audit = st
	}
	agent, err := subagent.NewWithTracker(
		einoNestedRunner{recorder: rec, audit: audit, session: session, logger: logger},
		subagent.Limits{
			// No turn in hand on this path (a one-shot `run` with the Eino loop),
			// so the deployment's own chat cap is the budget to inherit.
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
	einoSpawner = agent
	return agent, nil
}
