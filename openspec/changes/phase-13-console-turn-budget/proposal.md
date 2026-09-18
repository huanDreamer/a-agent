# Phase 13 — 对话预算：在网页里改，改完立即生效

## Summary

Phase 11 给一轮 turn 装上了三重预算（步数 / token / 墙钟），但**改预算只有一条路**：
编辑 `config.yaml` 然后重启进程。撞上步数上限时，用户看到的是这样一句：

> 已达到本轮最大工具调用步数 12。工作区里的改动已经落盘，回复「继续」可以接着做，或调大
> 本轮的上限（chat.max_steps / chat.turn_max_tokens / chat.turn_deadline_seconds）

这句话把动作交给了用户，而用户手里是网页控制台，没有 shell。于是现实里发生的是：一个
12 步就停下的重构任务，用户只能「继续」一次、再一次。本次变更把这三个数字搬进
**设置 → 对话预算**：面板读写的是**每轮生效值**，写进数据库，**下一条消息就用新值**。

## Why

- **上限的意义在于当时能改。** 用户在对话里发现「一轮不够」的那一刻，正是最想改它的
  那一刻；要求他去 shell 改文件 + 重启，等于把一次点击变成一次运维动作 —— 而重启还会
  中断正在跑的其它会话。
- **「是什么」和「默认是什么」必须一起显示。** 面板要能回答两个问题：现在这一轮最多能跑
  多少步（生效值），以及「恢复默认」会回到哪个值（配置文件里的值）。只显示生效值，用户
  无法判断自己有没有覆盖过。
- **三个维度必须能分别改。** 只把步数从 12 调到 60、顺手把 token 上限钉成当时的值，
  会让以后改 `config.yaml` 的 token 设置再也看不见 —— 因此未填写的字段不写库。
- **面板不能让「只调步数」看起来像完整答案。** `context.max_tokens` 决定循环内窗口压缩，
  它在启动时构造一次，**不是**本面板能改的；把它藏起来就会让用户以为「调大就完事了」，
  而真实结局是十几步后撞上下文上限报错。因此面板**报告**它，为 0 时明确警告。

## What Changes

### ADDED

1. **`internal/store`：一个通用的设置表**
   - 迁移 12：`app_settings(key, value, updated_at)`。
   - `GetTurnBudgetOverride` / `SetTurnBudgetOverride`：三个键
     （`chat.max_steps` / `chat.turn_max_tokens` / `chat.turn_deadline_seconds`）。
   - **只存显式覆盖**：字段为 `*int`，nil 表示「这个键不存在，按 config.yaml」——
     「恢复默认」是 DELETE 而不是把启动默认值抄一份进来，因此以后改配置文件依然可见。
   - 非整数的行按「无覆盖」处理（宁可回到配置默认，也不让一轮跑在界面上没显示过的数字上）。

2. **`internal/server`：预算的唯一解析点 + 一对接口**
   - `effectiveBudget(ctx)`：**逐字段**合并（覆盖值优先，其余取 config），每轮读一次。
   - `GET /api/chat/budget`：生效值 + 每个维度的来源（config / console）+ 默认值 +
     上限（500 / 20M / 24h）+ `context_max_tokens` 与 `context_requires_restart`。
   - `PUT /api/chat/budget`：稀疏写入；显式 0 是**真值**（不限），`reset_all` 清空覆盖。
     越界一律 400，**不静默截断**。
   - 写入后 `runnerCache.reset()`：runner 在构造时把预算烤进去了，不清缓存就会让已经用过的
     会话继续吃旧数字。

3. **`web`：设置 → 对话预算**
   - 新面板 `BudgetPanel.vue`：三行输入（生效值 + 占位符显示默认值 + 来源标签）、保存 /
     撤销修改 / 恢复默认、步数常用档（30 / 60 / 120）。
   - `web/src/budget.js`：输入文本 → PUT body 的**全部**规则（空 = 不写、0 = 不限、
     整数、上下限），因此面板只是渲染它的判断。
   - `chatStore.js`：`budget` 状态 + `loadBudget` / `saveBudget` / `resetBudget`；保存成功后
     同时刷新 catalog，避免「对话」页与「设置」页显示两个不同的步数上限。
   - `GET/PUT /api/chat/budget` 接进 `api.js`；子标签接进 `SETTINGS_TABS` 与 `SettingsView`。

### CHANGED

1. **预算不再烤进 server 的 runner**：`runnerFor` 构造 `chat.Config` 时不再传
   `MaxSteps/MaxTokens/Deadline`，三者在 `handleSendMessage` 里按**本轮**填入
   `chat.Request` —— 这样面板的改动不需要重建 runner 就能生效。
2. **`/api/chat/models` 的预算字段取自 `effectiveBudget`**：删掉 `chatMaxSteps` /
   `chatMaxTokens` / `chatTurnDeadline` 三个只读 config 的访问器，只留一个解析点。
3. **配置字段改名**：`server.Config.ChatMaxSteps/ChatMaxTokens/ChatTurnDeadline` →
   `DefaultChat*`，因为它们现在是**默认值**而不是生效值；另加 `ContextMaxTokens` 供面板
   报告与告警。
4. **默认值上调**：`configs/config.example.yaml` 与 `configs/config.yaml` 的
   `chat.max_steps` 12 → 60、`turn_max_tokens` 0 → 400000、`turn_deadline_seconds`
   0 → 10800、`context.max_tokens` 0 → 60000、`keep_recent` 10 → 12。
5. **修掉过时注释**：`configs/config.yaml` 与 `internal/config` 都声称
   `agent.max_steps` 会被 clamp 到 25，实际是 `agent.MaxStepsCeiling = 200`。

### 明确不做（本阶段）

- **不在网页改 `context.max_tokens`**：压缩器在启动时构造一次，改它必须重启。面板报告并
  在关闭时警告，但不假装能改。
- **不做每轮金额预算**：与 Phase 11 一致，只做 token。
- **不做每会话/每用户预算**：这是进程级（全局）设置，与 `config.yaml` 同一层级。

## Impact

- `internal/store`：迁移 12、`appsettings.go`、`Store` 接口两个方法。
- `internal/server`：新增 `budget.go`（解析、接口、校验、上限），`chat.go` /
  `chatmodel.go` / `server.go` 接线，删除三个只读访问器。
- `cmd/huan-agent`：`admin.go` 注入默认值与 `ContextMaxTokens`。
- `web/`：`budget.js`、`components/BudgetPanel.vue`、`chatStore.js`、`api.js`、
  `state.js`、`components/SettingsView.vue`、`scripts/check-budget.mjs`、
  `ssr-probe` 预算面板探针。
- `configs/`：`config.example.yaml`（受版本控制）与 `config.yaml`（本机私有）默认值。
- 测试：`internal/store/appsettings_test.go`、`internal/server/budget_test.go`。
