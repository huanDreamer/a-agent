# skill-system

## Purpose

把"提示词 + 工具组合"打包成可复用单元。Skill = 一个 markdown 文件（含 YAML frontmatter），加载后注入 system prompt 头部，并自动把白名单内的工具启用。

## Requirements

### R1: Skill 文件格式

每个 Skill 是一个 markdown 文件，结构：

```markdown
---
name: daily-summary
description: 总结过去 24 小时的 commit、issue、PR
tools: [time, calc]
---

# Daily Summary

你是 huan-agent 的日报助手。...

## 步骤

1. 调 `time` 拿到当前时间
2. ...
```

frontmatter MUST 解析为：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 是 | skill 唯一标识 |
| `description` | string | 是 | 一句话描述 |
| `tools` | []string | 否 | 启用的工具白名单；缺省 = 全部已注册工具 |

Body 部分（去掉 frontmatter 后的 markdown）MUST 作为 system prompt 内容。

### R2: 加载器

`Loader` MUST：

- `NewLoader(dir string) *Loader`
- `(l) LoadAll() ([]*Skill, error)` — 扫 `dir` 下所有 `*.md`
  - 解析失败的单个文件 MUST 累积到错误 slice（不中断整个 LoadAll）
  - 同名 skill 重复 MUST 报"duplicate skill name"错误
- `(l) Get(name string) (*Skill, bool)` — 查单个
- `(l) Names() []string` — 所有已加载 skill 名

### R3: SystemPrompt 注入

`(s *Skill) SystemPrompt() string` MUST 返回：

```
<description>

<body>
```

Agent 在收到 `--skill foo` 时 MUST 把对应 skill 的 `SystemPrompt()` 拼成 system message 注入 messages[0]。

### R4: 工具白名单联动

`--skill foo` 启用时：

- 如果 skill frontmatter 含 `tools`，MUST 设置 `tool.Registry.SetAllowList(skill.Tools)`
- 未指定 `tools` 时 MUST 保留 CLI 的 `--tools` 列表

### R5: 内置 Skill

`configs/skills/` 目录 MUST 至少包含：

- `daily-summary.md` — 日报总结
- `code-review.md` — 代码审查助手

启动时 `Loader` 默认加载 `./configs/skills/`，路径可在配置覆盖。

## Out of Scope

- 远端 Skill 仓库（git 拉取）—— 等 Skill 市场需要时再说
- Skill 依赖管理（一个 skill 引用另一个 skill）—— MVP skill 互相独立
- Skill 版本管理 / 签名验证
- Skill 匹配（自动根据用户输入选 skill）—— MVP 显式 `--skill`

## Dependencies

- `gopkg.in/yaml.v3`（frontmatter 解析）
- `internal/tool`（白名单联动）
- `internal/agent`（注入 system prompt）
