// Package subagent runs a nested agent loop and hands back one bounded report.
//
// What it buys is **context isolation, not compute**. Exploring a codebase to
// answer one question means twenty greps and thirty file reads; in the parent's
// window those fifty tool results stay for the rest of the turn and crowd out the
// code being changed. A subagent buys a window that is allowed to get dirty, and
// pays for it with one more model call sequence.
//
// Three properties shape everything here, and each exists because its absence
// turns a useful tool into a budget landmine:
//
//   - **The report is bounded.** Only the final text (truncated) and a structured
//     footer cross back. The nested tool results and reasoning do not — if they
//     did, there would be no point to any of it.
//   - **The capability is narrowed by default.** A subagent gets the parent's
//     read-only tools and nothing else; writes and commands have to be asked for
//     explicitly. "Explore and tell me" is the use case, and the safe default has
//     to match the common case.
//   - **The cost is the parent's cost.** Tokens and wall-clock count against the
//     parent turn's budget and share its deadline. A subagent that could outlive
//     the turn would make chat.turn_deadline_seconds and chat.turn_max_tokens
//     untrue, which is worse than not having subagents.
package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
)

// Runner runs one nested turn and returns what it produced.
//
// It is an interface so the policy above — limits, narrowing, truncation, budget —
// is written once, and each loop supplies only "how do I run a turn". The web
// console's loop satisfies it directly (see ChatRunner); Eino's ReAct loop can
// implement it the same way.
type Runner interface {
	// RunNested runs a self-contained turn: the messages given, the registry
	// given, and nothing else. It must not read the parent's history, and must not
	// publish its events as the parent's.
	RunNested(ctx context.Context, in NestedRequest) (NestedResult, error)
}

// NestedRequest is one nested turn.
type NestedRequest struct {
	// Messages are the entire conversation: a system prompt and the task.
	Messages []*schema.Message
	// Tools is the narrowed registry for this nested run.
	Tools *tool.Registry
	// Model drives the nested run. It is the parent turn's model unless the caller
	// asked for a different one.
	Model model.BaseChatModel
	// MaxSteps bounds the nested loop.
	MaxSteps int
	// SessionID and Scope keep the nested run attributable in the traces and the
	// usage table. They are the parent's, deliberately: a subagent's cost belongs
	// to the turn that asked for it.
	SessionID string
	Scope     string
}

// NestedResult is what a nested turn produced.
type NestedResult struct {
	Text string
	// Reasoning is kept for the log, not for the report: the parent's reader can
	// see what the subagent thought on its card, and the model never does.
	Reasoning string
	// Tools names what the nested run called, in order, for the footer.
	Tools []string
	// Steps is how many iterations ran.
	Steps int
	// StepsUnavailable reports that the runner cannot say how many iterations ran.
	//
	// It exists because the alternative is a lie with consequences: Eino's ReAct
	// loop returns only the final message, so a runner over it knows neither the
	// step count nor which tools ran. Reporting zero steps for a subagent that
	// worked for a minute reads as "it did nothing", and a parent model that
	// believes that will distrust a perfectly good report — which is exactly what
	// happened the first time this ran on the CLI.
	StepsUnavailable bool
	// Usage is the nested run's own usage, for the parent's budget.
	Usage Usage
	// StopReason is empty when the nested model answered on its own.
	StopReason string
}

// Usage is token accounting, mirrored here so this package does not depend on the
// chat package's types.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
}

// Limits bounds one subagent.
type Limits struct {
	// MaxSteps bounds the nested loop's iterations.
	MaxSteps int
	// MaxConcurrent bounds how many subagents may run at once across the whole
	// process. It is shared, not per parent: "spawn three explorations" is the
	// intended use, and four parents each spawning four is how a fan-out becomes a
	// bill.
	MaxConcurrent int
	// MaxReportChars truncates the report's text.
	MaxReportChars int
	// Timeout bounds one subagent, before the parent's own deadline is applied.
	Timeout time.Duration
}

// StepsUnknown is the step count a run reports when its runner cannot count them.
//
// It exists so a surface can tell "zero steps" from "nobody counted": Eino's ReAct
// loop returns only the final message, and a failed run never returns a count at
// all. A header that showed 0 for either would be stating something it does not
// know, in the one place a reader looks to judge whether the work happened.
const StepsUnknown = -1

