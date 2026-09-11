# Phase 5b — 任务清单

## 1. 流式对话内核

- [x] `internal/chat`：`Runner` 驱动 tool-calling 模型的 think→tool→observe 循环
- [x] 事件模型：`step_start` / `text_delta` / `reasoning_delta` / `tool_call` / `tool_result` / `usage` / `done` / `error`
- [x] 把工具参数 schema 转成模型可用的 `ParamsOneOf`（否则模型无法带参调用）
- [x] 工具失败不中断回合：回报给客户端 + 作为 observation 回喂模型
- [x] 未注册/未授权工具体面化拒绝（回喂模型而非静默跳过）
- [x] token 用量与 reasoning 跨步累加（只取最后一步会漏计）
- [x] 步数上限耗尽时给出明确说明而非假装完成
- [x] `Tracer` 窄接口 + `NopTracer` 默认（调用点无需判空）
- [x] 不修改调用方传入的历史切片
- [x] 覆盖率 88.8%

## 2. 会话持久化

- [x] 迁移 v4：`chat_sessions` + `chat_messages`（含索引与外键）
- [x] 会话 CRUD + `TouchChatSession`（活跃会话置顶）+ `DeleteChatMessages`
- [x] 消息按 id 升序返回；`MessageCount` 随列表返回
- [x] 删除会话级联删除消息，且显式删除避免依赖 pragma
- [x] 部分更新语义（改标题不清空模型）
- [x] 覆盖率 85.3%

## 3. 管理员 API

- [x] `GET /api/chat/models`（可选 provider/模型 + 工具清单 + 系统提示 + 步数）
- [x] 会话 CRUD：`GET/POST /api/chat/sessions`、`GET/PATCH/DELETE /api/chat/sessions/{id}`、`POST .../clear`
- [x] `POST /api/chat/sessions/{id}/messages` → **SSE 流式**（真流式，逐帧 flush）
- [x] 心跳注释帧 + 终止帧 `stream_end`
- [x] 客户端断开仍持久化已产出的答案（用独立 context，不依赖请求 context）
- [x] 用户消息在调用模型前落库
- [x] 工具调用写入审计日志，与会话记录一致
- [x] 首条消息自动生成会话标题
- [x] 切换 provider 时重新解析该 provider 的默认模型
- [x] 回放历史时过滤 reasoning/tool 元数据（避免 provider 拒绝）
- [x] 每会话按模型缓存 Runner，容量有界
- [x] 覆盖率 75.6%

## 4. 模型选择

- [x] `llm.Registry.GetWithModel`（按会话覆盖模型名）
- [x] `llm.Registry.Catalog` / `DefaultName`（供 UI 下拉）
- [x] `server.ModelBuilder` 适配器

## 5. Langfuse 对接

- [x] `internal/langfuse`：ingestion 批量上报（后台批处理、队列上限、丢弃计数）
- [x] Basic auth、成功判定含 207、失败重试一次
- [x] 未配置 = 完全不工作且不启协程；nil 接收者安全；`Close` 幂等且会 flush
- [x] 读接口：`ListTraces` / `GetTrace`（兼容 camelCase 与 snake_case）
- [x] `Tracer` 适配 `chat.Tracer`：trace / generation(含 usage) / span(含错误级别)
- [x] 修复 `EndTrace` 会用结束时间覆盖 trace 起始时间的问题
- [x] 修复子代理测试的竞态（flush 计数在发请求前自增）
- [x] 覆盖率 97.4%

## 6. 链路追踪 API

- [x] `GET /api/traces/status`（启用状态、host、投递计数）
- [x] `GET /api/traces` + `GET /api/traces/{id}`（服务端代理，密钥不下发浏览器）
- [x] 未启用时返回 `enabled:false` + 明确提示要配哪个字段（而非 404）
- [x] Langfuse 不可达返回 502（与"未启用"可区分）
- [x] 路由始终注册（未配置也能给出解释）

## 7. 配置与 CLI

- [x] `chat.enable` / `max_steps` / `history_limit` / `system_prompt`
- [x] `langfuse.enable` / `host` / `public_key` / `secret_key` / `environment` / `release`
- [x] `admin serve` 组装 chat 与 tracing；两者失败均不阻断服务启动
- [x] `config.example.yaml` 注释

## 8. 前端

- [x] 新增「对话」标签页（置于首位）与「链路追踪」标签页
- [x] 会话侧边栏：列表/新建/选择/重命名/清空/删除（带确认）
- [x] 模型选择器（标注默认与无密钥），切换即 PATCH 会话
- [x] SSE 解析（分块跨帧、心跳、多帧同块、stream_end 终止）
- [x] 流式答案 + 可折叠「思考过程」面板 + 工具调用卡片
- [x] 回合 token 用量与耗时、停止按钮（AbortController）、复制按钮
- [x] 手写基础 markdown 渲染（代码块/行内码/加粗），不使用 v-html
- [x] 自动滚动但尊重用户上滑
- [x] 链路追踪：状态条、trace 列表与筛选、观察瀑布图（按类型着色、错误标红）
- [x] 未启用时的引导式空状态；backend 故障提示与"未启用"区分
- [x] 空状态/加载态/错误重试齐全

## 9. Spec / 文档

- [x] openspec proposal（本目录）
- [x] `specs/web-chat/spec.md`
- [x] `specs/trace-visualization/spec.md`
- [x] `docs/admin.md` 增补对话与链路追踪章节
- [x] 进度页更新

## 10. 质量门禁

- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test ./...`（含 `-race`）
- [x] 新增包覆盖率 ≥ 70%（chat 88.8% / langfuse 97.4% / store 85.3% / server 75.6%）
- [x] `gofmt` 干净（本次改动涉及的文件）
- [x] 真实进程端到端验证
