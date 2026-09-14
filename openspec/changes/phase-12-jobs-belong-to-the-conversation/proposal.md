# Phase 12 — 后台进程属于会话，不属于设置

## Summary

Phase 10 把常驻进程做成了能力（`bash_background` / `bash_jobs` / `bash_output` /
`bash_stop`），并在控制台给了它一个落点：**设置 → 后台进程**。那个落点是错的。

后台进程是**这次对话产生出来的状态**：某一步 turn 起了它，之后的几步在看它，
进程随 agent 退出而终止。它不是一个"开关"，而设置页是放开关的地方 —— 所以它被埋在两个
只读清单中间，用户要停一个 dev server 得先想起来"设置"里有这么一栏。

本次变更把它搬回它发生的地方：

1. **来源记进作业**。`jobs.Spec` / `jobs.Job` 新增 `Scope`：启动它的那次对话（Web 会话 id /
   飞书 open_id，与 runner 发布在 turn context 上的 scope 同一个值）。工具层不 import
   `internal/chat`，而是通过 `BackgroundPolicy.Scope func(ctx) string` 由调用方注入
   `chat.ScopeFrom` —— 与 `Surface`、`Shell` 同样的接缝。
2. **会话顶部显示计数**。对话标题栏多一个 chip：本会话有进程时显示"运行中 N"，
   只有历史记录时显示"N 条进程记录"，本会话为空但同工作区有别的会话在跑时显示
   "同工作区 N 个后台进程"。没有任何相关作业时**不渲染**——常驻的 0 是噪音。
3. **点开是右侧抽屉**。`设置 → 后台进程` 整栏删除，能力搬进 `JobsDrawer.vue`：
   本会话的作业在前，**同一工作区的其他会话**的作业在后（工具本身就是按工作区管理作业的，
   这个抽屉是控制台里唯一能看到它们的地方——不能让它们变成看不见也停不掉的幽灵）。
4. **一个轮询器**。计数和列表读同一个 `jobsStore`：两个独立轮询会把请求翻倍，还会在
   停止请求在飞的时候给出两个不同的"在跑什么"。轮询只在**有运行中的作业**时进行，
   而"刚起了一个作业"由聊天流里的 `tool_result`（工具名匹配 bash_* 作业工具）触发刷新 ——
   这正是没有轮询可依赖的那一刻。

## Why

- **状态要出现在它产生的地方。** 用户看 dev server 的日志时，眼睛在对话上；让他为了
  停止一个进程而离开对话，是让工具"存在但不好用"。
- **一个会话可能起多个进程，两个会话可能共用一个工作区。** 只按工作区列（旧行为）会让
  会话 A 以为会话 B 的进程是自己起的；只按会话列又会让 B 的进程在控制台里彻底消失。
  两个分组同时给，并把"别的会话"说清楚。
- **删除设置里的那一栏不会丢东西。** 抽屉里的第二个分组就是原先那一栏能看到的全部
  （同一进程的作业），只是现在有了归属说明。

## What Changes

### ADDED

1. `jobs.Spec.Scope` / `jobs.Job.Scope`（JSON `scope`），随作业快照返回。
2. `builtin.BackgroundPolicy.Scope func(ctx context.Context) string`：由 `cmd/huan-agent`
   注入 `chat.ScopeFrom`；nil 或返回空串都表示"无会话"（纯 CLI 场景）。
3. `GET /api/jobs?session=<id>`：多返回一个 `scope` 字段，即该会话的作业所记录的 scope key。
   session id → scope key 的翻译只存在于服务端，浏览器不需要知道 scope 的存在。
4. `web/src/jobsStore.js`：计数与列表共用的唯一轮询器（含 `noteJobsToolRan`）。
5. `web/src/components/JobsDrawer.vue`：会话的作业抽屉（本会话 / 同工作区其他会话两组，
   日志跟随、停止、删除记录）。取代 `JobsPanel.vue`。

### REMOVED

- 设置 → 后台进程（`SETTINGS_TABS` 的 `jobs`、`SettingsView` 的映射与说明、`JobsPanel.vue`）。

### 不变

- 工具层的语义：`bash_jobs` 仍然只列**本工作区**的作业，跨工作区不可见也不可管理。
  抽屉额外显示同工作区其他会话的作业，是因为控制台能看到全进程，而模型不该看到。
- 作业的生命周期：随 agent 进程退出而终止。

## Impact

- `internal/jobs`：`Spec` / `Job` 各加一个字段。
- `internal/tool/builtin`：policy 增加 scope 读取器，启动时写入。
- `internal/server`：`/api/jobs` 支持 `?session=`。
- `cmd/huan-agent`：`bashPolicy` 之外，背景工具 policy 也接上 `chat.ScopeFrom`。
- `web/`：`jobsStore.js`、`JobsDrawer.vue`、`ChatView` 头部 chip + 抽屉、`chatStore` 的
  工具结果钩子；`SettingsView` / `state.js` 去掉 jobs 子页；`JobsPanel.vue` 删除。
- 文档：`docs/tools.md`、`docs/long-tasks.md`、`docs/admin.md` 里"后台进程在哪"的说法。
