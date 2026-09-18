# Phase 14 任务清单

## 1. `internal/store`：工具调用聚合

- [x] `InvocationTotals{Calls, DurationMs}` + `QueryInvocationTotals(ctx, sessionID)`
      （单条 `COUNT(*)` / `SUM(duration_ms)`，走 `idx_inv_session`）
- [x] `Store` 接口补该方法
- [x] 空会话返回 0（`COALESCE`），不是 NULL

## 2. `internal/server`：会话统计

- [x] `sessionStats` 结构 + JSON 标签（`turns` / `llm_calls` / `llm_duration_ms` /
      `tool_calls` / `tool_duration_ms` / `prompt_tokens` / `completion_tokens` /
      `total_tokens` / `messages`）
- [x] `sessionStatsFor(ctx, sessionID, msgs)`：消息在手里，工具耗时一次聚合查询
- [x] 口径：轮数 = 用户消息；模型调用 = assistant 消息；工具调用 = `tool_calls` 条目数
- [x] 复用既有 `parseUsage`（不新增第二个 usage 解析器）
- [x] `tool_calls` / `usage_json` 无法解析时不计数、不报错、不编数字
- [x] 审计聚合失败：warn + 工具耗时 0，其余数字照常
- [x] 转录工具调用数与审计行数不一致时记 debug（说明有调用没跑或审计丢行）
- [x] `handleGetSession` 返回 `stats`
- [x] 测试：多轮多工具会话的每个数字、空会话全 0、坏 JSON 容错、聚合查询失败降级

## 3. `web`：顶栏状态行

- [x] `src/sessionStats.js`：`normalizeStats`（非数字 → 0）、`statsSegments`（零/缺省字段不出段、
      轮数恒在）、`statsTitle`（逐段说明 + 「耗时不是墙钟长度」这句提醒）
- [x] `ChatView.vue`：状态行放在「共 N 条消息」左边，每段独立 hover
- [x] `chatStore.js`：`stats` 状态；`selectSession` 写入，`createSession` / `clearSession` 归零
- [x] `styles.css`：`.chat-stats`（段间可换行、段内不断行）
- [x] `scripts/check-stats.mjs` 接进 `npm run check:ui`
- [x] `ssr-probe`：对话顶栏探针（有数据的会话 + 空会话两种），并给探针加 DOMPurify 替身
      （Node 没有 DOM，而顶栏会间接 import markdown.js）
- [x] 重新构建 `internal/server/webui/dist` 与二进制

## 4. 校验

- [x] `node scripts/check-stats.mjs` 全绿
- [x] `npm run check:ui` 全绿（含新探针）
- [x] `go test ./internal/... ./cmd/...` 全绿
- [x] 用真实数据核对口径（会话 `1ec0f82c`：15 轮 / 14 次模型调用 / 421 次工具调用 /
      模型耗时 1142229ms / 工具耗时 182682ms / 7679458 tokens；转录与审计表的工具调用数一致）
- [ ] **待用户重启**：本机 8080 上跑的仍是旧二进制（`GET /api/chat/sessions/{id}` 里还没有
      `stats` 字段），重启后顶栏才会出现这行状态。按约定启动/重启由用户执行。
