# Phase 17 任务清单

## 1. `internal/retry`：退避策略

- [x] `Policy{ MaxAttempts, BaseDelay, MaxDelay, Multiplier, Jitter }` + `Default()`
- [x] `Attempts()` / `Retries()`：`MaxAttempts <= 1` 即"不重试"，零值可用
- [x] `Delay(n)`：`BaseDelay * Multiplier^(n-1)`，抖动在封顶**之前**应用，所以 `MaxDelay` 是真上限（与 feishu 那份相反，理由写在注释里）
- [x] `MaxDelay < BaseDelay` 时抬到 `BaseDelay`：一个会把退避越重试越短的 cap 是配置错误
- [x] `Op{ Fn, Retryable, OnRetry, MaxDelay }` + `Do(ctx, op)`：ctx 取消立即返回，永不重试调用方的取消
- [x] `Sleep(ctx, d)`
- [x] 测试：尝试次数、退避递增与封顶、抖动手上界、`Delay` 的边界、成功前的重试、不可重试立即返回、用完尝试返回最后一个错误、取消只跑一次、per-call `MaxDelay` 精确生效

## 2. `internal/llm`：单次模型调用重试

- [x] `Option` / `WithRetry(policy, logger)`；`New(p, opts...)` 包一层 `retryingModel`（variadic，既有调用点不必改）
- [x] `Registry.WithOptions` + 内部 `lookup`（一次加锁取回 provider 与 options）
- [x] `retryingModel` 实现 `Generate` / `Stream` / `WithTools` / `Name` / `Provider`
- [x] `Stream` 只重试**建立阶段**（含第一个 chunk 之前）；一旦有 chunk 交给读者，错误原样上抛（上层步骤级重试负责）
- [x] 空流不是失败：不重试，直接交给循环（现在的行为是"空回答"）
- [x] `IsRetryable(err)`：连接/超时/408/409/425/429/5xx 可重试；4xx（含 402）与 `context.Canceled` 不可重试
- [x] `(*LLMError).Retryable()`：让上层通过一行接口问同一个问题，不必 import 本包
- [x] 每次重试写一条 warn（op / provider / model / attempt / max_attempts / delay / error）
- [x] 测试：三次尝试后成功、401 不重试、用完尝试、取消只跑一次、流建立阶段重试、流中断不重试（已吐出的文本不重复）、空流直通、重试发同一份请求、`WithTools` 后仍带重试、非 tool 模型的 `WithTools` 拒绝、零策略不包、`Name`/`Provider` 透传、`IsRetryable` 全表、日志内容

## 3. `internal/chat`：步骤级重试

- [x] `Config.StepRetry retry.Policy`（runner 级，不是 per-turn 预算）
- [x] `streamStep`：一步的模型调用失败时，从**同一份历史**重跑（失败那次没产生 assistant 消息，历史干净）
- [x] 重试前发 `EventStepRetry{ step, attempt, max_attempts, delay_ms, error }`，`attempt` 是即将进行的尝试序号
- [x] `stepRetryable`：取消/超时/`Retryable() == false` 不重试
- [x] 预算闸门：预算已过不重试；等待被 `retryShare`（step 开始时剩余墙钟）截断
- [x] warn 日志（step / attempt / max_attempts / delay / steps_done / error）
- [x] 测试：中途断流后恢复（答案 = 第二次的）、事件顺序与字段、重试发同一份历史、零策略不重试、用完尝试、永久错误不重试、预算耗尽不重试、`stepRetryable` 全表、`retryShare` 边界

## 4. `internal/tool` + `builtin`：计划能力

- [x] `TaskStatus` 五值 + `ValidTaskStatus` / `TaskStatusList`
- [x] `Task` / `Plan`（`Empty` / `Count` / `Progress` / `Unfinished` / `Task` / `IDs` / `Summary` / `Render`）
- [x] `NextTaskID`：跳过最高编号，跨多次调用也不会重号
- [x] `TaskPatch{ Title, Status, Note *string }` + `Empty()`：`Note` 用指针区分"没说"与"清空"
- [x] `Planner` 接口（`Current` / `Replace` / `Append` / `Update`）+ `WithPlanner` / `PlannerFrom`
- [x] 四个工具：`plan_create` / `plan_add` / `plan_update` / `plan_read`，全部返回 `summary` + `checklist` + `note`
- [x] 校验（模型自己能改的部分）：goal/title 非空与长度、status 取值、task_id 存在、空 patch、重复 id
- [x] 模型-facing 描述写成规则（什么时候该用、多细、必须逐步更新）
- [x] 测试：建计划分配 id、保留模型给的 id、混用时避开撞号、空输入全表拒绝、没有 Planner 时四工具都给出可读拒绝、追加保留已完成进度、更新只动一条、`clear_note` 与省略 note 的区别、未知 id / 非法 status / 空 patch 的报错里带可修正信息、空计划时 `plan_read` 不发明计划、描述与 schema 质量
- [x] `schema_test.go` 的逗号截断守卫纳入四个输入结构

