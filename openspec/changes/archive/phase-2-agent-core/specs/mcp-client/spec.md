# mcp-client

## Purpose

实现 MCP (Model Context Protocol) stdio 客户端，让 huan-agent 能 spawn MCP server 子进程并把 server 暴露的 tools 桥接到 `internal/tool.Registry`，统一暴露给 Agent。

## Requirements

### R1: stdio 客户端

`Client` MUST 基于 `github.com/mark3labs/mcp-go` 的 stdio transport：

- `Connect(ctx, command string, args []string, env []string) (*Client, error)`
  - 启动子进程（`exec.CommandContext`）
  - 建立 stdio JSON-RPC 通道
  - 发送 `initialize` 握手
  - 返回 ready-to-use client
- 子进程启动失败 MUST 包装为 `mcp: spawn <cmd>: <err>` 错误
- `initialize` 失败 MUST kill 子进程

### R2: 工具发现

- `(c) ListTools(ctx) ([]*schema.ToolInfo, error)` — 调 `tools/list`
- 返回的 `ToolInfo` 字段 MUST 适配 eino schema（name / description / parameters），方便后续桥接

### R3: 工具调用

- `(c) CallTool(ctx, name, argumentsJSON) (string, error)` — 调 `tools/call`
- 工具返回的文本内容 MUST 提取为 string
- 工具返回错误 MUST 包装为 `mcp: tool <name> failed: <msg>`

### R4: 资源清理

- `(c) Close() error` MUST：
  - 发送 `shutdown`（如果协议要求）
  - 关 stdin 触发子进程 EOF
  - `Wait()` 子进程，带超时（5s）
  - 超时 MUST `cmd.Process.Kill()`
- 客户端 MUST 实现 `io.Closer`

### R5: 桥接到 Registry

`internal/mcp/bridge.go` MUST 提供：

- `RegisterMCPTools(reg *tool.Registry, c *Client) error`
  - 调 `c.ListTools` 拿到 tool 列表
  - 对每个 tool 包一个 `mcpAdapter`，实现 `tool.InvokableTool`，执行时转 `c.CallTool`
  - 用 tool name 注册到 registry
- 桥接失败 MUST 整体返回错误，已注册的 tool MUST 回滚（不留下半注册状态）

### R6: 配置

`MCPConfig.Servers` MUST 支持：

```yaml
mcp:
  servers:
    - name: filesystem
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
      env: []
```

启动时按顺序 Connect + RegisterMCPTools，任意一个失败 MUST 让 `huan-agent` 启动失败（fail-fast）。

## Out of Scope

- MCP SSE / HTTP transport（先 stdio）
- MCP 资源（resources） / 提示（prompts）发现 —— 仅 tools
- MCP 鉴权（OAuth）—— 等有需求再做
- 多 server 并发限制 —— MVP 顺序启动

## Dependencies

- `github.com/mark3labs/mcp-go`
- `internal/tool` 提供 Registry
- `internal/config` 提供 MCPConfig
