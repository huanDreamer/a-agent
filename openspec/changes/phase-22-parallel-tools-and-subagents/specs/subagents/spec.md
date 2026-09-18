# Capability: subagents

## Purpose

把"为了回答一个问题而必须读的三十个文件"放进**另一个上下文窗口**，只把结论带回主轮次。它买的
是上下文预算，不是算力：主 agent 的窗口留给代码本身，脏活留在子 agent 那里。

## Scope

- `internal/subagent` — 循环无关的子运行：限制、报告、预算并入。
- `internal/tool/builtin` — `spawn_agent` 工具。
- `internal/chat` / `internal/agent` — 各自提供一个 `Runner` 实现。
- `internal/server` — 嵌套事件的归属与 Web 卡片。

## Requirements (MUST)

1. **只回一份有界报告。** 子 agent 的最终文本（截断到 `subagent.max_report_chars`，默认 8000）
   加一份结构化尾注（步数、用过的工具、token 用量、`stop_reason`、失败原因）MUST 是唯一进入父
   轮次上下文的东西。子 agent 的中间工具结果与思考 MUST NOT 进入父上下文——这是本能力存在的
   全部理由，也 MUST 有测试守卫。
2. **能力默认收窄。** 子 agent 的注册表默认只含父注册表里的**只读**且 `ParallelSafe` 的工具。
   写入、执行与其它工具 MUST 只有通过显式参数列出才被授予，且仍然受父轮次的工作区策略与审批
   闸门约束（子 agent MUST NOT 成为绕过审批的路径）。
3. **一律不授予。** `ask_user`（它没有通向人的通道；需要决定就把问题写进报告）、`plan_*`（计划
   属于顶层轮次，子 agent 改看板会让用户看到不属于自己的进度）、`spawn_agent`（深度上限 1）。
4. **深度上限 1。** 子 agent MUST NOT 再派生子 agent。递归需要全局预算树，是另一个 change。
5. **预算并入父轮次。** 子 agent 的 token 与耗时 MUST 计入父轮次的预算与 usage 记录，并 MUST
   共享父轮次的 deadline。一个可以活得比轮次更久的子 agent 会让
   `chat.turn_deadline_seconds` 与 `chat.turn_max_tokens` 失去意义。
6. **有界并发。** `subagent.max_concurrent`（默认 2）MUST 是一个跨所有轮次共享的闸门；超出时
   排队，MUST NOT 报错。
7. **失败不连坐。** 子 agent 的失败 MUST 以一份错误报告的形式回到模型（作为工具结果），
   MUST NOT 让父轮次失败。
8. **取消传播。** 父轮次被停止或超时时，子 agent MUST 立即收敛，MUST NOT 留下仍在运行的
   goroutine。
9. **嵌套可见但不入上下文。** 子 agent 的事件 MUST 带上 `parent_tool_call_id`，使控制台能把它
   的步骤嵌在 `spawn_agent` 那张卡片下面；子 agent 的文本/思考增量 MUST NOT 被拼进父轮次的
   答案。每个子 agent MUST 是链路追踪里一个独立的 span。
10. **可配置关闭。** `subagent.enable=false` 时 `spawn_agent` MUST NOT 注册。

## 工具接口

```json
{
  "prompt": "调研 internal/chat 与 internal/agent 两个循环各自的工具执行路径，回一段结论",
  "name": "loop-survey",
  "tools": ["read_file", "grep", "glob"],
  "model": "deepseek-chat",
  "max_steps": 8,
  "timeout_seconds": 120
}
```

| 参数 | 必填 | 语义 |
|---|---|---|
| `prompt` | 是 | 交给子 agent 的任务；它看不到父轮次的任何历史 |
| `name` | 否 | 显示用短名，进日志、卡片与追踪 |
| `tools` | 否 | 额外授予的工具名（只读之外的能力才有必要列） |
| `model` | 否 | 覆盖 provider/model（便宜模型做探索是主要用法） |
| `max_steps` | 否 | 覆盖 `subagent.max_steps`，有上限 |
| `timeout_seconds` | 否 | 覆盖默认，且不得超过父轮次剩余时间 |

返回 = 报告文本 + 尾注。尾注 MUST 用结构化字段而不是自由文本，使调用方能判断"这个结论是基于
多少步得出的"。

## 事件

```json
{ "type": "tool_call", "step": 2, "tool_call_id": "call_9", "tool_name": "spawn_agent",
  "tool_args": "{…}" }
{ "type": "tool_call", "step": 2, "tool_call_id": "sub_1", "parent_tool_call_id": "call_9",
  "tool_name": "grep", "tool_args": "{…}" }
{ "type": "tool_result", "step": 2, "tool_call_id": "sub_1", "parent_tool_call_id": "call_9",
  "tool_result": "…" }
{ "type": "tool_result", "step": 2, "tool_call_id": "call_9", "tool_result": "报告…" }
```

- `parent_tool_call_id` 为空表示顶层轮次自身的事件（既有客户端的退化路径）。
- 子 agent 的 `text_delta` / `reasoning_delta` 同样带 `parent_tool_call_id`，累积器 MUST 据此
  分流，不把它们算进父轮次的答案。

## 配置

```yaml
subagent:
  enable: true
  max_steps: 8
  max_concurrent: 2
  max_report_chars: 8000
  timeout_seconds: 0     # 0 = 用父轮次剩余时间
```

## Non-goals

- 子 agent 默认具备写权限（需要它自己的审批与检查点故事）。
- 递归（深度 > 1）。
- 子 agent 的后台化 / 稍后收结果（会让它活得比轮次长，与共享 deadline 冲突）。
- 子 agent 之间互相通信或共享状态：它们各自独立，结论由主 agent 汇总。
- 把子 agent 的完整步骤存进数据库（它属于这一轮的展示，不属于会话历史）。
