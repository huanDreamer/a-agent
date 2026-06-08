# tool-registry

## Purpose

提供统一的 Tool 接口和注册中心，让内置工具、MCP 工具、未来用户自定义工具用一致的方式注册、被 Agent 查找、被 LLM 列出。

## Requirements

### R1: Tool 接口

系统 MUST 定义 `Tool` 接口，适配 eino `tool.InvokableTool`：

- `Info(ctx) (*schema.ToolInfo, error)` — 返回工具元数据（name / description / parameters JSON schema）
- `InvokableRun(ctx, argumentsInJSON string, opts ...tool.Option) (string, error)` — 执行工具，参数为模型生成的 JSON 字符串

系统 MUST 提供 `Spec` 视图（name / description / parameters schema），供注册中心在不实例化工具的情况下把工具列表发给 LLM。

### R2: 注册中心

`Registry` MUST 维护 `name → Tool` 映射 + `name → Spec` 缓存：

- `NewRegistry() *Registry`
- `(r) Register(t Tool) error` — 注册；重名 MUST 报错
- `(r) Get(name string) (Tool, bool)` — 按名查找
- `(r) Names() []string` — 返回所有已注册名字
- `(r) ListSpecs() []Spec` — 返回所有已注册 Spec

### R3: 工具白名单

- `(r) SetAllowList(names []string) error` — 设置白名单；空列表 = 允许所有
- `(r) AllowList() []string` — 当前白名单
- `(r) AllowedSpecs() []Spec` — 返回白名单内工具的 Spec（这是给 LLM 的）
- LLM MUST 只能看到白名单内的工具；调用未在白名单的工具 MUST 立即返回错误
- 启动时校验：白名单中所有名字 MUST 已注册，否则 `SetAllowList` 返回错误

### R4: 内置工具

MVP MUST 内置以下工具：

| Name | Description | Parameters |
|---|---|---|
| `time` | 返回当前时间，支持 timezone | `{ timezone?: string }` |
| `calc` | 求值简单数学表达式 | `{ expression: string }` |
| `echo` | 原样回显参数（调试用） | `{ text: string }` |

`calc` MUST 支持 `+ - * / ^ ( )` 与数字字面量，安全性 MUST 至少保证不执行任意代码（用表达式 parser，不用 `eval`）。

### R5: 错误处理

- 工具 panic MUST 被 recover 并包装为 error 返回给 LLM（不 crash agent）
- 工具执行超时（> 30s）MUST 被 ctx 取消并返回 timeout error
- 工具返回的 string MUST 不超过 32KB（防止 LLM context 爆炸）

## Out of Scope

- 异步 / 流式工具（`StreamableTool`）—— MVP 仅实现 `InvokableTool`
- 工具权限 UI（Phase 5）
- 用户确认流程（"是否执行危险工具"）—— MVP 直接执行 + 白名单控制
- 工具调用重试 —— LLM 自己决定是否再调

## Dependencies

- eino `components/tool` + `components/tool/utils.InferTool`（结构体 → Tool）
- eino `schema`（ToolInfo）
- 后续 Phase 4+ 飞书把 `tool.Registry` 暴露给 LLM 用
