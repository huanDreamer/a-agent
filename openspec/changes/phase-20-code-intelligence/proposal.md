# Phase 20 — 代码智能：接语言服务器，把"改完不知道对不对"这一环补上

## Summary

今天这个 agent 只有**字符串级**的代码理解能力：`read_file` / `list_dir` / `glob` / `grep`
（注册点 `cmd/huan-agent/workspace_tools.go:127-135`）。缺的不是数据，是**语义**：

1. **改完不知道有没有制造类型错误。** 唯一的反馈是让模型自己想起来跑 `go build` / `go vet`。
   对动态语言更无从判断。IDE 里这一步是实时的、免费的、不需要模型记得。
2. **"谁实现了这个接口""改这个签名要动哪些调用点"只能靠猜关键词。** `grep` 匹配的是字符串，
   漏掉一个调用点就留下半个编译不过的仓库，而漏掉的那次 grep 不会报错。
3. **没有符号级视图。** 仓库里有什么函数、什么类型，模型只能靠一次次 grep 拼。

本变更新增 `internal/lsp`：把语言服务器（首个目标是 `gopls`）作为**长驻子进程**接进来，暴露
诊断、跳转、引用、符号查询四类只读工具；并且——这是最关键的一半——**在 `write_file` /
`edit_file` 成功后，自动把它刚写的那个文件的诊断附在这次工具调用的结果里**。模型改完那一
步就看到了类型错误，不需要跑一次测试才发现。

> **对 `CAPABILITY-GAPS.md` 的一处更正。** 那份分析说这项改动"成本只是包一层已经装好的
> 工具"。核实结果：本机 `which gopls` **无输出**，`$(go env GOPATH)/bin` 里是
> `dlv / goctls / protoc-gen-go / wails / wire`，PATH 里唯一存在的语言服务器是
> `/usr/bin/clangd`。也就是说 gopls 需要安装（`go install golang.org/x/tools/gopls@latest`），
> 而"语言服务器不存在"是一个**必须被设计进去的常态**，不是异常——见下面的降级要求。

## Why

- **这是"改完 → 跑测试 → 发现类型错 → 再改"这个循环里最贵的一段。** 跑一次测试要几十秒到
  几分钟，而诊断在 IDE 里是毫秒级的。把它放回 agent 的工具结果里，等于把每次编辑的反馈延迟
  从"一次测试运行"降到"一次工具调用"。
- **反馈必须挂在编辑动作上，而不能等模型来问。** 一个需要模型主动调用的 `diagnostics` 工具，
  在"我改完了，看起来没问题"的心态下不会被调用——而这正是最需要它的时刻。所以本变更的核心
  不是新增一个查询工具，而是**改变已有编辑工具的返回值**。
- **符号级问题要用符号级回答。** "这个方法被谁实现了"的正确答案来自类型信息，不是正则。
  语言服务器的 `references` 恰好就是编译器眼里的调用点集合。
- **长驻进程是必需的，不是优化。** `gopls` 要索引整个模块，冷启动是秒级到十秒级；每次调用
  重启一次会让这个工具比 grep 还慢，从而永远不被使用。代价是**进程生命周期必须被认真管理**
  ——`internal/jobs` 已经解决过同一类问题（新会话 + 进程组 + SIGTERM→宽限→SIGKILL），
  这里的做法照着它来。
- **外部依赖必须可缺席。** 语言服务器是外部二进制：可能没装、版本不匹配、启动即崩。设计
  目标是**缺了它一切照旧**，只是少几个工具——不能影响启动，不能影响其它工具。

## What Changes

### ADDED

