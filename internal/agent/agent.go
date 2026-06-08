package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/usage"
)

// Config wires an Agent to its dependencies. Model is required; the
// other fields are optional. Tools may be nil for a chat-only agent.
type Config struct {
	// Model MUST implement model.ToolCallingChatModel.
	Model model.ToolCallingChatModel

	// Tools is the registry the agent publishes to the LLM. The
	// agent snapshots the registry's allowed specs at New() time;
	// mutating the registry afterwards requires a new Agent.
	Tools *tool.Registry

	// MaxSteps caps model→tool→model iterations. Default 12, max 25.
	MaxSteps int

	// Recorder persists LLM token usage. Optional.
	Recorder *usage.Recorder

	// Audit persists each tool invocation. Optional.
	Audit AuditSink

	// SessionIDFn returns the session id used for usage + audit rows.
	// Optional; if nil, "default" is used.
	SessionIDFn func() string

	Logger *zap.Logger
}

// Agent wraps eino's ReAct agent.
type Agent struct {
	cfg     Config
	react   *react.Agent
	maxStep int
}

// New constructs an Agent. It snapshots the registry's allowed specs
// at this point and binds them to the model.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.Model == nil {
		return nil, errors.New("agent: model is required")
	}
	if cfg.Tools == nil {
		cfg.Tools = tool.NewRegistry()
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if cfg.MaxSteps > 25 {
		cfg.MaxSteps = 25
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	specs, err := cfg.Tools.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: list tools: %w", err)
	}

	var baseTools []einotool.BaseTool
	for _, s := range specs {
		t, ok := cfg.Tools.Get(s.Name)
		if !ok {
			continue
		}
		baseTools = append(baseTools, registryAdapter{Tool: t, specName: s.Name})
	}

	toolsConfig := compose.ToolsNodeConfig{
		Tools:               baseTools,
		ExecuteSequentially: true, // deterministic; helps audit ordering
	}
	if cfg.Audit != nil {
		toolsConfig.ToolCallMiddlewares = []compose.ToolMiddleware{auditMiddleware(cfg.Audit, cfg.SessionIDFn, cfg.Logger)}
	}

	r, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: cfg.Model,
		ToolsConfig:      toolsConfig,
		MaxStep:          cfg.MaxSteps,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build react: %w", err)
	}

	return &Agent{
		cfg:     cfg,
		react:   r,
		maxStep: cfg.MaxSteps,
	}, nil
}

// Generate runs the ReAct loop and returns the final assistant message.
func (a *Agent) Generate(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	start := time.Now()
	out, err := a.react.Generate(ctx, messages)
	if err != nil {
		return nil, err
	}
	a.recordUsage(ctx, out, time.Since(start))
	return out, nil
}

// Stream runs the ReAct loop and returns a stream of the final
// assistant message chunks. Intermediate tool-call / tool-result
// steps are NOT exposed — the caller sees only the final answer.
func (a *Agent) Stream(ctx context.Context, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return a.react.Stream(ctx, messages)
}

func (a *Agent) recordUsage(ctx context.Context, msg *schema.Message, dur time.Duration) {
	if a.cfg.Recorder == nil || msg == nil {
		return
	}
	var u schema.TokenUsage
	if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
		u = *msg.ResponseMeta.Usage
	}
	sid := "default"
	if a.cfg.SessionIDFn != nil {
		sid = a.cfg.SessionIDFn()
	}
	_ = a.cfg.Recorder.Record(usage.Event{
		SessionID:        sid,
		Provider:         "<agent>",
		Model:            "<agent>",
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		DurationMs:       dur.Milliseconds(),
	})
}

// registryAdapter is the bridge between a tool.Registry entry and
// eino's tool.BaseTool + tool.InvokableTool interfaces.
type registryAdapter struct {
	Tool     any // satisfies both tool.BaseTool and tool.InvokableTool
	specName string
}

func (r registryAdapter) Info(ctx context.Context) (*schema.ToolInfo, error) {
	if t, ok := r.Tool.(einotool.BaseTool); ok {
		return t.Info(ctx)
	}
	return &schema.ToolInfo{Name: r.specName}, nil
}

func (r registryAdapter) InvokableRun(ctx context.Context, args string, opts ...einotool.Option) (string, error) {
	if t, ok := r.Tool.(einotool.InvokableTool); ok {
		return t.InvokableRun(ctx, args, opts...)
	}
	return "", fmt.Errorf("tool %q is not invokable", r.specName)
}

// auditMiddleware returns a ToolMiddleware that records each tool
// invocation to the given sink. Failures writing the audit row are
// logged at warn level and do NOT affect the tool result.
func auditMiddleware(sink AuditSink, sidFn func() string, logger *zap.Logger) compose.ToolMiddleware {
	sid := "default"
	if sidFn != nil {
		sid = sidFn()
	}
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				start := time.Now()
				out, err := next(ctx, input)
				dur := time.Since(start).Milliseconds()
				rec := InvocationRecord{
					SessionID:  sid,
					ToolName:   input.Name,
					Arguments:  input.Arguments,
					DurationMs: dur,
				}
				if out != nil {
					rec.Result = out.Result
				}
				if err != nil {
					rec.Err = err.Error()
				}
				if wErr := sink.RecordInvocation(ctx, auditFromRecord(rec)); wErr != nil {
					logger.Warn("audit write failed",
						zap.String("tool", input.Name),
						zap.Error(wErr))
				}
				return out, err
			}
		},
	}
}
