package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// DefaultMaxSteps caps model→tool→model iterations when a Request does not say
// otherwise. It mirrors the agent package's default so the web UI and the IM
// bot behave the same.
const DefaultMaxSteps = 12

// MaxStepsCeiling is the largest step budget a Runner accepts.
//
// It is a typo guard, not a policy. With a token budget and a deadline in place
// the step count is the least interesting of the three limits, but a configured
// 1200 is far more likely to be a slip than an intention, and the turn it
// produced would be paid for before anyone read the config back. Refusing it at
// construction is the one moment the mistake is still free.
const MaxStepsCeiling = 500

// Request is one conversational turn.
type Request struct {
	// Messages is the full conversation to send, oldest first. The caller owns
	// the history (the runner never mutates it).
	Messages []*schema.Message

	// SessionID and UserID are used for tracing and audit attribution.
	SessionID string
	UserID    string

	// Scope identifies what the turn belongs to when a capability is chosen per
	// conversation rather than globally: a web chat session, or a Feishu user.
	// The runner publishes it on the turn's context (see ScopeFrom) before any
	// tool runs, so a per-scope tool set and anything a tool reports share one
	// answer to "which scope is this turn in".
	//
	// It is deliberately distinct from SessionID: a Feishu user's usage rows are
	// attributed to their memory session, while their workspace choice belongs
	// to their open_id and must survive a new session.
	//
	// Empty means "no scoped capability": the turn runs in whatever default the
	// caller wired, which is what a deployment with no scopes configured wants.
	Scope string

	// MaxSteps overrides DefaultMaxSteps.
	MaxSteps int
	// MaxTokens overrides the configured per-turn token budget. 0 means "use the
	// runner's default", which is itself 0 (unlimited) unless configured.
	MaxTokens int
	// Deadline overrides the configured per-turn wall-clock budget. 0 means the
	// same as MaxTokens: use the runner's default.
	Deadline time.Duration
}

// Config wires a Runner.
type Config struct {
	// Model is the chat model to drive. Required.
	Model model.BaseChatModel
	// Tools is the tool registry. Optional: a nil or empty registry runs in
	// pure-chat mode.
	Tools *tool.Registry
	// ToolsFor, when set, returns the registry to use for one turn, overriding
	// Tools. It is how a tool set that depends on the turn is supplied — the
	// case it exists for is a conversation bound to a workspace, whose file tools
	// must resolve inside that workspace and whose write tools must be absent
	// when it is read-only.
	//
	// It is called once per turn (not per step) with the turn's context, so the
	// registry a turn starts with is the one every tool call in it uses.
	// Returning (nil, nil) runs the turn without tools, which is the fail-safe
	// direction: falling back to a differently bound registry would be worse
	// than having none, because writes would land somewhere the caller did not
	// choose.
	ToolsFor func(ctx context.Context) (*tool.Registry, error)
	// Tracer receives trace/span/generation events. Optional; a nil Tracer
	// disables tracing.
	Tracer Tracer
	// MaxSteps is the default step cap.
	MaxSteps int
	// MaxTokens is the default per-turn token budget, summed from the usage the
	// provider reports. 0 (the default) means unlimited.
	//
	// It is a second bound on purpose: a step cap alone says nothing about what
	// those steps cost, and a model that loops through long tool results can
	// spend an unbounded amount inside a handful of steps.
	MaxTokens int
	// Deadline is the default per-turn wall-clock budget. 0 means unlimited.
	Deadline time.Duration
	// Condenser bounds the in-loop history when a token budget is configured.
	// Nil means the history is sent whole, which is only safe for short turns:
	// every step resends everything the turn has accumulated so far.
	Condenser Condenser
	Logger    *zap.Logger
}

// Runner drives a streaming tool-calling conversation.
type Runner struct {
	model     model.BaseChatModel
	tools     *tool.Registry
	toolsFor  func(ctx context.Context) (*tool.Registry, error)
	tracer    Tracer
	maxSteps  int
	maxTokens int
	deadline  time.Duration
	condenser Condenser
	logger    *zap.Logger
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
	if cfg.MaxSteps > MaxStepsCeiling {
		return nil, fmt.Errorf("chat: MaxSteps %d exceeds the ceiling of %d", cfg.MaxSteps, MaxStepsCeiling)
	}
	if cfg.MaxTokens < 0 {
		return nil, fmt.Errorf("chat: MaxTokens %d is negative", cfg.MaxTokens)
	}
	if cfg.Deadline < 0 {
		return nil, fmt.Errorf("chat: Deadline %s is negative", cfg.Deadline)
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
		model:     cfg.Model,
		tools:     cfg.Tools,
		toolsFor:  cfg.ToolsFor,
		tracer:    tracer,
		maxSteps:  max,
		maxTokens: cfg.MaxTokens,
		deadline:  cfg.Deadline,
		condenser: cfg.Condenser,
		logger:    logger,
	}, nil
}

