package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// DefaultMaxSteps caps model→tool→model iterations when a Request does not say
// otherwise. It mirrors the agent package's default so the web UI and the IM
// bot behave the same.
const DefaultMaxSteps = 12

// Request is one conversational turn.
type Request struct {
	// Messages is the full conversation to send, oldest first. The caller owns
	// the history (the runner never mutates it).
	Messages []*schema.Message

	// SessionID and UserID are used for tracing and audit attribution.
	SessionID string
	UserID    string

	// MaxSteps overrides DefaultMaxSteps.
	MaxSteps int
}

// Config wires a Runner.
type Config struct {
	// Model is the chat model to drive. Required.
	Model model.BaseChatModel
	// Tools is the tool registry. Optional: a nil or empty registry runs in
	// pure-chat mode.
	Tools *tool.Registry
	// Tracer receives trace/span/generation events. Optional; a nil Tracer
	// disables tracing.
	Tracer Tracer
	// MaxSteps is the default step cap.
	MaxSteps int
	Logger   *zap.Logger
}

// Runner drives a streaming tool-calling conversation.
type Runner struct {
	model    model.BaseChatModel
	tools    *tool.Registry
	tracer   Tracer
	maxSteps int
	logger   *zap.Logger
}

// New builds a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Model == nil {
		return nil, errors.New("chat: model is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	max := cfg.MaxSteps
	if max <= 0 {
		max = DefaultMaxSteps
	}
	// Default to a no-op tracer so the loop never has to nil-check, and a
	// caller that does not care about tracing simply omits it.
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = NopTracer{}
	}
	return &Runner{
		model:    cfg.Model,
		tools:    cfg.Tools,
		tracer:   tracer,
		maxSteps: max,
		logger:   logger,
	}, nil
}

// toolInfos returns the model-facing specs for the permitted tools. It returns
// nil when there are no tools, which selects pure-chat mode.
func (r *Runner) toolInfos(ctx context.Context) ([]*schema.ToolInfo, error) {
	if r.tools == nil {
		return nil, nil
	}
	specs, err := r.tools.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat: list tools: %w", err)
	}
	if len(specs) == 0 {
		return nil, nil
	}
	infos := make([]*schema.ToolInfo, 0, len(specs))
	for _, s := range specs {
		info := &schema.ToolInfo{Name: s.Name, Desc: s.Description}
		if strings.TrimSpace(s.ParametersJSONSchema) != "" {
			params, perr := schemaToParams(s.ParametersJSONSchema)
			if perr != nil {
				// A tool with an unparsable schema is skipped rather than
				// failing the turn: the model simply cannot call it.
				r.logger.Warn("chat: skipping tool with an invalid parameter schema",
					zap.String("tool", s.Name), zap.Error(perr))
				continue
			}
			info.ParamsOneOf = params
		}
		infos = append(infos, info)
	}
	if len(infos) == 0 {
		return nil, nil
	}
	return infos, nil
}

