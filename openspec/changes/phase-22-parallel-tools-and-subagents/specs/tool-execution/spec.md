# Capability: tool-execution

## Purpose

让一轮里的多个工具调用能在安全的地方重叠执行（从而不再线性叠加延迟），同时保证**模型写的程序
顺序**不被重排：读可以并行，写与执行是屏障，结果永远按调用顺序组装。

## Scope

- `internal/tool` — 并发安全的显式声明。
- `internal/chat` — Web 循环的带屏障调度与结果组装。
- `internal/agent` — CLI / 飞书（Eino ReAct）路径的顺序承诺。
- `internal/config` — `tools.max_parallel`。

## Requirements (MUST)

1. **显式声明，缺省串行。** 并发安全 MUST 是一个显式声明（`ParallelSafe` / `Serial`），附着在
   工具上（装饰器形式，与 `tool.WithCapability` 同一个模式），MUST NOT 从 `CapRead` / `CapWrite` /
   `CapExec` 推断。**未声明的工具 MUST 按 `Serial` 处理。** 理由：能力标签回答的是"会不会改动
   工作区"，不是"能不能同时跑"——`ask_user`（挂住整轮）、`plan_*`（对同一份计划做
   read-modify-write）、`save_document`（写文档库）今天都带 `CapRead`，按标签推断并发安全会让
   它们一起飞。
2. **写与执行是屏障。** 一个 `Serial` 调用 MUST 等它之前的全部调用完成，且它之后的全部调用 MUST
   等它完成。这不是性能取舍而是正确性要求：「先写配置，再跑测试」被重排就会在旧配置上测试。
3. **结果按程序顺序组装。** `res.Tools` 与追加进历史的消息 MUST 与模型给出的 `ToolCalls` 逐位
   对应，与完成顺序无关。工具结果的观察值 MUST 与其 `tool_call_id` 严格配对。
4. **事件可以是完成顺序。** `tool_call` MUST 在调用开始时发出，`tool_result` MUST 在完成时发出，
   两者都带 `tool_call_id`。乱序完成是允许的：客户端按 id 配对（`internal/chat/events.go` 的
   `ToolCallID` 就是这个用途）。
5. **有界。** 段内并发 MUST 有上限（`tools.max_parallel`，默认 4）。`0` 或 `1` MUST 退化为
   今天的行为（逐事件与改动前一致）。
6. **失败隔离。** 单个调用的失败、超时或 panic MUST 只体现在它自己的结果里：MUST NOT 取消兄弟
   调用，MUST NOT 终止本轮。MUST 逐调用 `recover` panic 并转成该调用的工具错误——串行时代没有
   recover，一个 panic 会带走整个进程，并发之后这一点必须补上。
7. **取消传播。** 轮次 ctx 取消时 MUST 取消所有在飞的调用并等待它们收敛，MUST NOT 留下仍在
   运行的 goroutine。
8. **顺序承诺对每条路径都成立。** 两条执行路径（`internal/chat` 的 Web 循环与 `internal/agent`
   的 Eino ReAct）都 MUST 满足上面各条。`internal/agent` 今天用
   `compose.ToolsNodeConfig.ExecuteSequentially: true` 达到串行，注释写明理由是"审计顺序确定"；
   改成并发时 MUST 让审计记录仍可按程序顺序读（或保持串行），MUST NOT 让审计出现"后发的调用先
   记录"，并且 MUST 更新那句注释以反映真实理由。
9. **可观测。** 并发 MUST 可由既有的工具调用时长指标观察（`/metrics` 的
   `huan_agent_tool_call_duration_seconds` 与控制台链路追踪），MUST NOT 引入只在日志里可见的
   隐性并发。

## 并发标签（本节是当前的权威表）

| 标签 | 工具 |
|---|---|
| `ParallelSafe` | `read_file`、`list_dir`、`glob`、`grep`、`time`、`calc`、`echo`、`bash_output`、`diagnostics`、`goto_definition`、`find_references`、`workspace_symbols`、`spawn_agent` |
| `Serial` | `write_file`、`edit_file`、`apply_patch`、`bash`、`bash_background`、`bash_jobs`、`bash_stop`、`plan_create`、`plan_add`、`plan_update`、`plan_read`、`ask_user`、`save_document`、媒体工具 |

新增工具 MUST 做出显式选择；一个测试 MUST 断言每个已注册工具都在期望表里（缺声明时不会被静默
接受）。

## 配置

```yaml
tools:
  max_parallel: 4        # 同一个模型回复内、纯读工具之间的并发上限；0 或 1 = 串行

subagent:
  enable: true
  max_steps: 8
  max_concurrent: 2
  max_report_chars: 8000
  timeout_seconds: 0     # 0 = 用父轮次剩余时间
```

## 行为示例

模型一次回复里给出 `[grep, read_file, write_file, read_file, grep]`：

```
段 1: grep, read_file        → 并发（上限内）
屏障: write_file             → 单独执行，等段 1 完成
段 2: read_file, grep        → 并发
```

结果组装顺序恒为 `grep, read_file, write_file, read_file, grep`（程序顺序），与各段的完成时间无关。

## Non-goals

- 跨步骤并行：并行单位是**同一条模型回复内的多个调用**；步骤之间有模型思考，那是串行的。
- 自适应并行度（按 CPU / 延迟动态调节）：一个固定的可配置上限是人能理解与复现的。
- 把 `bash` 变成非屏障：它的语义就是"执行即改动"，即使命令只是 `ls`。
- 用并发打破 `ExecuteSequentially` 的审计承诺：顺序既是正确性也是可读性要求。
