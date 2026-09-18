# Phase 17 — 计划、重试与失败续跑

## Summary

长任务今天只有两档命运：跑完，或者**报废**。

1. **模型调用没有任何重试。** 一次 429 / 502 / `unexpected EOF` / 连接被重置，无论它发生在
   第一步还是第四十步，都直接让整轮 turn 失败（`internal/chat/runner.go` 的 `streamOnce`
   把错误原样抛给调用方）。长任务的失败概率与运行时长成正比，而现在的处理是"概率 1 就是
   全损"。
2. **失败之后只能从头再来。** 用户想接着跑，唯一的办法是把原话再说一遍；模型没有上一轮
   的任何观察值（tool result），于是它把已经读过的文件再读一遍、把已经改好的一半再改一遍。
   已经落盘的进展（工作区里的文件改动）与已经花掉的 token 都白费了。
3. **没有计划，也就没有"执行到哪了"。** 一轮 40 步的任务，中间断了之后没人能说清哪些做完
   了、哪些没做——包括模型自己。提示词里没有计划的概念，界面上也没有任何地方显示进度。
4. **界面上看不到任务本身。** 读者只能看到一串工具卡片和思考，无法一眼回答"还剩几件事"。

本次变更给长任务装上**三层恢复能力**，并把任务本身变成一等公民：

- **调用级重试**：模型调用失败（网络/限流/5xx）按指数退避自动重试，退避时间逐次拉长；
- **步骤级重试**：流已经吐了一半才断的调用，重跑这一步（并让界面丢掉那半截重复文本）；
- **任务级续跑**：重试仍然失败时，用户点「继续执行」，新一轮**带着上一轮的计划与已完成的
  步骤**接着跑，而不是从零重新思考。
- **计划工具 + 任务看板**：模型先建计划、每完成一步更新计划；看板固定在输入框上方，显示
  已完成 / 执行中 / 待执行。

## Why

- **失败是长任务的常态，不是异常。** 一次 60 分钟的任务要发几百次模型调用，任何 0.1% 的
  瞬时故障率都会让"不重试"变成"跑不完"。重试必须内建，而且必须是**退避**而不是忙等：限流
  的主要成因就是重试得太急。
- **"从零重来"消耗的是用户的钱和信任。** 一次中断后重头跑，等于把已经花掉的 token、已经
  做完的只读排查、已经改好的文件全部再付一遍。真正缺的不是算力，而是**接续点**。
- **计划是把"上下文"变成可传递的状态。** 一份带状态的任务清单让中断可解释、可续跑：模型
  读它就知道从哪继续，用户看它就知道还剩什么。它也是唯一能跨轮、跨重启存活的进展记录。
- **计划要被执行推着走，不能只是开场白。** 唯一的正确形态是"每完成一步立刻更新"，所以
  计划是一组工具（模型每步调用），不是一段提示词。
- **看板的位置就是它的语义。** 它是"这一轮正在为我做的事"，与输入框同层：不属于消息流的
  历史，也不属于侧栏的会话列表。

## What Changes

### ADDED

1. **`internal/retry`：可复用的退避策略**
   - `Policy{ Enabled, MaxAttempts, InitialBackoff, MaxBackoff, Multiplier, Jitter }` +
     `Delay(attempt)` + `Do(ctx, fn, onRetry)`：纯时间逻辑，不依赖 HTTP 或 eino。
   - 退避 `initial * multiplier^(attempt-1)`，封顶 `MaxBackoff`，默认带抖动（避免多个并发
     轮次同时醒来再次撞上同一个限流窗口）。
   - `Do` 尊重 ctx：取消立即返回，不再 sleep 一个没人等的间隔。

2. **`internal/llm`：单次模型调用重试**
   - `New` 返回的模型外面包一层 `retryingModel`（config 打开时），对 `Generate` 与
     `Stream` 的**建立阶段**（含第一个 chunk 之前）重试。
   - 可重试判定按语义而不是按"是不是 error"：超时 / 连接错误 / 408 / 429 / 5xx 可重试；
     400 / 401 / 403 / 404 / 422 / 余额不足等**直接失败**——重试只会把同一个错误再买一遍。
   - `*LLMError` 实现 `Retryable() bool`，让上层（runner）能问同一个问题而不必 import
     `internal/llm`。
   - 流已经发出内容之后再断，**不在这一层重试**：重放会让用户看到两遍同一句话。它交给
     步骤级重试，由后者显式通知界面丢弃半截文本。

