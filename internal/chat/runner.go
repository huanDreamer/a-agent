package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/retry"
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
	// MaxParallel is how many parallel-safe tool calls from one model reply may
	// run at once. 0 or 1 means one at a time, which is what every deployment got
	// before this option existed.
	//
	// It bounds I/O concurrency, not correctness: only tools that declared
	// themselves ParallelSafe may overlap at all, and the schedule still keeps the
	// model's program order (see runToolCalls).
	MaxParallel int
	// MaxTokens is the default per-turn token budget, summed from the usage the
	// provider reports. 0 (the default) means unlimited.
	//
	// It is a second bound on purpose: a step cap alone says nothing about what
	// those steps cost, and a model that loops through long tool results can
	// spend an unbounded amount inside a handful of steps.
	MaxTokens int
	// Deadline is the default per-turn wall-clock budget. 0 means unlimited.
	Deadline time.Duration
	// StepRetry is how one step is retried when its model call fails. The zero
	// value means one attempt, i.e. no retrying — the behaviour of a deployment
	// that never configured it.
	//
	// It sits at the step rather than at the model because it is the step that a
	// mid-stream failure damages: see streamStep.
	StepRetry retry.Policy
	// Condenser bounds the in-loop history when a token budget is configured.
	// Nil means the history is sent whole, which is only safe for short turns:
	// every step resends everything the turn has accumulated so far.
	Condenser Condenser
	Logger    *zap.Logger
}

