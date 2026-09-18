# Phase 22b — 任务清单

约定：`[x]` 完成，`[~]` 做了但方式不同（附理由），`[ ]` 未完成。

## 1. 记录（`internal/subagent/tracker.go`）

- [x] `Run`：ID / Session / Name / Prompt（有界）/ ParentToolCallID / Status / StartedAt /
      EndedAt / DurationMs / Steps / Tokens / Error / StopReason
- [x] `Tracker`：`Start` / `Finish` / `List`（新→旧）/ `Running` / `RunningAll`，并发安全
- [x] **运行中的记录永不淘汰**；已结束的按会话保留最近 `DefaultTrackerKeep`（20）条
- [x] `List` 返回副本：调用方改不动记录（否则 header 与 drawer 会互相污染）
- [x] 运行中的记录返回**实时**耗时，客户端不必自己算
- [x] `StepsUnknown = -1`，且 `Finish` 在"失败且未报步数"时保留哨兵值而不是接受 0
- [x] `Agent` 持有 tracker（`NewWithTracker`），`Spawn` 开始前登记、结束时结算
- [x] 测试 8 个（含 `-race`）：起止、实时耗时、运行中不被淘汰、已结束有界、按会话隔离、
      并发读写、任务摘要截断、nil tracker 不 panic

## 2. 并行扇出

- [x] `Agent.SpawnMany(ctx, opts)`：并发跑多个，按入参顺序返回报告；**单个失败不取消兄弟**
- [x] 走同一个全进程闸门（`max_concurrent`），所以"并行"不等于"无界"
- [x] `spawn_agent` 新增 `tasks: [...]`（与 `prompt` 互斥，两者都给则明确报错）
- [x] 每份报告都有独立条目（`reports[]`：task / report / steps / tools / tokens / failed），
      合并文本按任务编号分段，避免报告串味
- [x] 工具描述写明两种并行方式与适用场景
- [x] CLI 与飞书因此也能并行（扇出在工具内部，不依赖 Eino 循环的串行调度）

## 3. 服务端

- [x] `ChatDeps.Subagents`（`SubagentTracker` 接口）+ `SubagentMaxConcurrent`
- [x] `GET /chat/sessions/:id/subagents`；未启用时**不注册**
- [x] 会话不存在 → 404（而不是空列表，否则客户端 bug 看起来像"没派过"）
- [x] 一个进程一个 tracker（两个 spawner 共用），所以 header 看到的是**所有** surface 的派生
- [x] 测试 4 个：未启用 404、正常运行中/已结束与顺序、按会话隔离、未知会话 404

## 4. Web

- [x] `api.listSubagents`
- [x] `subagentsStore.js`：照 `jobsStore`（运行中才轮询、切换会话即清空、404 → 视为未启用）
- [x] `subagentsChip.js`：chip 措辞单独成模块（可测）
- [x] `ChatView.vue`：顶部 chip（仅在有东西可数时出现，`ok` 色调表示有运行中的）
- [x] `SubagentsDrawer.vue`：正在跑 / 已结束两组；显示状态、步数（不可得则明说）、token、耗时、
      失败原因、因预算提前结束的原因；**不提供停止按钮**（子 agent 随父轮次停止，给了也按不动）
- [x] 排队提示：`running_all > max_concurrent` 时说明还有几个在等
- [x] ssr-probe 新增 12 条断言：空态、运行中/已结束分组、任务文本、步数与 token、**"不可得"不显示 0**、
      已知的 0 仍然显示、失败原因、排队与上限、不提供停止、chip 的措辞与稀有性
- [x] `npm run check:ui` 全绿（**222 PASS**）；`npm run build` 重建 dist

## 5. 验证

- [x] `go test ./... -count=1` 全绿；`go vet ./...` 干净；`make build` 通过
- [x] `go test -race ./internal/subagent/` 通过（列表被并发读写）
- [x] 端到端（真模型 + `run`）：一次 `tasks` 调用派 3 个子 agent，9 秒跑完，
      0 工具失败，三个数字（7 / 5 / 6）与磁盘实际一致
- [x] **端到端（真控制台 + 真模型）**：登录后发一轮要求并行派 3 个子 agent，边跑边轮询
      `/subagents`，观察到：
      `running=2 total=2 max=2`（前两个起跑，第三个排队）→ `running=2 total=3`（第三个拿到槽位，
      同时第一个已完成 ok/2 步）→ `running=0 total=3`（三个全 ok，各 2 步，耗时 4.0–5.8s）
      —— 这同时证明了状态可见**与**闸门生效

## 6. 实施中抓到的真实问题

**问题 1：失败的运行把"步数不可知"覆盖成了 0。** `Spawn` 的失败分支调用
`Finish{Failed: true, Error: ...}` 时没有给 `Steps`，于是 Go 的零值 0 覆盖了 `Run` 初始的 `-1`
哨兵 —— 一个干了一分钟才失败的子 agent，在状态栏上显示"0 步"。是**我自己写的 probe 断言**
（"不可得时不得出现 0 步"）把它抓出来的。修法：失败分支显式传 `StepsUnknown`，并让 `Finish`
在"失败且未报步数"时保留哨兵值。

**问题 2（第 5 节顺带发现）：`run --output json` 里的嵌套事件之前只有 7 条而嵌套工具调用 4 个** ——
这是我核对并行证据时的观察，不是缺陷：扇出时三条子 agent 的事件都挂在**同一个**父调用下（这正是
设计：卡片只有一张）。
