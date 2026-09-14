# Phase 7 — 设置分区（多 tab）与「通过大模型对接 MCP / Skill」

## Summary

把 设置 从一页长卡片流改成五个分类 sub-tab（外观 / 模型 / MCP / 技能 /
服务与工具），并让这两条能力链路真正闭环：

1. **MCP**：服务器可以在网页里增删改、测试连接、立即生效（SQLite 持久化 +
   运行时热加载进 Web 对话的工具注册表）；
2. **技能（Skill）**：可以在网页里新建 / 编辑 / 删除 SKILL.md，启用的技能进入
   对话的系统提示，由模型通过 `skill` 工具按需读取正文；
3. **通过大模型对接**：MCP 与 技能 两个分区各有一个配置助手，用默认对话模型把
   一句自然语言（或粘贴的文档）变成一份配置草稿 —— **只生成草稿，不落盘**，
   由操作者确认后再保存。

## Why

设置页此前是六张卡片串在一起的长滚动（外观 / 服务信息 / 模型目录 / 模型管理 /
工作区与工具 / 技能开关），外观与技能相隔九屏，加一类设置就再长一截。统计监控
已经证明了 sub-tab 这个形状可用（`MONITOR_TABS` + `state.monitor` + `.subtabs`），
设置没有理由不照做。

更实质的问题是能力链路断在两头：

- **MCP 只有配置文件一条路**。`mcp.servers` 在进程启动时读取，改一个服务器要
  编辑 YAML 并重启；控制台既看不到配了什么，也无法验证能不能连上。而且
  **Web 对话根本没接 MCP**：`buildChatDeps` 只注册内置工具，配置里的 MCP 服务器
  只有 CLI `chat --tools` 会用。
- **技能只做到了开关**。`/api/skills` 能列出与启停，但技能正文从未进入 Web 对话
  的上下文，模型不知道有技能可用，也没有任何界面能编辑一份技能。

“接入 MCP / Skill” 这件事的现实难点也不是协议：是**配置从哪来**。MCP 服务器的
命令、包名、参数、环境变量需要一个一个查；SKILL.md 的 frontmatter 与正文结构要
手写。这正好是大模型擅长的部分 —— 但它不适合直接写配置：一个看似合理的错误包名
比一个明显的错误更难发现。所以本变更把 LLM 放在**提议**的位置，把写盘留给操作者
确认。

## What Changes

### ADDED

- **`store.mcp_servers`（migration 8）**：id / name / transport / command / args /
  env / url / headers / source / enabled / last_error，`args`、`env`、`headers`
  以 JSON 字符串数组存储。`source=config` 的行每次启动从配置文件重新同步，因此
  界面上只能切换 enabled（与 `llm_providers` 同一套规则）。
- **`internal/mcp` 运行时**：
  - `Transport` 三值（stdio / sse / http）与 `ParseTransport`，`Connect` 按传输
    方式建不同 client（stdio 起子进程；sse / http 先 `Start` 再握手，且用
    `context.WithoutCancel` 让连接的生命周期独立于触发它的那次请求）。
  - `ServerSpec.Fingerprint()`：只包含连接相关字段，因此**改名不会断连接**，
    改命令/参数/url 会重连。
  - `Manager`：把期望的服务器集合与实际连接对齐（`Apply`）——不变的保持不动、
    变化的断开重连、消失的注销工具并关闭连接；`Refresh` 强制重连（崩溃的子进程
    或断掉的 SSE 流靠它恢复）；`Inspect` 只连一次、列工具、立刻断开（测试连接）。
  - `ServerID`：从显示名派生稳定 slug（配置文件里的条目没有数据库 id）。
- **`internal/tool`**：`Registry.Unregister`，并让 `List` 在锁内快照后再调用
  `Info`，使热加载与正在进行的对话不会互相踩。
