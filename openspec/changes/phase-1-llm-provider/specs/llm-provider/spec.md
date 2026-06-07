# llm-provider

## Purpose

为 huan-agent 提供多 LLM provider 接入能力。所有 provider 必须实现统一的 eino `BaseChatModel` 接口，使得上层（agent、tool、skill）无需关心具体后端。

## Requirements

### R1: Provider 抽象

系统 MUST 定义 `Provider` 结构体，字段：

- `Name` string — provider 唯一标识
- `BaseURL` string — OpenAI 兼容 API 入口
- `APIKey` string — 鉴权 key（可为空，如 ollama）
- `Model` string — 默认模型名

系统 MUST 实现 `New(Provider) (BaseChatModel, error)` 工厂函数，返回实现 eino `BaseChatModel` 接口的对象。

### R2: Provider Registry

系统 MUST 提供 `Registry` 维护已配置的 provider 列表：

- `NewRegistry(cfg LLMConfig) *Registry` — 从配置构造
- `(r) Default() (BaseChatModel, error)` — 返回 `default_provider` 配置的 provider
- `(r) Get(name string) (BaseChatModel, error)` — 按名称获取；不存在返回错误
- 重复注册同名 provider MUST 报错

### R3: 流式与阻塞模式

`BaseChatModel` MUST 同时实现：

- `Generate(ctx, messages) (*Message, error)` — 阻塞，返回完整消息
- `Stream(ctx, messages) (*StreamReader[*Message], error)` — 流式

两条路径 MUST 都填充 `Message.ResponseMeta.Usage` (TokenUsage)：

- `PromptTokens` int
- `CompletionTokens` int
- `TotalTokens` int

### R4: 内置 provider 列表

MVP 阶段 MUST 至少支持以下 provider 配置（不需要都实现，配置解析正确即可）：

- `deepseek` — `https://api.deepseek.com/v1`
- `qwen` — `https://dashscope.aliyuncs.com/compatible-mode/v1`
- `glm` — `https://open.bigmodel.cn/api/paas/v4`
- `openai` — `https://api.openai.com/v1`
- `ollama` — `http://localhost:11434/v1`（API key 留空）

base_url 与 model 字段 MUST 可被 YAML 配置覆盖。

### R5: 错误处理

- 网络错误 MUST 包装为可识别的 `*LLMError`，包含 provider / model / status_code
- 4xx / 5xx 响应 MUST 透传错误 body
- 流式过程中 ctx 取消 MUST 立即中断，不 panic

## Out of Scope

- Function calling / Tool use（Phase 2）
- 多模态（图片、音频）
- 流式 reasoning_content（DeepSeek-R1 等）解析
- 模型微调
- Embedding 模型

## Dependencies

- eino `BaseChatModel` 接口
- `sashabaranov/go-openai` SDK（仅作为 HTTP 客户端，schema 用 eino 的）
