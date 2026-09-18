# web-chat（增量）：一轮对话属于会话，不属于连接

本次变更把"一次生成"从发起它的 HTTP 请求上解开：轮次活在服务端，按会话寻址，任何读者
可以在任何时刻 attach / detach / 再 attach。其余行为不变。

## API

### `POST /api/chat/sessions/{id}/messages`

只负责**开始**一轮，不再返回事件流。

- 202 + `{"turn": {"session_id", "streaming": true, "started_at"}}`。
- 409 `{"error"}`：该会话已有一轮在生成。**判定在写入用户消息之前**，因此被拒的消息不会
  在会话里留下一条没有回答的孤儿消息。
- 请求体与校验不变（`content` 可为空，但需带 `attachments`）；附件解析、历史构建、模型解析
  失败仍回 4xx JSON。
- 接受后立即 `TouchChatSession`：正在生成的会话成为"最近活动"，刷新页面时自动选中的就是它。

### `GET /api/chat/sessions/{id}/turn`

attach 到该会话正在生成的轮次。

- 有轮次：`text/event-stream`。**先重放这一轮已经发生的事件**（从头，同类 delta 已合并），
  再续上实时事件，最后以 `stream_end` 结束并关闭响应。
- 无轮次：200 + `{"streaming": false}`（不占连接）。
- 多个读者可以同时 attach；读者断开**不影响轮次**。
- 事件类型、字段、顺序与旧 POST 流完全一致（客户端 reducer 不需要第二套路径）。

### `POST /api/chat/sessions/{id}/turn/stop`

- 200 + `{"ok": true, "stopped": bool}`。取消该轮；已产生的部分保留并落库，
  消息的 `error` 为「本轮已被停止，回答不完整」。

### 会话载荷

`GET /api/chat/sessions` 的每一项与 `GET /api/chat/sessions/{id}` 的 `session` 增加
`streaming: bool`（取自服务端的在飞轮次；已结束但仍在保留窗口内的轮次**不算** streaming）。

## 行为

- **一轮属于会话**：轮次跑在自己的 context 上，与任何请求无关；进程退出时先取消在飞轮次
  并等它们落库，再关 store。
- **一轮一会话**：同一会话同时只允许一轮；要再发先停止或等它结束。
- **先落库、后结束流**：客户端收到 `stream_end` 就重载会话，届时那一行必须已经写入。
- **结束的轮次保留 2 分钟**：模型很快答完时读者可能尚未 attach，保留窗口保证 attach 永远
  能拿到完整事件流（而不是"什么都没有"）。
- **停止是真的停止**：与旧的"客户端不再接收、服务端照跑"不同，`/turn/stop` 取消运行。
- 客户端：切页面 / 切会话只 detach（关掉自己的读取），**不再停止轮次**；重新选中一条
  `streaming` 的会话时自动 attach。有轮次在生成时按 5s 轮询会话列表，维持侧边栏的
  「生成中」并发现别处开始的轮次。
- 侧边栏对 `streaming` 的会话显示「生成中」（替换该行的时间戳）。
