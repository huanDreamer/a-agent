# Phase 4 — Feishu (Lark) IM 任务清单

> openspec capabilities: `platform-feishu`

## 1. 依赖接入
- [x] 新增 `github.com/larksuite/oapi-sdk-go/v3@v3.11.0`（含 ws 依赖 gogo/protobuf、gorilla/websocket）
- [x] `go mod tidy` 收敛 go.sum

## 2. IM 适配层 `internal/platform/feishu`
- [x] `inbound.go` — `Inbound` 归一化 + `parseInboundMessage`（open_id / chat_id / message_id / text / msg_type）
- [x] `sender.go` — `Sender` 接口（ReplyText / ReplyCard 交互卡片）、`SendMessage` 可注入、`messageCreator` 隐藏 SDK 未导出类型
- [x] `bot.go` — `NewApp` WebSocket 长连接传输 + `Handler` / `HandlerFunc` + `RealSender`

## 3. serve 命令 `cmd/huan-agent/serve.go`
- [x] 每个 `open_id` 独立 session（`sessionMemory`：短期/长期 + 上下文压缩）
- [x] 模型支持 tool calling 时走 `agent.Agent`，否则 `BaseChatModel.Generate`
- [x] 指令 `/reset` `/remember` `/recall` `/provider`
- [x] main.go 精简：`serveCmd`/`runServe` 仅存于 serve.go

## 4. 配置
- [x] `FeishuConfig`（app_id / app_secret / domain / verification_token / encrypt_key），空 app_id 禁用
- [x] `config.example.yaml` 注释示例 + env 覆盖 `HUAN_FEISHU_*`

## 5. 测试（mock，无需真实 credentials）
- [x] `feishu_test.go` — inbound 解析、text/card 负载、错误传播、transport 构造
- [x] 覆盖率目标 ≥70% → **84.0%**

## 6. Spec / 文档
- [x] openspec proposal（`openspec/changes/phase-4-platform-feishu/proposal.md`）
- [x] capability spec（`specs/platform-feishu/spec.md`）
- [x] 手动配对文档（`docs/feishu.md`）

## 7. 质量门禁
- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test ./...`
- [x] Conventional Commit：`feat(feishu): add Feishu (Lark) IM bot integration via WebSocket`