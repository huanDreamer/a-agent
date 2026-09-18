# Phase 15 任务清单

## 1. `internal/server`：turn hub（`turns.go`）

- [x] `turnHub`：`begin` / `retire` / `find` / `running` / `stop` / `shutdown`
- [x] `liveTurn`：事件日志、订阅者门铃、取消句柄、`stopRequested` 标记
- [x] 日志合并同类 delta（长答案的重放成本 = 答案大小）
- [x] 游标 `{index, offset}` + `since(cursor)`：既补增量，也补"合并后新增的尾巴"
- [x] `subscribe()` 在同一把锁里取快照并登记，事件不漏不重
- [x] `emit` 永不阻塞运行（门铃非阻塞发送，慢读者靠游标追上）
- [x] 一轮一会话：`begin` 拒绝第二次；结束的轮次保留 2 分钟供晚到的 attach
- [x] `turnAccumulator`：把事件折进待落库的消息（原 `accumulate` 逻辑搬过来，两个消费者共用）
- [x] `summary(res)`：runner 的 Result 权威，事件累积作兜底
- [x] `shutdown(ctx)`：取消飞行中的轮次并等落库，避免在 store 关闭后写库
- [x] 被停止的轮次落库为「本轮已被停止，回答不完整」（不看 runner 是否把它当成正常结束）

## 2. `internal/server`：端点

- [x] `POST /chat/sessions/:id/messages` → 202 JSON；已有轮次 409（检查在写用户消息之前）
- [x] 接受消息时 `TouchChatSession`（刷新后仍能选中正在生成的会话）
- [x] `GET /chat/sessions/:id/turn` → SSE；无轮次时 `{"streaming":false}`
- [x] `POST /chat/sessions/:id/turn/stop`
- [x] `streamLiveTurn`：重放 + 跟随 + 心跳 + 客户端断开即退出（轮次不受影响）
- [x] 先落库、后写 `stream_end`（客户端收到结束就去重载，那一行必须已经在了）
- [x] 会话列表 / 单条会话带 `streaming`（`sessionView` 内嵌，不动 store 结构）
- [x] 删除旧的 `streamTurn`（POST 即流的写法）

## 3. `web`：attach 客户端

- [x] `api.js`：`startChatTurn` / `attachChatTurn` / `stopChatTurn`，移除 `streamChatTurn`
- [x] chatStore：模块级 `attachment`（会话 + AbortController），与视图解耦
- [x] `selectSession`：只 detach，不再停止轮次；`session.streaming` 则自动 attach
- [x] `sendMessage`：先 POST，再 attach（不 await 整轮）；失败保留草稿并回滚气泡
- [x] 发送后刷新列表（侧边栏立刻出现「生成中」，并启动轮询）
- [x] 有轮次在生成时 5s 轮询列表；发现"当前会话在别处开始了"就 attach
- [x] `stopStreaming` 改为请求服务端停止
- [x] 侧边栏会话行的「生成中」标记（替换时间戳）
- [x] 409 的处理：提示 + 保留草稿 + attach 到正在跑的那一轮

## 4. 测试

- [x] `readSSE` helper 改为「POST + attach」，22 处调用点不动
- [x] `TestTurn_ReaderReattachSeesTheAnswerFromTheStart`：中途断开再 attach，事件从头重放且完整
- [x] `TestTurn_DetachedReaderDoesNotCostTheAnswer`：无人观看也整轮落库
- [x] `TestTurn_SecondMessageIsRefusedWhileOneRuns`：409，停止后可再发
- [x] `TestTurn_StopKeepsWhatWasProduced`：停止后保留已产生内容并标注
- [x] `TestTurn_SessionsReportWhichOneIsGenerating`：`streaming` 标记
- [x] `TestTurn_AttachIsIdleWithoutATurn`：无轮次时立即回 JSON
- [x] `go test ./...` 全绿；`npm run check:ui` 全绿
- [x] 端到端（无头 Chrome + 慢模型）：切页面 / 切会话 / **整页刷新** 后未完成的回答都在屏幕上，且持续增长

## 5. 文档

- [x] 本提案 + spec 增量
