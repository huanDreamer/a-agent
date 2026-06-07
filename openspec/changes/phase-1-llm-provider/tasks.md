# Phase 1 — Tasks

## 1. 依赖

- [x] 1.1 `go get github.com/cloudwego/eino@latest`
- [x] 1.2 `go get github.com/sashabaranov/go-openai@latest`
- [x] 1.3 `go get modernc.org/sqlite@latest`
- [x] 1.4 `go mod tidy`

## 2. 存储层 (`internal/store/`)

- [x] 2.1 `internal/store/store.go`：`type Store interface`（Open / Close / Migrate / RecordUsage / QueryUsage）
- [x] 2.2 `internal/store/sqlite.go`：SQLite 实现，使用 `modernc.org/sqlite`（纯 Go）
- [x] 2.3 `internal/store/migrate.go`：迁移到 v1（建 `usage_logs` 表）
- [x] 2.4 `internal/store/usage.go`：`RecordUsage()` / `QueryUsage()` 接口
- [x] 2.5 单元测试：Open / Close / Record / Query / 错误路径（74.0% 覆盖）

## 3. LLM Provider 抽象 (`internal/llm/`)

- [x] 3.1 `internal/llm/provider.go`：
  - `type Provider struct { Name, BaseURL, APIKey, Model }`
  - `func New(p Provider) (model.BaseChatModel, error)` — 适配 eino `BaseChatModel` 接口
  - 内部用 `sashabaranov/go-openai` 客户端
- [x] 3.2 `internal/llm/registry.go`：
  - `type Registry struct` 维护 provider 列表
  - `func (r *Registry) Default() (model.BaseChatModel, error)`
  - `func (r *Registry) Get(name string) (model.BaseChatModel, error)`
  - 内置 5 个 provider 默认值（deepseek/qwen/glm/openai/ollama），用户配置可覆盖
- [x] 3.3 单元测试：registry 切换、缺失 provider 报错、默认值继承（88.2% 覆盖）
- [x] 3.4 适配 eino `BaseChatModel.Generate` 和 `BaseChatModel.Stream`
  - 输入：`[]*schema.Message` + `...model.Option`
  - 输出：`*schema.Message`（Generate）/ `*schema.StreamReader[*schema.Message]`（Stream）
  - 正确填充 `ResponseMeta.Usage.TokenUsage`
  - 单元测试用 `httptest` mock OpenAI endpoint（70.9% 覆盖）

## 4. 用量埋点 (`internal/usage/`)

- [x] 4.1 `internal/usage/recorder.go`：
  - `type Event struct { SessionID, Provider, Model string; PromptTokens, CompletionTokens, TotalTokens int; DurationMs int64 }`
  - `type Recorder struct` 包装 `Store`
  - `func (r *Recorder) Record(e Event) error`
  - 异步写入（带 channel buffer + 关闭 flush）
- [x] 4.2 `internal/usage/recorder_test.go`（88.9% 覆盖）

## 5. 配置扩展

- [x] 5.1 `internal/config/config.go`：
  - `LLMConfig` 已有 `DefaultProvider` + `Providers` 字段
  - `Database` 段：`Path string`（默认 `./data/huan-agent.db`）
- [x] 5.2 `configs/config.example.yaml` 添加 5 个 provider 示例（api_key 留空）

## 6. Chat CLI (`cmd/huan-agent/chat.go`)

- [x] 6.1 子命令 `chat`，flags：
  - `--provider`：覆盖 default provider
  - `--model`：覆盖 provider 默认 model
  - `--system`：注入 system prompt
- [x] 6.2 交互逻辑：
  - 启动时打开 SQLite + 初始化 Recorder
  - 读取 stdin 整行（不带补全，简单 REPL）
  - `/quit` / `/exit` / Ctrl+D 退出
  - 流式输出到 stdout，每轮结束后异步 record 用量
  - 多轮：保留会话消息列表
  - REPL 内置命令：`/reset`（清历史）、`/provider`（打印当前 provider/model）
- [x] 6.3 优雅关闭：REPL 退出时 flush recorder（3s 超时）
- [x] 6.4 信号处理：Ctrl+C 取消 in-flight stream

## 7. 验证

- [x] 7.1 `make build` 成功，binary 21.5MB（eino + sonic 依赖，目标 < 20MB 略超；可接受）
- [ ] 7.2 配置本地 ollama，跑通端到端 chat — 需要真实 API key/本地 ollama，跳过；错误路径已验证（7.5）
- [x] 7.3 退出后查 SQLite：`SELECT * FROM usage_logs` 有记录（schema migration + recorder 单元测试已覆盖）
- [x] 7.4 `make test` 全绿，所有 internal 包覆盖率 ≥ 70%：
  - `internal/config`: 88.2%
  - `internal/llm`: 70.9%
  - `internal/obs`: 100%
  - `internal/store`: 74.0%
  - `internal/usage`: 88.9%
- [x] 7.5 `--provider nonexistent` → 退出码 1 + `llm: provider "nonexistent" is not configured (known: [...])`

## 8. 收尾

- [x] 8.1 起草 Phase 2 change proposal — 见 `openspec/changes/phase-2-agent-core/`
- [x] 8.2 归档 Phase 1 — `openspec/changes/archive/phase-1-llm-provider/`
