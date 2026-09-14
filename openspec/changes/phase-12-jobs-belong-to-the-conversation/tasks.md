# Phase 12 任务清单

## 1. `internal/jobs`：作业记住它的会话

- [x] `Spec.Scope` + `Job.Scope`（JSON `scope`），随快照返回
- [x] 文档写明"无 scope"是合法状态（CLI 等无会话的调用方）
- [x] 既有测试不变（新字段不参与校验、不影响生命周期）

## 2. `internal/tool/builtin`：policy 注入 scope 读取器

- [x] `BackgroundPolicy.Scope func(ctx) string`，`backgroundConfig.scopeOf(ctx)` 统一空值语义
- [x] `bash_background` 启动时写入 `Scope`
- [x] 测试：注入的读取器被调用并落进作业；没有读取器时留下空 scope（不拒绝调用）
- [x] `bash_jobs` 的语义保持不变（只列本工作区）——描述与实现都没动

## 3. `internal/server`：按会话分组

- [x] `GET /api/jobs?session=<id>` 增加 `scope` 字段（服务端做 session id → scope key 的翻译）
- [x] 不带 `session` 时响应里没有 `scope` 键
- [x] 测试：scope 正确、两个会话的作业都在列表里且各自带自己的 scope、无 session 时无该键

## 4. `cmd/huan-agent`：接线

- [x] `workspace_tools.go` 的背景工具 policy 传 `chat.ScopeFrom`

## 5. 控制台

- [x] `web/src/jobsStore.js`：唯一的作业列表状态与轮询器，`sessionJobs` / `otherJobs` 派生
- [x] 轮询条件：存在运行中的作业；`noteJobsToolRan` 在聊天流的作业工具结果上触发刷新
- [x] 全局 刷新（`state.refreshToken`）也会重读列表
- [x] `JobsDrawer.vue`：右侧抽屉，本会话 / 同工作区其他会话两组，日志跟随 + 停止 + 删除记录
- [x] `ChatView` 头部 chip：三种文案（运行中 / 记录 / 同工作区），无相关内容时不渲染
- [x] `SettingsView` + `state.js` 去掉 jobs 子页；`JobsPanel.vue` 删除
- [x] 新图标 `terminal`；`.chip-btn` / `.jobs-drawer` / `.job-card` 等样式沿用既有变量
- [x] `vite build` + `npm run check:ui` 通过，dist 重建
- [x] 窄屏（≤900px）抽屉占满宽度并带遮罩；宽屏不遮罩（挡住对话就没有意义了）

## 6. 文档

- [x] `docs/tools.md`：后台进程那一节改成"计数在会话标题栏、列表在右侧抽屉"
- [x] `docs/long-tasks.md`：长任务与常驻进程的关系、去哪里看
- [x] `docs/admin.md`：后台进程的位置变化
- [x] 本 change 的 proposal + spec + tasks

## 7. 质量门禁

- [x] `gofmt` / `go build ./...` / `go vet ./...` / `go test ./...` 全过
- [x] 真实端到端（临时 DB + 临时工作区，deepseek）：让模型用 `bash_background` 起
      `python3 -m http.server 8123`，然后
      `GET /api/jobs?session=<id>` → `scope: web:<id>`、`running: 1`、
      作业带 `scope`/`workspace`/`requested_by`；不带 `session` 的请求没有 `scope` 键。