// scopeKey is the context key carrying a turn's scope. It is unexported so the
// only way to publish one is WithScope, which is what keeps every surface's
// spelling of "a scope" identical.
type scopeKey struct{}

// WithScope returns ctx carrying the scope a turn belongs to.
//
// An empty scope returns ctx unchanged: "no scope" is the absence of the value,
// not an empty string that a reader would have to distinguish from a real one.
func WithScope(ctx context.Context, scope string) context.Context {
	if strings.TrimSpace(scope) == "" {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFrom returns the scope the current turn belongs to, or "" when the turn
// was not scoped.
func ScopeFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(scopeKey{}).(string)
	return scope
}

// resolveTools returns the registry for one turn.
func (r *Runner) resolveTools(ctx context.Context) (*tool.Registry, error) {
	if r.toolsFor == nil {
		return r.tools, nil
	}
	reg, err := r.toolsFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat: resolve tools: %w", err)
	}
	return reg, nil
}

// toolInfos returns the model-facing specs for the permitted tools. It returns
// nil when there are no tools, which selects pure-chat mode.
func (r *Runner) toolInfos(ctx context.Context, reg *tool.Registry) ([]*schema.ToolInfo, error) {
	if reg == nil {
		return nil, nil
	}
	specs, err := reg.List(ctx)
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
	budget := r.budgetFor(req)
	started := nowFunc()

	// Publish the scope before anything else runs: a registry built for this
	// turn reads it, and so does anything a tool reports about where it ran.
	ctx = WithScope(ctx, req.Scope)

	// The registry is resolved once per turn and used for every step, so a turn
	// cannot half-run against one workspace and half against another.
	reg, err := r.resolveTools(ctx)
	if err != nil {
		return nil, err
	}
	infos, err := r.toolInfos(ctx, reg)
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
	// The head is computed once, from the window the caller sent: those are the
	// rules of the turn (the system prompt and whatever it prepended to it).
	// Deriving it again per step would pin the summary this runner inserted on
	// the previous step, so every pass would pin one message more and the window
	// would creep back up to the size the condensing exists to bound.
	head := leadingSystem(history)
	res := &Result{}

	traceID := r.tracer.StartTrace(ctx, TraceInfo{
		Name:      "chat.turn",
		SessionID: req.SessionID,
		UserID:    req.UserID,
		Input:     lastUserText(history),
	})
	// The trace id leaves with the result: the caller stores it alongside the
	// answer, which is what lets the conversation link to its own trace. The
	// runner is the only place that ever knows it.
	res.TraceID = traceID
	// The trace is closed exactly once, on every exit path.
	defer func() {
		r.tracer.EndTrace(ctx, traceID, res.Text)
	}()

	for step := 1; step <= budget.maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			emit(Event{Type: EventError, Step: step, Error: "cancelled"})
			return res, err
		}
		// The budgets that are not the step count are checked before spending
		// another model call, not after: the point of a budget is to stop before
		// the money is spent, and a step that ran is already paid for.
		if reason := budget.expired(started, res); reason != "" {
			return r.stopOnBudget(req, res, emit, history, reason, budget, started)
		}
		// Bound the window before the model sees it. Every step adds an assistant
		// message and its tool results, so an unbounded loop resends a growing
		// history: the cost per step rises while the answer is still being
		// worked out, which is what turns "a long task" into a context-limit
		// error twenty steps in.
		history = r.condense(ctx, res, emit, history, head)

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
			run := r.runTool(ctx, reg, tc, req, step, traceID, emit)
			res.Tools = append(res.Tools, run)
			history = append(history, &schema.Message{
				Role:       schema.Tool,
				Content:    toolResultContent(run),
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
			})
		}
	}

	// The step budget is exhausted: the loop's own bound is the check, so
	// reaching here means the model asked for another tool call at the cap.
	return r.stopOnBudget(req, res, emit, history, StopSteps, budget, started)
}

