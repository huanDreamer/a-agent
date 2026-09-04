# memory-system

## 目标

为 huan-agent 提供短期 + 长期记忆，按会话/用户命名空间隔离，MVP 用 JSONL 文件 + 关键词检索，不引入向量库。

## 类型与接口

- `memory.Store`：`Append` / `Read` / `Facts` / `AddFact` / `SearchFacts` / `Close`
- `memory.Entry { Kind, Content, Meta, CreatedAt }`；`Kind ∈ {user, assistant, tool, fact, summary}`
- `memory.Fact`：`{ Key, Value, Keywords, CreatedAt }`，按关键词索引
- `memory.Buffer`：短期记忆，最近 `max_turns` 轮，超出逐出 oldest
- `memory.Store` 以 JSONL 追加写，命名空间路径规范化（防穿越）

## 行为

- 短期记忆：会话内保留最近 N 轮（`memory.max_turns`）
- 长期记忆：`AddFact` 持久化到命名空间文件，`SearchFacts` 按关键词命中排序
- 命名空间隔离：`user-<id>/session-<id>` 互不串读

## 网关

- REPL `/remember key: value` → `AddFact`
- REPL `/recall <query>` → `SearchFacts`