// Defaults for Limits.
const (
	DefaultMaxSteps       = 8
	DefaultMaxConcurrent  = 2
	DefaultMaxReportChars = 8000
)

// Or returns the limits with defaults applied.
func (l Limits) Or() Limits {
	if l.MaxSteps <= 0 {
		l.MaxSteps = DefaultMaxSteps
	}
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = DefaultMaxConcurrent
	}
	if l.MaxReportChars <= 0 {
		l.MaxReportChars = DefaultMaxReportChars
	}
	return l
}

// Report is what a subagent hands back.
type Report struct {
	// Text is the bounded report, ready to hand to the parent model.
	Text string
	// Name is the display name, for the log and the card.
	Name string
	// RunID identifies this run in the tracker, so a surface can tie a report to
	// the entry in its list.
	RunID string
	// Tools names what the nested run called.
	Tools []string
	// Steps, Usage and StopReason describe the nested run.
	Steps      int
	Usage      Usage
	StopReason string
	// StepsUnavailable reports that the runner cannot say how many iterations ran
	// or which tools were used, so the footer says that instead of "0 步".
	StepsUnavailable bool
	// Truncated reports whether Text was cut down.
	Truncated bool
	// Failed is set when the nested run failed; Text then carries the reason and
	// the parent carries on.
	Failed bool
}

// Agent spawns subagents for one parent process.
type Agent struct {
	runner  Runner
	limits  Limits
	logger  Logger
	tracker *Tracker

	// gate bounds how many nested runs are in flight at once, process-wide.
	gate chan struct{}
}

// Logger is the slice of logging this package needs.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
}

// New builds an Agent.
func New(runner Runner, limits Limits, logger Logger) (*Agent, error) {
	return NewWithTracker(runner, limits, logger, NewTracker(0))
}

// NewWithTracker builds an Agent that records its runs in tracker.
//
// The tracker is shared by every Agent in a process on purpose: the console's
// header answers "what did this conversation delegate", and a per-agent list would
// show it only the subagents that came through one surface.
func NewWithTracker(runner Runner, limits Limits, logger Logger, tracker *Tracker) (*Agent, error) {
	if runner == nil {
		return nil, fmt.Errorf("subagent: a runner is required")
	}
	limits = limits.Or()
	if tracker == nil {
		tracker = NewTracker(0)
	}
	return &Agent{
		runner:  runner,
		limits:  limits,
		logger:  logger,
		tracker: tracker,
		gate:    make(chan struct{}, limits.MaxConcurrent),
	}, nil
}

// Tracker exposes the run list, for the surface that displays it.
func (a *Agent) Tracker() *Tracker {
	if a == nil {
		return nil
	}
	return a.tracker
}

// Limits reports the bounds in force, so a header can say what the cap is.
func (a *Agent) Limits() Limits {
	if a == nil {
		return Limits{}.Or()
	}
	return a.limits
}

// Options is one spawn request.
type Options struct {
	// Prompt is the task. It is the only thing the subagent is told.
	Prompt string
	// Name is the display name. Empty uses a generated one.
	Name string
	// Tools names capabilities to grant beyond the read-only set. A name the
	// parent does not have is refused rather than ignored.
	Tools []string
	// Model overrides the model for this subagent (a cheap model for exploration
	// is the main use). Nil uses the parent's.
	Model model.BaseChatModel
	// MaxSteps overrides the configured step cap for this spawn.
	MaxSteps int
	// Timeout overrides the configured timeout for this spawn. It is still capped
	// by the parent turn's deadline.
	Timeout time.Duration
	// ParentCallID is the tool call that asked for this run, so the tracker's entry
	// can be tied to the card in the conversation.
	ParentCallID string
	// Parent is what the turn published: the registry and model to derive from,
	// plus the session, scope and system prompt the nested run inherits.
	//
	// All of it comes from one place rather than being repeated here, because a
	// caller that set one and not the other would produce a nested run with the
	// right tools and no attribution — which is exactly the kind of half-wiring a
	// duplicated option invites.
	Parent tool.TurnResources
}

