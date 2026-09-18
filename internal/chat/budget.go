package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

// Stop reasons. A turn that ends without the model answering reports one of
// these on Result.StopReason and on the EventBudgetStop it emits, so a caller can
// tell "the model finished" from "the budget ran out" without parsing prose.
const (
	// StopSteps is the step budget: the model asked for another tool call at the
	// cap.
	StopSteps = "steps"
	// StopTokens is the token budget. It is measured from the usage the provider
	// reports, so a provider that reports none cannot be bounded this way.
	StopTokens = "tokens"
	// StopDeadline is the wall-clock budget.
	StopDeadline = "deadline"
)

// Condenser bounds the in-loop history. It is declared here rather than taken
// from internal/context so the runner depends on the capability, not on the
// package: a deployment that configured no token budget passes nil, and a test
// passes a double. *context.Manager satisfies it.
type Condenser interface {
	// CompressKeeping returns a window that fits the configured budget, keeping
	// the first head messages and — when pinLastUser is set — the most recent
	// user message verbatim. It returns the messages unchanged when nothing
	// needed folding.
	CompressKeeping(ctx context.Context, msgs []*schema.Message, head int, pinLastUser bool) ([]*schema.Message, string, error)
}

// turnBudget is one turn's resolved limits. A zero field means that dimension is
// unlimited, which is what an unconfigured deployment gets for everything except
// the step count.
type turnBudget struct {
	maxSteps  int
	maxTokens int
	deadline  time.Duration
}

// budgetFor resolves the turn's budget: the request wins, otherwise the runner's
// configured default.
func (r *Runner) budgetFor(req Request) turnBudget {
	b := turnBudget{maxSteps: r.maxSteps, maxTokens: r.maxTokens, deadline: r.deadline}
	if req.MaxSteps > 0 {
		b.maxSteps = req.MaxSteps
	}
	if req.MaxTokens > 0 {
		b.maxTokens = req.MaxTokens
	}
	if req.Deadline > 0 {
		b.deadline = req.Deadline
	}
	if b.maxSteps <= 0 {
		b.maxSteps = DefaultMaxSteps
	}
	return b
}

// expired reports why the turn may not spend another model call, or "" when it
// may.
//
// The order is fixed — deadline, then tokens — so the same turn always reports
// the same reason. The step count is not checked here: the loop's own bound
// enforces it, and reporting it from two places is how a reason ends up
// depending on which check ran first.
//
// It is called before each step rather than after, so a budget stops the next
// call instead of explaining the last one.
func (b turnBudget) expired(started time.Time, res *Result) string {
	if b.deadline > 0 && time.Since(started) >= b.deadline {
		return StopDeadline
	}
	if b.maxTokens > 0 && res.Usage.TotalTokens >= b.maxTokens {
		return StopTokens
	}
	return ""
}

// retryShare is the longest a retry wait may be, given what is left of the
// turn's wall clock: a step that has 4s of budget left must not sleep out a 20s
// backoff and hand back a turn that is minutes past its deadline.
//
// The wait is measured from the moment the step started rather than from each
// retry, which is deliberate: the budget is enforced between steps (see expired),
// so the honest bound is "what was left when this step began", not a number that
// shrinks as attempts of unknown length consume it. An unlimited turn (deadline
// 0) gets 0, which means "the policy's own cap".
func (b turnBudget) retryShare(started time.Time) time.Duration {
	if b.deadline <= 0 {
		return 0
	}
	left := b.deadline - time.Since(started)
	if left < 0 {
		return 0
	}
	return left
}

// stopNote is the sentence that explains a budget stop: what was hit, what it
// cost, and what the user can do about it. It is deliberately specific — "the
// answer may be incomplete" without "you hit the 12-step cap" leaves the reader
// with nothing to act on.
func stopNote(reason string, b turnBudget, res *Result, elapsed time.Duration) string {
	switch reason {
	case StopTokens:
		return fmt.Sprintf("已达到本轮 token 预算 %d（已用 %d，共 %d 步）", b.maxTokens, res.Usage.TotalTokens, res.Steps)
	case StopDeadline:
		return fmt.Sprintf("已达到本轮时间上限 %s（已跑 %s，共 %d 步）", b.deadline, elapsed.Round(time.Second), res.Steps)
	default:
		return fmt.Sprintf("已达到本轮最大工具调用步数 %d", b.maxSteps)
	}
}

// stopHint tells the reader what the next move is. It is stated once, after the
// reason, and stays true: the workspace changes are already on disk, so saying
// "continue" really does carry on — what does not carry over is the tool output
// of the previous turn, which is why this does not promise a resumption.
const stopHint = "工作区里的改动已经落盘，回复「继续」可以接着做，或调大本轮的上限" +
	"（chat.max_steps / chat.turn_max_tokens / chat.turn_deadline_seconds）"

// stopText builds the answer a budget stop returns: the last assistant text the
// model produced, plus an explanation. Preferring that text is the difference
// between "stopped after 12 steps with a partial answer" and "stopped after 12
// steps with nothing", and the partial answer is usually most of the work.
func stopText(history []*schema.Message, reason string, b turnBudget, res *Result, elapsed time.Duration) string {
	note := stopNote(reason, b, res, elapsed)
	base := lastAssistantText(history)
	if base == "" {
		// No partial answer: the only honest claim about the workspace is that
		// tools ran, so that is what the note offers. With none, it stays short
		// rather than promising a continuation of work that never happened.
		if len(res.Tools) > 0 {
			return "（" + note + "。" + stopHint + "；未能得出最终回答。）"
		}
		return "（" + note + "，未能得出最终回答）"
	}
	return base + "\n\n（" + note + "。" + stopHint + "；回答可能不完整。）"
}

// lastAssistantText returns the most recent assistant message that carries
// visible text. Assistant messages that only requested tool calls have none,
// which is exactly the case a partial answer has to skip past.
func lastAssistantText(history []*schema.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == schema.Assistant && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// leadingSystem counts the system messages at the head of a window. They are the
// rules of the turn, so a compression pass pins them.
func leadingSystem(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != schema.System {
			break
		}
		n++
	}
	return n
}
