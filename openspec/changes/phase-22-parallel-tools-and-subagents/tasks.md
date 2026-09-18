# Phase 22 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

实施记录（2026-09-18）：**两边都完成并验证**。并发这一半：声明机制、打标、调度器、装饰器
转发、并发安全的事件通路、配置、文档。子 agent 这一半：`internal/subagent` 核心、`spawn_agent`
工具、TurnResources 接线、配置、文档，并已用真模型端到端跑通。
实施中顺带修掉两个既有缺陷（第 9 节），以及两个由测试抓到的真实问题（第 13 节）。
**唯一剩下的是一条 UI**：子 agent 的步骤在控制台上没有可展开的卡片（报告与尾注有，链路追踪有）。

## 1. `internal/tool`：并发声明

- [x] `Concurrency` 类型：`Serial`、`ParallelSafe`，以及一个独立的零值 `Unspecified`
      （零值直接用 `Serial` 会让「没人声明」和「声明了串行」看起来一样，而一个测试必须能分辨
      这两件事，否则「显式声明」这条要求无法被检查）
- [x] `concurrencyFunc` 装饰器（照 `CapabilityFunc` 的样子），**并显式转发 `Capability()`**：
      嵌入 `Tool` 接口只提升该接口的方法，`Capability` 不在其中
- [x] `WithConcurrency(t, mode) Tool`（`Unspecified` 时原样返回，不套一层）
- [x] `ConcurrencyOf` 返回 `Unspecified`；`EffectiveConcurrency` 把它当 `Serial`（保守方向：
      没想过的工具单独跑只是慢，并行跑可能坏数据）
- [x] `Segment(modes) [][]int`：纯函数，程序顺序切成「连续 ParallelSafe 成一组 + 其余各自成组」
- [x] `UndeclaredTools` / `DescribeConcurrency` / `ValidateConcurrency`
- [x] 测试 10 个：默认串行、能力与并发是两个轴（用仓库真实的五个工具证明）、声明穿过三层
      装饰器、`Segment` 六种形状、`Segment` 是分区且不重排程序、`Unspecified` 是空操作、
      未声明清单、描述、校验
- [x] 测试：未声明 = `Serial`；声明往返；装饰器不改变 `Name`/`Description`/`Capability`；
      与 `WithCapability` 同时使用时的组合顺序（两个装饰器都要生效）

## 2. 给现有工具标注

- [x] `ParallelSafe`：`read_file`、`list_dir`、`glob`、`grep`、`time`、`calc`、`echo`、
      `bash_output`、Phase 20 的四个代码智能工具
- [x] `Serial`（每个都写了理由）：`write_file`、`edit_file`、`bash`、`bash_background`、
      `bash_jobs`、`bash_stop`、`plan_*`、`ask_user`、`save_document`、媒体工具
- [x] `ParallelSafe`：`spawn_agent`（"同时探三条路"是它的主要用法；并发总量由
      `subagent.max_concurrent` 单独闸门约束）
- [x] 测试：`TestEveryRegisteredToolDeclaresConcurrency`（**四个 surface 各跑一遍**，断言没有
      未声明的工具）与 `TestConcurrencyExpectations`（期望表 + 必须存在的工具清单，防止表变成
      空转还显示通过）

## 3. `internal/chat`：带屏障的有界调度

- [x] `Config.MaxParallel`（来自 `tools.max_parallel`，默认 4），`<=1` 时走原来的串行路径
- [x] `runToolCalls`：按 `tool.Segment` 分组，组内用带缓冲信号量的有界扇出，组间严格串行
- [x] 分段逻辑本身在 `internal/tool` 的 `Segment` 里（纯函数，单独测），调度器只消费它
- [x] 段与段之间严格串行（屏障语义：写/执行之前的调用先完成，之后的调用等它完成，有测试）
- [x] **结果按程序顺序组装**：`runToolCalls` 返回的切片按调用下标写入，历史消息按
      `msg.ToolCalls[i]` 取 ID 与名字，所以位置对齐是构造出来的，不是靠完成顺序碰巧一致
- [x] **事件按完成顺序发出**：`runTool` 内部照旧发 `tool_call` / `tool_result`（都带
      `ToolCallID`），所以并行时到达顺序就是完成顺序——客户端按 id 配对（`Event.ToolCallID`
      存在的意义），界面显示"3 个正在跑"并逐个落位
- [x] 逐调用 `recover`：panic 转成该调用的工具错误**并且发一条 `tool_result`**（否则界面上会
      留下一个永远不返回的调用），MUST NOT 带走进程
