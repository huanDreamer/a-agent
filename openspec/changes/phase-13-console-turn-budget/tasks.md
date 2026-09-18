# Phase 13 任务清单

## 1. `internal/store`：设置表

- [x] 迁移 12：`app_settings(key TEXT PRIMARY KEY, value TEXT, updated_at TIMESTAMP)`
- [x] `TurnBudgetOverride{ MaxSteps, MaxTokens, DeadlineSecs *int }`：nil = 该键不存在
- [x] `GetTurnBudgetOverride`：三个键一次读回；非整数值按「无覆盖」处理
- [x] `SetTurnBudgetOverride`：单事务内 upsert / delete，nil 字段即 DELETE
- [x] `Store` 接口补两个方法
- [x] 测试：未写过时三个字段都是 nil、往返读写（含显式 0）、单字段清除不影响其它字段、非整数值被忽略

## 2. `internal/server`：唯一解析点 + 接口

- [x] `effectiveBudget(ctx)`：逐字段合并 config 与覆盖；读失败 warn 并降级；`max_steps <= 0` 归一到 `chat.DefaultMaxSteps`
- [x] `TurnBudgetStore` 小接口 + 类型断言（store 未实现时按 config 走）
- [x] `GET /api/chat/budget`：生效值 / 来源 / 默认值 / 上限 / `context_max_tokens` / `context_requires_restart`
- [x] `PUT /api/chat/budget`：稀疏写入、`reset_all`、越界 400、写后 `runnerCache.reset()`、info 日志
- [x] 上限常量 `MaxConsoleSteps`（= `chat.MaxStepsCeiling`）/ `MaxConsoleTurnTokens` / `MaxConsoleTurnDeadline`
- [x] `handleSendMessage`：预算在组装 `chat.Request` 时解析一次，随本轮传入
- [x] `runnerFor`：`chat.Config` 不再带预算（避免缓存 runner 吃旧数字）
- [x] `handleChatModels` 改用 `effectiveBudget`；删掉 `chatMaxSteps` / `chatMaxTokens` / `chatTurnDeadline`
- [x] `server.Config`：`ChatMaxSteps/ChatMaxTokens/ChatTurnDeadline` → `DefaultChat*`，新增 `ContextMaxTokens`
- [x] `registerBudgetRoutes` 接进 `registerRoutes`
- [x] 测试：鉴权、初始值来自 config、单字段覆盖后另一字段来源仍为 config、越界全部 400 且不留痕、`reset_all` 回到 config、**改预算后下一轮真的按新上限停**（含 `/chat/models` 同步）、token 维度按 provider usage 收口、store 读失败降级、nil store 仍可用 config

## 3. `cmd/huan-agent`

- [x] `admin.go` 注入 `DefaultChatMaxSteps` / `DefaultChatMaxTokens` / `DefaultChatTurnDeadline` / `ContextMaxTokens`

## 4. `web`：设置 → 对话预算

- [x] `src/budget.js`：`parseBudgetField` / `parseBudgetForm` / `budgetForm` / `defaultForm` / `formatSeconds` / `formatBudget` / `sourceLabel` / `BUDGET_LIMITS`
- [x] `src/components/BudgetPanel.vue`：三行输入 + 来源标签 + 常用档、保存 / 撤销修改 / 恢复默认、逐字段错误、成功与失败提示、压缩关闭告警
- [x] `src/chatStore.js`：`budget` 状态 + `loadBudget` / `saveBudget` / `resetBudget`（保存后刷新 catalog）
- [x] `src/api.js`：`chatBudget` / `saveChatBudget`
- [x] `src/state.js` + `SettingsView.vue`：新增「对话预算」子标签（含注释说明它为什么与其它面板不同）
- [x] `scripts/check-budget.mjs` 接进 `npm run check:ui`
- [x] `ssr-probe`：渲染面板并断言生效值、来源标签、`不限`、压缩开启/关闭两种提示
- [x] 重新构建 `internal/server/webui/dist`（嵌入包）

## 5. 配置与注释

- [x] `configs/config.example.yaml`：`chat.max_steps` 60 / `turn_max_tokens` 400000 / `turn_deadline_seconds` 10800 / `context.max_tokens` 60000 / `keep_recent` 12，并注明这些是启动默认值、可被控制台覆盖
- [x] `configs/config.yaml`（本机）：同上，并补 `chat:` 段（原先整段缺失，落到内置默认 12）
- [x] 修掉 `agent.max_steps` 的过时注释（「capped to 25 internally」→ `MaxStepsCeiling = 200`）

## 6. 验证

- [x] `go build ./...`、`go vet ./...`、`gofmt -l` 干净
- [x] `go test ./...` 全绿
- [x] `cd web && npm run check:ui` 全绿
- [x] 重启服务后实测：`/api/chat/models` 与 `/api/chat/budget` 报 60 / 400000 / 10800，`in_turn_compression: true`
- [x] 实测 PUT `max_steps=100` 后 `/api/chat/models` 立即变 100，`max_steps=0` 返回 400 且不留痕，`reset_all` 回到 60
