# Phase 16 任务清单

## 1. `internal/store`：步骤列

- [x] migration 13：`ALTER TABLE chat_messages ADD COLUMN steps TEXT NOT NULL DEFAULT ''`
- [x] `ChatMessage.Steps`（JSON 字符串，`json:"steps,omitempty"`）
- [x] `AppendChatMessage` / `ListChatMessages` 读写 `steps`
- [x] store 测试：steps 往返、无关行读回空串

## 2. `internal/chat`：步骤是消息的一部分

- [x] `ToolRun.Step`（1 起，0 表示未知）
- [x] `Step` 结构 + `Result.Plan []Step`（不叫 `Steps`，那个字段是步数计数）
- [x] `EventStepEnd`（`step_end`）事件类型与文档
- [x] runner：一步有工具时发 `step_end` 并记入 `Plan`；最后一步的文字是答案
- [x] runner 测试：多步的 `Plan` 形状、`step_end` 的位置与内容、直接回答不发 `step_end`

## 3. `internal/server`：落库

- [x] `turnAccumulator` 按 `step` 归并事件（step_start / reasoning / text / tool_call / tool_result）
- [x] `turnSummary` + `summary()`，`persistTurn` 写 `steps` 列（reasoning / tool_calls 照写）
- [x] 被停止 / 失败的轮次保留已完成步骤（runner 的 Plan 缺席时用事件累积兜底）
- [x] server 测试：流式一轮后 `steps` 列的形状、`step_end` 事件、半途中止的一轮

## 4. `web`：按步骤渲染

- [x] `src/steps.js`：持久化 JSON → 渲染模型；空值从 `tool_calls` + `reasoning` 退化重建
- [x] chatStore：`step_start` / `step_end` 开步收步，事件按步归并；答案取最后一步的文字
- [x] ChatMessage.vue：步骤块（摘要行 + 展开的思考与工具卡片）在答案之上，答案独占正文
- [x] 折叠规则：流式中展开、答案落地自动折叠未手动操作过的步骤、挂载时即生效（attach 到已在跑的一轮）
- [x] styles.css：步骤块、摘要行、顺序线的样式（亮/暗两套，只用 token）
- [x] `ssr-probe`：步骤渲染的断言（折叠 / 展开 / 退化 / ask_user 过滤），并把 MarkdownText 换成探针桩

## 5. 文档

- [x] `docs/admin.md`：端点表与实际一致、`step_end`、`steps` 列、按步骤展示的行为
- [x] `web/README.md`：气泡的渲染结构、`src/steps.js` 的角色、探针为什么替换 MarkdownText、浏览器验证怎么跑

## 6. 验证

- [x] `go test ./...`
- [x] `cd web && npm run check:ui`（ALL PROBES PASSED）
- [x] `cd web && npm run build`
- [x] `bash web/.verify/step-run.sh`：真实服务端 + 脚本化模型 + 无头 Chrome，24 项流程断言 + 21 项布局断言全绿

## 7. 遗留

- [ ] `openspec validate`（本机没有 openspec CLI，change 目录按既有 phase 的格式手写）
- [ ] 归档：把 `openspec/changes/phase-16-turn-steps/` 归入 specs
