package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// The resume brief: what a new turn is told about the one that died.
//
// It exists because the alternative — the user repeats themselves and the model
// starts over — throws away everything the failed turn actually accomplished.
// The work that survives an interruption lives in three places, and all three
// are cheap to hand over: the plan (what was intended and what is done), the
// stored steps (what was attempted and what the tools answered), and the
// workspace itself (where the changes already are).
//
// It is text rather than a replayed tool exchange on purpose: a tool message is
// only valid as the answer to a specific tool_call, and a turn that died
// mid-step has calls without results — sending those is exactly what providers
// reject. A brief is what remains true without pretending the exchange was
// complete.
const (
	// resumeBriefMaxRunes bounds the whole brief. It is a prompt, not an archive:
	// past a few kilobytes the marginal step adds cost and noise, and the model
	// is told to re-read what it needs.
	resumeBriefMaxRunes = 6000
	// resumeToolResultMaxRunes bounds one tool observation quoted into the brief.
	resumeToolResultMaxRunes = 800
	// resumeToolArgsMaxRunes bounds one tool call's arguments.
	resumeToolArgsMaxRunes = 200
	// resumeStepTextMaxRunes bounds one step's own text.
	resumeStepTextMaxRunes = 300
	// resumeAnswerMaxRunes bounds the quoted tail of the previous answer.
	resumeAnswerMaxRunes = 1500
)

// handleResumeTurn continues the conversation's last turn from where it stopped.
//
// The reader reaches this by pressing 继续执行 on the board (or on a failed
// answer), which is a deliberate act: it spends money, and the decision to keep
// going belongs to whoever is paying. What the endpoint guarantees is not that
// the task will finish, but that the model will not have to work out what it
// already did — see resumeBrief.
func (s *Server) handleResumeTurn(ctx context.Context, c *app.RequestContext) {
	sess, err := s.store.GetChatSession(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "get chat session", err)
		return
	}
	if s.turns.running(sess.ID) {
		c.JSON(http.StatusConflict, map[string]string{
			"error": "这个对话已有一轮正在生成，请等它结束或先停止它",
		})
		return
	}

	msgs, err := s.store.ListChatMessages(ctx, sess.ID, 0)
	if err != nil {
		s.fail(c, "list chat messages", err)
		return
	}
	plan := s.sessionPlan(ctx, sess.ID)
	prev := lastAssistantMessage(msgs)
	if !resumable(prev, plan) {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": "这个对话没有需要继续的任务：上一轮正常结束了，计划里也没有未完成的条目",
		})
		return
	}

	goal := lastUserText(msgs)
	reason := interruptionReason(prev)
	brief := buildResumeBrief(resumeBriefInput{
		goal:      goal,
		plan:      plan,
		prev:      prev,
		workspace: s.sessionWorkspaceName(ctx, sess),
	})

	// The visible half: one line in the transcript saying the user asked to carry
	// on. The brief above is deliberately *not* stored — a wall of internal state
	// rendered as if the user had typed it would be worse than useless to a
	// reader scrolling back through the conversation.
	userText := resumeUserText(plan)
	history, err := s.buildHistoryWithContext(ctx, sess,
		[]*schema.Message{{Role: schema.User, Content: brief}},
		&schema.Message{Role: schema.User, Content: userText})
	if err != nil {
		s.fail(c, "build chat history", err)
		return
	}
	// SessionStart, source "resume" — the value Claude Code reports when a
	// session is continued with --resume/--continue, which is what 继续执行 is.
	// What it injects is added to the context the brief already carries, so a
	// hook that restates the working rules reaches the model on the turn that
	// needs them most.
	if s.claudeCode != nil {
		s.sessionStartHooks(ctx, sess, "resume")
		history = insertHookContext(history, s.claudeCode.SessionContext(sess.ID))
		// A resumed turn is a new prompt as far as the protocol is concerned: it
		// has no UserPromptSubmit of its own, so the id is minted here and travels
		// with the tool and Stop events that follow.
		s.claudeCode.SetPromptID(sess.ID, uuid.NewString())
	}

	s.logger.Info("chat: resuming an interrupted turn",
		zapString("session", sess.ID),
		zapString("reason", reason),
		zap.Int("unfinished", unfinishedTasks(plan)),
		zap.Int("messages", len(msgs)),
	)

	// newTask is false: this turn continues the one that died, so its plan is the
	// context being handed over rather than a leftovers to sweep up.
	s.launchTurn(ctx, c, sess, store.ChatMessage{Role: store.RoleUser, Content: userText}, history, false)
}

// resumable reports whether there is anything to carry on with.
//
// Two things qualify, and they are different situations: a turn that did not
// finish (an error, or a budget that ran out) and a plan with work left in it.
// A conversation whose last turn answered normally and whose plan is complete has
// nothing to resume, and saying so is better than starting a turn that would ask
// the model to "continue" a finished job.
func resumable(prev *store.ChatMessage, plan *tool.Plan) bool {
	if unfinishedTasks(plan) > 0 {
		return true
	}
	return prev != nil && (prev.Error != "" || prev.StopReason != "")
}

// unfinishedTasks counts the plan's open work, with no plan counting as none.
func unfinishedTasks(plan *tool.Plan) int {
	if plan == nil {
		return 0
	}
	return plan.Unfinished()
}

// resumeUserText is the line the transcript shows for a resume.
func resumeUserText(plan *tool.Plan) string {
	if n := unfinishedTasks(plan); n > 0 {
		return fmt.Sprintf("继续执行（还有 %d 项未完成）", n)
	}
	return "继续执行"
}

