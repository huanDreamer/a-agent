# Phase 12 任务清单

## 1. `internal/tool`：提问的词汇表

- [x] `Option` / `Question` / `Answer` + `Answer.Status` 常量（`answered` / `timeout` / `cancelled`）
- [x] `Asker` 接口、`WithAsker` / `AskerFrom`（空 Asker 视为没有，不制造"半可用"状态）
- [x] 测试：注入/读取、未注入时 `ok=false`、nil ctx 不 panic

## 2. `internal/tool/builtin`：`ask_user`

- [x] `NewAskUserTool()`（`utils.InferTool`，参数结构带 jsonschema 描述）
- [x] 校验：question 非空、选项 ≤ 8、label 非空且不重复、既无选项又禁自定义 → 拒绝
- [x] 无 Asker → 明确错误（告诉模型当前通道不能提问）
- [x] 结果结构 `{status, question, selected, text, answer, note}`，`answer` 归一
- [x] 工具描述写明：什么时候该问（需要人做选择 / 补充只有人知道的信息）、什么时候不该问
      （能自己查证的、纯确认的、一次问一堆的）
- [x] 测试：正常回答、多选、自定义输入、超时 note、取消、三种校验失败、无 Asker、选项去重
      与数量上限

## 3. `internal/chat`：事件

- [x] `EventAsk` + `Event.Ask / AskID / AskStatus / AskAnswer`
- [x] 测试：两种形态的 JSON 形状（前端依赖它）

## 4. `internal/server`：挂起与应答

- [x] `questionHub`：注册 / 取回 / 按 id 应答 / 到期清理（并发安全）
- [x] `turnAsker`：分配 id → 推 `ask_user(pending)` → 等答案 / 超时 / ctx 取消 → 推结局事件
- [x] `POST /api/chat/sessions/{id}/questions/{qid}/answer` + 全部校验分支
- [x] `ChatDeps.AskTimeout`，`handleSendMessage` 把 asker 注入本轮 ctx
- [x] 日志：等待时长、结局、问题 id、session
- [x] 测试：hub 单测（应答 / 超时 / 取消 / 重复应答 404 / 跨会话 404 / 非法选项 400）；
      端到端（脚本模型发起 `ask_user` → 读到事件 → POST 答案 → 同一轮继续并给出最终回答 →
      助手消息里同时落盘问题与答案）；超时路径

## 5. 接线与配置

- [x] `chat.ask_user_timeout_seconds`（默认 600，0 = 不限）+ `AskUserTimeout()`
- [x] `configs/config.example.yaml` 同步
- [x] `cmd/huan-agent/admin.go`：`Surface == "web"` 时注册 `ask_user`；传入 `AskTimeout`
- [x] `cmd/huan-agent` 测试：web 注册、feishu/cli 不注册

## 6. Web 控制台

- [x] `src/ask.js`：`askFromEvent` / `askFromTool` / `askFromItem`（实时与历史归一）
- [x] `src/components/AskUserCard.vue`：pending / submitting / answered / timeout / history
- [x] `chatStore`：`ask_user` 事件、`turn.asks`、`submitAsk()`、`pendingAsk` 派生
- [x] `api.js`：`answerQuestion(sessionId, questionId, body)`
- [x] `ChatMessage.vue`：渲染卡片、把 `ask_user` 从工具卡片里排除
- [x] `styles.css`：卡片样式（只用设计令牌）
- [x] `ssr-probe`：`renderAskUserCard` + 断言（未选中时提交禁用、选项渲染、已提交锁定、
      超时文案、单选切自定义清空选择），以及 `captureAnswerRequest`（答案请求的路径与请求体，
      前端 URL 与服务端路由的唯一交叉验证）

## 7. 校验

- [x] `gofmt` / `go build ./...` / `go vet ./...` / `go test ./...` / `golangci-lint run`
- [x] `cd web && npm run check:ui`
- [x] `cmd/huan-agent/admin.go` / `workspace_tools.go` 的 `toolSetOptions.Surface` 注释同步
      （说明 surface 现在也用于「只在一个 surface 注册」的工具）

## 8. 文档

- [x] `docs/admin.md`：卡片怎么用、配置项、等待计入墙钟预算、刷新即作废
- [x] `web/README.md`：接口表新增应答接口、SSE 说明补 `ask_user`、目录结构补组件
