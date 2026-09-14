# Phase 5c — 任务清单

> 目标：链路追踪不再依赖外部 Langfuse，trace 落本地 SQLite，控制台直接看。
> Langfuse 降级为**可选单向镜像**（只写不读），保留 eval / prompt 管理的出口。
>
> **交付状态**：第一刀（"能点进去"最小切片）已完成并验证——任务组 2、3、3b、6b 的部分
> 与 5、6 的部分已落地；实时（5b/6a）、保留策略、飞书接入（7）、配色与状态拆分仍未做。
> 已完成的条目勾选为 `[x]`，未做的保留 `[ ]`。

## 0. 最小切片：从对话点进 trace（已完成）

> 范围：trace 落本地库 + trace id 贯通到消息 + 两个入口（逐轮 / 会话级）。
> 不含实时、保留策略、镜像降级、飞书接入。

- [x] 迁移 v7：`traces` + `observations` + `chat_messages.trace_id`
- [x] `internal/store/trace.go`：写入、结束、列表（session/user/name 过滤 + 分页）、取详情
- [x] `internal/tracing`：`Recorder`（同时实现 `chat.Tracer` 与 `server.TraceReader`）+ `Multi` 扇出
- [x] `chat.Result.TraceID` 由 runner 传出（此前 traceID 只在 runner 内部存活）
- [x] `internal/server/chat.go` 把 trace id 落进助手消息；失败回合保留 trace id
- [x] `cmd/huan-agent/admin.go` 装配：本地 Recorder 优先，Langfuse 作为镜像
- [x] 前端：`state.focusTrace` 一次性交棒 + `openTrace()` / `claimTraceFocus()`
- [x] 前端：助手消息 `turn-usage` 行的逐轮入口（不受 `usage` 是否存在限制）
      —— 已用真实鼠标点击在浏览器中验证
- [x] 前端：对话头部 `ChatView.vue` 的会话级入口（预填 session 过滤）
- [x] 前端：**侧边会话列表**每行的 链路 动作（悬停显现，与重命名/清空/删除同组）
      —— 已用真实鼠标点击在浏览器中验证
- [x] 修复 `TraceView.vue` 缺失的 `JsonBlock` import（trace 级 input/output 此前一直空白）
- [x] 前端：`TraceView` 消费并清除 focus；缺失 trace 显示"已不在保留范围内"
- [x] 测试：端到端断言"一轮对话 → 消息带 trace_id → `/api/traces/{id}` 可解析 → 按 session 可列出"
- [x] 覆盖率：tracing 94.1% / store 85.6% / server 80.9%
- [x] 文档：`docs/admin.md` 链路追踪一节与排障条目改成本地存储口径

### 交付过程中发现并向用户暴露的问题

- **`TraceView.vue` 从来没有 import 过 `JsonBlock`。** 该文件的 trace 级 input/output
  两块自 Phase 5b 起就一直是**空白**——`<JsonBlock>` 被当成未解析的原生标签渲染成
  `<jsonblock>`，而生产构建把 Vue 的 "Failed to resolve component" 警告剥掉了，
  所以控制台一声不响、没人发现。是浏览器验证抓到的（DOM 里出现 `<jsonblock>`）。
  已修（补 import），并用 `web/.verify/probe-imports.mjs` 全量扫描确认仅此一处。
  副作用：本次"input 不截断"的改动原先对这两块根本没有作用对象。
- **`chat.Event.MessageID` 是死字段**（见下）。

### 交付过程中修正的假设与取舍

- **`done` 帧不需要携带 trace id。** 原任务写的是"`done` 事件带 `trace_id`"，但
  `chatStore.js` 在一轮结束后会做一次**权威重载**（`selectSession` →
  `buildItems`），消息完全由 API 重建。所以只需要让消息接口返回 `trace_id`，
  流式帧不必承担这件事——少一处需要保持同步的状态。
- **`chat.Event.MessageID` 是死字段。** 它声明为"set on done"，但全仓库从未被赋值
  （服务端在 runner 之外落库，拿不到消息 id）。这一刀没有动它，但它是个陷阱：
  想给前端传 id 的人会先看到它。建议后续单独清理。
- **`internal/langfuse` 的共享类型没有搬。** 引用点 222 处（110 处在它自己的测试里），
  纯机械重命名不该混进功能改动。代价是 `internal/tracing` 临时 import
  `internal/langfuse` 取词汇，已在包注释里标注。见任务组 1。

### 浏览器验证（真实 Chrome + 真实服务端）