## 5. `internal/store`：计划落库

- [x] 迁移 14：`chat_plans(session_id PK, goal, tasks, revision, updated_at)`，外键级联删除
- [x] `ChatPlanRow` + `GetChatPlan`（`ErrNotFound`）/ `SetChatPlan`（upsert）/ `DeleteChatPlan`
- [x] `Store` 接口三个方法
- [x] 测试：往返、`ErrNotFound`、"还没有计划"与"计划为空"不同、upsert 覆盖、删除幂等、随会话删除、空 session id 拒绝

## 6. `internal/server`：接线与事件

- [x] `turnPlanner`（`tool.Planner` 的实现）：每轮读一次库、变更后同步落库再发 `plan` 事件（顺序：先落库后广播）
- [x] `chat.plan.max_tasks` 在 planner 里强制（配置项归部署，不属于工具）
- [x] `normalizeTasks`：缺/非法 status 收敛为 `pending`（planner 是最后一道防线）
- [x] `PlanConfig` / `RetryConfig` 接入 `ChatDeps`（`StepRetry` / `PlanEnable` / `PlanMaxTasks`）
- [x] `startTurn` 在 turn ctx 上装 planner（`tool.WithPlanner`），与 asker 同一模式
- [x] `GET /chat/sessions/:id` 响应增加 `plan`（null = 没有计划）
- [x] 「清空对话」同时删除计划
- [x] `turnAccumulator` 处理 `step_retry`：只回滚**那一步**的文本与思考（`answerMark` / `reasoningMark`），不新建步骤、不动前面几步
- [x] `launchTurn` 抽出"历史就绪之后"的公共部分（发消息与续跑共用），并把 409 检查提前到写用户消息**之前**
- [x] 测试：发布并落库、更新与 revision、未知 id 拒绝、超限拒绝（不落库）、追加保留进度、跨轮读回、脏 status 退化、`sessionPlan` 无计划返回 nil、`defaultPlanMaxTasks` 与 config 默认值一致、累积器的回滚语义

## 7. `internal/server`：失败续跑

- [x] `POST /chat/sessions/:id/resume`：202 同 messages 形状；409 已在跑；400 无可续跑并说明
- [x] `resumable`：上一轮失败/停因，或计划有未完成项（`skipped` 不算未完成）
- [x] `buildResumeBrief`：原始目标 / 中断原因 / 计划现状 / 已做步骤与工具观察 / 工作区 / 下一步指令
- [x] 简报不进历史：`buildHistoryWithContext` 把它插在存储历史与新 user 消息之间
- [x] 可见的用户消息 `继续执行（还有 N 项未完成）`
- [x] 截断如实说明（每条工具输出 800 字、整份 6000 字上限）
- [x] 测试：注入内容端到端（模型真的收到简报与真实任务 id）、简报不进库、`GET session` 带计划、无可续跑 400 且不调模型、运行中 409 且不留孤儿消息、未知会话 404、简报小节顺序与退化分支、截断上限、`resumable` 全表

## 8. 配置与提示词

- [x] `config.RetryConfig`（`enable` / `max_attempts` / `base_delay_ms` / `max_delay_ms` / `multiplier` / `jitter`）+ `Policy()`
- [x] `LLMConfig.Retry` + `RetryPolicy()`；`ChatConfig.StepRetry` + `StepRetryPolicy()`
- [x] `PlanConfig{ enable, max_tasks }` + `MaxTasksOr()`
- [x] `SetDefaults`：retry 默认打开（3 次 / 800ms / 30s / 2x / jitter）、step_retry（3 次 / 1.5s / 20s）、plan 打开、max_tasks 50
- [x] 三个构造点接线：`cmd/huan-agent/admin.go`（builder + 兜底 runner + ChatDeps）、`serve.go`（飞书 registry + runner）、`chat.go`（CLI registry + `llm.New`）
- [x] `server.NewCatalogModelBuilder` 增加 `Retry` 选项并传给 `llm.New`
- [x] `runnerFor` 把 `s.chat.StepRetry` 交给每个 per-session runner
- [x] `internal/prompt/surface_web.md`：计划优先、逐步更新、续跑时先 `plan_read` 且不重做已完成步骤/不重放有副作用的操作
- [x] `configs/config.example.yaml`：`llm.retry` / `chat.step_retry` / `chat.plan` 三段注释 + 长任务段落补一句恢复链路
- [x] `docs/long-tasks.md`：三层恢复、任务计划与看板、排障表新行
- [x] 测试：`Policy()` 的默认与关闭语义、`MaxTasksOr`、`Load("")` 的三项默认值确实是"打开"

## 9. Web：任务看板