// Spawn runs one subagent and returns its report.
//
// It never returns an error for a failing subagent: a failure is a report that
// says so, because the model asked a question and "the helper could not answer"
// is an answer it can act on. An error here means the machinery failed.
func (a *Agent) Spawn(ctx context.Context, opts Options) (Report, error) {
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return Report{}, fmt.Errorf("subagent: prompt is required")
	}
	if opts.Parent.Registry == nil || opts.Parent.Model == nil {
		return Report{}, fmt.Errorf("subagent: 这一轮没有可用的工具注册表或模型（spawn_agent 需要在一次正常轮次内调用）")
	}

	narrowed, err := Narrow(opts.Parent.Registry, opts.Tools)
	if err != nil {
		return Report{}, err
	}
	if narrowed.Empty() {
		return Report{}, fmt.Errorf("subagent: 可授予的工具为空，子 agent 什么也做不了")
	}

	// The gate is taken by the caller, not by the nested run: a subagent that
	// waits for a slot should wait before its own timeout starts ticking.
	select {
	case a.gate <- struct{}{}:
		defer func() { <-a.gate }()
	case <-ctx.Done():
		return Report{}, fmt.Errorf("subagent: %w", ctx.Err())
	}

	maxSteps := a.limits.MaxSteps
	if opts.MaxSteps > 0 && opts.MaxSteps < maxSteps {
		maxSteps = opts.MaxSteps
	}

	// The timeout is capped by what is left of the parent turn: a subagent that
	// outlived its turn would make the turn's deadline untrue.
	runCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	mdl := opts.Model
	if mdl == nil {
		mdl = opts.Parent.Model
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = shortName(prompt)
	}

	// Record the run before it starts, so a surface can show it while it works —
	// which is the whole point of the list: a delegation that becomes visible only
	// after it finishes answers the wrong question.
	run := a.tracker.Start(opts.Parent.SessionID, name, prompt, opts.ParentCallID)

	start := time.Now()
	res, err := a.runner.RunNested(runCtx, NestedRequest{
		Messages: []*schema.Message{
			{Role: schema.System, Content: nestedSystemPrompt(opts.Parent.SystemPrompt)},
			schema.UserMessage(prompt),
		},
		Tools:     narrowed,
		Model:     mdl,
		MaxSteps:  maxSteps,
		SessionID: opts.Parent.SessionID,
		Scope:     opts.Parent.Scope,
	})

	report := Report{Name: name, Usage: res.Usage, RunID: run.ID}
	if report.Usage.DurationMs == 0 {
		report.Usage.DurationMs = time.Since(start).Milliseconds()
	}
	if err != nil {
		// Steps stay unknown rather than becoming zero: the runner returned an error
		// instead of a result, so its step count was never reported — and a failed
		// run shown as "0 步" reads as "it did nothing" when it may have worked for a
		// minute before failing. The first end-to-end run of the header showed
		// exactly that.
		a.tracker.Finish(run.ID, Finish{Failed: true, Error: err.Error(), Steps: StepsUnknown})
		// A failed subagent is an observation, not a turn failure: the parent keeps
		// running and decides what to do about it.
		report.Failed = true
		report.Text = fmt.Sprintf("子 agent「%s」没有跑完：%v\n它没有返回结论，可以改用更小的任务重试，或者自己直接做。", name, err)
		if a.logger != nil {
			a.logger.Warn("subagent failed", "name", name, "error", err.Error())
		}
		return report, nil
	}

	report.Steps = res.Steps
	report.StepsUnavailable = res.StepsUnavailable
	report.Tools = res.Tools
	report.StopReason = res.StopReason
	report.Text, report.Truncated = bound(res.Text, a.limits.MaxReportChars)
	if strings.TrimSpace(report.Text) == "" {
		report.Text = fmt.Sprintf("子 agent「%s」没有给出结论（可能是任务太大，或者它把话都花在工具调用上了）。", name)
	}
	report.Text += footer(report)

	steps := report.Steps
	if report.StepsUnavailable {
		// The header shows "不可得" for this rather than a zero that reads as "it did
		// nothing".
		steps = StepsUnknown
	}
	a.tracker.Finish(run.ID, Finish{
		Failed:     false,
		Steps:      steps,
		Tokens:     report.Usage.TotalTokens,
		StopReason: report.StopReason,
	})

	if a.logger != nil {
		a.logger.Debug("subagent finished",
			"name", name, "steps", report.Steps, "tools", len(report.Tools),
			"tokens", report.Usage.TotalTokens, "stop_reason", report.StopReason,
			"truncated", report.Truncated)
	}
	return report, nil
}

