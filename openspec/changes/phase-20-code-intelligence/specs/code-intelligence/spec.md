# Capability: code-intelligence

## Purpose

给 agent 编译器级的代码理解能力：诊断、跳转到定义、真实引用集合、符号查询。这些问题的正确
答案来自类型信息而不是字符串匹配——`grep` 能告诉你某个名字出现在哪些行，只有语言服务器能告诉
你"这个接口有几个实现"以及"改这个签名要动哪些调用点"。

## Scope

- `internal/lsp` — 语言服务器的进程生命周期、LSP 协议、位置转换、诊断缓存。
- `internal/tool/builtin` — 四个只读代码智能工具与它们的注册条件。
- `internal/config` — `tools.lsp.*`。
- `cmd/huan-agent` — 按 surface 与工作区组装。

## Requirements (MUST)

1. **长驻进程，按需启动。** 每个（工作区根，服务器命令）MUST 至多有一个语言服务器进程，MUST
   在首次使用时才启动，MUST NOT 在进程启动时预启动。理由：语言服务器要索引整个模块，冷启动
   是秒级；每次调用重启会让它比 `grep` 更慢，从而永远不被使用。
2. **进程不泄漏。** 语言服务器 MUST 在独立进程组中启动，MUST 在空闲回收（`idle_timeout_seconds`）
   与 agent 进程退出时按 `SIGTERM → 宽限 → SIGKILL` 收掉**整个进程组**。关闭 MUST 幂等。
3. **协议正确性。** MUST 使用 `Content-Length` 分帧的 JSON-RPC 2.0，MUST 正确处理粘包与拆包、
   MUST 在服务器发来无法处理的请求时回一个明确的"不支持"响应（MUST NOT 让它把自己的请求挂死）、
   MUST 在读循环退出时唤醒所有等待中的请求。
4. **位置编码必须显式转换。** LSP 的位置是 UTF-16 code unit 偏移，工具对外 MUST 使用 1-based
   行列（与 `read_file` 的行号一致）。两个坐标系之间的转换 MUST 集中在一处，MUST 有覆盖中文、
   emoji（代理对）、制表符与 CRLF 的表驱动测试。**这是最容易静默出错的地方**：错的表现是
   "跳到错的行"，而不是一个错误。
5. **能力协商。** 工具 MUST 只暴露服务器在 `initialize` 结果里声明支持的能力。声称支持但实际
   不支持的服务器 MUST 表现为"该工具不注册"，而不是"调用时失败"。
6. **四个工具。** MUST 提供 `diagnostics`、`goto_definition`、`find_references`、
   `workspace_symbols`，全部标记为读能力（`CapRead`），MUST 在只读工作区下可用（它们不写任何
   东西）。路径 MUST 经工作区沙箱解析，返回值 MUST 是工作区相对路径。
7. **可缺席。** 语言服务器是外部二进制，可能未安装、版本不符或启动即崩。这种情况下：
   - agent 的启动 MUST NOT 受影响；
   - 相关工具 MUST NOT 注册（一个必然失败的工具比没有这个工具更糟）；
   - MUST 记一条日志，包含缺失的命令名与安装方式；
   - MUST NOT 反复重试启动（每次调用都试一次会让每个工具调用都多一次进程启动失败的开销）。
8. **诊断语义。** `publishDiagnostics` 是异步推送：空数组的意思是"这个文件的诊断清空了"，
   MUST NOT 被当作"还没收到"。等待诊断稳定 MUST 采用去抖 + 静止窗口 + **硬上限**，超时 MUST
   返回"尚未就绪"的明确状态，MUST NOT 无限等待。
9. **过滤与上限。** 默认只返回 `error` 与 `warning`（`Information` / `Hint` 会把上下文灌满而
   信息量极低），并 MUST 有单次返回条数上限，被截断时 MUST 如实说明还有多少条。
10. **可观测。** 服务器启动、握手耗时、回收、降级、等待超时 MUST 各有一条日志；工具调用时长
    MUST 进既有的 `/metrics` 与控制台链路追踪，使"等诊断等了 2 秒"是可见的而不是隐藏延迟。

## API

| 工具 | 参数 | 返回 | 能力 |
|---|---|---|---|
| `diagnostics` | `path?`, `severity?`, `limit?` | 按文件分组的诊断（路径 + 1-based 行列 + 严重度 + 消息 + 来源） | `CapRead` |
| `goto_definition` | `path`, `line`, `column` | 定义位置 + 该处上下文行 | `CapRead` |
| `find_references` | `path`, `line`, `column`, `include_declaration?` | 引用位置列表 + 计数 | `CapRead` |
| `workspace_symbols` | `query`, `limit?` | `文件:行` + 符号种类 + 名字 | `CapRead` |

## 配置

```yaml
tools:
  lsp:
    enable: true
    diagnostics_wait_ms: 2000
    idle_timeout_seconds: 600
    max_diagnostics: 20
    servers:
      - name: gopls
        command: gopls
        args: ["-mode=stdio"]
        languages: ["go"]
        root_markers: ["go.mod", "go.work"]
        init_options: {}
        enabled: true
```

- `servers` 为空时使用内置默认表（首个条目是 gopls）。默认值是一张可被完全覆盖的数据表，与
  `DefaultOpenVikingInclude` 同一个做法：集中一处、有注释、有测试。
- 语言匹配按文件扩展名，工作区根按"最近的含 `root_markers` 的祖先目录"，找不到则回落到工作区根。
- `tools.lsp.enable=false` MUST 让本能力完全消失（不启动进程、不注册工具、编辑结果不变）。

## 行为

- **改签名的正确顺序**由提示词要求：先 `find_references` 拿到全部调用点，再逐处修改。`grep`
  在这里是错误工具，它的漏检不会报错。
- 坐标越界、文件不存在、行列指向空白位置等输入 MUST 返回可读的中文原因作为工具观察值，让模型
  下一步改对；MUST NOT panic、MUST NOT 终止本轮。
- 一个文件被多个服务器覆盖时，按服务器表顺序选第一个匹配（不做多服务器合并）。
- 语言服务器自身的崩溃 MUST 只影响依赖它的那一次调用，后续调用 MUST 能重新启动它（或按"不可用
  不重试"策略给出明确原因）。

## Non-goals

- 补全、签名帮助、格式化、code action、hover。
- 跨文件重命名（`WorkspaceEdit` 的应用属于 Phase 24 的 `multi-file-edits`）。
- 每轮结束时的全仓诊断扫描：无差别扫描会在中型仓库产生几千条提示，把上下文灌满；反馈只发生在
  刚被编辑的文件上（见 `edit-feedback`）。
- 多语言的功能验收：结构是语言无关的，但本能力只为 Go 做验收。
- 远程 / 容器内的语言服务器（只有本地 stdio 子进程）。