- [x] 失败隔离：一个调用的错误不影响兄弟（有测试）；ctx 取消时每个调用各自走既有的取消路径
      （`runTool` 里已有），`wg.Wait()` 保证收敛
- [x] 未知工具名沿用既有的"未知工具"结果路径，且不参与并发（视为屏障）
- [x] 测试（`internal/chat/parallel_test.go`，8 个，**全部在 `-race` 下通过**）：
  - [x] 三个纯读调用并发（用时间窗重叠断言，不是断言耗时）
  - [x] `[read, write, read]` 的窗口两两不相交（写是屏障）
  - [x] `[write, write]` 不重叠
  - [x] 结果顺序 = 程序顺序（**最慢的排第一个**，这正是按完成顺序实现会做错的地方）
  - [x] `max_parallel=0` 与 `=1` 时调用不重叠（回归闸门）
  - [x] `max_parallel=2` 时峰值并发恰好 2（上限是约束不是建议）
  - [x] 一个调用 panic 时其余照常完成，panic 变成工具错误并发事件
  - [x] 一个调用失败不取消兄弟

## 4. `internal/agent`：审计顺序承诺的重新表述

- [x] 决定：保持串行（理由见 `internal/agent/agent.go` 的注释）
- [x] `internal/agent/agent.go` 那句注释已替换为完整理由
      （替换 `deterministic; helps audit ordering`）
- [x] 不改并发，因此不需要给审计补程序序号
- [x] 测试：串行保证审计顺序；`TestEveryRegisteredToolDeclaresConcurrency` 保证 Eino 路径注册的
      工具也有声明，将来真改并发时不会因缺声明而出错

## 5. 事件与累积器

- [x] `chat.Event.ParentToolCallID`（+ `chat.Nested` 的 `WithNested` / `NestedFrom`）
- [x] `internal/server/turns.go` 的累积器：带 `parent_tool_call_id` 的文本/思考增量 MUST NOT
      进父轮次的答案；它们归属到那张 `spawn_agent` 卡片
- [x] 顶层事件该字段为空（回归闸门：既有会话的 reader 行为不变，有测试）
- [x] 测试：嵌套增量不进答案、嵌套工具调用挂在父卡片下、缺字段时退化到顶层行为

## 6. `internal/subagent`

- [x] `subagent.Runner` 接口（`RunNested`）：用给定的注册表、模型与步数上限跑一问，返回文本 + 工具清单 + usage
- [x] **两个实现都有了**：`nestedChatRunner`（控制台与 `run` 的 chat 循环）与 `einoNestedRunner`
      （CLI 与飞书用的 Eino ReAct 循环）。两者都起一个**独立的嵌套 agent**，只共享父轮次的
      session id（用量与审计归发起它的那一轮）
- [x] `Limits{ MaxSteps, MaxConcurrent, MaxReportChars, Timeout }` + `Or()`（默认 8 / 2 / 8000）
- [x] `Narrow(parent, extra)`：只继承父注册表里**只读且并行安全**的工具；`tools:` 显式授予写/执行
      （授予的是父轮次自己的工具实例，所以审批闸门/检查点/沙箱仍然生效）
      （仍受父轮次的工作区策略与审批闸门约束）
- [x] 一律不授予：`ask_user`、`plan_*`、`spawn_agent`（各自有具名理由，显式请求也被拒）
- [x] 报告：最终文本截断到 `max_report_chars`（截断如实说明）+ 结构化尾注（步数、用过的工具、token 用量、
      `stop_reason`、失败原因）
- [x] 中间过程不外泄：子 agent 的工具结果与思考 MUST NOT 进入父上下文（`internal/server/nested_test.go`
      断言答案里没有它们；端到端实测父上下文的 21103 tokens 里没有那 24 次文件读取）—— 测试：
      假模型断言）
- [x] 预算：token 与耗时计入父轮次并共享 deadline（`SessionID`/`Scope` 取自父轮次）；deadline 到了
      子 agent 一起停）
- [x] 并发闸门：`max_concurrent` 全局信号量（一个进程一个 `Agent`）；超出时排队而不是报错（有测试断言峰值恰好为 2）
- [x] 失败是**报告**而不是错误：父轮次继续跑，报告里说明失败与下一步建议
- [x] 取消：父轮次 stop 时子 agent 收敛（有测试）；闸门取用也观察 context
- [x] 测试 14 个：报告截断与尾注；只读工具集（写工具缺席）；显式 `tools:` 放开；`ask_user`/`plan_*`
      一律缺席；嵌套增量不进父答案；预算并入父轮次；deadline 传播；并发上限生效；
      失败不连坐；并发调用与 `-race`

