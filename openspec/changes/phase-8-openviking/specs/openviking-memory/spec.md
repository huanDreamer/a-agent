# Capability: openviking-memory

## Purpose

让长期记忆同时拥有**本地真源**与**语义可检索的远端副本**：对话轮次与关键事实进
OpenViking，召回优先走向量检索；OpenViking 出问题时对话与本地记忆不受影响。

## Scope

- `internal/memory` — `OpenVikingStore` 装饰器与统计。
- `cmd/huan-agent/memory.go` — 会话记忆的装配与召回。
- `internal/config` — `openviking.memory.*`。

## Requirements (MUST)

1. **本地为真源。** `Append` / `AddFact` MUST 先完成本地 JSONL（或既有本地商店）写入；
   本地写失败 MUST 返回错误，远端写失败 MUST NOT 影响本地结果。
2. **双写记忆。** 当 `openviking.memory.enable` 为真时，`user` / `assistant` 轮次
   MUST 批量推送到 openviking 会话并提交（`commit`），使 openviking 侧做长期记忆抽取。
3. **攒批。** 轮次 MUST 按 `flush_every` 攒批提交（`0` = 每轮立即提交）；
   `Flush` / `Close` MUST 把剩余消息提交掉，保证进程退出不丢。
4. **事实直写。** `AddFact` MUST 除提交抽取外，另写一条**不依赖 VLM** 的直写记录
   （append 到 `viking://.../memory/journal.md`），使 VLM 未开通时关键事实仍可被
   `find` 检索到。两条路互为补充，任一条失败 MUST NOT 让 `AddFact` 失败。
5. **语义召回。** `SearchFacts` MUST 优先用 openviking 的语义检索（`Find`，限定
   `recall_target` 子树），把命中条目映射回 `Fact`；当检索不可用、超时或返回空时
   MUST 回落本地关键词索引，MUST NOT 返回错误。
6. **降级是常态。** OpenViking 不可用时 MUST 只记 WARN（含 `IsUnavailable` 判定结果）
   并计入统计，MUST NOT 阻断对话、MUST NOT 让 `/remember` 失败。
7. **统计可见。** 提供 `Stats()`：成功写入数、失败数、待提交消息数、最近一次错误与
   时间；供状态接口与控制台展示。
8. **目标隔离。** 召回 MUST 默认限定在当前用户子树（`viking://user/<user>/huan-agent`），
   避免命中同一 openviking 实例里其它项目的资料；范围 MUST 可配置。
9. **可测。** 装饰器 MUST 能用假客户端（httptest）在没有真实 openviking 的情况下
   测试：攒批、提交、降级、召回回落。

## Non-goals

- 迁移历史 JSONL 到 openviking。
- 记忆的删除/遗忘策略（openviking 侧能力，本次不暴露）。
- 多用户隔离（控制台与 IM 都是单用户场景）。
