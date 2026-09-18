# Phase 22 — 并行与分工：工具并发，以及让脏上下文去别处

## Summary

这一阶段有两件事，共同点是"把一轮的墙钟与上下文预算花得更值"。

### 1. 工具是串行的——但两个循环的理由不同

`CAPABILITY-GAPS.md` 说的是"架构层面串行"。核实结果更细：**仓库里有两个独立的执行循环，两个
都是串行的，但原因不一样。**

- **Web 循环**：`internal/chat/runner.go:377` 一个手写的 `for` 循环（`for _, tc := range
  msg.ToolCalls`）。它是串行的原因只是**没有人写过调度**。
- **CLI / 飞书循环**：`internal/agent/agent.go` 把工作交给 Eino 的 `react.Agent`。Eino 的
  `compose.ToolsNodeConfig` 有一个 `ExecuteSequentially` 字段，**默认 `false` 即并行**
  （`compose/tool_node.go:171-174` 与 `parallelRunToolCall`）。而 `internal/agent/agent.go:107`
  显式把它设成 `true`，注释写着理由：

  ```go
  ExecuteSequentially: true, // deterministic; helps audit ordering
  ```

  也就是说这条路是**刻意**串行的，为的是审计顺序确定。

所以"把它改成并发"不是一处改动，而是两处，且其中一处推翻了一个写在代码里的理由。本变更的
设计必须同时满足：**并行执行，但审计与工具结果的顺序仍然确定**。

### 2. 并行有一个没人检查过的前提：能力标签不等于并发安全

今天的能力标签（`CapRead` / `CapWrite` / `CapExec`）回答的是"**它会不会改动工作区**"，不是
"**它能不能和别人同时跑**"。按现状的标签直接并发，会撞上三件事：

| 工具 | 今天的标签 | 并发后果 |
|---|---|---|
| `ask_user` | `CapRead`（`cmd/huan-agent/chat.go:477`） | 一次冒两张卡片；而它本来就会**挂住整轮**等着，语义上不是"一个可并行的读" |
| `plan_create` / `plan_add` / `plan_update` / `plan_read` | `CapRead`（`chat.go:498`） | 对同一份计划做 read-modify-write：并发的两次 `plan_update` 会丢更新、把看板搞乱 |
| `save_document` | `CapRead`（`chat.go:522`） | 并发写文档库（幂等性未定义） |
| `read_file` / `grep` / `glob` / `list_dir` | `CapRead` | 真正的纯读，可以并发 |

结论：**并发安全必须是一个显式声明，缺省必须是"串行"**。按 `CapRead` 推断并发安全，等于让
`ask_user` 与 `plan_*` 一起飞——而这正是"顺手把只读工具并发掉"最容易犯的错。

### 3. 子 agent：完全没有（已核实）

全仓搜 `subagent|sub-agent|delegate`，源码只有 3 处命中，全部无关：`internal/memory/openviking.go:246,252`
的 `Delegates to the local store` 注释与 `internal/memory/openviking_test.go:474`。

缺它的代价是**探索污染上下文**：为了改一个函数，先 grep 20 次、读 30 个文件摸清结构，这 50 次
工具结果全部留在这一轮的上下文里。理想形态是子 agent 带着脏上下文去调研，只回一份结论，主
agent 的窗口留给代码本身。

## Why

- **延迟是线性叠加的，而其中大部分是白等。** 模型要同时读 5 个文件、跑 2 条命令，现在就是 7 倍
  延迟。读文件是 I/O 等待，等待可以重叠。
- **但顺序是模型写的程序。** 一轮里的多个工具调用不是一堆无关的请求，而是一段有顺序的代码：
  「先写配置，再跑测试」如果被重排，测试就会跑在旧配置上。所以**写/执行必须是屏障**：它之前
  的所有调用先做完，它之后的所有调用等它做完。这不是性能妥协，是正确性要求。
- **审计顺序是已经写进代码的承诺。** `agent.go:107` 的注释说明有人判断"顺序确定"比"省这点
  延迟"更重要。并行的正确做法是**结果按程序顺序组装**（`res.Tools` 与发回模型的历史消息严格
  对应 `msg.ToolCalls` 的顺序），而不是放弃顺序。事件的发出顺序可以是完成顺序——客户端已经
  按 `ToolCallID` 配对（`internal/chat/events.go:99-101`），所以乱序到达本来就安全。
- **并发要能取消、能隔离崩溃。** 一个工具的失败是它自己的观察值，不该连坐兄弟调用；一个工具的
  panic 现在会直接带走整个进程（串行下没有 recover），并发之后必须逐调用 recover。
