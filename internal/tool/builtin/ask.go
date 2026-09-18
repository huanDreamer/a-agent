package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/tool"
)

// AskUserToolName is the tool a model calls to put a question to the person
// using the console.
const AskUserToolName = "ask_user"

// maxAskOptions caps the choices one card may offer. The limit is about the
// card, not about the model: eight rows is already a scrolling decision, and a
// model that produced twenty is really asking an open question with decoration,
// which allow_custom answers better.
const maxAskOptions = 8

// AskUserOption is one choice in a question.
type AskUserOption struct {
	Label string `json:"label" jsonschema:"description=选项文案。它就是答案本身（简短、可直接当成结论），不要写编号或前缀,required"`
	// No comma anywhere in a jsonschema description: eino splits the tag on
	// commas, so one would silently truncate the text.
	Description string `json:"description,omitempty" jsonschema:"description=可选的一行解释：选它意味着什么、代价是什么。例如「已有实例，迁移成本低」"`
}

// AskUserInput is the parameter schema for the "ask_user" tool.
type AskUserInput struct {
	Question string          `json:"question" jsonschema:"description=要问的问题，一句话说清。不要在一个问题里塞多个不相关的决定,required"`
	Header   string          `json:"header,omitempty" jsonschema:"description=卡片标题，不超过 12 字（例如 数据库选型）。留空则只显示问题"`
	Options  []AskUserOption `json:"options,omitempty" jsonschema:"description=2-6 个互斥选项（最多 8 个）。选项要具体、能被直接采纳；需要人自由发挥时不要给选项，改用 allow_custom"`
	Multi    bool            `json:"multi_select,omitempty" jsonschema:"description=允许选多个。默认 false（单选）"`
	// A pointer for the same reason the grep tool uses one: the model may omit
	// the field, and "omitted" must mean "allow free text" rather than "false".
	AllowCustom *bool `json:"allow_custom,omitempty" jsonschema:"description=允许用户自己输入。默认 true。只有当你确实只接受上述选项时才设为 false"`
}

// AskUserOutput is what the tool returns, and what the model reads as the
// observation of this call.
//
// Answer repeats what Selected/Text already say as the one line the model should
// use when it explains what it did. It exists because a model reading
// {"selected":["Postgres"]} sometimes asks again; a finished sentence is harder
// to misread.
type AskUserOutput struct {
	Status   string   `json:"status"`
	Question string   `json:"question"`
	Selected []string `json:"selected,omitempty"`
	Text     string   `json:"text,omitempty"`
	Answer   string   `json:"answer"`
	Note     string   `json:"note,omitempty"`
}

// askUserDescription is the model-facing contract of the tool. It is written as
// rules rather than as capabilities on purpose: the failure mode of a "you may
// ask the user" tool is a model that asks about everything it could have worked
// out itself, and this text is the only place that can prevent it.
func askUserDescription() string {
	return "向用户提问，并在对话里显示一张可交互卡片（选项 + 自己输入 + 提交），等用户回答后把答案作为本次调用的结果返回。" +
		"用于「必须由人决定」或「只有人知道」的事：在两个都可行的方案之间选一个、确认一个影响面大的操作、索要缺失的参数（路径、账号、目标环境、口径）。" +
		"不要用于：能自己查证或推断的事（先自己查，查不到再问）、汇报进度或征求同意、一次问多个不相关的决定（应拆成多次调用）。" +
		"选项要能被直接采纳为决定；需要用户自由发挥时不要造选项，去掉 options 让人自己写。" +
		"用户可能超时未答或本轮被中断，此时结果里的 status 不是 answered，请按 note 自行判断继续，不要重复问同一个问题。"
}

// NewAskUserTool returns the ask_user tool. The answerer is read from the turn's
// context (tool.AskerFrom) rather than captured here: one tool set is shared by
// every conversation of a surface, and each turn has its own.
func NewAskUserTool() (einotool.InvokableTool, error) {
	return utils.InferTool(AskUserToolName, askUserDescription(), askUser)
}