// Run executes one turn, emitting events as they occur, and returns the result.
//
// The returned error is a run-level failure (the model could not be called at
// all). A failed *tool* is not a run failure: it is reported through a
// tool_result event with ToolError set, and the model gets a chance to react to
// it, which is the behaviour a user expects from an agent.
func (r *Runner) Run(ctx context.Context, req Request, emit Emitter) (*Result, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	maxSteps := req.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.maxSteps
	}

	infos, err := r.toolInfos(ctx)
	if err != nil {
		return nil, err
	}

	// Bind tools once per run. A model that does not support tool calling
	// silently degrades to pure chat rather than failing the turn.
	mdl := r.model
	if len(infos) > 0 {
		if tcm, ok := r.model.(model.ToolCallingChatModel); ok {
			bound, berr := tcm.WithTools(infos)
			if berr != nil {
				r.logger.Warn("chat: model rejected tools; continuing without them", zap.Error(berr))
			} else {
				mdl = bound
			}
		} else {
			r.logger.Warn("chat: model does not support tool calling; continuing without tools")
		}
	}

	history := append([]*schema.Message(nil), req.Messages...)
	res := &Result{}

	traceID := r.tracer.StartTrace(ctx, TraceInfo{
		Name:      "chat.turn",
		SessionID: req.SessionID,
		UserID:    req.UserID,
		Input:     lastUserText(history),
	})
	// The trace is closed exactly once, on every exit path.
	defer func() {
		r.tracer.EndTrace(ctx, traceID, res.Text)
	}()

	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			emit(Event{Type: EventError, Step: step, Error: "cancelled"})
			return res, err
		}
		res.Steps = step
		emit(Event{Type: EventStepStart, Step: step})

		msg, callUsage, genErr := r.streamOnce(ctx, mdl, history, step, traceID, emit)
		if genErr != nil {
			if errors.Is(genErr, context.Canceled) {
				emit(Event{Type: EventError, Step: step, Error: "cancelled"})
				return res, genErr
			}
			emit(Event{Type: EventError, Step: step, Error: genErr.Error()})
			return res, genErr
		}

		history = append(history, msg)

		// Accumulate usage and reasoning across every model call in the turn,
		// so the caller can bill the whole turn and the UI can show the full
		// thinking trace rather than only the last step's.
		res.Usage.PromptTokens += callUsage.PromptTokens
		res.Usage.CompletionTokens += callUsage.CompletionTokens
		res.Usage.TotalTokens += callUsage.TotalTokens
		res.Usage.DurationMs += callUsage.DurationMs
		if callUsage.TotalTokens > 0 || callUsage.DurationMs > 0 {
			u := callUsage
			emit(Event{Type: EventUsage, Step: step, Usage: &u})
		}
		if msg.ReasoningContent != "" {
			res.Reasoning += msg.ReasoningContent
		}

		if len(msg.ToolCalls) == 0 {
			res.Text = msg.Content
			emit(Event{Type: EventDone, Step: step, Text: res.Text})
			return res, nil
		}

		// Announce and run each tool the model asked for.
		for _, tc := range msg.ToolCalls {
			run := r.runTool(ctx, tc, req, step, traceID, emit)
			res.Tools = append(res.Tools, run)
			history = append(history, &schema.Message{
				Role:       schema.Tool,
				Content:    toolResultContent(run),
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
			})
		}
	}

	// Step budget exhausted: return what we have plus a clear explanation, so
	// the UI shows why it stopped instead of appearing to hang.
	res.Text = exhaustedMessage(history)
	emit(Event{Type: EventDone, Text: res.Text})
	return res, nil
}

// streamOnce performs one streaming model call, forwarding deltas as they
// arrive and returning the assembled message.
func (r *Runner) streamOnce(ctx context.Context, mdl model.BaseChatModel, history []*schema.Message,
	step int, traceID string, emit Emitter) (*schema.Message, Usage, error) {

	genID := r.tracer.StartGeneration(ctx, GenInfo{
		TraceID: traceID,
		Name:    fmt.Sprintf("step-%d", step),
		Model:   modelName(r.model),
		Input:   history,
		Step:    step,
	})

	stream, err := mdl.Stream(ctx, history)
	if err != nil {
		r.tracer.EndGeneration(ctx, genID, nil, Usage{}, err.Error())
		return nil, Usage{}, err
	}
	defer stream.Close()

	var (
		chunks    []*schema.Message
		reasoning strings.Builder
		usage     Usage
	)
	started := nowFunc()

	for {
		chunk, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			r.tracer.EndGeneration(ctx, genID, nil, usage, rerr.Error())
			return nil, usage, rerr
		}
		if chunk == nil {
			continue
		}
		chunks = append(chunks, chunk)

		// Reasoning is streamed separately from the answer so the UI can put it
		// in its own panel.
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
			emit(Event{Type: EventReasoningDelta, Step: step, Text: chunk.ReasoningContent})
		}
		if chunk.Content != "" {
			emit(Event{Type: EventTextDelta, Step: step, Text: chunk.Content})
		}
		if u := chunk.ResponseMeta; u != nil && u.Usage != nil {
			usage = Usage{
				PromptTokens:     u.Usage.PromptTokens,
				CompletionTokens: u.Usage.CompletionTokens,
				TotalTokens:      u.Usage.TotalTokens,
			}
		}
	}

	msg, cerr := schema.ConcatMessages(chunks)
	if cerr != nil {
		r.tracer.EndGeneration(ctx, genID, nil, usage, cerr.Error())
		return nil, usage, fmt.Errorf("chat: concat stream: %w", cerr)
	}
	if msg == nil {
		msg = &schema.Message{Role: schema.Assistant}
	}
	if reasoning.Len() > 0 {
		msg.ReasoningContent = reasoning.String()
	}

	usage.DurationMs = nowFunc().Sub(started).Milliseconds()
	r.tracer.EndGeneration(ctx, genID, msg.Content, usage, "")
	return msg, usage, nil
}

