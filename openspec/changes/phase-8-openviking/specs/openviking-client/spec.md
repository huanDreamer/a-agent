# Capability: openviking-client

## Purpose

把 OpenViking 的 HTTP API 收进一个可测试的 Go 客户端，让「记忆」「文档」两条业务
链路共用同一份传输、认证与错误语义，而不是各自拼 curl。

## Scope

- `internal/openviking` — REST 客户端与错误类型。
- `internal/config` — `openviking` 配置节与默认值。
- `cmd/huan-agent` — 客户端装配。

## Requirements (MUST)

1. **配置驱动。** BaseURL / APIKey / Account / User / Timeout MUST 全部来自配置
   （Viper，可用 `HUAN_OPENVIKING_*` 覆盖），MUST NOT 硬编码；`base_url` 为空时
   MUST 报错而不是静默指向某个默认主机。
2. **URL 规整。** 结尾多余 `/` MUST 被去掉；`base_url` 已带 `/mcp` 等路径时 MUST
   只在末尾拼接 API 路径，不重复前缀。
3. **认证。** `api_key` 非空时 MUST 以 `X-API-Key` 发送；同时 MUST 发送
   `X-OpenViking-Account` / `X-OpenViking-User`（非空时）。dev 模式（无 key）MUST
   正常工作。
4. **错误信封。** openviking 的 `{"status":"error","error":{"code","message"}}`
   MUST 被翻译成 `*APIError`，并保留 HTTP 状态码与 `x-request-id`（若响应头有）。
   非 2xx 但不是该信封时 MUST 也返回 `*APIError`（带原始摘要）。
5. **可用性判定。** 提供 `IsUnavailable(err)`：网络错误、超时、连接被拒、5xx MUST
   判定为不可用（调用方据此降级）；4xx 语义错误（如 URI 非法、模型未开通）MUST NOT
   被当作不可用。
6. **超时。** 每个请求 MUST 受 `context` 与客户端超时双重约束；`content/write` 与
   `resources` 这类可能触发后台处理的调用 MUST 允许调用方显式指定更长的等待。
7. **API 面。** MUST 提供 `Health`、`Find`、`Remember`（批量消息 + 可选 commit）、
   `WriteContent`、`ReadContent`、`Stat`、`UploadTemp`、`AddResource`、`TaskStatus`。
8. **凭据不外泄。** APIKey MUST NOT 出现在任何错误信息、日志或返回给前端的结构里。
9. **可测试。** 客户端 MUST 允许注入 `*http.Client` / base transport，使全部行为可
   用 `httptest` 覆盖，不需要真的起一个 openviking。

## Non-goals

- 账户、用户、API Key 管理接口。
- sessions 生命周期管理、snapshot、ovpack 导入导出。
- 重试与熔断（降级由调用方按 `IsUnavailable` 决定）。
