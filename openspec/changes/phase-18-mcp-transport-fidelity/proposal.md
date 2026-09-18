# Phase 18 — MCP 传输保真：让 CLI 与飞书真的能连上 MCP

## Summary

这是一次**修复**，不是一个新能力。它的起因是核实 `CAPABILITY-GAPS.md` 时发现的一个
可复现的启动失败：

```
$ ./bin/huan-agent chat --tools
Error: build agent: connect mcp openviking: mcp: openviking: command is required for a stdio server
```

按仓库自带的 `configs/config.yaml`，**`chat --tools` 根本起不来**，飞书那条路径同样构不出
工具集。也就是说：这个 agent 在以"写代码的生产力工具"这个身份被使用之前，先在自己的默认
配置下无法启动。任何后续阶段（一次性执行入口、诊断、审批、并发）都无法端到端验证。

根因不是配置写错，而是 `cmd/` 下手写的两处 `mcp.ServerSpec` 构造**丢字段**：

- `cmd/huan-agent/chat.go:382-387` 只拷了 `Name / Command / Args / Env`；
- `cmd/huan-agent/workspace_tools.go:540-545` 同样。

而 OpenViking 的 MCP 条目是 `ApplyOpenVikingMCP()` **自动生成**的，并且明确设成了 http
（`internal/config/openviking.go:383-388`，带 `Transport: "http"`、`URL: .../mcp`、鉴权
`Headers`）。字段一丢，`Transport` 为空，`ServerSpec.ResolvedTransport()` 按历史约定回落
到 `stdio`（`internal/mcp/client.go:78-81`），`Command` 自然为空，于是
`ServerSpec.Validate()` 报出上面那句（`internal/mcp/client.go:92`）。

对照组是同仓已经写对的两份实现：`internal/server/mcp.go` 的 `specFromStore`（第 507 行）
与 probe 用的构造（第 380 行）都完整传了 `Transport / URL / Headers`。**同一个仓库里，控制台
能连、CLI 和飞书不能连**——这正说明它是漏改，而不是设计。

## Why

- **一个静默的分叉比一个坏掉的开关更贵。** 三处构造同一个类型、只有一处写全，导致"配置
  文件里能声明的东西"和"CLI 实际能连的东西"不是一回事。修法必须是把三处收敛成一处，而不是
  在另外两处各补三个字段——否则第四处很快会出现。
- **错误信息把责任推给了用户。** `command is required for a stdio server` 会让运维去翻配置
  文件找一个根本不存在的 stdio 条目，而真正的问题在代码里。修完之后，同类失败必须报出**实际
  使用的传输方式与缺的字段**，让人一眼看出是配置还是代码。
- **它是后续所有阶段的地基。** Phase 19 的 `run` 子命令、Phase 20 的诊断、Phase 21 的审批、
  Phase 22 的并发，验收都要跑真实的 agent 轮次。启动都过不去的话，那些阶段只能靠单元测试
  自证，而这正是当初漏掉这个 bug 的原因。
- **顺手修掉两处同类漏改。** `chat.go` 的 MCP 连接循环没有像另外两处那样检查
  `IsEnabled()`，于是被停用的服务器在 CLI 里仍会被连接；`configs/skills/` 下两个技能的
  `tools` 清单写成了 `[calc, echo]` / `[time, echo]`，在 `SetAllowList` 的"收窄"语义下
  （`internal/tool/registry.go:203-232`），一个**代码评审**技能反而拿不到 `read_file` /
  `grep` / `glob`，连 diff 都读不到。

## What Changes

### ADDED