// stopOnBudget ends the turn with what it has, plus a reason the caller and the
// user can act on.
//
// It is a normal end of the turn, not a failure: Run returns a nil error, the
// last event is EventDone, and several steps of real work (files written, tests
// run) are already on disk. The one thing it must never be is silent, which is
// why it emits EventBudgetStop, logs, and sets Result.StopReason — a budget stop
// that only shows up as a sentence inside the answer cannot be counted,
// alerted on, or seen again after a page reload.
func (r *Runner) stopOnBudget(req Request, res *Result, emit Emitter,
	history []*schema.Message, reason string, budget turnBudget, started time.Time) (*Result, error) {

	elapsed := time.Since(started)
	res.StopReason = reason
	res.Text = stopText(history, reason, budget, res, elapsed)

	r.logger.Warn("chat: turn stopped on its budget",
		zap.String("reason", reason),
		zap.Int("steps", res.Steps),
		zap.Int("tokens", res.Usage.TotalTokens),
		zap.Duration("elapsed", elapsed),
		zap.Int("tools", len(res.Tools)),
		zap.String("session", req.SessionID),
	)

	emit(Event{
		Type:      EventBudgetStop,
		Step:      res.Steps,
		Reason:    reason,
		Tokens:    res.Usage.TotalTokens,
		ElapsedMs: elapsed.Milliseconds(),
		Text:      res.Text,
	})
	emit(Event{Type: EventDone, Step: res.Steps, Text: res.Text})
	return res, nil
}

// condense bounds the in-loop history to the configured token budget.
//
// What it pins is the point of it: the system prompt states the rules and the
// most recent user message states what was asked, so both survive verbatim while
// the middle — the tool exchanges of the steps already taken — is folded into a
// summary. A window that drops either is a turn that forgot its instructions or
// its goal, which is a worse failure than a large prompt.
//
// A failure to condense is not a failure of the turn: the model call may still
// succeed with a bigger window, and aborting a long task over a summary call
// would be the wrong trade.
func (r *Runner) condense(ctx context.Context, res *Result, emit Emitter, history []*schema.Message, head int) []*schema.Message {
	if r.condenser == nil {
		return history
	}
	before := len(history)

	out, summary, err := r.condenser.CompressKeeping(ctx, history, head, true)
	if err != nil {
		r.logger.Warn("chat: condensing the history failed; sending it whole",
			zap.Int("step", res.Steps+1), zap.Int("messages", before), zap.Error(err))
		return history
	}
	if len(out) == before && summary == "" {
		return history
	}

	r.logger.Info("chat: condensed the in-loop history",
		zap.Int("step", res.Steps+1),
		zap.Int("messages_before", before),
		zap.Int("messages_after", len(out)),
		zap.Int("tokens_so_far", res.Usage.TotalTokens),
	)
	emit(Event{
		Type:   EventContextCompressed,
		Step:   res.Steps + 1,
		Tokens: res.Usage.TotalTokens,
		Text:   fmt.Sprintf("上下文已压缩：%d 条消息 → %d 条（保留系统提示与本轮目标）", before, len(out)),
	})
	return out
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
func (r *Runner) runTool(ctx context.Context, reg *tool.Registry, tc schema.ToolCall, req Request,
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

	t, ok := toolsTool(reg, run.Name)
	if !ok {
		run.Err = fmt.Sprintf("unknown tool %q", run.Name)
	} else if !reg.IsAllowed(run.Name) {
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

	// Measure before reporting. A deferred assignment here would run after the
	// emit, so every tool would be reported as taking 0ms — which is what the
	// UI's 耗时 column and the audit log would then faithfully record.
	run.DurationMs = nowFunc().Sub(started).Milliseconds()

	r.tracer.EndSpan(ctx, spanID, toolSpanOutput(run), run.Err)
	emit(Event{
		Type: EventToolResult, Step: step,
		ToolCallID: run.ID, ToolName: run.Name,
		ToolResult: run.Result, ToolError: run.Err,
		DurationMs: run.DurationMs,
	})
	return run
}

// toolsTool looks a tool up in the registry a turn is running against,
// tolerating a nil registry (a turn with no tools at all).
func toolsTool(reg *tool.Registry, name string) (tool.Tool, bool) {
	if reg == nil {
		return nil, false
	}
	return reg.Get(name)
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
