# context-management

## 目标

让长会话的上下文窗口有界：估算 token 用量，超预算时自动压缩（把旧轮次合成摘要，保留最近 K 条）。

## API

- `context.Budget{ MaxTokens, KeepRecent, Summarizer }`
- `context.Manager`：`ShouldCompress(msgs)` → `Compress(ctx, msgs) ([]*schema.Message, summary string, err)`
- `context.Estimator` + `DefaultEstimator`（≈ chars/4 + 4 头部开销）
- `context.Summarizer`：`Summarize(ctx, preMsgs) (string, error)`

## 行为

- `MaxTokens == 0` → 禁用压缩
- 超出预算且 `KeepRecent < len(msgs)` → 把 `len-K` 条旧消息交给 Summarizer（或占位符）产出摘要，前插一条 `System` 摘要消息，保留最近 K 条
- 摘要失败或 `KeepRecent >= len` → 保底返回原窗口或占位摘要，不产生空窗口

## 接入

- REPL 在每次发请求前对组装窗口调用 `Compress`
- `Summarizer` 默认用 `llmSummarizer`（LLM 自摘要），`context.summarize=false` 时降级为占位符