1. **`internal/lsp`：协议与进程**
   - **传输**：LSP over stdio，JSON-RPC 2.0，`Content-Length: N\r\n\r\n<payload>` 分帧。
     分帧、请求/响应关联（id → 等待中的 channel）、通知分发三件事各自独立可测，不依赖真服务器。
   - **握手**：`initialize`（`rootUri` = 工作区根、`capabilities`、`initializationOptions`）→
     等 `initialize` 响应 → 发 `initialized` → 按需 `textDocument/didOpen`。
   - **生命周期**：每个（工作区根，服务器命令）一个进程，**首次使用时才启动**。
     空闲回收（`idle_timeout_seconds`，默认 600）与进程退出时统一 `SIGTERM → 宽限 → SIGKILL`
     并回收整个进程组，确保崩掉的服务器不留孤儿进程。
   - **位置编码**：LSP 的位置默认是 UTF-16 code unit 偏移，而我们的工具与文件是 UTF-8 字节。
     转换必须集中在一个地方并**表驱动测试**（中文、emoji、制表符、行尾 CRLF 都要覆盖）。
     这是这类集成最容易出错的一次性错误，出错的表现是"跳到错的行"而不是报错。
   - **诊断收集**：服务器以 `textDocument/publishDiagnostics` 推送。客户端维护每个 URI 的
     最新诊断集 + 一个版本/时间戳，并提供**等待稳定**的能力：一次编辑之后诊断是异步到达的，
     立刻查询会拿到空或过期结果。等待策略是"去抖 + 静止窗口 + 硬上限"，**超时返回"尚未就绪"
     而不是无限等**。
   - **不可用即降级**：二进制缺失 / 启动失败 / 握手中止时，记一条 warn（含缺失的命令名与安装
     提示），把该服务器标记为不可用并**不再重试**（除非配置变更或显式重连）。MUST NOT 阻塞
     启动、MUST NOT 影响其它工具。

2. **四个只读工具（`internal/tool/builtin`）**

   | 工具 | 参数 | 语义 | 能力 |
   |---|---|---|---|
   | `diagnostics` | `path?`, `severity?`, `limit?` | 不带 `path` 时按文件分组返回整个工作区的诊断 | `CapRead` |
   | `goto_definition` | `path`, `line`, `column` | 跳转到定义 | `CapRead` |
   | `find_references` | `path`, `line`, `column`, `include_declaration?` | 真实调用点/引用集合 | `CapRead` |
   | `workspace_symbols` | `query`, `limit?` | 按名字查符号（文件:行 + 种类 + 名字） | `CapRead` |

   - 全部走 `workspace.Resolve` / `Rel` 做路径约束，返回值里的路径一律工作区相对路径。
   - `line` / `column` 用 1-based（与 `read_file` 的行号一致），实现内部转 0-based 给 LSP——
     模型看到的两套编号必须一致，否则它会跳错位置然后怀疑工具。
   - 没有可用语言服务器时，这些工具**不注册**（与 `ask_user` / `plan_*` 同一个原则：一个必然
     失败的工具比没有这个工具更糟），并记一条 info 说明原因与安装方式。
   - 只读工作区（`tools.read_only`）下这些工具照常可用：它们不写任何东西。

3. **写/编辑后的自动诊断（见 `edit-feedback` spec）**
   - `write_file` / `edit_file` 成功返回后，如果该文件有语言服务器覆盖，把它的诊断附在工具
     结果里（`error` / `warning`，默认最多 20 条），没有诊断时附一行明确的"无诊断"。
   - 等待上限 `tools.lsp.diagnostics_wait_ms`（默认 2000）；超时则附"诊断尚未就绪"，**绝不
     让编辑被诊断拖慢**。
   - 由一个共享的 `lsp.Augmenter` 实现，`write_file` / `edit_file` / 以后的 `apply_patch`
     都调它，不各写一份。

