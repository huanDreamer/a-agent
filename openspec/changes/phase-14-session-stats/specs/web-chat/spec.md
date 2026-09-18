# web-chat（增量）

本次变更在对话顶栏增加一行统计；其余行为不变。

## API

- `GET /api/chat/sessions/{id}` 的响应增加 `stats`：

```json
{
  "messages": 120,
  "turns": 7,
  "llm_calls": 23,
  "llm_duration_ms": 184000,
  "tool_calls": 41,
  "tool_duration_ms": 5200,
  "prompt_tokens": 812345,
  "completion_tokens": 41230,
  "total_tokens": 853575
}
```

- 字段口径（服务端固定，客户端只渲染）：
  - `turns`：用户消息数，即这段对话里发起的请求数；
  - `llm_calls`：assistant 消息数 —— 一条 assistant 消息就是一次完成的模型调用；
  - `llm_duration_ms`：这些调用 provider 上报耗时之和；
  - `tool_calls`：assistant 消息里 `tool_calls` 的条目数（页面上工具卡片的同源计数）；
  - `tool_duration_ms`：`tool_invocations` 审计表里这段会话的实测耗时之和；
  - `*_tokens`：provider 上报的 usage 之和；provider 不上报时保持 0，不估算。
- 两个耗时都**不是**这段对话的墙钟长度：排队、步与步之间的思考、以及用户本人的停顿都不计。

## 行为

- 顶栏在「共 N 条消息」之前渲染：`轮 X · 模型 N 次 · 时长 · 工具 N 次 · 时长 · T tokens`。
- 值为 0 或缺失的维度**不出段**（空对话只显示「轮 0」），因此「0 次」永远不会看起来像
  一次测量结果；token 为 0（provider 未上报）时同样不显示。
- 每个段各自带 hover 说明（口径），整行另有说明：耗时不是墙钟长度。
- 数字随每轮结束后的 authoritative reload 刷新；不做飞行中的实时递增 —— 未落库的数字在
  结束时必然跳变，回退的数字比稍慢的数字更容易被误读。
- `clearSession` 后归零；`createSession` 时为零值；历史对话（没有 `stats` 的旧响应）按 0 处理。
