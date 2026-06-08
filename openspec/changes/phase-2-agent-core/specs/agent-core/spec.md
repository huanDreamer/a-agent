# agent-core

## Purpose

实现 ReAct Agent 主循环：LLM 思考 → 决定调工具 → 执行工具 → 把结果回填 → 再次询问 LLM，直到 LLM 输出无 tool_call 的最终消息。

## Requirements

### R1: Agent 结构

`Agent` MUST 持有：

- `model model.BaseChatModel`（来自 `internal/llm`）
- `tools *tool.Registry`（来自 `internal/tool`）
- `maxSteps int`（默认 10）
- `recorder *usage.Recorder`（可选，用于把每次 LLM 调用的 token 用量写入 SQLite）
- `audit *AuditLog`（可选，工具调用审计）
- `logger *zap.Logger`

### R2: 同步 / 流式入口

`Agent` MUST 实现：

- `Generate(ctx, messages) (*schema.Message, error)` — 阻塞，返回最终 assistant 消息
- `Stream(ctx, messages) (*schema.StreamReader[*schema.Message], error)` — 流式返回 assistant 内容；tool_call 中间步骤对调用方不透明（不向外暴露中间流）

### R3: ReAct 循环

Agent MUST 使用 eino `flow/agent/react` 实现循环：

- 调 model
- 如果 message 含 `ToolCalls`：依次执行 tools（受白名单控制），把每条 `tool` 消息追加到 history
- 回到第 1 步，直到 model 输出无 `ToolCalls` 的 message
- `maxSteps` 超出 MUST 终止并返回 `agent: max steps exceeded` 错误

### R4: 工具调用审计

每次 tool 执行 MUST 记录一条 `tool_invocations` 行：

| 字段 | 类型 | 说明 |
|---|---|---|
| id | INTEGER PK | |
| session_id | TEXT NOT NULL | 会话标识 |
| tool_name | TEXT NOT NULL | 工具名 |
| arguments | TEXT NOT NULL | 入参 JSON |
| result | TEXT | 出参（错误时为空）|
| error | TEXT | 错误信息（成功时为空）|
| duration_ms | INTEGER NOT NULL | |
| created_at | TIMESTAMP | |

失败 MUST 写入数据库（不丢失错误信息），写入失败 MUST 走 zap.Warn 兜底。

### R5: LLM 用量埋点

每次 model 调用结束 MUST 从 `ResponseMeta.Usage` 提取 token 计数，调用 `usage.Recorder.Record(Event)`。

### R6: 错误恢复

- 单个 tool panic MUST 被 recover 并作为该 tool 的 error 返回给 LLM
- 单个 tool error MUST 包装为 `tool result: <error>` 文本继续后续 ReAct 步骤（不立即终止）
- Model 调用 error MUST 终止 agent 并把 error 透传

### R7: Skill 注入

`Agent.Generate` / `Stream` 接受可选 `[]*skill.Skill`，启动时把每个 skill 的 `SystemPrompt()` 拼成 system message 注入 messages 列表头部。

## Out of Scope

- 多 Agent 协作（Phase 7）
- 计划 / 任务分解（Plan-and-Execute）—— MVP 仅 ReAct
- 长期记忆 / 上下文压缩（Phase 3）
- 人机协作确认工具调用

## Dependencies

- eino `flow/agent/react`
- `internal/llm` 提供 model
- `internal/tool` 提供 registry
- `internal/usage` 提供 recorder
- `internal/store` 持久化审计（`tool_invocations`）
- `internal/skill`（Phase 2 同阶段）可选注入