`web/.verify/tr-run.sh`（沿用项目既有的 `.verify` 本地 harness，**该目录被 gitignore**）：
构建 UI 与 Go 二进制 → 起真实 admin server（**不配任何 langfuse**）→ 把两条带 trace 的
会话（含一个 >5KB 的 prompt）灌进 SQLite → 无头 Chrome over CDP 用**真实鼠标事件**跑两个
phase。结果：24 项检查全过、0 失败，无 console 错误。

- `sidebar`：会话列表行的 链路 动作（4 个行操作里的第一个，悬停显现）→ 真点击 →
  落在链路追踪、session 过滤预填 `sess-tr-a`、列表恰好 1 行。
- `message`：回答的 turn footer 里的 链路 按钮 → 真点击 → 加载 `tr-a`；
  trace input 5026 字符、**无「已截断」**、尾部标记 `ZZTAILZZ` 在；展开瀑布行后
  observation input 5043 字符同样完整。

`layout` phase 专门量本轮的行内布局（回答 footer 的 链路 按钮）：

```
footer items: prompt 1.4K | completion 22 | 合计 1.4K tokens | 耗时 2.3s | →→→ 链路
row box:      left 336  right 1208  lines 1
button box:   left 1145 right 1208  cy 246.09
耗时 box:     left 613.75 right 665.45 cy 246.09
```
- 按钮右边缘 = 行右边缘，gap **0.00px**（贴住最右）
- 按钮距 耗时 **479.55px**（此前是 12px 紧跟最后一个数字）
- 垂直中心与 耗时 差 **0.00px**，与行内每个数字都差 0.00px
- 左侧数字未位移（首项 left 336 = 行 left 336）
- 视口收窄到 420px、行折成 2 行后，按钮仍在最右（gap 0.00px）且不溢出

回答 footer 的收尾调整（同一 phase 内一并量了）：

- `助手` 标签移除；`.msg-head` 只在**流式进行中**渲染（承载"生成中"），回答结束后整个 head 不再存在
- `复制` 从 head 移到 footer，与 `链路` 同组（`.turn-actions`）：两者相距 **6.00px**、中心线 **Δcy 0.00px**、
  整组贴右 **gap 0.00px**、与行内数字中心对齐 **worst Δ 0.00px**
- 复制功能实测：点击后剪贴板内容 = 回答原文，按钮变为"已复制"
- **footer 的渲染条件加了 `item.text`**：它原本是 `usage || traceId`，把复制按钮挪进来之后，
  一个"有正文、无用量、无 trace"的回合（迁移前写入的行就是这样）会丢掉复制按钮。已用
  这种形状的 fixture 断言守住。
- `.msg-role` 的 CSS 一并删除（不再有使用者）

排障过程中记下的五条经验，都写进了 harness：

- **必须显式设视口**（`cdp.viewport(1280, 800)`）。小于 900px 时侧边栏变成 off-canvas
  抽屉，行操作被排到画布外（实测按钮 x = -129），真鼠标点不到，而 `visibility`/`opacity`
  仍是 `visible`/`1`，所以"可见性"断言会假通过。
- **悬停显现的按钮不能在同一个 tick 里点**：`mouseMoved` 与 `mousePressed` 同一帧发出时，
  press 落在元素变为可命中的那一帧之前。
- 兜底逻辑绝不能替代真点击：一旦"真点击失败就用合成 `click()` 重试"，真路径坏掉时脚本
  依然全绿。已移除该兜底，诊断信息只报告原因。
- **注入到 `cdp.eval` 的探针是模板字面量**：探针代码里出现反引号（哪怕在注释里）会截断
  外层模板、导致语法错误，而错误要等三次浏览器尝试跑完才暴露。已在 `tr-run.sh` 里加
  `node --check` 前置检查。
- **探针不要按位置点按钮**：footer 里按钮的先后顺序会变（复制被挪进来就把"第一个按钮"从
  链路换成了复制）。一律按 `title` 选，否则一次布局调整就会让测试点错对象、报出假失败。
- **`SKIP_WEB_BUILD=1` 只在产物比 `web/src` 新时才允许跑**：验证一个旧产物却宣称验证了当前
  代码，比不验证更糟。该守卫在本次交付中真的触发过（工作区当时有更新的源码）。

## 1. 抽包：观测词汇中立化

- [ ] 新建 `internal/tracing`，把 `TraceSummary` / `Observation` / `TraceDetail` /
      `TraceFilter` / `Usage` / `Stats` / `ErrDisabled` 从 `internal/langfuse` 迁入
      （**当前为临时状态**：`internal/tracing` 反向 import `internal/langfuse` 取类型）
