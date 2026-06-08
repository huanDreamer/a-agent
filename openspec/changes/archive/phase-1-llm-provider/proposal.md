# Phase 1 — LLM 接入与基础对话

## Why

Phase 0 给了我们一个可运行但"哑"的二进制。下一步是让用户能真的和 LLM 说话，否则这个 agent 没有意义。

需要实现：
1. **多 LLM provider 切换** — DeepSeek / Qwen / GLM / OpenAI / Ollama 全是 OpenAI 兼容协议，统一一个适配层即可
2. **流式输出** — 用户在 CLI 里等不了非流式
3. **用量埋点** — token 费用是核心成本，必须一开始就统计
4. **本地持久化** — MVP 用 SQLite，无需运维数据库

不做的话，所有后续 phase（Agent / MCP / Skill）都跑不起来。

## What Changes

### ADDED

- 依赖：
  - `github.com/cloudwego/eino` v0.9.x（ChatModel 抽象）
  - `github.com/sashabaranov/go-openai` v0.20+（OpenAI 兼容 SDK）
  - `modernc.org/sqlite` v1.34+（纯 Go SQLite，避免 CGO 依赖）
- `internal/store/`：SQLite 抽象 + migration
- `internal/llm/`：provider 抽象 + 多 provider 实现
- `internal/usage/`：用量埋点 + 查询接口
- 新增子命令 `huan-agent chat`：交互式 REPL（流式 + 多轮上下文）
- 配置文件 `llm.providers` 段扩展
- 用量表 `usage_logs`

### NOT in this change

- 工具调用 / Function calling（Phase 2）
- Agent 循环（Phase 2）
- IM 接入（Phase 4）
- Admin UI（Phase 5）
- 长期记忆 / 摘要（Phase 3）

## Impact

| 影响面 | 影响 |
|---|---|
| 依赖 | +3 个 module（eino / go-openai / sqlite）|
| 配置 | `llm.providers.<name>` 新增 api_key / base_url / model 字段 |
| 存储 | 新增 `data/huan-agent.db`（SQLite）|
| CLI | 新增 `chat` 子命令 |
| Build | binary 体积 +5-8MB（预计）|

## Success Criteria

- [ ] 配置文件能定义多个 provider，通过 `default_provider` 选一个
- [ ] `huan-agent chat` 启动后输入 `hello` 能看到流式输出
- [ ] 多轮对话上下文正确（assistant 回复后用户能接着问）
- [ ] 退出 REPL 后 `usage_logs` 表里有本次对话的 token 记录
- [ ] `huan-agent chat --provider qwen --model qwen-plus` 能切换 provider
- [ ] 切换到不存在的 provider → 明确错误
- [ ] `make test` 全绿，`internal/llm` + `internal/usage` + `internal/store` 覆盖率 ≥ 70%
- [ ] binary 体积 < 20MB

## Provider Matrix

| Name | Base URL | 默认 Model | 备注 |
|---|---|---|---|
| `deepseek` | `https://api.deepseek.com/v1` | `deepseek-chat` | |
| `qwen` | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `qwen-plus` | 阿里云百炼 |
| `glm` | `https://open.bigmodel.cn/api/paas/v4` | `glm-4-plus` | 智谱 |
| `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` | |
| `ollama` | `http://localhost:11434/v1` | `llama3.2` | 本地，api_key 留空 |

## Risks

| 风险 | 缓解 |
|---|---|
| eino v0.9 API 还在变 | 锁定版本，封装自有 adapter，未来换 eino-ext 不影响上层 |
| OpenAI 协议各家有微小差异（tool_call 格式、reasoning_content） | 在 adapter 层处理，schema 统一 |
| 流式输出 backpressure | 用 channel + ctx 取消 |
| API key 误提交 | 配置走 env 优先 + gitignore 严格 |
| SQLite 锁竞争 | MVP 单进程，加 `_busy_timeout=5s` 即可 |

## Rollback

无破坏性，纯新增子命令 + 新表。