// askUser validates the model's question, puts it to the turn's answerer and
// renders the answer.
func askUser(ctx context.Context, in AskUserInput) (AskUserOutput, error) {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return AskUserOutput{}, errors.New("question 不能为空：请写清你要用户决定什么")
	}

	options, err := askOptions(in.Options)
	if err != nil {
		return AskUserOutput{}, err
	}
	allowCustom := in.AllowCustom == nil || *in.AllowCustom
	if len(options) == 0 && !allowCustom {
		return AskUserOutput{}, errors.New("没有给选项又不允许用户自己输入：用户无法回答，请给出 options，或把 allow_custom 设为 true")
	}

	asker, ok := tool.AskerFrom(ctx)
	if !ok {
		return AskUserOutput{}, errors.New("当前通道无法向用户提问（只有网页控制台支持交互卡片）：请直接用文字回答，并把需要用户决定的地方写清楚")
	}

	started := time.Now()
	answer, err := asker.Ask(ctx, tool.Question{
		Header:      strings.TrimSpace(in.Header),
		Text:        question,
		Options:     options,
		MultiSelect: in.Multi,
		AllowCustom: allowCustom,
	})
	if err != nil {
		return AskUserOutput{}, fmt.Errorf("提问失败：%w", err)
	}

	out := AskUserOutput{
		Status:   string(answer.Status),
		Question: question,
		Selected: answer.Selected,
		Text:     strings.TrimSpace(answer.Text),
	}
	out.Answer, out.Note = renderAskAnswer(out, time.Since(started))
	// The asker's own remark rides next to the answer rather than replacing it:
	// what was chosen and what to do about it are different things, and a
	// repeated question needs both.
	if n := strings.TrimSpace(answer.Note); n != "" {
		if out.Note != "" {
			out.Note = n + "；" + out.Note
		} else {
			out.Note = n
		}
	}
	return out, nil
}

// askOptions normalises the offered choices and refuses the shapes that would
// produce an unusable card. Every refusal is a readable sentence, because its
// only reader is the model — which can fix the call in its next step, and cannot
// ask a person to fix it.
func askOptions(in []AskUserOption) ([]tool.Option, error) {
	if len(in) > maxAskOptions {
		return nil, fmt.Errorf("选项最多 %d 个，收到 %d 个：请合并或删减；需要自由回答时改用 allow_custom", maxAskOptions, len(in))
	}
	out := make([]tool.Option, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, raw := range in {
		label := strings.TrimSpace(raw.Label)
		if label == "" {
			return nil, fmt.Errorf("第 %d 个选项的 label 为空：每个选项都要有能被直接采纳的文案", i+1)
		}
		if _, dup := seen[label]; dup {
			return nil, fmt.Errorf("选项 %q 重复：选项文案必须互不相同", label)
		}
		seen[label] = struct{}{}
		out = append(out, tool.Option{Label: label, Description: strings.TrimSpace(raw.Description)})
	}
	return out, nil
}

// renderAskAnswer turns an outcome into what the model should read.
//
// The two strings it returns are different in kind on purpose: Answer is the
// fact (what was chosen), Note is what to do about it. A timeout that returned
// only an empty answer would leave the model guessing whether the person chose
// nothing or the machinery broke.
func renderAskAnswer(out AskUserOutput, waited time.Duration) (string, string) {
	switch tool.AnswerStatus(out.Status) {
	case tool.AnswerAnswered:
		answer := strings.Join(out.Selected, "、")
		switch {
		case answer != "" && out.Text != "":
			answer += "（补充：" + out.Text + "）"
		case answer == "":
			answer = out.Text
		}
		return answer, ""
	case tool.AnswerTimeout:
		return "", fmt.Sprintf("用户 %s 内没有回答：请基于最合理的假设继续，并在最终回答里说明你选了哪个假设；不要重复问同一个问题",
			waited.Round(time.Second))
	case tool.AnswerCancelled:
		return "", "本轮已被中断（用户停止或关闭了页面），用户没有回答：请停止等待，把当前进展和待确认的问题一起讲清楚"
	default:
		return "", fmt.Sprintf("没有得到有效回答（status=%s）：请自行判断如何继续", out.Status)
	}
}
