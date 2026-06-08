# usage-tracking

## Purpose

记录每次 LLM 调用的 token 用量、模型、provider、耗时，为后续用量统计 / 费用计算 / 限流提供基础。

## Requirements

### R1: 数据持久化

每次 LLM 调用结束后 MUST 异步写入 `usage_logs` 表，字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| id | INTEGER PK AUTOINCREMENT | |
| session_id | TEXT NOT NULL | 区分不同会话 |
| provider | TEXT NOT NULL | provider 名称 |
| model | TEXT NOT NULL | 实际调用的模型 |
| prompt_tokens | INTEGER NOT NULL | 输入 token 数 |
| completion_tokens | INTEGER NOT NULL | 输出 token 数 |
| total_tokens | INTEGER NOT NULL | 总 token 数 |
| duration_ms | INTEGER NOT NULL | 调用耗时（毫秒）|
| created_at | TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP | 写入时间 |

### R2: Recorder 接口

`Recorder` MUST 提供：

- `Record(Event) error` — 异步入队（非阻塞）
- `Close() error` — 同步 flush 队列并关闭 store

异步队列 MUST 是有界 channel（默认 1000），写满时阻塞 back-pressure 而不是丢弃。

### R3: 异步写盘不阻塞主流程

- 写入失败的 event MUST 被记录到日志（zap.Warn）但不 panic
- 进程退出时 MUST 确保已 flush 队列（用 signal handler 配合 `Close`）

### R4: 查询接口（MVP 可选）

`QueryUsage` 接收 `UsageFilter`（session_id / provider / time range），返回 `[]UsageRecord`。MVP 可以只暴露给后续 phase 使用，本 phase 不强制有 CLI 入口。

## Out of Scope

- 费用计算（按 ¥/$ per 1k tokens）— Phase 5
- 限流（按用量）— Phase 6
- 实时 dashboard
- 跨设备同步

## Dependencies

- `internal/store` 提供 `RecordUsage` / `QueryUsage` 接口
- 配置文件 `database.path` 决定存储位置
