# task-plan

## 目标

把"这一轮要做的几件事"变成**显式、可读、可续跑**的状态：模型先建计划，每完成一步更新计划；
人随时能在输入框上方看到还剩什么；中断之后，计划就是接续点。

## 数据模型（wire 契约）

计划和任务在 Go 与 JS 两侧是同一个形状（`tool.Plan` / `tool.Task`，snake_case JSON）：

```json
{
  "goal": "把 chat 的失败续跑做出来",
  "revision": 4,
  "updated_at": "2026-09-17T10:00:00Z",
  "tasks": [
    { "id": "t1", "title": "读 runner.go 的循环", "status": "done", "note": "已经确认 step 边界" },
    { "id": "t2", "title": "加步骤级重试", "status": "in_progress", "note": "" },
    { "id": "t3", "title": "跑 go test ./...", "status": "pending" }
  ]
}
```

- `status` ∈ `pending` | `in_progress` | `done` | `failed` | `skipped`。
- `id` 由服务端分配（`t1`, `t2`, …），在计划的整个生命周期内稳定——`plan_update` 靠它定位。
- `revision` 每次变更 +1，`updated_at` 是服务端时间；客户端只读，用于"比本地新"的判断。
- 空计划用 `null` 表示，而不是 `{tasks: []}`：没计划与"计划里没有任务"是两件事。

## API

- `tool.Planner`（`internal/tool`）：
  - `Current(ctx) (Plan, bool, error)`
  - `Replace(ctx, Plan) (Plan, error)`
  - `Append(ctx, []Task) (Plan, error)`
  - `Update(ctx, taskID string, TaskPatch) (Plan, error)`
  - 上下文注入：`tool.WithPlanner(ctx, p)` / `tool.PlannerFrom(ctx)`
- 工具（`internal/tool/builtin`，仅 Web 面注册）：

| 工具 | 参数 | 语义 |
|---|---|---|
| `plan_create` | `goal`, `tasks[{id?, title}]` | 建立/替换整份计划，所有任务初始 `pending` |
| `plan_add` | `tasks[{title}]` | 追加任务（不改变已有任务的状态） |
| `plan_update` | `task_id`, `status?`, `note?`, `title?` | 改一个任务；`status` 是最常用的字段 |
| `plan_read` | — | 读当前计划（续跑时先读它） |

- 每个工具的输出都包含 `summary`（一行进度，如 `已完成 1/3，进行中：加步骤级重试`）与
  `checklist`（渲染好的清单），所以模型不需要为了看进度而多调一次 `plan_read`。
- 事件：`{"type":"plan","step":2,"plan":{…}}`，每次计划变更广播一次。
- `GET /api/chat/sessions/{id}` 的响应体增加 `"plan": {…}|null`。
- `POST /api/chat/sessions/{id}/plan/` 不存在：计划只能由模型通过工具改，人的操作是"继续执行"
  （见 `turn-recovery`）。

## 行为

- 工具在**没有 Planner 的通道**里不注册（CLI、飞书）：一个必然失败的工具比没有这个工具更糟。
- 工具执行失败（比如 `task_id` 不存在）返回**可读的中文错误**，作为工具观察值交给模型，让
  它下一步改对；不 panic、不终止本轮。
- `plan_update` 的合法输入：`status` 必须是 5 个枚举值之一；`note` 覆盖，`note: ""` 清空。
  一次只能改一个任务——"一次改一件"是执行推着计划走的保证。
- 计划在**第一步之前**建立是有意为之的约束，由系统提示词要求（多步任务先 `plan_create`），
  而不是由代码强制：强制会让"一句话的小问题"也多出一次模型调用。
- 一份计划最多 50 个任务、单个标题最多 200 字符：超出的部分报错而不是静默丢弃（模型需要
  知道自己给的清单没被完整接受）。
- 计划随会话删除/清空一起消失（`chat_plans` 外键级联 + 清空接口显式删除）。
- **计划属于一次请求，不属于整个会话**：用户发来新消息（新请求）时，如果现有计划已经
  **全部完成**（所有任务都是 `done` 或 `skipped`，即 `Plan.Unfinished() == 0`），服务端
  在开跑之前把它删掉，并在这一轮的事件流里广播一个**空计划**，于是所有打开着的页面
  立刻收起看板。有一条未完成的计划不会被清掉——它还是「继续执行」的接续点。
  - 判据用"还有没有未完成项"而不是"上一轮成功与否"：上一轮失败但计划全完成时，新消息
    同样意味着新工作；上一轮成功但计划留着尾巴时，那条尾巴仍然值得显示。
  - **续跑不清**（`POST .../resume`）：被续的那份计划正是要交给模型的上下文。

## 看板（Web）

- 位置：**输入框正上方**，与 `ChatComposer` 同一层（不属于消息流的历史）。
- 内容：目标（一行，过长截断）+ 进度 `3/7` + 进度条 + 三组：
  - **执行中**：`in_progress`
  - **待执行**：`pending`（`failed` 也留在这里，标红并显示失败原因；`skipped` 显示为已跳过）
  - **已完成**：`done`
- 没有计划时**整个组件不渲染**。
- 可折叠（折叠后只留一行进度），折叠状态在当前浏览会话内保持。
- 计划变更（`plan` 事件）实时反映到看板：模型每更新一次任务，看板就动一次。
- **发新消息时立刻清掉已完成的看板**：客户端在 `sendMessage` 里就先判一次（`planResumable`
  为假即清），所以看板在点发送的瞬间消失，而不是等服务端那一轮回来；服务端随后广播的空
  计划是同一个结果，两者幂等。
- 空计划就是"没有计划"：客户端把它归一成 `null`，看板整体不渲染。

## 边界

- 不做任务依赖/并行调度：计划是给人看、给模型读的清单，顺序由模型自己定。
- 不做跨会话共享计划。
- 不做看板上的手工编辑：计划是模型的执行状态，手改会让它和实际进展脱节；人的干预手段是
  在对话里说，或者点「继续执行」。