// lastAssistantMessage returns the most recent stored assistant turn, which is
// the one that either failed or left work behind.
func lastAssistantMessage(msgs []store.ChatMessage) *store.ChatMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == store.RoleAssistant {
			return &msgs[i]
		}
	}
	return nil
}

// lastUserText returns the most recent message the human wrote. It is the goal
// of the task being resumed: everything the plan says is in service of it.
func lastUserText(msgs []store.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != store.RoleUser {
			continue
		}
		if text := strings.TrimSpace(msgs[i].Content); text != "" {
			return text
		}
	}
	return ""
}

// interruptionReason renders why the previous turn stopped, for the brief.
func interruptionReason(prev *store.ChatMessage) string {
	if prev == nil {
		return "上一轮的记录不完整（可能被服务重启打断）"
	}
	switch {
	case prev.Error != "":
		return prev.Error
	case prev.StopReason != "":
		return "达到本轮预算而停止：" + prev.StopReason
	default:
		return "上一轮没有留下完成标记"
	}
}

// resumeBriefInput is everything the brief is built from.
type resumeBriefInput struct {
	goal      string
	plan      *tool.Plan
	prev      *store.ChatMessage
	workspace string
}

// buildResumeBrief renders the context a resumed turn starts with.
//
// The order of the sections is the order the model needs them in: what it is
// trying to achieve, why the last attempt stopped, what the plan says is left,
// what was already done, and what the last attempt had said when it died. Each
// section is dropped when it has nothing to say — an empty heading teaches the
// model to skim the ones that matter.
func buildResumeBrief(in resumeBriefInput) string {
	var b strings.Builder
	b.WriteString("[接续执行] 上一轮任务没有跑完，现在从中断处继续。已经完成的工作不要重做。\n\n")

	if goal := strings.TrimSpace(in.goal); goal != "" {
		b.WriteString("【原始目标】\n" + truncateRunes(goal, 800) + "\n\n")
	}
	b.WriteString("【中断原因】\n" + interruptionReason(in.prev) + "\n\n")

	if in.plan != nil && !in.plan.Empty() {
		b.WriteString("【计划现状】\n" + in.plan.Render() + "\n\n")
	} else {
		b.WriteString("【计划现状】\n（还没有计划：请先用 plan_create 把剩下的工作立成清单，再逐步执行）\n\n")
	}

	if prev := in.prev; prev != nil {
		if steps := renderStoredSteps(prev.Steps); steps != "" {
			b.WriteString("【上一轮已经做过的步骤】\n" + steps + "\n\n")
		}
		if tail := strings.TrimSpace(prev.Content); tail != "" {
			b.WriteString("【上次中断前最后的输出（可能被截断）】\n" +
				truncateRunes(tail, resumeAnswerMaxRunes) + "\n\n")
		}
	}
	if ws := strings.TrimSpace(in.workspace); ws != "" {
		b.WriteString("【工作区】" + ws + "（已经改动过的文件就在这里，不要从头重写）\n\n")
	}

	b.WriteString("请先调用 plan_read 确认计划的当前状态，然后从第一个未完成的任务继续；" +
		"已经标成 done 的步骤不要重复执行。如果上面的信息不足以判断当前真实状态，" +
		"重新执行必要的只读命令（读文件、看 git status、跑一次只读检查）来确认——" +
		"但不要重放有副作用的操作（不要重复写文件、重复执行已经执行过的命令）。")

	return truncateRunes(b.String(), resumeBriefMaxRunes)
}

// renderStoredSteps renders the previous turn's iterations as the list of what
// was already attempted.
//
// It quotes each step's own words and the result of every tool it called, which
// is the part that cannot be recovered from anywhere else: the files a step
// changed are on disk, but *why* it changed them and what the tool said back is
// only in this row.
func renderStoredSteps(stepsJSON string) string {
	if strings.TrimSpace(stepsJSON) == "" {
		return ""
	}
	var steps []chat.Step
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		// A row from a build that stored a different shape. The rest of the brief
		// still stands: the plan and the answer tail say most of what matters.
		return ""
	}
	var b strings.Builder
	for _, step := range steps {
		line := fmt.Sprintf("- 步骤 %d", step.Index)
		if said := firstLineRunes(step.Text, resumeStepTextMaxRunes); said != "" {
			line += "：" + said
		}
		b.WriteString(line + "\n")
		for _, call := range step.Tools {
			b.WriteString("    · " + renderStoredCall(call) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderStoredCall renders one tool invocation as a single line.
func renderStoredCall(call chat.ToolRun) string {
	head := call.Name + "(" + truncateRunes(collapseSpaces(call.Args), resumeToolArgsMaxRunes) + ")"
	switch {
	case call.Err != "":
		return head + " → 失败：" + truncateRunes(collapseSpaces(call.Err), resumeToolResultMaxRunes)
	case strings.TrimSpace(call.Result) == "":
		return head + " → 完成（无输出）"
	default:
		return head + " → 完成：" + truncateRunes(collapseSpaces(call.Result), resumeToolResultMaxRunes)
	}
}

// collapseSpaces folds a multi-line tool result into one line, because a quoted
// block of output inside a bullet list is where a brief turns into a wall.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes bounds s to max runes, marking that it was cut.
//
// Runes and not bytes: this text is mostly Chinese, so a byte limit would cut a
// brief to a third of the characters it claims to allow — and cut it mid-rune,
// which is how a prompt ends up with invalid UTF-8 inside it.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…（已截断）"
}

// firstLineRunes returns the first non-empty line of s, bounded to max runes. A
// step's text is often several lines of narration, and the brief only needs the
// gist of it.
func firstLineRunes(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		if text := strings.TrimSpace(line); text != "" {
			return truncateRunes(text, max)
		}
	}
	return ""
}