- **子 agent 的价值是上下文隔离，不是"更多算力"。** 它买个"可以随便脏的窗口"，代价是一次额外
  的模型调用序列。所以它必须**只回一份有界的报告**，中间过程留在它自己的窗口里。
- **子 agent 必须是可预期的便宜。** 它能递归、能并行、能烧钱，所以深度上限 1、并发有闸门、
  费用并入父轮次预算——否则一次 `spawn_agent` 就是一颗成本上的地雷。

## What Changes

### ADDED

1. **`internal/tool`：并发安全的显式声明（缺省串行）**
   - `Concurrency` 声明 + `WithConcurrency(t, Concurrency) (Tool, error)`，与既有的
     `CapabilityFunc` / `WithCapability` 同一个装饰器模式（`internal/tool/capability.go:41-56`）。
   - 取值：`ParallelSafe`（可与同批的其它 `ParallelSafe` 调用并发）与 `Serial`（默认：既是屏障，
     也要等前面的调用做完）。**未声明的一律 `Serial`**——一个没想过并发问题的工具不该被自动
     并行，这与 `CapabilityOf` 把未声明者当作 `CapRead` 的谨慎方向一致，但默认值相反，因为
     这里的默认值决定的是正确性而不是暴露范围。
   - 标注：`read_file` / `list_dir` / `glob` / `grep` / `time` / `calc` / `echo` / Phase 20 的
     四个只读代码智能工具 / `bash_output`（读日志）→ `ParallelSafe`；`write_file` / `edit_file` /
     `bash` / `bash_background` / `bash_stop` / `plan_*` / `ask_user` / `save_document` / 媒体工具
     → `Serial`。

2. **`internal/chat`：带屏障的有界调度（Web 循环）**
   - 把 `runner.go:377` 的循环换成调度：按 `Serial` 调用把本步的调用列表切成若干段，每段内的
     `ParallelSafe` 调用并发执行（上限 `tools.max_parallel`，默认 4；`0`/`1` = 今天的行为）。
   - **结果按程序顺序组装**：`res.Tools` 与追加进 `history` 的工具消息 MUST 与 `msg.ToolCalls`
     逐位对应（协议按 `tool_call_id` 配对，但位置对齐是既有代码与测试依赖的性质）。
   - **事件按完成顺序发出**：每个调用开始时先发 `tool_call`，完成时发 `tool_result`（都带
     `ToolCallID`），所以界面能显示"3 个正在跑"并逐个落位。
   - **失败隔离**：单个调用的错误/超时只写进它自己的结果；**逐调用 recover panic** 并转成工具
     错误（现状是串行且无 recover，一个 panic 杀掉整个进程）。
   - **取消**：ctx 取消时取消所有在飞的调用，等它们收敛；一个调用失败**不**取消兄弟。
   - 未知/未声明的工具按 `Serial` 处理：不在注册表里的名字沿用既有"未知工具"结果，且不参与并发。

3. **`internal/agent`：让 `ExecuteSequentially` 的理由继续成立**
   - `ExecuteSequentially: true` 的注释理由是审计顺序。改为并发时 MUST 让审计中间件仍然按
     **程序顺序**记录，或者保持串行——二选一，由实现决定，但 MUST NOT 让审计日志出现
     "后发的调用先记录"。
   - 具体做法：审计中间件只记录、不排序，由循环在调用全部结束后按程序顺序补一条汇总（或给每条
     记录带上程序序号，让读者能排）。选择的方案 MUST 写进 `internal/agent/audit.go` 的注释里，
     替换掉今天那句 `deterministic; helps audit ordering`。

4. **`internal/subagent`：子 agent**
   - 一个**循环无关**的实现：`subagent.Runner` 接口表达"用给定的注册表、模型、步数上限跑一问",
     由两个循环各自实现（`internal/chat.Runner` 与 `internal/agent.Agent` 都已经是可复用的
     循环）。这样预算、报告截断、限制的规则只写一遍。
   - 新工具 `spawn_agent`：参数 `{ prompt, name?, tools?, model?, max_steps?, timeout_seconds? }`。
   - **只回一份有界报告**：子 agent 的最终文本（截断到 `subagent.max_report_chars`，默认
     8000）+ 一个结构化尾注（步数、用过的工具、token 用量、`stop_reason`）。它**中间的工具结果
     与思考 MUST NOT 进入父轮次的上下文**——这就是它存在的全部理由。
   - **能力默认收窄**：子 agent 只注册 `ParallelSafe` 的**只读**工具，`tools:` 显式列出的才额外
     授予（仍需通过父轮的审批闸门与工作区策略）。`ask_user`、`plan_*`、`spawn_agent` 一律不给：
     提问要作为报告的一部分交上去；计划属于顶层轮次（子 agent 改看板会让用户看到不属于自己的
     进度）；递归留给以后的阶段（本阶段深度上限 1）。
   - **预算并入父轮次**：子 agent 的 token 与耗时 MUST 计入父轮次的预算与 usage 记录，共享父轮的
     deadline。一个可以活得比轮次更久的子 agent 会让 `chat.turn_deadline_seconds` 失去意义。
   - **并发**：`spawn_agent` 标 `ParallelSafe`（"同时探三条路"是它的主要用法），但共享一个全局
     并发闸门（`subagent.max_concurrent`，默认 2），避免一次派生出 4 个各派 4 个。
   - **失败不连坐**：子 agent 失败返回一份错误报告，父轮次继续。

