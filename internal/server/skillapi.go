package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// registerSkillTool adds the 技能 tool to the chat's registry, once, at
// construction.
//
// It is a no-op without a registry (chat disabled) or without a skills
// directory (there can never be a skill to load), and it tolerates a name that
// is already taken: an operator's own tool called "skill" wins over ours.
func (s *Server) registerSkillTool() error {
	if s.chat.Tools == nil || s.skillState.loader == nil {
		return nil
	}
	built, err := newSkillTool(s.skillState)
	if err != nil || built == nil {
		return err
	}
	if _, exists := s.chat.Tools.Get(skillToolName); exists {
		return nil
	}
	if err := s.chat.Tools.Register(built); err != nil {
		return fmt.Errorf("register %s tool: %w", skillToolName, err)
	}
	return nil
}

// handleSkillDetail returns one skill with its markdown body: what the editor
// loads when the operator opens a skill.
func (s *Server) handleSkillDetail(_ context.Context, c *app.RequestContext) {
	name := strings.TrimSpace(c.Param("name"))
	detail, err := s.skillState.detail(name)
	if err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"skill": detail})
}

// handleSaveSkill creates or replaces a skill file.
func (s *Server) handleSaveSkill(_ context.Context, c *app.RequestContext) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "skill name is required"})
		return
	}
	var body struct {
		Description string   `json:"description"`
		Tools       []string `json:"tools"`
		Body        string   `json:"body"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	tools, dropped := s.filterKnownTools(body.Tools)
	detail, err := s.skillState.write(name, skillDraft{
		Description: body.Description,
		Tools:       tools,
		Body:        body.Body,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Info("skill saved", zapString("skill", name))
	out := map[string]any{"skill": detail}
	if len(dropped) > 0 {
		// Reported rather than silently dropped: a skill whose tool list names
		// tools this build does not have would fail every allow-list call, so
		// the tool is removed — and the operator is told which.
		out["dropped_tools"] = dropped
	}
	c.JSON(http.StatusOK, out)
}

// handleDeleteSkill removes a skill file.
func (s *Server) handleDeleteSkill(_ context.Context, c *app.RequestContext) {
	name := strings.TrimSpace(c.Param("name"))
	if err := s.skillState.remove(name); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Info("skill deleted", zapString("skill", name))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "name": name})
}

// filterKnownTools keeps only the tool names this registry actually has.
//
// A skill's frontmatter `tools` field is an allow-list. A name that does not
// exist makes the allow-list unusable, so an unknown name is dropped and
// reported instead of being written into the file.
func (s *Server) filterKnownTools(names []string) (kept, dropped []string) {
	if len(names) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	if s.chat.Tools != nil {
		for _, n := range s.chat.Tools.Names() {
			known[n] = true
		}
	}
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if known[n] {
			kept = append(kept, n)
			continue
		}
		dropped = append(dropped, n)
	}
	return kept, dropped
}

// skillDraftSystemPrompt is the instruction for the 技能写作助手.
//
// It asks for the file's parts separately (description, tools, body) rather
// than a whole markdown document, because the server renders the frontmatter:
// that keeps the YAML valid no matter what the model writes, and keeps the
// tool allow-list in a field the console can check before saving.
const skillDraftSystemPrompt = `你是 huan-agent 的 Skill（技能）编写助手。Skill 是一份可复用的操作指令：当用户的任务与它的描述相符时，Agent 会读取这份指令，然后按指令里的步骤使用工具完成任务。

用户会用一句话说明他想要什么技能。你要写出一份高质量的技能定义。

只输出一个 JSON 对象。不要输出解释文字，不要用 markdown 代码块。字段如下：
{
  "name": "技能名：只能用小写字母、数字、点、横线、下划线，以字母或数字开头，例如 code-review",
  "summary": "一句话说明你写的这个技能做什么、以及使用时需要注意什么",
  "description": "一句话描述这个技能在什么时候该被使用（会显示给模型用于判断是否调用）",
  "tools": ["该技能需要使用的工具名数组，必须从下面给出的可用工具里选；用不到就给空数组"],
  "body": "技能的正文，markdown 格式。写清楚：适用场景、具体步骤（有序列表）、每一步用哪个工具、注意事项与输出格式要求。要具体可执行，不要写空话。正文里不要包含 frontmatter（--- 包裹的部分），那部分由系统生成。"
}

规则：
1. name 必须符合上面的字符限制，不要用中文或空格。
2. tools 只能从给定的可用工具列表里选，宁少勿多；用不到工具就给 []。
3. body 用中文写（除非用户明确要求英文），务必具体：每一步说清调用哪个工具、传什么参数、如何判断成功。
4. 不要编造不存在的工具名。`

// handleDraftSkill asks the model to write a skill from a description.
func (s *Server) handleDraftSkill(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Description string `json:"description"`
		// Name is an optional explicit name, used when the operator is creating
		// a skill under a name they have already chosen.
		Name string `json:"name"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	desc := strings.TrimSpace(body.Description)
	if desc == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "请描述你想要的技能（description）"})
		return
	}

	user := "用户想要的能力：\n" + desc
	if n := strings.TrimSpace(body.Name); n != "" {
		user += "\n\n技能名已确定为：" + n + "（name 字段请用它）"
	}
	user += "\n\n可用的工具：" + s.availableToolNames()

	result, err := s.draftJSON(ctx, skillDraftSystemPrompt, user)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = stringField(result.Fields, "name")
	}
	tools, dropped := s.filterKnownTools(stringListField(result.Fields, "tools"))

	var warnings []string
	if !skillNamePattern.MatchString(name) {
		warnings = append(warnings, fmt.Sprintf("技能名 %q 不能用作文件名（只能用小写字母、数字、. - _，且以字母或数字开头），请修改后再保存", name))
	}
	if len(dropped) > 0 {
		warnings = append(warnings, "以下工具不存在，已从 tools 中移除："+strings.Join(dropped, "、"))
	}
	bodyText := stringField(result.Fields, "body")
	if strings.TrimSpace(bodyText) == "" {
		warnings = append(warnings, "模型没有给出正文（body），请补充后再保存")
	}

	c.JSON(http.StatusOK, map[string]any{
		"draft": map[string]any{
			"name":        name,
			"description": stringField(result.Fields, "description"),
			"tools":       orEmptyStrings(tools),
			"body":        bodyText,
		},
		"warnings": warnings,
		"notes":    result.Notes,
		"raw":      result.Raw,
		"model":    map[string]string{"provider": result.Provider, "model": result.Model},
	})
}

// availableToolNames renders the registry's tool names for the prompt.
func (s *Server) availableToolNames() string {
	if s.chat.Tools == nil {
		return "（当前没有可用工具）"
	}
	names := s.chat.Tools.Names()
	if len(names) == 0 {
		return "（当前没有可用工具）"
	}
	return strings.Join(names, "、")
}

// orEmptyStrings guarantees a JSON array rather than null.
func orEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// skillFromName loads a skill for a caller that only has a name, reporting a
// not-found error the API can turn into a 404.
var errSkillNotFound = errors.New("unknown skill")

// requireSkill resolves a skill name or fails with errSkillNotFound.
func (s *Server) requireSkill(name string) (skillDetail, error) {
	detail, err := s.skillState.detail(name)
	if err != nil {
		return skillDetail{}, fmt.Errorf("%w: %s", errSkillNotFound, name)
	}
	return detail, nil
}