- [ ] 迁移时保留宽容解码（camelCase 与 snake_case 双写、数字字符串、缺字段不报错），
      镜像的导出侧仍然需要它
- [ ] `internal/langfuse` 改为 import `internal/tracing`，只留 ingestion 客户端 +
      `chat.Tracer` 适配器；删掉读客户端（`ListTraces` / `GetTrace`）
- [ ] 更新外部引用（仅 3 个文件：`cmd/huan-agent/admin.go`、`internal/server/traces.go`、
      `internal/server/traces_test.go`）
- [ ] 确认 `web/src/components/TraceView.vue` 的消费字段不变（snake_case 契约不动）

## 2. 本地存储

- [x] 迁移 v7：`traces`（id / name / session_id / user_id / input / output /
      started_at / ended_at）与 `observations`（id / trace_id / parent_id / type /
      name / model / step / input / output / level / status_message /
      prompt_tokens / completion_tokens / total_tokens / started_at / ended_at）
- [x] 迁移 v7 同时加 `chat_messages.trace_id TEXT NOT NULL DEFAULT ''`（同一次 change，
      避免为了一列再开一个版本）；历史消息为空串 = "这条消息没有 trace"
- [x] `observations.trace_id` 外键 → `traces.id` ON DELETE CASCADE；`parent_id` 无父时为
      trace id（与现有 langfuse tracer 的 `ParentObservationID: traceID` 语义一致）
- [x] 索引：`traces(started_at DESC)`、`traces(session_id)`、`traces(user_id)`、
      `observations(trace_id, started_at)`
- [x] `store.RecordTrace` / `RecordObservation` / `EndTrace` / `EndObservation`
- [x] `store.ListTraces(filter)`：按时间倒序、支持 session / user / name 过滤与 limit/offset，
      返回 `TraceSummary`（含 latency；usage 合计按需 join 聚合）
- [x] `store.GetTrace(id)`：一次取回 trace + 其 observations，拼成 `TraceDetail` 树
- [ ] `store.PruneTraces(policy)`：按 age 与 count 双阈值，**分批 DELETE + 批间让出连接**，
      返回删除条数 —— **未做，两张表目前无界增长**（切片刻意不带配置，见组 4）
- [ ] `store.ClearTraces`：显式删除 observations 再删 traces（不依赖 pragma）—— 未做
- [x] `store.ChatMessage` 增 `TraceID`，`AppendChatMessage` 落库，会话消息接口原样带出
- [x] 覆盖率 ≥ 70%

## 3. 写入路径（Recorder）

- [x] `tracing.Recorder` 实现 `chat.Tracer`：`StartTrace` 返回落库后的 trace id，
      `StartGeneration` / `StartSpan` 落 observation 起始行
- [x] `EndTrace` / `EndGeneration` / `EndSpan` 更新结束时间、输出、usage、level、status_message
- [x] **同步写**（不引入缓冲队列）：本地写无网络延迟，缓冲只会多一类"trace 丢失"缺陷
- [x] 每次写带**短超时**（`SetMaxOpenConns(1)` 是单写连接，写可能排在别的语句后面）；
      失败仅累计计数 + 打日志，绝不向会话传播
- [x] 终止类写入（End*）使用**脱离请求的 context**（与"客户端断开仍持久化已产出的答案"一致）
- [x] usage 换算：`chat.Usage`（prompt / completion / total / duration_ms）→ observation 列
- [x] `tracing.Multi` 扇出：本地优先，镜像失败不影响本地、不影响会话
- [x] 禁用时（`tracing.enable: false`）整体退化为 `NopTracer`，不启协程、不做 IO
- [ ] `tracing.Hub`：有界订阅者集合；每次写入/关闭后广播
      `Change{TraceID, Kind, At}`
- [ ] 发布必须**非阻塞**：订阅者慢、缓冲满或已取消 → 直接摘掉并计数，
      绝不阻塞写入或对话（否则观测会反噬被观测的系统）
- [ ] 无订阅者时不分配 channel、不起 goroutine（控制台没开时零成本）
- [ ] 通知只携带"哪个 trace 变了"，不携带观测载荷：控制台收到后重新读库，
      保持 snake_case 契约只有一份
- [x] 计数量：written / failed / dropped / pruned / subscribers-dropped
- [x] 覆盖率 ≥ 70%

## 3b. trace id 贯通（对话 → trace）

- [x] `chat.Result` 增 `TraceID`，`chat.Runner` 把已有的本地 `traceID` 传出
      （目前它只在 runner 内部存活，`Result` 与所有 `Event` 都不带）
