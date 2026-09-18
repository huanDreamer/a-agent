# Phase 15 — 一轮对话属于会话，不属于连接

## Summary

一次生成不再绑定在发起它的那个 HTTP 请求上。轮次活在服务端的 **turn hub** 里，按会话
（session）索引；浏览器通过一个独立的 attach 端点读取它：服务端先把这一轮**已经发生的
事件重放**，再续上实时事件。于是——

- 切到「设置」「统计监控」再回来：答案还在，并且**继续在长**；
- 切到另一个会话再切回来：同理；侧边栏给正在生成的那条会话标「生成中」；
- **整页刷新**：页面重新选回那条正在生成的会话并自动 attach，未完成的回答照旧显示，并
  接着往下长；
- 多标签页/多端同时看同一轮：都看得到。

## Why

原来这一轮是 POST `/messages` 这条 SSE 响应本身：谁持有连接谁才有输出，连接一断，
屏幕上未落库的部分就再也拿不回来。之前的修复只保证了**不丢**（客户端断开后服务端继续
跑完并整轮落库），但读者仍然**看不见**——切走再回来只能看到已经写进数据库的部分，正在
生成的内容要等它结束才出现。用户要的是"随时能看到"，那是另一个层级：

- **答案的可见性不该由一次连接决定。** 生成是服务端的工作，连接只是观察窗口。
- **重放比快照更可靠。** attach 服务端把这一轮的事件从头重放，所以"中途加入"和"从头就
  在"看到的是同一串事件，客户端只需要一个 reducer，不需要两套渲染路径。
- **刷新是常态，不是异常。** 长任务动辄几分钟，刷新页面必须能接回现场。
- **停止要变成真的停止。** 有了 hub，"停止"才有一个可以寻址的对象：取消那一轮，而不是
  只断开自己的连接（旧行为：客户端不看了，服务端照样把 token 烧完）。

## What Changes

### ADDED

1. **`internal/server/turns.go` — turn hub（一轮一会话）**
   - `turnHub`：`map[sessionID]*liveTurn`，配 `begin`（已有轮次则拒绝）/`retire`/`find`/
     `running`/`stop`/`shutdown`。
   - `liveTurn`：事件日志 + 订阅者 + 取消句柄。日志按**同类 delta 合并**存储，因此重放
     成本是答案大小而不是 delta 个数。
   - 订阅者用**门铃 + 游标**：`notify` 通道只发信号（不阻塞运行），读者醒来后按游标
     `since(cursor)` 取增量。游标带"上一条 delta 已读到第几字节"，所以合并后仍在增长的
     那一条只会补发新增的尾巴，不会重发整段、也不会漏。
   - 轮次跑在自己的 context 上，与任何请求无关；`stop()` 取消它并把该轮标记为"被停止"，
     落库时写明"本轮已被停止，回答不完整"。
   - 结束的轮次**保留 2 分钟**：模型很快答完时，读者可能还没 attach 上，保留窗口让 attach
     永远能拿到完整事件流（否则会出现"POST 完 attach 却什么都没有"的竞态）。
   - `shutdown(ctx)`：进程退出前取消飞行中的轮次并等它们落库，避免在 store 关闭之后写库。

2. **`internal/server/chat.go` — 端点拆分**
   - `POST /api/chat/sessions/{id}/messages`：**只负责开始**，202 + JSON。会话已有轮次时
     409（检查放在写用户消息之前，不留下没人回答的孤儿消息）。接受消息时 `TouchChatSession`，
     让"最后活动时间"把正在生成的会话排到最前——刷新后自动选中的就是它。
   - `GET /api/chat/sessions/{id}/turn`：**attach**，SSE。无轮次时立刻回
     `{"streaming": false}`（不占着连接）。
   - `POST /api/chat/sessions/{id}/turn/stop`：停止该轮。
   - 落库仍在轮次结束时发生，且**先落库、后通知读者 stream_end**：客户端收到流结束就去
     重载会话，那一行必须已经在。

3. **会话载荷增加 `streaming`**（列表与单条都带，取自 hub）：侧边栏据此标「生成中」，
   刷新后的页面据此决定 attach 哪一条。

4. **`web`：attach 客户端**
   - `attachChatTurn` 读取 attach 流；`startChatTurn` 只发送；`stopChatTurn` 停止。
   - chatStore 持有**一个 attachment**（会话 + AbortController）。切会话/切页面只
     `detach`（关掉自己的读取），**不再停止这一轮**；`selectSession` 看到
     `session.streaming` 就自动 attach。
   - 有一轮在生成时按 5s 轮询会话列表，维持侧边栏的「生成中」，并发现"别处开始的轮次"。

### REMOVED

- `web/src/api.js` 的 `streamChatTurn`（POST 即流）——被 start/attach 取代。
- chatStore 里的 `stopStreaming()` 由"abort 自己的连接"改为"请求服务端停止"。