## 7. `spawn_agent` 工具

- [x] 参数：`prompt`（必填）、`name?`、`tools?`、`model?`、`max_steps?`、`timeout_seconds?`
- [x] 标 `ParallelSafe` 且 `CapRead`（它自己不写工作区；被授予的工具各自受策略约束）
- [x] 模型-facing 描述写成规则：什么时候该派（多文件探索、需要读很多才回答得了的问题）、
      什么时候不该派（一问就知道的事；结论要精确到行的工作）
- [x] `schema_test.go` 的逗号截断守卫纳入输入结构（描述里的逗号用分号写）
- [x] 测试：参数校验全表；`subagent.enable=false` 时工具不注册（`registerSpawnAgentTool` 提前返回）；
      `max_steps` 上限只能降不能升；
      `model` 覆盖生效；未知 `tools` 名字给出可读拒绝

## 8. Web：子 agent 卡片

- [~] **没有单独做 `SubagentCard.vue`**：嵌套动作显示在 spawn 调用**既有的工具卡片**里（一行一个
      动作 + 类型标签），报告与尾注就是工具结果。少一个组件、复用既有的折叠与状态渲染，
      代价是卡片不是为子 agent 定制的（没有专门的用量行）
- [x] `chatStore`：按 `parent_tool_call_id` 归到父卡片（四条事件路径：文字/思考/工具调用/工具结果）
- [x] 子 agent 的文本增量不显示为助手消息，只显示在卡片内部（服务端累积器与 store 两侧都有断言）
- [x] `ssr-probe` 8 条断言：规范化、未知 kind 丢弃、工具结果/错误保留、存下来的调用重建嵌套、
      这一轮只有一个工具调用（孤儿退化由服务端测试覆盖）
- [x] `npm run check:ui` 全绿（208 PASS）；`npm run build` 已重建 dist

## 9. 配置与文档

- [x] `ToolsConfig.MaxParallel` + `MaxParallelOr()`（`tools.max_parallel`，默认 4）+ `SetDefaults`
- [x] `SubagentConfig{ Enable, MaxSteps, MaxConcurrent, MaxReportChars }` + 各 `Or()`（`TimeoutSeconds`
      改成了每次调用的 `timeout_seconds` 参数，配置里没有全局默认 —— 见下方说明）
      `Or()` 方法（沿用既有 *Or 命名）
- [x] `SetDefaults`：`tools.max_parallel=4`、`subagent.enable=true`、`max_steps=8`、
      `max_concurrent=2`、`max_report_chars=8000`、`timeout_seconds=0`
- [x] `configs/config.example.yaml`：`max_parallel` 与 `subagent` 两段带注释（含"0/1 = 串行"）
- [x] `docs/tools.md`："Parallel tool calls" 与 "Subagents" 两节（并发规则、屏障、结果顺序不变、
      `spawn_agent` 的继承与永不授予、嵌套动作的可见性）
      用法与默认只读
- [x] `docs/long-tasks.md` 新增「Subagents: context isolation, not compute」一节（成本归父轮次、
      全进程并发闸门、默认只读）
- [x] 记录 `CAPABILITY-GAPS.md` 的更正：串行在两处、理由不同（`runner.go` 是没人写调度，
      `agent.go:107` 是刻意的审计顺序）；以及"按 `CapRead` 推断并发安全"是不可行的

## 10. 验证

- [x] `go test ./... -count=1` 全绿
- [x] **`go test -race ./internal/... ./cmd/...` 全绿**（并发路径必须跑 race；这也是这个
      仓库第一次让整个 `-race` 套件通过——顺带修掉了两个既有缺陷，见第 9 节）
- [x] `make build` 通过；`go vet` + `gofmt -l` + `-race` 通过；`golangci-lint` 见本节末尾那条
- [x] 结果顺序与界面呈现不变有测试（程序顺序、最慢的排第一）；"明显变快"用**时间窗重叠**断言
      而不是耗时（比计时稳定），所以没有单独的人工计时
- [x] `tools.max_parallel=0/1` 时不重叠 —— 有自动化测试，不是人工对比
- [x] 端到端（真模型 + `huan-agent run`）：`spawn_agent` 派一个子 agent 调研 `internal/tool`，
      子 agent 跑 8 步读 24 个文件，父轮次只拿到几百字结论（那 24 次读取不在父上下文里）
      两个循环的工具执行路径，各回一段结论"` 能返回两份结论，且主轮次上下文里没有中间的
      几十次文件读取（人工核对链路追踪的 token 数）