// runTool executes one tool call and reports it.
func (r *Runner) runTool(ctx context.Context, tc schema.ToolCall, req Request,
	step int, traceID string, emit Emitter) ToolRun {

	run := ToolRun{
		ID:   tc.ID,
		Name: tc.Function.Name,
		Args: tc.Function.Arguments,
	}
	emit(Event{
		Type: EventToolCall, Step: step,
		ToolCallID: run.ID, ToolName: run.Name, ToolArgs: run.Args,
	})

	spanID := r.tracer.StartSpan(ctx, SpanInfo{
		TraceID: traceID,
		Name:    "tool." + run.Name,
		Input:   map[string]any{"name": run.Name, "arguments": run.Args},
	})
	started := nowFunc()
	defer func() { run.DurationMs = nowFunc().Sub(started).Milliseconds() }()

	t, ok := r.toolsTool(run.Name)
	if !ok {
		run.Err = fmt.Sprintf("unknown tool %q", run.Name)
	} else if !r.tools.IsAllowed(run.Name) {
		// A disallowed tool is refused rather than silently skipped, and the
		// refusal is fed back to the model so it can choose another approach.
		run.Err = fmt.Sprintf("tool %q is not permitted", run.Name)
	} else {
		out, err := t.InvokableRun(ctx, run.Args)
		if err != nil {
			run.Err = err.Error()
		} else {
			run.Result = out
		}
	}

	r.tracer.EndSpan(ctx, spanID, toolSpanOutput(run), run.Err)
	emit(Event{
		Type: EventToolResult, Step: step,
		ToolCallID: run.ID, ToolName: run.Name,
		ToolResult: run.Result, ToolError: run.Err,
		DurationMs: run.DurationMs,
	})
	return run
}

// toolsTool looks a tool up, tolerating a nil registry.
func (r *Runner) toolsTool(name string) (tool.Tool, bool) {
	if r.tools == nil {
		return nil, false
	}
	return r.tools.Get(name)
}

// toolSpanOutput renders a tool run for tracing.
func toolSpanOutput(run ToolRun) map[string]any {
	out := map[string]any{"name": run.Name, "duration_ms": run.DurationMs}
	if run.Err != "" {
		out["error"] = run.Err
		return out
	}
	out["result"] = truncate(run.Result, 4000)
	return out
}

// toolResultContent is what the model sees as the tool's observation. A failure
// is reported as an error string so the model can adapt instead of assuming an
// empty success.
func toolResultContent(run ToolRun) string {
	if run.Err != "" {
		return "error: " + run.Err
	}
	return run.Result
}

// exhaustedMessage explains a step-budget stop, preferring the last assistant
// text when there is one so the user still sees something useful.
func exhaustedMessage(history []*schema.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == schema.Assistant && strings.TrimSpace(m.Content) != "" {
			return m.Content + "\n\n（已达到最大工具调用步数，回答可能不完整）"
		}
	}
	return "（已达到最大工具调用步数，未能得出最终回答）"
}

// lastUserText returns the most recent user message, for the trace input.
func lastUserText(history []*schema.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == schema.User {
			return truncate(history[i].Content, 2000)
		}
	}
	return ""
}

// truncate bounds a string for tracing, on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Trim to a rune boundary so tracing never emits invalid UTF-8.
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