- [x] ~~`done` 事件带 `trace_id`~~ **经查不需要**：前端在一轮结束后会做权威重载
      （`chatStore.js` → `selectSession` → `buildItems`），消息完全由 API 重建，流式帧
      不必承担这件事——少一处需要保持同步的状态。（顺带发现 `Event.MessageID` 是死字段：
      注释写着 set on done，全仓库从未赋值。）
- [x] `internal/server/chat.go` 把它写进助手消息（`ChatMessage.TraceID`）
- [x] `done` 帧与落库用同一个 id，且**先落库再回帧**，避免前端拿到一个查不到的 id
- [x] 无 trace 时（追踪关闭）留空，前端不渲染入口，而不是给一个点了没反应的按钮
- [x] 测试：一轮对话结束后，消息里的 trace_id 能在库里查到对应 trace

## 4. 配置

- [ ] `tracing.enable`（默认 **true**）、`tracing.retention_days`（默认 14，0 = 关闭按时间裁剪）、
      `tracing.max_traces`（默认 5000，0 = 关闭按条数裁剪）、`tracing.prune_interval`（默认 1h）
- [ ] 全部走 `viper.SetDefault`，禁止硬编码；补 `configs/config.example.yaml` 注释块
- [ ] `langfuse.*` 语义改为"镜像"，并校验：配了 host 但缺 key 时报 warn 而不是静默（沿用现有行为）
- [ ] 启动时跑一次裁剪；之后按 `prune_interval` 周期跑，随进程退出而停止
- [ ] 配置单测覆盖默认值与边界（负数 / 0 / 超大值）

## 5. 服务端

- [x] `server.TraceReader` 由本地 Recorder 提供；`/api/traces` 与 `/api/traces/{id}` 不再出网
- [ ] `GET /api/traces/status` 拆开两个事实：`backend`（local / disabled）、`mirror`（on/off/host）+ 计数
      —— 未做；加 `Backend()` 要改 `server.TraceReader` 接口并波及 558 行的 traces_test.go
- [ ] `GET /api/traces` 响应新增 `backend` 字段，其余字段形状不变 —— 未做
- [x] 三态可区分：未启用（`enabled:false` + message）/ 已启用但无数据（空数组 + `enabled:true`）/
      单条 trace 已不存在（**404 + 说明**，本次新加，与"后端故障 502"区分开）
- [ ] `DELETE /api/traces`：清空，返回删除条数；需登录态 —— 未做
- [ ] 镜像不可达时报 502 且措辞指向"镜像" —— 未做（镜像目前只写、无读路径；
      原读侧的 502 语义已换成"缺失 trace → 404"）
- [ ] `cmd/huan-agent/admin.go` 装配：Recorder + 可选镜像 → `Multi` → 同时喂 `Tracer` 与 `Traces`
- [ ] `server/traces_test.go` 改为对内存 store 断言 —— 未做（原有 fake reader 测试仍通过；
      新增的 `trace_link_test.go` 已用真实 store 覆盖读路径）
- [x] 覆盖率 ≥ 70%

## 5b. 实时推送

- [ ] `GET /api/traces/stream`：`text/event-stream`，订阅 `tracing.Hub`
- [ ] 首帧发一个 `ready`（让前端能区分"连上了但还没变化"和"连不上"），
      并带当前 `backend` / `mirror` 状态
- [ ] 心跳注释帧 `: ping`（沿用对话 SSE 的写法），否则反代会在空闲时掐掉连接
- [ ] 客户端断开或 `ctx` 取消时**必须**退订，否则 Hub 会积累僵尸订阅者
- [ ] 复用 `web/src/sse.js` 的 `createSseParser`（它已按 POST 流写就，`fetch` 读 GET 流同样适用）
- [ ] 前端不用 `EventSource`：会话端已经是 fetch + 手写帧解析，保持一份 SSE 实现、
      一套鉴权失败处理（`setUnauthorizedHandler`）
- [ ] 单测：订阅者在写入后收到通知；慢订阅者被摘掉且写入耗时不受影响
      （需要真实的时间上界断言，不是纯粹的 channel 计数）

## 6. 前端

- [ ] 状态条：区分"本地库 / 本地 + Langfuse 镜像"，展示 written / failed / pruned
      —— 未做区分；计数标签已从 Langfuse 口径的 `sent` 改成本地口径的 `written`