5. **事件与可观测性**
   - 嵌套事件 MUST 带上 `parent_tool_call_id`（新增字段），使客户端能把子 agent 的步骤嵌在
     `spawn_agent` 那张卡片下面。顶层轮次自身的事件该字段为空。
   - 子 agent 的文本/思考增量 MUST NOT 拼进父轮次的答案（累积器要按 `parent_tool_call_id`
     分流）——否则答案里会混进一段"内部调研"。
   - 每个子 agent 一条独立的链路追踪 span；Web 上是一张可展开的工具卡片（内部步骤可查看，
     但不在模型上下文里）。
   - 日志：子 agent 的起止、步数、用量、`stop_reason` 各一条（带父轮次与调用 id）。

6. **配置**
   - `tools.max_parallel`（默认 4；`0` 或 `1` = 串行，即今天的行为）。
   - `subagent.enable`（默认 true）、`subagent.max_steps`（默认 8）、`subagent.max_concurrent`
     （默认 2）、`subagent.max_report_chars`（默认 8000）、`subagent.timeout_seconds`（默认
     继承父轮次剩余时间）。

### 明确不做（本阶段）

- **不做子 agent 的写入能力默认开放。** 默认只读 + 显式 `tools:` 才放开；"能写代码的子 agent"
  需要它自己的审批与检查点故事（现在那些是挂在轮次上的），是另一个 change。
- **不做递归子 agent（深度 > 1）。** 深度上限 1 是能被人算清成本的上限；递归需要全局预算树，
  那是 Phase 21 的检查点之外的另一套记账。
- **不做子 agent 的后台化**（"派出去、稍后收结果"）。它会让子 agent 活得比轮次长，与共享
  deadline 冲突；真要长时间跑，用 `bash_background` 跑一个 `huan-agent run`（Phase 19）。
- **不做工具调用的跨步并行**：并行的单位是**同一个模型回复内的多个调用**，不是把多个步骤揉在
  一起——步骤之间有模型思考，那是串行的。
- **不按 `CapRead` 推断并发安全。** 见上：这个推断会把 `ask_user` 和 `plan_*` 一起并发掉。
- **不改 `bash` 的语义**：它仍是 `Serial`（执行即屏障），`bash_background` 也是。
- **不做并行度的自适应调节**（按 CPU/延迟动态调）。一个固定的、可配置的上限是人能理解的。

## Impact

- 新增包：`internal/subagent`。
- 新增文件：`internal/tool/concurrency.go`、`internal/subagent/{subagent,runner,report}.go`、
  `internal/tool/builtin/subagent.go`、`web/src/components/SubagentCard.vue`。
- 改动：`internal/chat/runner.go`（调度与结果组装）、`internal/chat/events.go`
  （`parent_tool_call_id`）、`internal/agent/{agent,audit}.go`（顺序承诺的重新表述）、
  `cmd/huan-agent/{chat,workspace_tools}.go`（并发声明与 `spawn_agent` 注册）、
  `internal/config/config.go`、`internal/server/turns.go`（累积器分流嵌套事件）、
  `web/src/{chatStore.js,styles.css}`、`docs/tools.md`、`docs/long-tasks.md`。
- 依赖：Phase 18（启动）、Phase 21（审批闸门是子 agent 能力收窄的前提）。与 Phase 20 无关。
- 兼容性：`tools.max_parallel` 默认 4 会让**多只读调用的一步变快**，但结果顺序不变，
  所以对模型与界面是"更快且一样"。`subagent.enable` 默认打开只是多了一个工具（模型自己决定
  用不用）。要完全回到今天的行为，两个开关都能关。
- 风险与对策：并发 + 现有写工具的组合是最容易出数据损坏的地方。对策是**默认串行 + 屏障语义 +
  只把明确声明的纯读工具并发**，并且用一个测试断言"同一轮里对同一文件的两次写不会重叠执行"。
