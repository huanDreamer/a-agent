package main

// Feishu's workspace surface: the commands, the natural-language switch, and the
// replies that make a switch visible.
//
// A switch is the one conversational action in this bot that changes where the
// agent's next writes land, so the rules here are deliberately conservative:
//
//   - the explicit commands come first and mean exactly one thing;
//   - a prose message is only read as a switch when it is a short, verb-initial
//     request naming a workspace (the parsing lives in internal/workspaces, and
//     is tested against the requests that must NOT be read as switches);
//   - an unrecognised name is answered with the list rather than guessed, and a
//     name that matches several workspaces is never resolved by picking one;
//   - every answer states the workspace that is now in effect and the directory
//     it resolves to, because a silently wrong target is the failure mode that
//     matters here.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/platform/feishu"
	"github.com/huan/huan-agent/internal/workspaces"
)

// scopeFor returns the workspace scope of the message's sender.
//
// The open_id rather than the chat id: in a group every sender keeps their own
// choice, so one person switching projects does not move everybody's.
func scopeFor(in feishu.Inbound) string {
	return workspaces.FeishuScope(in.OpenID)
}

// workspacesEnabled reports whether the bot has a workspace layer to talk about.
func (h *botHandler) workspacesEnabled() bool {
	return h.wsMgr != nil
}

// handleWorkspaceCommand answers the /workspace family. The bool is false when
// the text is not one of these commands at all.
func (h *botHandler) handleWorkspaceCommand(ctx context.Context, in feishu.Inbound, text string) (bool, error) {
	lower := strings.ToLower(text)
	switch lower {
	case "/workspace", "/workspace?", "/ws":
		return true, h.replyCurrentWorkspace(ctx, in)
	case "/workspaces", "/ws list", "/workspace list":
		return true, h.replyWorkspaceList(ctx, in)
	}
	if strings.HasPrefix(lower, "/workspace ") {
		return true, h.switchWorkspace(ctx, in, text[len("/workspace "):])
	}
	if strings.HasPrefix(lower, "/ws ") {
		return true, h.switchWorkspace(ctx, in, text[len("/ws "):])
	}
	return false, nil
}

// replyCurrentWorkspace reports the workspace this sender is in.
func (h *botHandler) replyCurrentWorkspace(ctx context.Context, in feishu.Inbound) error {
	if !h.workspacesEnabled() {
		return h.reply(ctx, in, "工作区未启用：服务端没有可用的工作区配置。")
	}
	spec, err := h.wsMgr.Active(ctx, scopeFor(in))
	if err != nil {
		return h.reply(ctx, in, "读取当前工作区失败: "+err.Error())
	}
	return h.reply(ctx, in, formatWorkspaceDetail("当前工作区", spec))
}

// replyWorkspaceList lists what can be switched to.
func (h *botHandler) replyWorkspaceList(ctx context.Context, in feishu.Inbound) error {
	if !h.workspacesEnabled() {
		return h.reply(ctx, in, "工作区未启用：服务端没有可用的工作区配置。")
	}
	specs, err := h.wsMgr.List(ctx)
	if err != nil {
		return h.reply(ctx, in, "读取工作区列表失败: "+err.Error())
	}
	current, err := h.wsMgr.Active(ctx, scopeFor(in))
	if err != nil {
		return h.reply(ctx, in, "读取当前工作区失败: "+err.Error())
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("共 %d 个工作区（当前：%s）：", len(specs), current.Name))
	for _, spec := range specs {
		mark := "  "
		if spec.Name == current.Name {
			mark = "▸ "
		}
		b.WriteString(fmt.Sprintf("\n%s%s", mark, describeWorkspace(spec)))
	}
	b.WriteString("\n\n切换：`切换到 <名字>` 或 `/workspace <名字>`")
	return h.reply(ctx, in, b.String())
}

