# Phase 8 — 任务清单

## 1. 客户端 `internal/openviking`

- [x] `Config`（BaseURL / APIKey / Account / User / Timeout）+ `New`（默认值、URL 规整）
- [x] 请求封装：认证头、身份头、JSON 编解码、`context` 超时
- [x] `*APIError`（Status / Code / Message / RequestID）+ `IsUnavailable` 判定
- [x] `Health` / `Find` / `Remember`（batch messages + 可选 commit）/ `TaskStatus`
- [x] `WriteContent` / `ReadContent` / `Stat` / `Forget`
- [x] `UploadTemp`（multipart）/ `AddResource`
- [x] 测试：httptest 覆盖成功、错误信封、超时、认证头、URL 拼接 → 覆盖率 84.5%

## 2. 配置 `internal/config`

- [x] `OpenVikingConfig` + `OpenVikingMemoryConfig` + `OpenVikingDocumentsConfig` + `OpenVikingMCPConfig`
- [x] 默认值与 `SetDefaults`（默认 `enable: false`，不改变现有行为）
- [x] 取值助手：`Timeout()`、`Subtree()`、`DocumentsRoot()`、`RecallScope()`、
      `MaxFileBytes()`、`WorkspaceRoot()`、`StateFile()`、`SyncInterval()`
- [x] MCP 自动注册：`ApplyOpenVikingMCP`（已声明同名则跳过、幂等）
- [x] 测试：默认值、env 覆盖（`HUAN_OPENVIKING_*`）、MCP 合并幂等 → 覆盖率 86.5%

## 3. 记忆镜像 `internal/memory`

- [x] `OpenVikingStore`（装饰器实现 `Store`）：`Append` / `Read` / `Facts` / `AddFact` / `SearchFacts` / `Close`
- [x] 轮次攒批（`flush_every`）+ `Flush`（按命名空间分批，不串会话）
- [x] `AddFact` 双路：`Remember` 提交抽取 + 直写 `journal.md`（append，404 时 create）
- [x] `SearchFacts` 先语义检索、空结果或失败回落本地关键词
- [x] 降级：不向调用方返回错误，只记 WARN + 计数
- [x] `Stats()`（提交/事实/失败/待提交/最近错误）→ 供状态接口
- [x] 顺带修复：`Facts()` / `SearchFacts` 回读时从 Meta 还原 keywords（否则关键词回落实际不可用）
- [x] 测试 → 覆盖率 86.9%

## 4. 文档同步 `internal/documents`

- [x] `Syncer` + `Config`；`Save`（frontmatter、slug、内容哈希防覆盖、`wait=true`）
- [x] `SyncWorkspace`：include/exclude glob（支持 `**`）、大小上限、二进制跳过 / upload
- [x] 状态文件（sha256 增量，原子写入；损坏/版本不符退化为全量）+ `Documents()` 清单
- [x] `Report`（Scanned / Uploaded / Unchanged / Skipped / Failed / Errors）
- [x] 并发保护（`ErrSyncing`）、失败逐文件收集不中断
- [x] 测试：增量只传变化、排除规则、超大跳过、二进制策略、状态往返 → 覆盖率 85.6%

## 5. 门面、工具与接线

- [x] `internal/viking`：客户端 + 镜像 + 同步器的装配与 `Status()`（覆盖率 89.5%）
- [x] 内置工具 `save_document`（`internal/tool/builtin/openviking.go`）
- [x] `cmd/huan-agent/memory.go`：商店改为进程级共享 + 镜像包装，`sessionMemory.close` 不再关共享商店
- [x] `cmd/huan-agent/viking.go`：`viking status|sync|save|remember|search` + 退出同步 + 定时同步循环
- [x] `cmd/huan-agent/chat.go` / `serve.go` / `admin.go`：装配门面、作用域内 defer Close/Flush
- [x] 测试：工具 schema 与参数校验、门面降级与共享镜像

## 6. Admin API 与前端

- [x] `internal/server/openviking.go`：status / sync / save / documents / flush
- [x] 路由注册与 nil-safe 注入（未启用时返回 `enable:false`，不是 500）
- [x] 测试：启用/未启用、409 冲突、400 校验、空列表非 null、未登录 401
- [x] `web/src/components/OpenVikingPanel.vue` + `SETTINGS_TABS` 新 tab + `api.js`
- [x] `npx vite build` 通过，`internal/server/webui/dist` 重新生成

## 7. 验收

- [x] `go build ./...`
- [x] `go test ./...`（新增模块覆盖率 84.5% / 85.6% / 86.9% / 89.5%）
- [x] `go vet ./...` + `gofmt`（**golangci-lint 未安装在本机**，未能执行 `make lint`）
- [x] 端到端（本机 ov 0.4.19）：
      `viking remember` → `viking search` 命中 `memory/journal.md`；
      `viking save` → 命中 `documents/2026/09/...`；
      `viking sync` → 2 上传 / 1 跳过、二次 2 unchanged → `find` 命中工作区文件；
      admin API 的 status / sync / save / documents / flush 全部实测通过
- [x] `configs/config.yaml`（本机启用）+ `configs/config.example.yaml`（示例）+ `docs/openviking.md`
- [x] 验收产生的 openviking 数据与会话已清理

## 待办（环境侧，非本次代码问题）

- [ ] 本机 OpenViking 的 VLM 模型未开通（`ModelNotOpen`），`session_commit` 抽取任务
      失败；需在方舟控制台开通 `doubao-seed-2-0-code-preview-260215`，或改
      `~/.openviking/ov.conf` 的 `vlm.model`。在此之前关键事实依靠 journal 通道仍可检索。
