# Phase 3 — Memory & Context 管理

## Why

Phase 2 让 huan-agent 能"做事"，但每个会话是无状态的：聊得越久，`history` 越长，最终撑爆 LLM context window，且重启后（或更换会话后）完全忘了以前的内容。真实个人助手必须"有记忆、不至于爆上下文"。

需要解决：

1. **长会话不爆炸** — trigger 阈值到达时，把旧轮次压缩成摘要，始终保留最近 K 条，让 context 窗口有界。
2. **短期记忆** — 会话内保留最近 N 轮，超出即逐出。
3. **长期记忆** — 跨会话保留关键事实，支持按关键词检索（MVP 用 JSONL + 关键词索引，不引入向量库）。
4. **可配置** — token 预算、保留轮数、是否用 LLM 自摘要都走配置。

不做的话，Phase 4（飞书）接入后的长对话会立刻遇到 context 溢出问题。

## What Changes

### ADDED

- `internal/memory/`：长期记忆 JSONL 存储（按 user/session 命名空间隔离）+ 关键词索引 + 短期 Buffer（最近 N 轮）
- `internal/context/`：token 估算 + 自动压缩（Summarize 摘要旧轮次、始终保留最近 K 条）
- 配置节 `memory.*`、`context.*`（Viper + env 覆盖）
- chat CLI 接入：`/remember`、`/recall` 命令；会话结束写入长期记忆
- 摘要器：able 用 LLM 自己摘要（`context.Summarize` 为 true 时）
- openspec：`memory-system`、`context-management` 能力

### 不在本阶段

- 向量库 / embedding（MVP 用关键词索引，Phase 7 再说）
- 记忆的 Web UI 管理
- 长期记忆自动抽取事实摘要（需 embedding / 规则抽取，后续）
- 多用户路由（Phase 4 一起）
- 记忆的过期 / TTL

## 变更文件

```
internal/memory/memory.go        # Store / Entry / Fact + JSONL 实现
internal/memory/buffer.go        # 短期记忆 Buffer（最近 N 轮）
internal/memory/jsonl.go         # JSONL 读写 + 关键词工具
internal/memory/memory_test.go   # 测试
internal/context/context.go      # Budget / Manager / Summarizer / 估算器
internal/context/context_test.go # 测试
internal/config/config.go        # memory / context 配置节
cmd/huan-agent/memory.go         # sessionMemory：REPL 侧组装
cmd/huan-agent/chat.go           # 接入 memory+context + REPL 命令
configs/config.example.yaml      # memory / context 示例
```

## 验证标准

- `go build ./...` 通过
- `go test ./...` 通过，memory / context 覆盖率 ≥ 70%
- `golangci-lint run` 通过
- REPL 中长会话触发摘要（`context.max_tokens` 设置较小值时）
- `/remember`、`/recall` 生效