## 11. 本阶段尚未开始的部分（子 agent 那一半）

按优先级。都是干净的起点，除了已完成的并发部分之外不依赖任何东西：

1. **`internal/subagent`**：循环无关的 `Runner` 接口 + 限制 + 有界报告 + 预算并入父轮次。
   设计已在 `specs/subagents/spec.md` 里（默认只读、深度上限 1、报告截断到 `max_report_chars`、
   共享父轮次 deadline、`max_concurrent` 全局闸门、一律不授予 `ask_user` / `plan_*` / `spawn_agent`）。
2. **`spawn_agent` 工具**，声明 `ParallelSafe`（"同时探三条路"是它的主要用法）。
3. **`parent_tool_call_id`**：事件上的新字段、累积器按它分流（嵌套增量不进父答案）、
   Web 上归到那张 spawn 卡片下面。
4. **`SubagentCard.vue`** + ssr-probe + 重建 `internal/server/webui/dist`。
5. **`config.SubagentConfig`** + `docs/tools.md` / `docs/long-tasks.md` 的相应段落。

并发那一半已经为它准备了两个前提：`spawn_agent` 只要声明 `ParallelSafe` 就会自动与同批的只读
调用重叠；而"只回一份有界报告"正是并发安全的前提——子 agent 的中间过程不进入父上下文，也就不与
父轮次的任何东西共享可变状态。

## 12. 实施中顺带修掉的两个既有缺陷（第 9 节所指）

这两个都不是本阶段引入的，而是 `-race` 第一次跑全仓时暴露出来的：

**缺陷 1：`internal/server/openviking_test.go` 的 stub 有数据竞争。**
`stubConsole` 的字段被 HTTP handler（Hertz 的 goroutine）写、被测试 goroutine 直接读。唯一读过它
值的地方都改成了加锁的访问器（`flushCount` / `savedDocuments` / `wasFullSync`）。一个会竞争的
测试不只是"可能失败"——它可能因为读到旧值而**通过**，那比失败更糟。

**缺陷 2：Phase 20 的 `TestOpenSendsFullContentOnEveryChange` 是 flaky 的。**
`client.Open` 发的是**通知**，没有响应可以等，所以测试在发送后立刻断言 fake server 收到了——这
是在和对方读循环赛跑。`-race` 下变慢就暴露了。修法是新增 `waitForCount` 轮询等待，而不是断言
一个刚刚异步发出的东西到达了。

两个缺陷的教训是同一个：**"我发出去的东西对方已经收到了"需要同步点，通知没有同步点。**


## 14. 子 agent 的可见性（`parent_tool_call_id`）

- [x] `chat.Event.ParentToolCallID` + `chat.Nested`（`WithNested` / `NestedFrom`）：**谁拥有轮次，
      谁发布发射器**——工具在启动时构建一次，不可能知道这次调用属于哪一轮、发射器在哪
- [x] `runTool` 在调用工具之前把「父调用 id + 撞上标记的 emitter」放到 ctx 上，所以嵌套事件
      自动带上所属调用的 id，调用方**不可能忘记**
- [x] 嵌套 runner：有 sink 就把子 agent 的事件送进父轮次的流，没有就照常跑（**没人看不是不跑的
      理由**：报告照样产出，只是看不到过程）
- [x] `chat.ToolRun.Nested` + `NestedCall`：嵌套动作**存进轮次**，所以刷新之后看到的和实时看到的是
      同一件事
- [x] 服务端累积器：带 `ParentToolCallID` 的事件**只记到发起它的那次调用下面**——
      **文字不进答案**（子 agent 的意义就是它的过程不进父上下文；进了答案等于给读者看模型没写过的
      字，而且存下来的轮次不是模型的输出），**工具调用不进这一轮的步骤**（否则计划看起来做了它
      委派出去的事）
- [x] 同名同类增量合并（一段话是一个块，不是每个 token 一条）
- [x] 父调用不存在的孤儿事件**丢弃**而不是塞进错误的位置
- [x] `run --output json` 的每个工具带上 `nested`：一次性运行的读者没有别的窗口看子 agent
- [x] Web：`steps.js` 的 `normalizeNested`（刷新后仍在）、`chatStore` 按 `parent_tool_call_id`
      归类（`text_delta` / `reasoning_delta` / `tool_call` / `tool_result` 四条路径）、
      `ChatMessage.vue` 的嵌套列表 + `styles.css`
