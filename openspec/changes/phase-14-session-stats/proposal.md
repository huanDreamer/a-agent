# Phase 14 — 对话顶部状态：轮数 / 模型调用 / 工具调用 / token

## Summary

对话顶栏在「共 N 条消息」之前只有一个数字：消息条数。它既不是这一轮的成本，也不是这段
对话的工作量 —— 一条消息可能触发 40 次工具调用和 800 万 token，也可能什么都不做。本次变更
在它左边加一行状态：

```
轮 7 · 模型 23 次 · 3m04s · 工具 41 次 · 5.2s · 854K tokens · 共 120 条消息
```

每个数字各自带一个 hover 说明（口径 + 原始精度），因为一行紧凑的数字最容易让人猜错口径。

## Why

- **「这段对话花了多少」是刷新页面之后还想知道的事。** 现在要回答它得翻用量页并按会话
  过滤；而顶栏就在眼前。
- **两个耗时必须分开。** 模型调用耗时与工具耗时是两种完全不同的成本：一个是 token 买来的
  时间，一个是本地/远端执行的时间。合成一个数就分不清「模型慢」还是「命令慢」。
- **统计口径必须只有一个。** 浏览器数一遍、服务端数一遍，两边就会在刷新前后给出不同的
  数字。因此聚合放在服务端，随 `GET /api/chat/sessions/{id}` 一起返回。
- **零不该看起来像测量值。** 没有工具调用的对话不显示「工具 0 次」；provider 不上报 usage
  时不显示「0 tokens」。

## What Changes

### ADDED

1. **`internal/store`：一次聚合查询**
   - `QueryInvocationTotals(ctx, sessionID) (InvocationTotals{Calls, DurationMs}, error)`：
     工具耗时不在消息里（消息只存工具的输出），只在审计表 `tool_invocations` 里；
     读整页审计行只为加一个数会让对话越长越慢，因此按 `session_id`（已有索引）聚合。

2. **`internal/server`：会话统计（`sessionstats.go`）**
   - `sessionStats{Messages, Turns, LlmCalls, LlmDurationMs, ToolCalls, ToolDurationMs,
     PromptTokens, CompletionTokens, TotalTokens}`，随 `GET /api/chat/sessions/{id}` 的
     `stats` 字段返回。
   - 口径明确写进注释并在本提案里固定：**轮数 = 用户消息数**；**模型调用 = assistant 消息数**
     （一条 assistant 消息就是一次完成的模型调用）；**工具调用 = assistant 消息里 `tool_calls`
     的条目数**（与页面上渲染的工具卡片同源，因此不可能与画面不一致）；耗时分别是 provider
     上报的单次耗时之和与审计表实测耗时之和；token 来自 provider 上报的 usage。
   - 聚合失败不是页面失败：审计聚合读不到时记一条 warn，工具耗时按 0 报，其余数字照常。

3. **`web`：顶栏状态行**
   - `src/sessionStats.js`：`normalizeStats` / `statsSegments` / `statsTitle` —— 纯函数，
     负责「哪些字段值得一提、按什么顺序、每个的 hover 说明是什么」。
   - `ChatView.vue` 顶栏在「共 N 条消息」之前渲染这行；`chatStore.js` 的 `stats` 状态由
     `selectSession` 写入、`createSession` / `clearSession` 归零；每轮结束后的
     authoritative reload 已经会刷新它，无需新的轮询。
   - `styles.css` 的 `.chat-stats`：flex 行 + 允许在段之间换行。

### 明确不做

- **不做跨会话/全局统计**：那是「统计监控」页的事，这里只描述当前这段对话。
- **不显示费用**：价格表匹配是另一层（`internal/pricing`），且未匹配时要显示「未知」，
  不适合塞进一行状态；顶栏只给 token。
- **不做实时逐字更新**：统计在每轮结束时随会话重载刷新。飞行中的数字没有落库，
  显示出来只会在结束时跳变（甚至回退），而回退的数字比稍慢的数字更容易被误读。

## Impact

- `internal/store`：`invocation.go`（聚合）、`store.go`（接口）。
- `internal/server`：新增 `sessionstats.go`，`chat.go` 的 `handleGetSession` 加上 `stats`。
- `web/`：新增 `src/sessionStats.js`、`scripts/check-stats.mjs`；改动 `ChatView.vue`、
  `chatStore.js`、`styles.css`、`package.json`（check 脚本）；`ssr-probe` 增加对话顶栏探针
  与 DOMPurify 替身（Node 没有 DOM）。
- 测试：`internal/server/sessionstats_test.go`（含真实形状的多轮/多工具会话、空会话、
  坏 JSON 容错、聚合查询失败降级）。