- **MCP API**（`/api/mcp/*`）：列表（含 `runtime` 实时状态）、保存、删除、
  测试已保存的服务器、测试未保存的定义（probe）、重连、以及 `draft` 配置助手。
- **技能文件 API**：`GET /api/skills/{name}`（正文）、`PUT`（写盘）、`DELETE`
  （删文件）、`POST /api/skills-draft`（写作助手）。渲染 frontmatter 由服务端负责，
  `tools` 里不存在的工具会被剔除并回报（否则技能的 allow-list 必然不可用）。
- **技能接进对话**：`skill` 工具（模型按需读取某个已启用技能的正文）+ 系统提示里
  的「可用技能」段（只列名字与描述）。两者都在每轮对话时现算，因此启停立刻生效。
- **`server.SyncConfigServers`**：把配置文件的 `mcp.servers` 镜像进数据库，并在
  条目从配置文件消失时删除对应的 config 行（用户自建的行不动）。
- **前端**：`SETTINGS_TABS` + `state.settings` + `SettingsView` 的 sub-tab 外壳；
  `AppearancePanel` / `ModelPanel` / `McpPanel` / `SkillPanel` / `ServicePanel`；
  `SkillPanel` 取代原 `SkillsPanel`（列表 + 编辑器 + 写作助手）。

### CHANGED

- `config.MCPServer` 增加 `transport` / `url` / `headers` / `enabled`；
  `env`、`headers` 保持字符串数组（`KEY=value`、`Name: value`）。
- `Server.buildHistory` 用 `systemPrompt()`（基础提示 + 可用技能段）而不是
  `chatPrompt()`。
- `Server.Start` 在关闭时 `mcp.Close()`：stdio 服务器是子进程，不关就是每连一次
  泄漏一个进程。

## Design decisions

- **草稿不落盘。** `draft` 接口只返回候选配置，保存是另一个显式请求。模型最可能
  出错的地方是包名，而一个错误配置能连上的概率很低、排查成本很高 —— 让操作者过一
  眼比让模型自己写便宜得多。
- **测试连接与保存分离，且 probe 不注册。** 表单里的「测试连接」拨号一次就断开，
  正在编辑的定义不会先进入运行时、让模型调用一个连不上的工具。
- **不认识就说不认识。** 草稿里没把握的字段由模型写进 `summary`，需要填的密钥一律
  留空，服务端再逐条回报「STUB_TOKEN 还没有值」。宁可要一份带警告的草稿，也不要
  一份看起来完整的错误配置。
- **工具名冲突不致命。** 两个 MCP 服务器暴露同名工具时，先注册的胜出，冲突记进该
  服务器的 `last_error` 并在界面上显示 —— 整台服务器因为一个撞名就不可用是不划算的。
- **配置来源只读。** `mcp.servers` 里的条目每次启动都会被重新同步，所以在界面上只
  允许切换 enabled，其余字段直接拒绝并指向配置文件（沿用 `llm.providers` 的做法）。
- **技能不进系统提示正文。** 只列名字与描述、正文由 `skill` 工具按需取，避免每轮
  对话都把若干份指令全文塞进上下文。
- **设置页不自己滚动。** 沿用 `.page` / `.page-scroll`：只有内层面板滚动，文档本身
  不动。

## Impact

- 迁移：新增 migration 8（`mcp_servers`），对既有数据库是纯增量。
- 兼容：`mcp.servers` 既有配置条目继续可用（缺省即 stdio），并会出现在控制台里。
- 新增接口：`/api/mcp/*`、`/api/skills/{name}` 的 GET/PUT/DELETE、
  `/api/skills-draft`。既有 `/api/skills` 与 `POST /api/skills/{name}` 语义不变
  （响应多了 `file` / `bytes` 字段）。
- 工具集合变化：Web 对话的注册表里多了一个 `skill` 工具；MCP 服务器连接后其工具
  也会出现在 `/api/chat/models` 的 `tools` 里。