- [x] `web/src/plan.js`：`normalizePlan` / `planGroups` / `planProgress` / `planResumable` / `planHeadline`（纯函数、容忍脏数据）
- [x] `planResumable` 与服务端 `Plan.Unfinished()` 同口径（`skipped` 不算未完成）
- [x] `web/src/components/TaskBoard.vue`：目标 + `n/m` + 进度条 + 折叠 + 三组（执行中 / 待执行 / 已完成），failed 在待执行里标红，skipped 划掉；无计划整体不渲染
- [x] `chatStore`：`plan` / `planCollapsed` 状态、`selectSession` 读 `res.plan`、`plan` 与 `step_retry` 事件、`resumeTurn()`
- [x] `api.js`：`resumeChatTurn`
- [x] `ChatView` 把看板放在 `<ChatComposer>` 上方，失败气泡加「继续执行」
- [x] `step_retry` 渲染成该步骤上的一行提示，并丢掉该步骤已收到的半截文本
- [x] `styles.css` 看板样式（只用 token）、`icons.js` 的清单图标
- [x] `ssr-probe` 断言：空计划不渲染、三组、failed 位置、折叠、按钮出现/消失、脏数据退化、进度、`step_retry` 文案与回滚、`/resume` 路径与 409/400 处理
- [x] `npm run check:ui` 全绿（组件绑定守卫 + 三个 check 脚本 + SSR 探针 + vite build）
- [x] `npm run build` 重建 `internal/server/webui/dist`（嵌入二进制的那份）

## 9b. 计划的生命周期（一次请求一份）

- [x] `turnRun.newTask`：`handleSendMessage` 为 true，`handleResumeTurn` 为 false
- [x] `clearFinishedPlan`：`Plan.Unfinished() == 0` 时删库 + 广播空计划；有未完成项则原样保留
- [x] 顺序在 planner 之前：新一轮读到的是清掉之后的状态
- [x] 客户端 `sendMessage` 先清（点发送即消失），与服务端广播幂等
- [x] 测试：完成（含 skipped）清掉且发事件、有 pending/in_progress/failed 时保留、无计划是 no-op、端到端「第二轮事件流里有空计划 + 库里没了 + GET 会话为 null」、未完成计划保留、`/resume` 不清且简报里仍带清单
- [x] SSR 探针：发新消息清掉已完成计划、保留未完成计划、空计划归一成 null（看板不渲染）
- [x] 文档：`specs/task-plan`、`docs/long-tasks.md`、`docs/admin.md`

## 9c. 一轮结束后过程默认折叠

- [x] `steps.js` 的 `processShouldBeOpen({streaming, pinned})`：跑着时展开、结束后折叠、读者点过
      之后由读者说了算（纯函数，SSR 探针可直接断言真值表）
- [x] `ChatMessage.vue`：过程块加一个折叠行（`执行过程` + `N 次工具调用` + `M 条消息` + 失败标记
      + 工具合计耗时），折叠时不渲染本体（步骤/思考/卡片都不在 DOM 里）
- [x] 计数与渲染同口径（`ask_user` 不算工具调用，它是卡片），悬停提示写明「M 条消息 = 本轮模型交互数」
- [x] 答案在过程之外、之下，永不折叠；没有步骤的轮次不渲染折叠行
- [x] `styles.css`：折叠行沿用 `.step-head` 的视觉语言，计数用 `tabular-nums`（数字变化时不抖）
- [x] SSR 探针：折叠态没有步骤/思考/卡片、折叠行文案与计数、流式态展开、折叠规则真值表
- [x] 浏览器验证（`web/.verify/step-run.sh`，真 Chrome + 真服务端 + mock 模型）：折叠行文案与高度、
      折叠时不渲染本体、答案紧跟其后、点击展开后步骤与思考回来、刷新后又收起、深浅主题颜色可读；
      同时更新了这次之前就已过期的两条缩进断言
- [x] 规范与文档：`specs/web-chat/spec.md`（MODIFIED）、`docs/admin.md`、`web/README.md`

## 10. 验证

- [x] `go test ./... -count=1` 全绿（连跑三次都是 0 失败）
- [x] 顺带修掉一个与本变更无关的既有 flake：`internal/mcp` 的 `Apply` 遍历 map 决定连接顺序，
      于是"两个 server 暴露同名工具时谁保留名字"由 Go 的随机 map 顺序决定，
      `TestManager_ToolNameCollisionIsReported` 约每十次失败一次。改为按调用方给的顺序连接
      （`manager.go`，两处小改）。
- [x] `make build` 通过
- [x] `configs/config.example.yaml` 加载后 `llm.retry` / `chat.step_retry` / `chat.plan` 的值符合预期（手工核对）
- [x] 真实浏览器端到端（`bash web/.verify/step-run.sh`：真 Chrome over CDP + 真 admin server +
      OpenAI 兼容 mock 模型）：一轮的流式展开 / 结束折叠 / 点击展开 / 刷新后又折叠，全部通过
- [ ] 真实模型端到端（需要 API key 与一次真实的长任务）：建计划 → 看板推进 → 制造一次失败 → 「继续执行」
