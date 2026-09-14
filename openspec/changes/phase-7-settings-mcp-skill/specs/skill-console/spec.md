# Capability: skill-console

## Purpose

让技能（Skill）从「只有开关」变成可以在控制台里创建、编辑、删除，并且真的进入
对话上下文——以及让大模型能帮忙把一句需求写成一份草稿。

## Scope

- `internal/skill` — 记录每个技能来自哪个文件。
- `internal/server` — 技能文件的读写接口、`skill` 工具、系统提示注入。
- `web/src` — 设置 → 技能。

## Requirements (MUST)

1. **读。** `GET /api/skills` MUST 返回每个技能的名字、描述、启停状态、声明的工具、
   文件名与字节数；`GET /api/skills/{name}` MUST 返回正文，使界面展示的与加载的
   是同一份内容。
2. **写。** `PUT /api/skills/{name}` MUST 原子写（临时文件 + rename），MUST 由服务
   端渲染 YAML frontmatter（name 取 URL 里的名字，改名 MUST NOT 发生：启停状态按
   名字记录，改名会丢状态），MUST 拒绝空正文，MUST 把名字限制为
   `[a-z0-9][a-z0-9._-]{0,63}`（同时是路径穿越的防线）。
3. **工具白名单。** frontmatter 的 `tools` 里不存在的工具名 MUST 被剔除并在响应里
   回报（否则 `SetAllowList` 必然失败，技能不可用）。
4. **删。** `DELETE /api/skills/{name}` MUST 删除文件并清掉它的停用记录；只有停用
   记录、没有文件时也算删除成功（那正是需要清理的残留）。
5. **写回的技能立刻生效。** 保存一个此前被停用的技能 MUST 重新启用它（重写它就是
   在把它拿回来），且 MUST 立即可被模型加载。
6. **进对话上下文。** 启用的技能 MUST 出现在对话的系统提示里（名字 + 描述），
   正文 MUST NOT 全量进提示；模型 MUST 能通过 `skill` 工具按名字取到已启用技能的
   正文（含它声明的工具范围）。停用的技能 MUST 既不在提示里，也 MUST NOT 能通过
   工具读到（错误信息里列出可用技能名，便于模型自行纠正）。
7. **热生效。** 启停与保存 MUST 在下一轮对话生效，不需要重启。
8. **写作助手。** `POST /api/skills-draft` MUST 用默认对话模型产出一份草稿
   （name / description / tools / body），MUST 只返回草稿而不写文件，MUST 清洗
   `tools`（未知工具剔除并警告）、校验名字可作文件名，MUST 在模型不可用时以明确的
   错误说明缺少什么配置。