// bound truncates a report and says so.
//
// Truncation is reported for the same reason the file tools report it: a model
// that does not know its input was cut will read the last line as the conclusion.
func bound(text string, maxChars int) (string, bool) {
	if maxChars <= 0 || len([]rune(text)) <= maxChars {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:maxChars]), true
}

// footer is the structured tail: what the report cost and how it was produced, so
// the parent can judge how much to trust it.
func footer(r Report) string {
	var b strings.Builder
	b.WriteString("\n\n---\n")
	switch {
	case r.StepsUnavailable:
		fmt.Fprintf(&b, "子 agent「%s」：步数与工具明细不可得（这条路上跑的是 Eino 的 ReAct 循环，"+
			"只返回最终消息）", r.Name)
	default:
		fmt.Fprintf(&b, "子 agent「%s」：%d 步", r.Name, r.Steps)
	}
	if len(r.Tools) > 0 && !r.StepsUnavailable {
		counts := map[string]int{}
		order := make([]string, 0, len(r.Tools))
		for _, name := range r.Tools {
			if counts[name] == 0 {
				order = append(order, name)
			}
			counts[name]++
		}
		parts := make([]string, 0, len(order))
		for _, name := range order {
			parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
		}
		fmt.Fprintf(&b, "，用了 %s", strings.Join(parts, "、"))
	}
	fmt.Fprintf(&b, "，%d tokens", r.Usage.TotalTokens)
	if r.StopReason != "" {
		fmt.Fprintf(&b, "，因预算提前结束（%s）", r.StopReason)
	}
	if r.Truncated {
		b.WriteString("；报告过长已截断")
	}
	b.WriteString("。")
	return b.String()
}

// shortName derives a display name from the task, for the log and the card.
func shortName(prompt string) string {
	prompt = strings.Join(strings.Fields(prompt), " ")
	if prompt == "" {
		return "subagent"
	}
	runes := []rune(prompt)
	if len(runes) > 24 {
		return string(runes[:24]) + "…"
	}
	return prompt
}

// nestedSystemPrompt wraps the parent's prompt with the rules that make a
// subagent a subagent.
//
// The parent's prompt is included because a subagent is the same agent with a
// narrower job and a narrower tool set — telling it nothing about how this
// deployment expects work to be done would make it a different, worse agent.
func nestedSystemPrompt(parent string) string {
	var b strings.Builder
	b.WriteString("# 你是一个子 agent\n\n")
	b.WriteString("你被派来做一件事，做完就把结论交回去。你**看不到**发起你的那一轮的对话历史，")
	b.WriteString("也没有人可以问你问题——没有提问工具，也没有人工确认。\n\n")
	b.WriteString("规则：\n")
	b.WriteString("- **只做被交代的那件事。** 不要顺手改别的东西，不要扩大范围。\n")
	b.WriteString("- **结论要能独立看懂。** 你交回去的这段文字是对方能看到的全部，所以把「结论 + 依据（文件:行号）+ 不确定的地方」写清楚。\n")
	b.WriteString("- **不要复述你的过程。** 工具调用的细节对方看不到也不关心，只写结论和依据。\n")
	b.WriteString("- 你的工具被收窄过（默认只有只读工具）。需要写或执行而没有被授予时，在结论里说明你做不到，不要绕。\n")
	b.WriteString("- 长度控制在几百字以内：这份报告会占用对方的上下文。\n")
	if p := strings.TrimSpace(parent); p != "" {
		b.WriteString("\n以下是这个部署给主 agent 的说明，同样适用于你：\n\n")
		b.WriteString(p)
	}
	return b.String()
}