- [x] 空态拆分：未启用（给出 `tracing.enable` 提示）vs 已启用但还没有 trace
- [x] Langfuse 深链 chip 仅在 `mirror.host` 非空时出现，且不带任何密钥
- [ ] 保留策略可见：显示 retention_days / max_traces，并提供"清空追踪数据"（两步确认）
- [x] `TraceView.vue` 顶部注释里的 `CONFIG_SNIPPET`（现在还在教用户配 langfuse 四项）改掉

### 6a. 实时

- [ ] `useTraceStream`：共享一条订阅（面板与对话页共用，不要每处开一条连接）
- [ ] 列表：收到通知后**原地更新**受影响的那一行，不整表重刷、不抖动滚动位置
- [ ] 详情：正在跑的 trace 随观测出现而增长；观测结束时补上结束时间
- [ ] **进行中渲染**：没有 `end_time` 的观测画成"进行中"，不能画成 0 宽度的条；
      列表与详情头部都要能看出这条 trace 还在跑
- [ ] 部分瀑布图必须一眼可辨，不能被误读成完整的一次调用
- [ ] 连不上 / 服务端没有该端点 / 断流：明确显示"非实时"，并回落到手动刷新，
      绝不把旧数据当新数据展示
- [ ] 手动刷新在健康状态下也保留
- [ ] `TraceWaterfall.vue`：仅增加"进行中"这一种渲染，几何算法不动

### 6b. 对话 ↔ trace 互跳

- [x] `state` 增一次性字段 `focusTrace`（前端没有 router，导航就是 `tab` + `monitor`）
- [x] 助手消息带 `trace_id` 时渲染"链路"入口；没有则**不渲染**
- [x] 点击 → `tab='monitor'`、`monitor='traces'`、`focusTrace=<id>`；
      `TraceView` 激活时消费并**清除**该字段（否则之后手动进 tab 会被强制选中）
- [ ] trace 详情带 `session_id` 时渲染"回到对话"入口，点击跳到该会话 —— 未做
      （数据已就绪：trace 的 `session_id` 已在响应里，测试有断言）
- [x] 目标已被裁剪/清空：显示"该 trace 已不在保留范围内"这类说明，
      不能空白面板、不能无限转圈
- [ ] 从正在进行的对话里点进去，能直接看到这条 trace 的实时增长 —— 属 5d（实时）

## 7. CLI / 飞书路径接入

> 现状：只有 Web 对话会上报，`internal/agent`（eino ReAct，飞书与 CLI 主入口）完全没有
> tracer 调用点。只看得见网页对话的观测后端是半成品。
> 这一组如果膨胀，可拆成独立 change。

- [ ] `internal/agent` 接入 `chat.Tracer` 同一接缝：每轮一个 trace、每次模型调用一个 generation
- [ ] 工具调用记为 span，失败带 error level（与 Web 路径语义一致，便于对照）
- [ ] 带上 session id / user id（飞书 open_id 已有归属逻辑，复用现成字段）
- [ ] `cmd/huan-agent/serve.go`（飞书）与 `cmd/huan-agent/chat.go`（CLI）装配同一个 Recorder
- [ ] 断言两条路径产出的 trace 结构一致（同一个 `TraceDetail` 形状）

## 8. 文档与验收

- [ ] `docs/admin.md` 补链路追踪一节：数据在哪、保留多久、怎么清、Langfuse 何时还需要
- [ ] `docs/status.html` 更新阶段状态
- [ ] `openspec/specs/project.md` 的 Capabilities 表补 `local-tracing`
- [ ] `PLAN.md` 的观测章节同步（原文写的是 Langfuse-only）
- [ ] `make lint` 通过
- [ ] `make test` 通过，新增包覆盖率 ≥ 70%
- [ ] 手工验收：全新机器、不配任何 langfuse、只跑 Web 对话 → 链路追踪 tab 直接有数据
- [ ] 手工验收：把 SQLite 文件权限改成只读 → 会话仍能正常完成，只是 trace 写失败被计数
- [ ] 手工验收（实时）：打开链路追踪 tab 不动它，在飞书里发一条消息 →
      trace 自己出现，瀑布图随步骤逐步长出来，最后一个观测结束时补上结束时间
- [ ] 手工验收（实时）：跑一轮长对话，观察写入耗时没有被观测拖慢（对比关闭 tracing 的耗时）
- [ ] 手工验收（互跳）：对话里点"链路" → 直接落在对应 trace 上；
      再从 trace 点"回到对话" → 回到原会话且定位正确
- [ ] 手工验收（健壮性）：把 `retention_days` 调到 0 天并触发裁剪后，
      点一条旧消息的"链路"→ 显示"已不在保留范围内"，不是空白