1. **`cmd/huan-agent`：唯一的配置→运行态转换**
   - 新增 `mcpSpecFromConfig(config.MCPServer) (mcp.ServerSpec, bool)`（落在
     `cmd/huan-agent/mcp.go`）：解析 transport（`mcp.ParseTransport`）、补齐
     `ID / Name / Transport / Command / Args / Env / URL / Headers`，第二个返回值表示
     `IsEnabled()` 的结果。**解析失败不静默回落**：非法 transport 报错并指出取值，而不是
     当成 stdio 继续跑。
   - `cmd/huan-agent/chat.go:382` 与 `cmd/huan-agent/workspace_tools.go:540` 两处循环改为
     调用它，并统一 `IsEnabled()` 跳过语义（`chat.go` 目前是漏的）。
   - 连接失败的错误里带上传输方式：`connect mcp %s (%s): %w`。

2. **回归测试：字段不能再被丢掉**
   - `cmd/huan-agent/mcp_test.go`：表驱动断言 http/sse/stdio 三种输入经
     `mcpSpecFromConfig` 之后 `Transport / URL / Headers / Command / Args` 逐字段保真；
     断言非法 transport 报错；断言 `enabled: false` 被跳过；断言 headers 顺序与内容不变
     （鉴权头顺序是契约的一部分）。
   - 一个**防回归的构建级测试**：用仓库自带的 `configs/config.example.yaml` 打开
     OpenViking 注册，断言 `buildAgentTools`（CLI 路径）能成功返回，而不是只测转换函数——
     这个 bug 的形状是"函数对了但调用点没接线"，只测函数会再次漏掉。

3. **`configs/skills/`：修正两个技能的 tools 清单**
   - `code-review.md`：`[read_file, grep, glob, list_dir, calc]`——评审要读 diff、要能搜
     调用点和上下文；保留 `calc`（复杂度/内存估算），去掉只能复读的 `echo`。
   - `daily-summary.md`：`[time, read_file, grep]`——复盘要能看当天改了什么。
   - 两个文件正文里"可用工具"那一段同步改写：现在的文字把 `echo` 说成"回显结论便于复制"，
     而正文本身就是模型的输出，不需要一个工具来复读它。

4. **文档**
   - `docs/tools.md`：补一段"技能声明 `tools` 即收窄"的警告——清单写漏一项，模型在下一次
     调用时才发现，而报错点是工具名未知（`SetAllowList` 校验的是名字是否已注册，不是它是否
     需要），所以写成 `[calc, echo]` 是**合法但无用**的配置，不会被任何检查拦住。
   - `docs/status.md` 或 `docs/tools.md` 的排障段落：记录这次故障的形状（"配置声明了 http，
     报错却说 stdio"），并给出定位命令。

### 明确不做（本阶段）

- **不重构 `internal/config.MCPServer` 与 `mcp.ServerSpec` 的类型关系。** 两个类型分属不同
  层（配置 / 客户端），把转换集中到一处已经消除漏改的可能；合并类型会牵动控制台与 store。
- **不给 `stdio` 之外的传输加新能力**（OAuth、streamable-http 重连策略等）。
- **不改 `mcp-runtime` 的既有控制台行为**：那里已经是正确实现，本次只补它的 Requirement 文案，
  让"每个 surface 都必须保真"成为显式约束，而不是靠巧合。
- **不动 `ResolvedTransport()` 的"空即 stdio"回落**：历史配置文件里没有这个字段，回落是
  有意的兼容行为；本次要的是**不再产生空的 transport**，而不是改回落的语义。

## Impact

- 新增文件：`cmd/huan-agent/mcp.go`、`cmd/huan-agent/mcp_test.go`。
- 改动：`cmd/huan-agent/{chat.go,workspace_tools.go}`、`configs/skills/{code-review,daily-summary}.md`、
  `docs/tools.md`。
- 兼容性：无配置变更，无 schema 变更，无 API 变更。行为变化只发生在"以前会启动失败"的路径上。
- 验收命令（本变更的完成定义）：
  - `./bin/huan-agent chat --tools` 在 `configs/config.yaml` 下进入 REPL，不报 MCP 错误；
  - `./bin/huan-agent chat --tools --skill code-review` 端到端能读到用户指定的文件；
  - `go test ./... -count=1` 全绿。