// Runner drives a streaming tool-calling conversation.
type Runner struct {
	model       model.BaseChatModel
	tools       *tool.Registry
	toolsFor    func(ctx context.Context) (*tool.Registry, error)
	tracer      Tracer
	maxSteps    int
	maxParallel int
	maxTokens   int
	deadline    time.Duration
	stepRetry   retry.Policy
	condenser   Condenser
	logger      *zap.Logger
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
		model:       cfg.Model,
		tools:       cfg.Tools,
		toolsFor:    cfg.ToolsFor,
		tracer:      tracer,
		maxSteps:    max,
		maxParallel: cfg.MaxParallel,
		maxTokens:   cfg.MaxTokens,
		deadline:    cfg.Deadline,
		stepRetry:   cfg.StepRetry,
		condenser:   cfg.Condenser,
		logger:      logger,
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

// systemPromptOf returns the system message a turn was given, for the tools that
// hand it on (a subagent inherits the deployment's instructions rather than
// starting from none).
func systemPromptOf(messages []*schema.Message) string {
	for _, m := range messages {
		if m != nil && m.Role == schema.System {
			return m.Content
		}
	}
	return ""
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

	// Publish what a turn-scoped tool needs to run something of its own — the
	// registry above and the model driving this turn. Both are per-turn facts, so
	// a tool that captured them at construction would hand its subagent the wrong
	// workspace or the wrong model, and only on some deployments.
	ctx = tool.WithTurnResources(ctx, tool.TurnResources{
		Registry:     reg,
		Model:        r.model,
		SessionID:    req.SessionID,
		Scope:        req.Scope,
		SystemPrompt: systemPromptOf(req.Messages),
	})
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

		msg, callUsage, genErr := r.streamStep(ctx, mdl, history, step, traceID, budget, started, res, emit)
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
			// The last step is the one that answered: its text is the answer, and
			// it is the only step whose text is.
			res.Plan = append(res.Plan, planStep(step, msg.ReasoningContent, msg.Content))
			emit(Event{Type: EventDone, Step: step, Text: res.Text})
			return res, nil
		}

		// The step is over and it is going to act: everything it said was a
		// preamble to that action, and belongs inside the step. A reader that has
		// only the text deltas cannot tell — they arrived before the model said
		// whether it wanted a tool — so the boundary is announced here.
		emit(Event{Type: EventStepEnd, Step: step, Text: msg.Content})

		// Run the tools the model asked for: the ones that declared themselves
		// parallel-safe may overlap, and everything else is a barrier.
		//
		// The results are assembled in the order the model wrote the calls, no
		// matter what order they finished in. That is not tidiness: the messages
		// that go back to the model are matched to its tool calls by position, and
		// a plan step lists its calls in the order that explains it.
		runs := r.runToolCalls(ctx, reg, msg.ToolCalls, req, step, traceID, emit)
		var calls []ToolRun
		for i, run := range runs {
			res.Tools = append(res.Tools, run)
			calls = append(calls, run)
			history = append(history, &schema.Message{
				Role:       schema.Tool,
				Content:    toolResultContent(run),
				ToolCallID: msg.ToolCalls[i].ID,
				ToolName:   msg.ToolCalls[i].Function.Name,
			})
		}
		res.Plan = append(res.Plan, Step{
			Index:     step,
			Reasoning: msg.ReasoningContent,
			Text:      msg.Content,
			Tools:     calls,
		})
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

// streamStep performs one step's model call, retrying it while the failure looks
// like something another attempt could survive.
//
// The retry belongs to the *step* rather than to the model call because that is
// what a mid-stream failure damages. A stream that dies after the model has
// started answering cannot be retried where it failed — the caller has already
// been handed half an answer, and llm's wrapper deliberately refuses to replay
// that (see internal/llm/retry.go). Here the history is still exactly what the
// step started with, so re-running it is free of that problem: the half-answer
// belongs to the failed attempt, which produced no message at all, and the
// client is told to drop it by the step_retry event this emits first.
//
// Waits are capped by the wall clock the turn had left when the step began, so a
// retry cannot sleep past a deadline that was about to fire. The deadline itself
// stays a between-steps bound (see turnBudget.expired): no model call in flight
// is interrupted, and that includes the attempt a wait is followed by.
func (r *Runner) streamStep(ctx context.Context, mdl model.BaseChatModel, history []*schema.Message,
	step int, traceID string, budget turnBudget, started time.Time, res *Result, emit Emitter) (*schema.Message, Usage, error) {

	if r.stepRetry.Attempts() <= 1 {
		return r.streamOnce(ctx, mdl, history, step, traceID, emit)
	}

	var (
		msg   *schema.Message
		usage Usage
	)
	err := r.stepRetry.Do(ctx, retry.Op{
		Fn: func(ctx context.Context) error {
			m, u, err := r.streamOnce(ctx, mdl, history, step, traceID, emit)
			if err != nil {
				return err
			}
			msg, usage = m, u
			return nil
		},
		// Asked of the error itself rather than of the caller: *llm.LLMError
		// answers through a one-method interface, so this package never has to
		// import a provider implementation to know a 401 from a dropped socket.
		Retryable: func(err error) bool {
			if !stepRetryable(err) {
				return false
			}
			// A retry that starts after the budget expired would spend a model
			// call the turn is no longer allowed to make.
			return budget.expired(started, res) == ""
		},
		MaxDelay: budget.retryShare(started),
		OnRetry: func(info retry.Info) {
			r.logger.Warn("chat: step failed, retrying",
				zap.Int("step", step),
				zap.Int("attempt", info.Attempt),
				zap.Int("max_attempts", info.MaxAttempts),
				zap.Duration("delay", info.Delay),
				zap.Int("steps_done", res.Steps),
				zap.Error(info.Err),
			)
			emit(Event{
				Type:        EventStepRetry,
				Step:        step,
				Attempt:     info.Attempt,
				MaxAttempts: info.MaxAttempts,
				DelayMs:     info.Delay.Milliseconds(),
				Error:       info.Err.Error(),
			})
		},
	})
	if err != nil {
		return nil, Usage{}, err
	}
	return msg, usage, nil
}

// stepRetryable decides whether a failed step is worth running again.
//
// Cancellation and a deadline are never retried. Cancellation is a decision —
// the user pressed stop, or the process is going away. A deadline means either
// the turn's own context is over (nothing left to spend) or the call already
// exhausted a timeout farther down, where it was retried as many times as the
// llm policy allowed; another attempt at this level is not the answer to either.
//
// An error that knows it is permanent (a 400, a 401, an exhausted balance) is
// not retried either: the second attempt would produce the same sentence for the
// same money. Everything else is treated as transient, because from here a
// network blip and an unclean stream end look alike.
func stepRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var decided interface{ Retryable() bool }
	if errors.As(err, &decided) {
		return decided.Retryable()
	}
	return true
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
// runToolCalls runs one step's tool calls, overlapping the ones that are safe to
// overlap.
//
// The schedule comes from the tools' own declarations (see tool.Segment): a
// maximal run of parallel-safe calls becomes one group, and every other call is a
// group of its own — which is what makes a write or a command a barrier. The
// model expressed a program when it emitted them in an order, and this keeps that
// program's meaning while dropping the parts of its latency that were only ever
// waiting.
//
// The returned slice is always in the model's order, whatever order the calls
// actually completed in.
func (r *Runner) runToolCalls(ctx context.Context, reg *tool.Registry, calls []schema.ToolCall,
	req Request, step int, traceID string, emit Emitter) []ToolRun {

	runs := make([]ToolRun, len(calls))
	if len(calls) == 0 {
		return runs
	}

	modes := make([]tool.Concurrency, len(calls))
	for i, tc := range calls {
		t, ok := reg.Get(tc.Function.Name)
		if !ok {
			// An unknown tool is handled inside runTool, which reports it as the
			// observation the model needs; it cannot be classified, so it is a
			// barrier.
			modes[i] = tool.Serial
			continue
		}
		modes[i] = tool.EffectiveConcurrency(t)
	}

	limit := r.maxParallel
	if limit < 1 {
		limit = 1
	}

	for _, group := range tool.Segment(modes) {
		if len(group) == 1 || limit == 1 {
			for _, i := range group {
				runs[i] = r.runTool(ctx, reg, calls[i], req, step, traceID, emit)
			}
			continue
		}

		// A bounded fan-out rather than an errgroup: the calls are independent by
		// declaration, one failing is an observation rather than a reason to
		// abandon the others, and a bounded number in flight is what keeps "read
		// these five files" from opening five hundred descriptors.
		sem := make(chan struct{}, limit)
		var wg sync.WaitGroup
		for _, i := range group {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				// A tool that panics must not take the turn — and with it the
				// process — down. Serially a panic was fatal too, but serially
				// nobody had made ten of them happen at once.
				defer func() {
					if rec := recover(); rec != nil {
						runs[i] = ToolRun{
							ID:   calls[i].ID,
							Name: calls[i].Function.Name,
							Args: calls[i].Function.Arguments,
							Step: step,
							Err:  fmt.Sprintf("工具执行时 panic：%v", rec),
						}
						emit(Event{
							Type:       EventToolResult,
							Step:       step,
							ToolCallID: calls[i].ID,
							ToolName:   calls[i].Function.Name,
							ToolError:  runs[i].Err,
						})
					}
				}()
				runs[i] = r.runTool(ctx, reg, calls[i], req, step, traceID, emit)
			}(i)
		}
		wg.Wait()
	}
	return runs
}

