# Phase 4 补完 — 任务清单

## 1. 入站消息

- [x] `Inbound` 增加 `Resources` / `Title` / `Mentioned`
- [x] `Resource` + `ResourceKind`（image/file/audio/media/sticker）
- [x] `post` 富文本扁平化（段落、链接、@、内联图片、code_block、hr）
- [x] `Handled()` 类型门禁（text / post / 带资源）
- [x] `PromptText()` 生成模型提示（含附件占位描述）
- [x] 畸形内容 best-effort 降级（不阻断投递）

## 2. 附件下载

- [x] `ResourceDownloader` + `RealDownloader`
- [x] 走 message-resource API（image 用 image，其余用 file）
- [x] 单附件大小上限（`DefaultMaxResourceBytes` / `max_download_mb`）
- [x] 文件名安全化（无路径分隔符、无前导点、按键去重）
- [x] 越界文件清理（超限时不留下半截文件）

## 3. 出站富文本

- [x] `MarkdownToCardElements`（代码块 / 分隔线 / 分块）
- [x] `SplitMarkdownBlocks`（围栏内不切分、支持 ~~~、未闭合围栏）
- [x] `CardTitleFromMarkdown`（首个标题 → 首行 → 默认值，UTF-8 安全截断）
- [x] 单元素内容预算（`MaxCardContentBytes`），超长按行/码点切分
- [x] `buildCard` / `buildText` 统一构造 payload

## 4. 韧性

- [x] `RetryPolicy` + `DefaultRetryPolicy`
- [x] `WithRetry`（指数退避、ctx 取消即停、返回最后一个错误且可 `errors.Is`）
- [x] `NonRetryable`（永久错误码分类：权限/token/无效请求，5xx 可重试）
- [x] `Limiter` 令牌桶（stdlib 实现，无新依赖，支持并发与 ctx）
- [x] `WithRateLimit` / `WithSendTimeout`
- [x] `WrapCreate` 组合三层（限流 → 重试 → 单次超时）

## 5. 传输模式

- [x] `Mode` + `ParseMode`（websocket 默认，支持 ws/http/webhook 别名）
- [x] HTTP 回调传输（复用同一 dispatcher）
- [x] `/healthz` 健康检查
- [x] 优雅关闭（SIGINT/SIGTERM + Shutdown 超时）
- [x] 校验 verification token（SDK 仅在握手阶段校验）
- [x] 保留签名校验；两者都未配置时告警
- [x] 请求体大小上限

## 6. 会话与交互

- [x] `Sender` 扩展：`SendText` / `SendCard` / `UpdateCard` / `Recall`
- [x] create 原语返回 message id（占位卡片替换的前提）
- [x] 「正在思考…」占位卡片 → 生成后就地更新为答案
- [x] 更新失败时回退为新消息（保证答案必达）
- [x] 附件下载后把本地路径拼进提示
- [x] 下载失败降级为说明，不阻断回答
- [x] 斜杠命令仅对纯文本生效

## 7. 配置

- [x] `feishu.thinking`
- [x] `feishu.retry_attempts` / `retry_base_delay_ms` / `send_timeout_ms`
- [x] `feishu.rate_limit_per_sec` / `rate_limit_burst`
- [x] `feishu.download_dir` / `max_download_mb`
- [x] `apps.<name>.transport` / `callback_addr` / `callback_path`
- [x] 默认值 + `config.example.yaml` 注释

## 8. Spec / 文档

- [x] openspec proposal（本目录）
- [x] capability spec：`specs/platform-feishu/spec.md`
- [x] capability spec：`specs/user-routing/spec.md`（Phase 4 承诺但缺失）
- [x] `docs/feishu.md` 更新（权限、传输模式、附件、占位、排错）

## 9. 质量门禁

- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test ./...`（含 `-race`）
- [x] feishu 包覆盖率 ≥ 70% → **92.2%**
- [x] `gofmt` 干净（本次改动涉及的文件）
