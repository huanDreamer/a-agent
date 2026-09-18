# turn-budget（增量）

Phase 11 定义了「一轮预算」的语义与停因；本次变更只补一件事：**预算的生效值可以由控制台
逐字段覆盖，并且在下一轮立即生效**。其余行为不变。

## API

- 解析顺序（server 侧，唯一入口 `Server.effectiveBudget(ctx)`）：**控制台覆盖值 > config.yaml**，
  **逐字段**判断。某一维度没有覆盖时，它的值就是 `config.yaml` 的值，且来源标记为 `config`。
- `GET /api/chat/budget` →
  `{ max_steps, turn_max_tokens, turn_deadline_seconds, default_max_steps,
     default_turn_max_tokens, default_turn_deadline_seconds,
     source_max_steps, source_turn_max_tokens, source_turn_deadline_seconds,
     max_steps_limit, max_turn_max_tokens_limit, max_turn_deadline_seconds_limit,
     has_override, context_max_tokens, context_requires_restart }`
  - `source_*` ∈ `config` / `console`；`has_override` 为三个来源里任意一个是 `console`。
  - `default_*` 是「恢复默认」会回到的值（来自 config），使面板能给它命名。
- `PUT /api/chat/budget` ← 稀疏 body：
  - `{ "max_steps"?: int, "turn_max_tokens"?: int, "turn_deadline_seconds"?: int, "reset_all"?: bool }`
  - 缺省字段**不动**；`turn_max_tokens` / `turn_deadline_seconds` 的显式 `0` 是「不限」这一
    真实取值，必须与「未提供」区分（因此服务端用指针字段，前端空字符串 ≠ `0`）。
  - 响应是写入后的同一个 GET 快照，让界面渲染「服务端接受了什么」而不是「我提交了什么」。
- 上限（超出即 400，不静默截断）：`max_steps` ∈ `[1, 500]`（= `chat.MaxStepsCeiling`）、
  `turn_max_tokens` ∈ `[0, 20000000]`、`turn_deadline_seconds` ∈ `[0, 86400]`。
- store：`GetTurnBudgetOverride` / `SetTurnBudgetOverride`，字段是 `*int`；nil 表示该键不存在。

## 行为

- **每轮读一次**：预算在 `handleSendMessage` 组装 `chat.Request` 时解析并写入
  `Request.MaxSteps/MaxTokens/Deadline`，因此
  - 面板的改动对**下一条消息**生效，无需重启；
  - 一轮飞行中被改预算不会移动这一轮的天花板；
  - server 侧 `runnerFor` 构造的 `chat.Config` **不带预算**（否则缓存里的 runner 会一直用
    构造时的旧数字）。
- **写入后清 runner 缓存**：`runnerCache.reset()`。缓存里的 runner 仍能正确工作（预算按轮传入），
  但显式失效让「缓存与生效值」之间不存在可观测的延迟。
- **存储失败降级**：读覆盖失败时用 config 值并在日志里 warn（一次数据库故障不该停掉对话）；
  store 不支持这两个方法（测试替身 / 老实现）时同样按 config 走，`PUT` 返回 501 而不是假装写入。
- **非法值**：`max_steps = 0` 被拒绝（0 没有「默认」以外的含义，而面板显示 0、服务端跑 60
  是比报错更糟的答案）。
- `GET /api/chat/models` 的 `max_steps` / `turn_max_tokens` / `turn_deadline_seconds`
  与面板同源（都走 `effectiveBudget`），因此「对话」页与「设置」页不可能显示两个数字。
- 面板**不**报告为「已生效」的维度：`context.max_tokens`（循环内窗口上限）在启动时构造一次，
  面板只读地展示它，为 0 时给出显式警告（「只调步数」的真实结局是撞上下文上限）。

## 接入

- 控制台：设置 → 对话预算（`web/src/components/BudgetPanel.vue`），输入规则全部在
  `web/src/budget.js`（`node scripts/check-budget.mjs` 覆盖：空值语义、0 的语义、整数与上下限、
  稀疏 body、逐字段错误）。
- 存储：`app_settings`（迁移 12）。同一张表是通用键值表，下一个需要「运行时可改」的设置
  不必再加一张表。
