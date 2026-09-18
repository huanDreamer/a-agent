# llm-retry

## 目标

让"一次模型调用失败"不再等于"整个任务报废"。失败的调用按**指数退避**重试；确定重试也没用的
失败立即返回，不浪费用户的钱；流已经吐出一半再断的情况，交给上层（步骤级重试）处理，因为
只有它知道怎么让界面不看到重复内容。

## API

- `internal/retry`：
  - `retry.Policy{ Enabled bool; MaxAttempts int; InitialBackoff, MaxBackoff time.Duration;
    Multiplier float64; Jitter bool }`
  - `(Policy) Delay(attempt int) time.Duration` —— `attempt` 从 1 开始（"第几次重试"）。
  - `(Policy) Do(ctx, fn func(context.Context) error, onRetry func(RetryInfo)) error`
  - `retry.Disabled` —— 零值策略，`Do` 直接执行一次。
- `internal/llm`：
  - `Provider.Retry retry.Policy`；`New` 在 `Enabled` 时把模型包成 `retryingModel`。
  - `(*LLMError) Retryable() bool`。
- `internal/chat`：`Config.StepRetry retry.Policy`（步骤级，见 `turn-recovery` 的补充说明）。

## 行为

- 退避序列：`InitialBackoff * Multiplier^(attempt-1)`，封顶 `MaxBackoff`。`Jitter` 打开时每条
  间隔乘以 `[0.5, 1.0)` 的系数（**只缩短不延长**，因此封顶是硬上限）。
- `MaxAttempts` 是**总尝试次数**（含首次）：`MaxAttempts: 3` = 首次 + 2 次重试。`<= 0` 视为
  1（等于关闭）。
- 可重试：网络/连接类错误、超时、`context.DeadlineExceeded`（调用自己的超时，不是调用者的
  取消）、HTTP 408 / 409 / 425 / 429 / 5xx、以及不携带状态码且不是解析错误的传输失败。
- 不可重试：400 / 401 / 403 / 404 / 413 / 422、余额/配额类 4xx（402）、请求构造与解析错误、
  `context.Canceled`。
- 调用者的取消（`context.Canceled`）永远不重试：用户按了停止，或者服务在退出。
- `Stream` 的重试只覆盖**建立阶段**：从发起请求到第一个 chunk 抵达。一旦有 chunk 交给读者，
  后续错误原样上抛（重放会重复输出）。
- 每次重试写一条 warn 日志：provider、model、attempt、delay、error。
- 重试对上层透明：`retryingModel` 实现 `model.BaseChatModel` 与 `model.ToolCallingChatModel`，
  `WithTools` 返回的副本仍然带重试。

## 配置

```yaml
llm:
  retry:
    enable: true
    max_attempts: 3        # 总尝试次数（含首次）
    initial_backoff_ms: 800
    max_backoff_ms: 30000
    multiplier: 2.0
    jitter: true
```

- 缺失时用上面的默认值（`enable` 默认 true）。
- `max_backoff_ms < initial_backoff_ms` 时抬到 `initial_backoff_ms`（否则退避会越重试越短，
  与意图相反）。

## 边界

- 不做模型/provider 之间的故障转移（换一个 provider 继续跑）：那是路由策略，不是重试。
- 不做请求去重：一次调用被重试时，上游可能已经处理过第一次请求（对 chat 补全而言是浪费
  token，不是错误）。
- 不重试工具调用：工具的错误是观察值，模型的下一步会处理它（现状不变）。