// Narrow derives a subagent's registry from the parent's.
//
// The default is the parent's **read-only, parallel-safe** tools, and the caller
// can add names explicitly. Three things are never granted, whatever is asked for:
//
//   - `ask_user`: there is no channel from a subagent back to a person, so a card
//     could only hang.
//   - `plan_*`: the plan belongs to the top-level turn. A subagent editing it
//     would move a task board the user is watching on behalf of work they cannot
//     see.
//   - `spawn_agent`: depth is capped at one. Recursion needs a global budget tree,
//     which is a different change.
//
// And a granted write or command tool still passes through the parent's own
// policies (approval, checkpoints, the workspace sandbox), because the registry
// these tools were wrapped in is where those live.
func Narrow(parent *tool.Registry, extra []string) (*tool.Registry, error) {
	if parent == nil {
		return tool.NewRegistry(), nil
	}
	out := tool.NewRegistry()

	// Everything the parent permits, filtered down.
	ctx := context.Background()
	specs, err := parent.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("subagent: list parent tools: %w", err)
	}
	granted := make(map[string]bool, len(extra))
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if forbiddenForSubagents(name) {
			return nil, fmt.Errorf("subagent: %s 不能授予子 agent（%s）", name, forbiddenReason(name))
		}
		if _, ok := parent.Get(name); !ok {
			return nil, fmt.Errorf("subagent: 无法授予 %q：这一轮没有这个工具", name)
		}
		granted[name] = true
	}

	for _, spec := range specs {
		tl, ok := parent.Get(spec.Name)
		if !ok {
			continue
		}
		if forbiddenForSubagents(spec.Name) {
			continue
		}
		if !granted[spec.Name] && !mayBeInherited(tl) {
			continue
		}
		if err := out.Register(tl); err != nil {
			// A name that cannot be registered here is skipped rather than fatal:
			// the parent's set is the thing that must stay intact.
			continue
		}
	}
	return out, nil
}

// mayBeInherited reports whether a tool comes along without being asked for.
//
// The test is capability **and** concurrency, and both halves matter:
//
//   - Read-only, because "explore and tell me" is the use case and a subagent that
//     can write without being asked is a subagent nobody asked to write.
//   - Parallel-safe, because a read-only tool that must not overlap anything is
//     one that parks the turn (ask_user) or mutates shared state (plan_update,
//     save_document). Handing that to a nested run reproduces the same problem one
//     level down, where it is harder to see.
func mayBeInherited(tl tool.Tool) bool {
	if CapabilityOfForSubagent(tl) != tool.CapRead {
		return false
	}
	return tool.EffectiveConcurrency(tl) == tool.ParallelSafe
}

// CapabilityOfForSubagent is tool.CapabilityOf, named so the rule above reads as
// one sentence.
func CapabilityOfForSubagent(tl tool.Tool) tool.Capability { return tool.CapabilityOf(tl) }

// forbiddenForSubagents names what a subagent never gets.
func forbiddenForSubagents(name string) bool {
	switch name {
	case "ask_user", "spawn_agent":
		return true
	}
	return strings.HasPrefix(name, "plan_")
}

func forbiddenReason(name string) string {
	switch {
	case name == "ask_user":
		return "子 agent 没有通向人的通道，卡片只会一直挂着"
	case name == "spawn_agent":
		return "深度上限是 1，递归需要全局预算树"
	default:
		return "计划属于顶层轮次，子 agent 改看板会让用户看到不属于自己的进度"
	}
}

// Gate exposes the concurrency gate's current occupancy, for tests and metrics.
func (a *Agent) Gate() int { return len(a.gate) }

// SpawnMany runs several subagents at once and returns their reports in order.
//
// This is what makes "explore three subsystems in parallel" work on every surface,
// including the two whose loop runs a reply's tool calls one at a time (the CLI and
// the bot drive Eino's ReAct loop, which is sequential by design). The fan-out is
// inside the tool, so it does not depend on the loop's own scheduling — and it goes
// through the same process-wide gate, so "parallel" never means "unbounded".
//
// A failing task does not cancel its siblings: the caller asked several questions
// and should see all the answers. Each task's failure is reported in its own report.
func (a *Agent) SpawnMany(ctx context.Context, opts []Options) ([]Report, error) {
	if len(opts) == 0 {
		return nil, fmt.Errorf("subagent: 没有要执行的任务")
	}
	reports := make([]Report, len(opts))
	var wg sync.WaitGroup

	for i := range opts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			report, err := a.Spawn(ctx, opts[i])
			if err != nil {
				// A machinery failure (no registry, no tools to grant) is reported as
				// that task's failure rather than cancelling the batch: the others may
				// well be fine.
				report = Report{
					Name:   shortName(opts[i].Prompt),
					Failed: true,
					Text:   fmt.Sprintf("这个子 agent 没能启动：%v", err),
				}
			}
			reports[i] = report
		}(i)
	}
	wg.Wait()
	return reports, nil
}
