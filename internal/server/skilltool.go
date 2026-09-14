package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/tool"
)

// skillToolName is the tool the model calls to read one skill's instructions.
//
// It is deliberately a single generic tool rather than one tool per skill: a
// skill's body is prompt text of arbitrary length, and putting every enabled
// skill in the system prompt on every turn would spend the whole context window
// on instructions the turn may not need. The prompt lists the names and
// descriptions; the model pulls in the body when the task matches.
const skillToolName = "skill"

// skillToolInput is the parameter schema of the skill tool.
type skillToolInput struct {
	Name string `json:"name" jsonschema:"description=技能名称，必须与系统提示中列出的技能名完全一致"`
}

// newSkillTool builds the skill tool over state. It returns nil when no skills
// directory is configured, because then there can never be a skill to load.
func newSkillTool(state *skillState) (tool.Tool, error) {
	if state == nil || state.loader == nil {
		return nil, nil
	}
	built, err := utils.InferTool(skillToolName,
		"加载一个已启用技能（Skill）的完整指令。技能是预先写好的操作步骤；当任务与系统提示里列出的某个技能描述相符时，"+
			"先调用本工具取得该技能的指令，再按指令执行。参数 name 必须是系统提示中列出的技能名。",
		func(_ context.Context, in skillToolInput) (string, error) {
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return "", fmt.Errorf("name 不能为空；可用技能：%s", state.availableNames())
			}
			sk, ok := state.enabledSkill(name)
			if !ok {
				return "", fmt.Errorf("没有已启用的技能 %q；可用技能：%s", name, state.availableNames())
			}
			body := sk.SystemPrompt()
			if len(sk.Frontmatter.Tools) > 0 {
				// Worth saying: the skill's own frontmatter restricts which tools
				// it expects to have, and the model should stay inside that set
				// while following it.
				body += "\n\n（该技能声明的工具范围：" + strings.Join(sk.Frontmatter.Tools, "、") + "）"
			}
			return body, nil
		})
	if err != nil {
		return nil, fmt.Errorf("build %s tool: %w", skillToolName, err)
	}
	return tool.WithCapability(built, tool.CapRead), nil
}

// availableNames renders the enabled skill names for an error message.
func (s *skillState) availableNames() string {
	skills := s.enabledSkills()
	if len(skills) == 0 {
		return "（当前没有启用任何技能）"
	}
	names := make([]string, 0, len(skills))
	for _, sk := range skills {
		names = append(names, sk.Frontmatter.Name)
	}
	return strings.Join(names, "、")
}

// skillsPromptSection renders the 可用技能 block appended to the system prompt,
// or "" when nothing is enabled.
//
// The names and descriptions are what let the model decide to call the skill
// tool at all; the instructions themselves stay out of the prompt until it does.
func (s *Server) skillsPromptSection() string {
	skills := s.skillState.enabledSkills()
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## 可用技能（Skills）\n")
	b.WriteString("下列技能是已启用的可复用指令集。当用户的任务与某个技能描述相符时，" +
		"先调用 `" + skillToolName + "` 工具并传入该技能名，取得完整指令后再执行；不要在没有读取指令的情况下凭猜测执行技能任务。\n")
	for _, sk := range skills {
		b.WriteString("- ")
		b.WriteString(sk.Frontmatter.Name)
		if desc := strings.TrimSpace(sk.Frontmatter.Description); desc != "" {
			b.WriteString("：")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// systemPrompt returns the system prompt for a turn: the operator's prompt (or
// the default) plus the available-skills section.
//
// It is assembled per turn rather than cached so enabling a skill in
// 设置 → 技能 takes effect on the next message.
func (s *Server) systemPrompt() string {
	base := s.chatPrompt()
	section := s.skillsPromptSection()
	if section == "" {
		return base
	}
	return base + "\n\n" + section
}
