# Phase 2 — Tasks

## 1. 依赖

- [ ] 1.1 `go get github.com/mark3labs/mcp-go@latest`
- [ ] 1.2 `go get gopkg.in/yaml.v3@latest`
- [ ] 1.3 `go mod tidy`

## 2. 工具层 (`internal/tool/`)

- [ ] 2.1 `internal/tool/tool.go`：
  - `type Tool interface { Info(ctx) (*schema.ToolInfo, error); InvokableRun(ctx, argsJSON string) (string, error) }`
  - 适配 eino `tool.InvokableTool` 接口
  - `type Spec struct { Name, Description string; ParametersJSONSchema string }`
- [ ] 2.2 `internal/tool/registry.go`：
  - `type Registry struct` 维护 `name → Tool` + `name → Spec`
  - `NewRegistry() *Registry`
  - `(r) Register(t Tool) error` — 重名报错
  - `(r) AllowList(names []string) error` — 设置白名单
  - `(r) List() []Spec` — 返回白名单内的 Spec（供 LLM）
  - `(r) Get(name string) (Tool, bool)`
  - `(r) Names() []string`
- [ ] 2.3 `internal/tool/builtin/time.go`：返回当前时间，支持 timezone 参数
- [ ] 2.4 `internal/tool/builtin/calc.go`：用 `github.com/Knetic/govaluate` 或手写解析器求值简单数学表达式
- [ ] 2.5 `internal/tool/builtin/echo.go`：原样回显（用于测试）
- [ ] 2.6 单元测试：注册/重复/白名单/执行路径

## 3. Agent 循环 (`internal/agent/`)

- [ ] 3.1 `internal/agent/agent.go`：
  - 包装 eino `flow/agent/react.Agent`
  - `type Agent struct { model model.BaseChatModel; tools *tool.Registry; maxSteps int; logger *zap.Logger }`
  - `func New(cfg Config) (*Agent, error)`
  - `(a) Generate(ctx, messages, opts...) (*schema.Message, error)` —— 同步版本
  - `(a) Stream(ctx, messages, opts...) (*schema.StreamReader[*schema.Message], error)` —— 流式版本（合并 ReAct 中间步骤的流）
  - `func (a *Agent) invokeWithAudit(ctx, name, args, sessionID) (string, error)` —— 包装工具调用，写审计日志
- [ ] 3.2 `internal/agent/audit.go`：
  - 工具调用审计：`type Invocation struct { SessionID, ToolName, Arguments, Result string; Err error; DurationMs int64; CreatedAt time.Time }`
- [ ] 3.3 单元测试：mock 工具 + mock model（用 httptest 模拟 OpenAI tool_call 响应），验证 ReAct 跑完

## 4. MCP stdio 客户端 (`internal/mcp/`)

- [ ] 4.1 `internal/mcp/client.go`：
  - 包装 `mcp-go` stdio transport
  - `type Client struct { cmd *exec.Cmd; session *mcp.ClientSession }`
  - `func Connect(ctx, command string, args []string, env []string) (*Client, error)`
  - `(c) ListTools(ctx) ([]*schema.ToolInfo, error)` —— 列出 MCP server 暴露的 tools
  - `(c) CallTool(ctx, name, argsJSON) (string, error)`
  - `(c) Close() error` —— 关 session + kill 子进程
- [ ] 4.2 `internal/mcp/bridge.go`：
  - `func RegisterMCPTools(reg *tool.Registry, c *Client) error` —— 把 MCP tools 包装成 `tool.InvokableTool` 注册到 registry
- [ ] 4.3 单元测试：用 mcp-go 自带的 in-memory test server 验证 ListTools / CallTool

## 5. Skill 系统 (`internal/skill/`)

- [ ] 5.1 `internal/skill/skill.go`：
  - `type Skill struct { Name, Description, Body string; Tools []string; Frontmatter map[string]any }`
  - `func LoadFile(path string) (*Skill, error)` —— 解析 markdown + YAML frontmatter
  - `func (s *Skill) SystemPrompt() string` —— 把 skill 拼成可注入 system 的字符串
- [ ] 5.2 `internal/skill/loader.go`：
  - `type Loader struct { dir string }`
  - `func NewLoader(dir string) *Loader`
  - `(l) LoadAll() ([]*Skill, error)` —— 扫目录下所有 `.md`，错误累积返回
  - `(l) Get(name string) (*Skill, bool)`
- [ ] 5.3 `configs/skills/daily-summary.md`：内置模板
- [ ] 5.4 `configs/skills/code-review.md`：内置模板
- [ ] 5.5 单元测试：frontmatter 解析、错误累积、查不到 name

## 6. 配置扩展

- [ ] 6.1 `internal/config/config.go`：
  - `AgentConfig` 段：`MaxSteps int`（默认 10）、`DefaultTools []string`、`SkillsDir string`（默认 `./configs/skills`）
  - `MCPConfig` 段：`Servers []MCPServer{Name, Command, Args, Env}`
- [ ] 6.2 `configs/config.example.yaml` 添加示例

## 7. Chat CLI 扩展

- [ ] 7.1 `cmd/huan-agent/chat.go` 新增 flags：
  - `--tools`：逗号分隔的工具名白名单
  - `--skill`：单个 skill 名称
  - `--no-agent`：禁用 agent loop，退化为普通 chat（默认开 agent）
- [ ] 7.2 启用 agent 时：
  - 启动时构建 `tool.Registry`（注册内置工具 + 加载 MCP servers + 加载 skills）
  - 注入 skill 的 system prompt 到首条消息
  - 工具调用结果同步输出（"🔧 time() → 2026-06-08 12:00:00"）
  - 每轮结束 async record：usage（已有）+ tool invocations

## 8. 存储扩展

- [ ] 8.1 `internal/store/migrate.go`：新增 v2 migration 建 `tool_invocations` 表
- [ ] 8.2 `internal/store/audit.go`：`RecordInvocation` / `QueryInvocations`
- [ ] 8.3 单元测试

## 9. 验证

- [ ] 9.1 `make build` 成功
- [ ] 9.2 `make test` 全绿，所有 internal 包覆盖率 ≥ 70%
- [ ] 9.3 端到端 smoke（不调真实 provider）：
  - 写一个 mock LLM 跑通"用户问时间 → agent 调 time 工具 → 返回给用户"
  - 验证 `tool_invocations` 表里有记录
- [ ] 9.4 mcp-go in-memory server 跑通 ListTools / CallTool
- [ ] 9.5 skill loader 能加载 `daily-summary` / `code-review` 两个模板

## 10. 收尾

- [ ] 10.1 起草 Phase 3 change proposal
- [ ] 10.2 归档 Phase 2
