# 通用 Agent 系统提示词：从一句话到一套规则

## Summary

huan-agent 发给模型的 system prompt 至今是两段各自维护的英文：Web 控制台一条常量、飞书机器人
一个函数，内容是「你是 huan-agent，一个有用的个人 AI 助手，回答要简洁，命令没有终端」。
`huan-agent chat` 更彻底 —— 不给 `--system` 时它**根本没有 system 消息**。

本次变更给三个界面同一份**中文通用 Agent 提示词**，并按界面各加一段：

1. 新包 `internal/prompt`：`base.md`（身份 / 语言与表达 / 做事方式 / 工具使用 / 边界与安全）
   加每个界面一个 `surface_*.md`，`prompt.For(surface)` 拼装，`go:embed` 打进二进制。
2. Web 控制台、飞书机器人、`huan-agent chat` 都用它；CLI 从「没有提示词」变成「有提示词」。
3. `chat.system_prompt` 成为统一的覆盖入口（`prompt.Effective`）：填了就整套替换，控制台与
   飞书共用同一个字段；CLI 仍由 `--system` / `--skill` 控制。
4. 提示词包自带测试（全中文、体积预算、各界面段落差异），并新增 `docs/prompt.md`。

## Why

- **一句话提示词留白太多。** 「有用的助手」没有告诉模型任何它最常做错的事：先查证再断言、
  改完要验证、不要扩大范围、不确定就说不确定。这些不是风格偏好，而是可以观察到的失败模式。
- **两个界面各自维护会分叉。** 控制台和飞书本来就该是同一个 Agent，只是输出形式不同；两份
  提示词意味着改一处忘一处，而忘掉的那一处没人会发现。
- **语言要一致。** 工具描述、技能段落、控制台界面都是中文。提示词写英文，等于让模型在
  「用中文写规则」和「用英文写规则」的两套指令之间自己选一个。
- **提示词是每一步的成本。** `context.max_tokens` 的循环内压缩**逐字保留** system 提示，
  所以它的体积要乘以步数来算；没有测试卡住长度，一个「顺手再加三条」的过程会把它悄悄翻倍。
- **`chat.system_prompt` 只覆盖了一半的界面。** 同名字段在控制台生效、在飞书被忽略，是纯粹的
  意外：运维写下自己的提示词时，期待的是「这个部署按我说的说话」。

## What Changes

### ADDED

1. **`internal/prompt`：提示词的唯一出处**
   - `base.md`：身份、语言与表达、做事方式、工具使用、边界与安全。
   - `surface_web.md` / `surface_cli.md` / `surface_feishu.md`：每个界面一段，写明这个界面
     **有**什么（Markdown 渲染？卡片？附件？）和**没有**什么（`ask_user`、终端分页器、超宽表格）。
   - `prompt.Base()`、`prompt.For(surface)`、`prompt.Effective(override, surface)` 与
     `SurfaceWeb` / `SurfaceCLI` / `SurfaceFeishu` 常量。
   - `prompt_test.go`：全中文（拦英文虚词 + 汉字占比）、体积（12 KiB 上限）、各界面段落、
     未知界面回落到基础提示词、override 的「替换而非追加」。

### MODIFIED

1. **`internal/server/chat.go`**：`defaultSystemPrompt` 这个英文常量删除，`chatPrompt()` 改为
   `prompt.Effective(s.chat.SystemPrompt, prompt.SurfaceWeb)`；覆盖优先级只剩一处实现。
2. **`cmd/huan-agent/serve.go`**：`botHandler.systemPrompt()` 返回
   `prompt.Effective(cfg.Chat.SystemPrompt, prompt.SurfaceFeishu)` —— 飞书开始读
   `chat.system_prompt`，与控制台同一规则；`cfg` 为 nil 时不 panic。
3. **`cmd/huan-agent/chat.go`**：`--skill` > `--system` > CLI 默认提示词，空会话不再没有 system。
4. **测试**：`cmd/huan-agent/serve_test.go` 新增 `TestBotSystemPromptFollowsTheOperator`；
   `internal/server/traces_test.go`、`internal/server/skillapi_test.go` 改为引用
   `prompt.For(prompt.SurfaceWeb)` 而不是本地常量副本。
5. **文档**：新增 `docs/prompt.md`（提示词在哪、各界面拿到什么、两条硬性约束、改动检查清单）；
   `docs/admin.md`、`docs/feishu.md`、`configs/config.example.yaml`、`CLAUDE.md` 同步。

## Impact

- **兼容**：`chat.system_prompt` 的语义不变（空白等于没填），只是覆盖面从控制台扩到飞书；
  不配置的部署行为变化是「默认提示词换成了中文的通用 Agent 提示词」。
- **行为变化**：`huan-agent chat` 不带 `--system`/`--skill` 时第一次有了 system 消息。
- **不变**：技能的**可用技能**段落仍由 `internal/server/skilltool.go` 每轮拼在末尾；飞书没有
  技能面板，因此没有这一段。
- **不在本次范围**：把提示词做成运行时（数据库）可改；按会话定制提示词；提示词的多语言切换。