3. **`internal/chat`：步骤级重试与两个新事件**
   - `Config.StepRetry retry.Policy`；`streamOnce` 失败时按策略重跑**这一步**（历史不变，
     半截输出作废），重试前检查本轮墙钟预算，睡眠不越过 deadline。
   - 新事件 `step_retry{ step, attempt, max_attempts, delay_ms, error }`。
   - 新事件 `plan{ step, plan }`（见下）。

4. **`internal/tool` + `builtin`：计划能力**
   - `tool.Plan / tool.Task / tool.TaskStatus`，`tool.Planner` 接口（`Current` / `Replace` /
     `Append` / `Update`），`WithPlanner` / `PlannerFrom` 上下文注入——与 `ask_user` 的
     `Asker` 同一套做法，因为同样的三个包要就同一份词汇达成一致。
   - 四个工具：`plan_create`（建计划，替换）、`plan_add`（追加任务）、`plan_update`（改一个
     任务的状态/备注）、`plan_read`（读当前计划）。返回值里永远带一份渲染好的清单，所以模型
     每调用一次就看到最新进度，不需要额外读一次。
   - 仅在 Web 面注册（与 `ask_user` 同理）：没有 Planner 的通道（CLI / 飞书）装上只会是
     一个必然失败的工具。

5. **`internal/store`：计划落库**
   - 迁移 14：`chat_plans(session_id PK, goal, tasks JSON, revision, updated_at)`，随会话
     级联删除；`GetChatPlan` / `SetChatPlan` / `DeleteChatPlan`。「清空对话」一并清掉。

6. **`internal/server`：接线、看板数据与失败续跑**
   - 每轮在 turn 的 context 上装一个 `turnPlanner`（写库 + 发 `plan` 事件），于是计划在
     **生成过程中**就实时出现在界面上，刷新后仍在。
   - `GET /api/chat/sessions/{id}` 的响应增加 `plan`。
   - `POST /api/chat/sessions/{id}/resume`：从**上一轮的落库记录 + 当前计划**拼一份接续简报
     （原始目标 / 中断原因 / 计划现状 / 已完成步骤与工具观察 / 最后一段输出），作为模型的
     上下文注入新一轮，并在会话里留一条可见的 `继续执行` 用户消息。会话已有轮次时 409；没有
     可接续的东西时 400 并说明原因。
   - 简报里的工具输出按条截断并**如实说明被截断**（模型需要知道它看到的不是全文）。

7. **Web：任务看板**
   - 新组件 `TaskBoard.vue`，固定在输入框**上方**：目标 + 进度（n/m + 进度条）+ 三组
     （执行中 / 待执行 / 已完成），失败的任务留在「待执行」组并标红，可折叠。
   - 无计划时整个组件不渲染（一个空的看板是噪音）。
   - `继续执行`：计划有未完成项且当前没有轮次在跑时出现，点击即 `POST .../resume`，界面
     行为与发消息一致（立刻显示用户气泡 + attach 新一轮）。
   - `step_retry` 事件渲染成该步骤上的一行提示，并**清空该步骤已收到的半截文本**。

### 明确不做（本阶段）

- **自动续跑**（服务重启后自己接着跑）：续跑由人决定，因为它要花的是人的钱；本阶段只把
  "接续点"做成可点击的一次操作。真正的后台任务调度是另一个 change。
- **飞书 / CLI 的计划与续跑**：看板与续跑按钮是 Web 控制台的交互，其他通道先只享受重试。
- **计划的嵌套/依赖图**：一张扁平清单足够表达"还剩几件事"，依赖关系交给模型自己排序。
- **跨会话共享计划**：计划属于一个会话，与它的上下文同生共死。

## Impact

- 新增包：`internal/retry`。
- 新增文件：`internal/llm/retry.go`、`internal/tool/plan.go`、`internal/tool/builtin/plan.go`、
  `internal/server/plan.go`、`internal/server/resume.go`、`web/src/components/TaskBoard.vue`。
- 改动：`internal/chat/{runner,events}.go`、`internal/llm/provider.go`、`internal/store/{store,migrate,chatsession}.go`、
  `internal/server/{chat,turns}.go`、`internal/config/config.go`、`internal/prompt/surface_web.md`、
  `configs/config.example.yaml`、`web/src/{api.js,chatStore.js,styles.css}`、
  `web/src/components/ChatView.vue`。
- 兼容性：全部新配置项都有"保持今天行为"的默认值吗？——不。**重试默认打开**（这是本次变更
  的目的），但次数保守（单次调用 3 次尝试、单步 2 次额外尝试），并且**只对可重试错误生效**。
  计划工具只影响 Web 面，禁用方式是把 `chat.plan.enable` 设为 false。