func (r *Runner) runTool(ctx context.Context, reg *tool.Registry, tc schema.ToolCall, req Request,
	step int, traceID string, emit Emitter) ToolRun {

	run := ToolRun{
		ID:   tc.ID,
		Name: tc.Function.Name,
		Args: tc.Function.Arguments,
		Step: step,
	}
	emit(Event{
		Type: EventToolCall, Step: step,
		ToolCallID: run.ID, ToolName: run.Name, ToolArgs: run.Args,
	})

	// Anything this call spawns reports through the same stream, tagged with this
	// call's id. Published here because this is the only place that knows both the
	// emitter and which call is running — a tool itself knows neither.
	// The nested events are also collected here, so the call's own record carries
	// what it spawned. Without that the report reaches a caller that reads the
	// result (the one-shot command, the stored turn) as a card that spawned
	// something and never said what came of it.
	var (
		nestedMu  sync.Mutex
		nestedLog []NestedCall
	)
	ctx = tool.WithToolCallID(ctx, run.ID)
	ctx = WithNested(ctx, Nested{
		ParentCallID: run.ID,
		Emit: func(nested Event) {
			nested.ParentToolCallID = run.ID
			// A nested event is never a step of the parent's: its step number would
			// place a subagent's tool call inside the parent's plan.
			nested.Step = step
			nestedMu.Lock()
			nestedLog = appendNestedCall(nestedLog, nested)
			nestedMu.Unlock()
			emit(nested)
		},
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
	nestedMu.Lock()
	run.Nested = nestedLog
	nestedMu.Unlock()
	return run
}

// appendNestedCall folds one nested event into the call's own record.
//
// It coalesces the same way every other consumer does — consecutive deltas of one
// kind become one entry — because a per-token list is unreadable and would make the
// stored turn large for no information.
func appendNestedCall(log []NestedCall, e Event) []NestedCall {
	switch e.Type {
	case EventTextDelta, EventReasoningDelta:
		kind := "text"
		if e.Type == EventReasoningDelta {
			kind = "reasoning"
		}
		if n := len(log); n > 0 && log[n-1].Kind == kind {
			log[n-1].Text += e.Text
			return log
		}
		return append(log, NestedCall{Kind: kind, Text: e.Text})
	case EventToolCall:
		return append(log, NestedCall{Kind: "tool", Name: e.ToolName, ID: e.ToolCallID})
	case EventToolResult:
		for i := range log {
			if log[i].Kind == "tool" && log[i].ID == e.ToolCallID {
				log[i].Result = e.ToolResult
				log[i].Err = e.ToolError
				return log
			}
		}
	}
	return log
}

// toolsTool looks a tool up in the registry a turn is running against,
// tolerating a nil registry (a turn with no tools at all).
func toolsTool(reg *tool.Registry, name string) (tool.Tool, bool) {
	if reg == nil {
		return nil, false
	}
	return reg.Get(name)
}

// planStep is one iteration with no tool calls: the step that answered.
//
// Its text is kept on the step as well as on the result, so a reader of the plan
// sees a uniform shape — every step has the text it produced — rather than having
// to know that the last one is special.
func planStep(step int, reasoning, text string) Step {
	return Step{Index: step, Reasoning: reasoning, Text: text}
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