// switchWorkspace moves this sender into a workspace and says so.
//
// The name is resolved exactly, then by unique prefix, and only then by asking
// the model to pick from the list — never by choosing among several matches,
// because a coin flip here lands the agent's next writes in the wrong project.
func (h *botHandler) switchWorkspace(ctx context.Context, in feishu.Inbound, want string) error {
	if !h.workspacesEnabled() {
		return h.reply(ctx, in, "工作区未启用：服务端没有可用的工作区配置。")
	}
	want = strings.TrimSpace(want)
	scope := scopeFor(in)

	specs, err := h.wsMgr.List(ctx)
	if err != nil {
		return h.reply(ctx, in, "读取工作区列表失败: "+err.Error())
	}
	before, err := h.wsMgr.Active(ctx, scope)
	if err != nil {
		return h.reply(ctx, in, "读取当前工作区失败: "+err.Error())
	}
	if want == "" {
		return h.reply(ctx, in,
			"要切到哪个工作区？用法：`切换到 <名字>`，例如 `/workspace "+exampleWorkspace(specs, before.Name)+"`。\n"+
				"发送 `/workspaces` 可以看到全部工作区。")
	}

	spec, match := workspaces.ResolveName(specs, want)
	switch match {
	case workspaces.MatchAmbiguous:
		return h.reply(ctx, in, fmt.Sprintf(
			"「%s」匹配到多个工作区，请说得更具体：\n%s", want, formatNames(ambiguousNames(specs, want))))
	case workspaces.MatchNone:
		// The model gets one chance to read a fuzzy request ("切到电商那个"),
		// and it can only answer with a name from the list it is given.
		if picked, ok := h.pickWorkspaceWithModel(ctx, want, specs); ok {
			spec = picked
			break
		}
		return h.reply(ctx, in, fmt.Sprintf(
			"没有叫「%s」的工作区。现有：%s\n（新建工作区请到网页控制台 设置 → 工作区）",
			want, formatNames(specsToNames(specs))))
	}

	after, err := h.wsMgr.Select(ctx, scope, spec.Name)
	if err != nil {
		return h.reply(ctx, in, "切换失败: "+err.Error())
	}
	h.logger.Info("feishu workspace switched",
		zap.String("open_id", in.OpenID),
		zap.String("from", before.Name), zap.String("to", after.Name), zap.String("root", after.Root))

	if before.Name == after.Name {
		return h.reply(ctx, in, formatWorkspaceDetail("已经在这个工作区", after))
	}
	return h.reply(ctx, in, fmt.Sprintf("已切换工作区：%s → %s\n%s\n\n接下来的文件读写与命令都会在这个目录里进行。",
		before.Name, after.Name, describeWorkspace(after)))
}

// parseWorkspaceIntent reads a prose message as a workspace request, when it is
// one.
//
// The workspace names are what make this safe in both directions: a name that
// exists may be recognised from a loose phrasing, while an unknown name is only
// read as a workspace after an unambiguous verb and only as a single short
// token. "在工作区里建个 hello.go" and "用 Python 写个脚本" mention workspaces
// and tools, and are answered normally.
func (h *botHandler) parseWorkspaceIntent(ctx context.Context, in feishu.Inbound, text string) workspaces.Intent {
	if !h.workspacesEnabled() {
		return workspaces.Intent{}
	}
	specs, err := h.wsMgr.List(ctx)
	if err != nil {
		// A listing failure must not turn a normal request into an error: the
		// message is simply answered as what it plainly is.
		h.logger.Warn("feishu: list workspaces for intent parsing failed", zap.Error(err))
		return workspaces.Intent{}
	}
	names := specsToNames(specs)
	return workspaces.ParseSwitchIntent(text, names)
}

