# Phase 2 — Agent 核心 + MCP + Skill

## Why

Phase 1 让 huan-agent 能"说"了，但还不能"做"。一个真正的 agent 必须能调用工具、按多步推理、根据上下文激活专门的技能。本阶段把 huan-agent 从"聊天器"升级为"能办事的助手"。

需要解决：

1. **工具调用** — LLM 输出 tool_call → agent 执行 → 把结果回填为 tool 消息 → 再问 LLM
2. **工具管理** — 多种来源（内置 / MCP）需要统一注册中心 + 权限控制
3. **可观测性** — 谁在什么时候调了哪个工具，结果是什么，要留痕
4. **技能复用** — 同一组工具 + 提示词要能打包成"技能"，用户用一句 `/daily-summary` 触发
5. **MCP 协议** — 工具生态的主流标准，stdio 客户端先把骨架搭起来

不做的话，后续 Phase 3（记忆 / 上下文）、Phase 4（飞书）都没有 agent loop 可用。

## What Changes

### ADDED

- 依赖：
  - `github.com/mark3labs/mcp-go` v0.x（MCP stdio 客户端）
  - `gopkg.in/yaml.v3` v3.x（Skill frontmatter 解析）
- `internal/tool/`：Tool 注册中心 + 权限白名单 + 内置工具
- `internal/agent/`：基于 eino `flow/agent/react` 的 ReAct 循环
- `internal/mcp/`：MCP stdio 客户端，把 MCP 工具桥接到 `tool.Registry`
- `internal/skill/`：Skill 加载（markdown + YAML frontmatter），可挂到 agent
- 工具调用审计日志（`tool_invocations` 表）
- 内置工具：`time`（当前时间 / 时区）、`calc`（表达式求值）、`echo`（调试用）
- 内置 Skill 模板：`daily-summary`、`code-review`
- chat CLI 新增 `--tools` / `--skill` flag

### NOT in this change

- MCP SSE 客户端（先做 stdio；SSE 等有人提需求再做）
- Skill 匹配（向量 / embedding）—— MVP 仅支持显式 `--skill` 或聊天中 `@skill` 触发
- 工具权限 UI（Phase 5 Admin 一起做）
- 工具调用确认（"是否执行 rm -rf？"）—— MVP 直接执行，权限白名单控制范围
- 长期记忆 / 摘要（Phase 3）
- 飞书 / Web UI（Phase 4+）

## Impact

| 影响面 | 影响 |
|---|---|
| 依赖 | +2 个 module（mcp-go / yaml.v3）|
| 配置 | `agent` 段：默认 tool 白名单 + skill 路径 |
| 存储 | 新增 `tool_invocations` 表 |
| CLI | `chat` 新增 `--tools` `--skill` flags |
| 行为 | 启用 tools 时 chat 会执行 LLM 请求并把 tool 结果拼回 prompt |

## Success Criteria

- [ ] `huan-agent chat --tools time,calc` 能让 LLM 决定调用工具并把结果合并到回复
- [ ] tool 白名单生效：未在白名单的工具即使注册了也不传给 LLM
- [ ] 工具执行失败时把错误回填给 LLM 而不是 crash agent
- [ ] 退出 REPL 后 `tool_invocations` 表有本次会话所有工具调用记录
- [ ] `internal/skill` 能从 `configs/skills/*.md` 加载 Skill，frontmatter 解析正确
- [ ] `--skill daily-summary` 启用后 system prompt 包含该 skill 的内容
- [ ] 内置 mcp stdio 客户端能 spawn 一个 mcp server 子进程并列出 tools（用 `mcp-go` 的 example 验证）
- [ ] `make test` 全绿，Phase 2 新增包覆盖率 ≥ 70%
- [ ] 端到端 smoke：`echo` 工具 + mock LLM（不调真实 provider）跑通 agent loop

## Risks

| 风险 | 缓解 |
|---|---|
| LLM 不支持 tool_call（部分老模型） | 自动检测 `tool_calls` 字段；不启用 tools 时退化为普通 chat |
| Tool call 死循环（一直调同一个工具） | 限制 max_steps（默认 10），超出报错 |
| MCP 子进程僵死 | context.Cancel + subprocess.Wait + 超时 kill |
| Skill frontmatter 解析失败 | 启动时校验目录下的 .md，加载失败只 warn 不阻塞启动 |
| eino ReAct 与我们的 usage recorder 整合 | 用 eino callback 拦截 OnEnd 事件，提取 TokenUsage |

## Rollback

纯新增（tool / agent / mcp / skill 包 + 新 flag + 新表），不影响现有 chat 行为。
如需回滚，drop 工具表 + 移除 `--tools` flag 即可。