- [x] 测试：服务端 4 个（答案不被污染、嵌套调用不算这一轮的工具、增量合并、孤儿丢弃、
      顶层事件不受影响），ssr-probe 8 条（规范化、未知 kind 丢弃、工具结果/错误保留、
      存下来的调用重建嵌套、这一轮只有一个工具调用），`go test ./...`、`npm run check:ui`
      （**208 PASS**）、`npm run build` 重建 dist
- [x] 端到端（真模型）：`spawn_agent` 一次调用，JSON 里该调用下有 **11 条嵌套动作**
      （子 agent 的思考、`glob`、`list_dir`、六个并行 `read_file`），父轮次答案里没有它们

### 实施中抓到的两个问题

**问题 1：`--output json` 看不到嵌套动作（真实缺口，已补）。** 嵌套事件只走 `emit`（到了控制台），
没有进 `chat.ToolRun`，所以一次性运行的读者——以及任何读结果的人——看到的是一张"派生了东西但
没说结果"的卡片。修法是在 `runTool` 里同时收集（同样的合并规则），并在 `run.go` 的输出结构里
带上这个字段。

**问题 2：Web 里我给 `text_delta` / `reasoning_delta` 加了重复的 `case` 分支（真实缺陷，已修）。**
JS 的 `switch` 第一个匹配的分支生效，所以后加的两个分支永远不会执行——嵌套文字会照旧被当成父轮次
的答案。修法是把判断放进**既有的**分支开头。


## 15. 补齐 Eino 那条路（第二轮）

第一轮只实现了 chat 循环的嵌套 runner，于是 **CLI 与飞书上 `spawn_agent` 会注册但每次调用都失败** ——
比"这个工具不存在"更糟。本轮补齐：

- [x] `einoNestedRunner`：用收窄后的注册表起一个独立的 `agent.Agent`（自己的步数上限），
      `Generate` 一问，返回文本与 usage
- [x] `internal/agent` 发布 `tool.TurnResources`（`withTurnResources`）：**这是让它真的能跑起来的关键** ——
      工具在启动时构建，不可能知道这一轮的注册表与模型，所以由拥有轮次的一方发布（chat 循环早就这么做了）
- [x] CLI（`buildAgent`，含把 usage recorder 传进来）与飞书（`buildFeishuTooling`）两处接线；
      飞书没有审批通道，所以它本来就不注册写/执行工具，收窄之后无可授予——这是正确结果而不是特例
- [x] 端到端验证：`chat --tools` 里 `spawn_agent` 真的跑起来并返回正确结论（子 agent 用 `glob`
      数出 6 个 .go 文件，父轮次核对一致）

### 顺带修掉一个会误导模型的问题

**Eino 的 ReAct 循环只返回最终消息，所以这条路上的 runner 既不知道步数也不知道用过哪些工具。**
它最初如实填了 `Steps: 0`、`Tools: nil` —— 而报告尾注就写成"0 步"，于是**父模型据此怀疑一个其实
干得不错的子 agent**（第一次真机跑的时候它就是这么说的："它很可能没有真正调用工具就作答了"）。
修法是给 `NestedResult`/`Report` 加 `StepsUnavailable`，尾注改成明说"步数与工具明细不可得
（这条路上跑的是 Eino 的 ReAct 循环）"。**一个报告说"0 步"和说"不知道"是两件完全不同的事，
而前者会让模型做出错误判断。**
- [~] `golangci-lint run`：**工具已装好并跑过**（`go install` 需要 workspace 之外的写权限，已获准）。
      本阶段新增/改动的代码是**干净**的；仓库里还剩 20 条**既有**告警，在本次工作**没有创建**的文件里
      （用 `git status` 逐个核对过：`memory.go`、`agent/audit.go`、`auth.go`、`skillapi.go`、`skills.go`、
      `mcp/manager_test.go`、`ask_test.go`、`usage.go`、`llm/retry_test.go` 等）。另外 `internal/lsp/sysproc.go`
      的 `processAlive` 是**误报**：它被 `//go:build integration` 的测试用着，而 linter 默认不编译带 tag 的文件
      —— 就是那个在 Phase 24 藏了一个编译不过的集成测试文件的同一个盲点。
      本阶段引入的 5 处死代码已在最后一轮清掉（`registerStaticTools`、`Server.checkpoints`、
      `workspaceToolSet.checkpoints`、`approvalGate.timeout`、`errNoApprover`）与 1 处 errcheck
      （`run.go` 的用量记录）。