4. **配置**

   ```yaml
   tools:
     lsp:
       enable: true
       attach_diagnostics: true      # 写/编辑结果里附上该文件的诊断
       diagnostics_wait_ms: 2000     # 等诊断稳定的硬上限
       idle_timeout_seconds: 600     # 空闲多久回收语言服务器进程
       max_diagnostics: 20           # 单次附给模型的诊断条数上限
       servers:
         - name: gopls
           command: gopls
           args: ["-mode=stdio"]
           languages: ["go"]
           root_markers: ["go.mod", "go.work"]
           init_options: {}
           enabled: true
   ```

   - `servers` 为空时使用内置默认表（首个条目就是 gopls），与
     `internal/config/openviking.go:177-180` 的 `DefaultOpenVikingInclude` 同一个做法：默认值
     作为一个可被完全覆盖的数据表集中在一处，并有测试断言它与文档一致。
   - 语言→服务器按文件扩展名 + `root_markers` 匹配；匹配不到就返回"该文件类型没有语言服务器"
     的可读原因（而不是静默空结果）。

5. **可观测性**
   - 新工具的调用自动进 `/metrics`（`huan_agent_tool_call_duration_seconds`）与控制台链路追踪，
     所以"等诊断等了 2 秒"是可观测的，而不是隐藏延迟。
   - warn/info 日志：服务器启动、握手耗时、回收、降级、等待超时，各一条。

6. **文档**
   - `docs/tools.md` 新增"代码智能"一节：四个工具、什么时候用、语言服务器怎么装
     （`go install golang.org/x/tools/gopls@latest`）、装不上会怎样（工具消失，其余一切照旧）。
   - `configs/config.example.yaml` 的 `tools.lsp` 段带完整注释。
   - 明确记录 `CAPABILITY-GAPS.md` 的那处更正：gopls 不在本机，成本不只是"包一层"。

### 明确不做（本阶段）

- **不做跨文件重命名。** `textDocument/prepareRename` / `rename` 会返回一个 `WorkspaceEdit`，
  而"把一批编辑原子地落到多个文件"是 Phase 24 的职责。本阶段连只读预览也不做——一个只能看
  不能用的重命名工具会诱使模型退回到逐文件 `edit_file`。
- **不做补全、签名帮助、格式化、code action。** 补全对"模型自己写代码"的形态没有用处；
  格式化/代码动作是写操作，要等审批（Phase 21）到位。
- **不做"每轮结束时自动全量诊断"。** 无差别的全仓诊断会把上下文灌满（一个中型仓库的
  unused/staticcheck 提示是几千条）。反馈只在**刚被编辑的文件**上出现。
- **不承诺多语言一次做完。** 结构上是语言无关的（服务器表 + 扩展名匹配），但首个目标是 Go；
  其它语言的服务器条目可以有，但本变更不为它们做验收。
- **不做诊断的审批或拦截。** 诊断只是信息，不阻塞、不询问。

## Impact

- 新增包：`internal/lsp`（协议、进程管理、诊断缓存、位置转换）。
- 新增文件：`internal/lsp/{client,protocol,server,diagnostics,position}.go`、
  `internal/tool/builtin/codeintel.go`、`internal/prompt/` 下的工具使用指引片段。
- 改动：`cmd/huan-agent/workspace_tools.go`（注册新工具 + 组装 Augmenter）、
  `internal/tool/builtin/files.go`（编辑成功后调 Augmenter）、`internal/config/config.go`
  （`tools.lsp.*`）、`configs/config.example.yaml`、`docs/tools.md`。
- 依赖：Phase 18（CLI 路径要先能启动）；Phase 19 让本阶段可以端到端验收。
- 兼容性：`tools.lsp.enable` 默认打开，但**没有语言服务器时它什么都不做**——没有 gopls 的部署
  与本变更之前完全一致。`attach_diagnostics` 默认打开，它只增加工具结果里的信息，不改变工具
  的输入或成功/失败语义。
- 风险与对策：外部进程是最不可控的部分。对策是把协议层与进程层分开测（协议层用进程内假
  服务器，不需要 gopls），并且加一个 `//go:build integration` 的真 gopls 测试守住
  "真的能报出一个类型错误"这条底线。