// pickWorkspaceWithModel asks the default model to choose from the known
// workspaces, for a request whose name does not resolve on its own.
//
// The model's answer is resolved against the same list it was given, so a wrong
// or invented name simply does not resolve and the caller answers with the list.
// That is the whole safety argument for involving a model here: it cannot widen
// the set of workspaces, only recognise one that already exists.
func (h *botHandler) pickWorkspaceWithModel(ctx context.Context, want string, specs []workspaces.Spec) (workspaces.Spec, bool) {
	if h.cm == nil {
		return workspaces.Spec{}, false
	}
	names := specsToNames(specs)
	prompt := fmt.Sprintf("用户说：%q\n可选工作区（只能从这些里选）：%s\n"+
		"如果这句话是在指定其中一个工作区，只回答那个名字；若不是，只回答「未知」。",
		want, strings.Join(names, "、"))

	out, err := h.cm.Generate(ctx, []*schema.Message{
		{Role: schema.System, Content: "你只回答一个工作区名字，或回答「未知」。"},
		{Role: schema.User, Content: prompt},
	})
	if err != nil || out == nil {
		h.logger.Warn("feishu: workspace name resolution call failed", zap.Error(err))
		return workspaces.Spec{}, false
	}
	answer := strings.TrimSpace(out.Content)
	if answer == "" || strings.Contains(answer, "未知") {
		return workspaces.Spec{}, false
	}
	spec, match := workspaces.ResolveName(specs, answer)
	if match == workspaces.MatchExact || match == workspaces.MatchFuzzy {
		h.logger.Info("feishu: workspace name resolved by the model",
			zap.String("asked", want), zap.String("resolved", spec.Name))
		return spec, true
	}
	return workspaces.Spec{}, false
}

// describeWorkspace renders one workspace in a single line: its name and the
// directory it points at. A workspace has no policy to report — what the agent
// may do is the server's configuration, not a property of the directory.
func describeWorkspace(spec workspaces.Spec) string {
	return fmt.Sprintf("`%s` · %s", spec.Name, spec.Root)
}

// formatWorkspaceDetail renders a workspace as a small block.
func formatWorkspaceDetail(title string, spec workspaces.Spec) string {
	var b strings.Builder
	b.WriteString(title + "：`" + spec.Name + "`")
	b.WriteString("\n目录：" + spec.Root)
	if spec.Missing {
		b.WriteString("\n⚠ 该目录已不存在，请到控制台重新指定或删除这个工作区")
	}
	if spec.SessionCount > 0 {
		b.WriteString(fmt.Sprintf("\n此工作区有 %d 个对话", spec.SessionCount))
	}
	return b.String()
}

// specsToNames returns the workspace names, sorted for stable output.
func specsToNames(specs []workspaces.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	sort.Strings(out)
	return out
}

// ambiguousNames returns the candidates a fuzzy request matched.
func ambiguousNames(specs []workspaces.Spec, want string) []string {
	low := strings.ToLower(strings.TrimSpace(want))
	var out []string
	for _, spec := range specs {
		name := strings.ToLower(spec.Name)
		if strings.HasPrefix(name, low) || strings.Contains(name, low) {
			out = append(out, spec.Name)
		}
	}
	sort.Strings(out)
	return out
}

// exampleWorkspace returns a name to put in an example command: one that is not
// the workspace the person is already in, so the example shows a switch rather
// than a no-op.
func exampleWorkspace(specs []workspaces.Spec, current string) string {
	for _, spec := range specs {
		if !strings.EqualFold(spec.Name, current) {
			return spec.Name
		}
	}
	if len(specs) > 0 {
		return specs[0].Name
	}
	return "<名字>"
}

// formatNames renders a name list for a message.
func formatNames(names []string) string {
	if len(names) == 0 {
		return "（无）"
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, "`"+n+"`")
	}
	return strings.Join(quoted, "、")
}

// feishuHelpText is the /help answer, including the workspace commands when the
// feature is available.
func (h *botHandler) feishuHelpText() string {
	base := "Commands:\n/reset 清空会话\n/remember key: value 记住事实\n/recall query 检索记忆\n/provider 查看模型"
	if !h.workspacesEnabled() {
		return base
	}
	return base + "\n/workspace 当前工作区\n/workspaces 列出工作区\n/workspace <名字> 切换工作区\n" +
		"也可以直接说：「切换到 <名字> 工作区」"
}
