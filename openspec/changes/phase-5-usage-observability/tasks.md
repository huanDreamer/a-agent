# Phase 5 — 任务清单

## 1. 存储层：归属与聚合

- [x] 迁移 v3：`usage_logs` / `tool_invocations` 增加 `user_id`（默认空串）+ 索引
- [x] `UsageEvent` / `UsageRecord` / `UsageFilter` 增加 `UserID`、`Since`、`Until`
- [x] `UsageWindow` / `UsageTotals` / `UsageGroupRow` / `UsageDayRow` / `ProviderModelGroup`
- [x] `QueryUsageTotals`（调用数、三类 token、耗时、去重用户/会话）
- [x] `QueryUsageByUser` / `ByModel` / `ByProvider` / `ByProviderModel`
- [x] `QueryUsageByDay`：按 UTC 日分桶（兼容 Go 写入的时间格式）
- [x] `QueryUsageRecent`
- [x] 共享 WHERE/投影构造，避免列表与聚合过滤漂移
- [x] `Users` 排除空 `user_id`
- [x] JSON tag 定义 API 线上结构

## 2. 价格与费用

- [x] `internal/pricing`：`Rate` / `Table` / `Cost`
- [x] 解析优先级 `provider/model` → `model` → `provider` → fallback
- [x] 大小写/空白归一化；键名与查询参数都归一化
- [x] 非法输入防御（负 token、负/NaN/Inf 费率 → 0）
- [x] 输入 map 防御性拷贝；不可变 → 并发只读安全
- [x] 覆盖率 100%

## 3. 指标

- [x] `internal/metrics`：8 个采集器 + `build_info`
- [x] `huan_agent_` 前缀与 help 文案
- [x] 私有 registry（不碰全局默认注册器）
- [x] 重复注册返回错误而非 panic
- [x] nil 接收者安全（可关闭观测）
- [x] 启动即发布 `build_info`，`/metrics` 永不为空
- [x] 覆盖率 100%

## 4. 管理员服务

- [x] `internal/server`（Hertz）：路由装配 + 优雅关闭
- [x] bcrypt 单用户登录；哈希非法时启动即失败
- [x] 会话 cookie：HttpOnly + SameSite=Lax；仅 TLS 时 Secure
- [x] 登录失败限流（窗口内超限后拒绝，含正确密码）
- [x] 未鉴权受保护端点返回 401 JSON
- [x] REST API：summary / by-model / by-provider / by-user / by-day / recent
- [x] audit 查询（tool / user / 时间窗过滤）
- [x] skills 列表与开关（覆盖文件，不改 markdown）
- [x] `GET /metrics`（Prometheus 文本格式 + 正确 Content-Type）
- [x] `GET /api/health` / `/api/meta`
- [x] 费用总额按 (provider, model) 逐对求和（而非单次查表）
- [x] 空窗口费用为「已知 0」，未匹配价格为「未知」
- [x] Hertz 日志接入 zap（告警级别），启动不再刷路由表
- [x] Hertz `Shutdown` 不再卡住（用 `Run` 而非 `Spin`）

## 5. Web 管理界面

- [x] `web/`：Vue 3 + Vite，仅 vue/vite/plugin-vue 三个依赖
- [x] 构建产物输出到 `internal/server/webui/dist/` 并嵌入二进制
- [x] 资源使用相对路径 + 长缓存；SPA 深链回退
- [x] 未鉴权/401 自动回到登录页
- [x] 登录页、仪表盘、按模型、按用户、调用记录、审计日志、技能
- [x] 手写内联 SVG 图表；全零序列不产生 NaN
- [x] 空状态、加载态、错误横幅 + 重试
- [x] 费用未知渲染 `—`；小于 $0.0001 渲染 `< $0.0001`
- [x] 时间范围选择器（24 小时 / 7 天 / 30 天 / 全部）
- [x] 响应式（1280 / 768 / 380px 无横向溢出）

## 6. CLI

- [x] `huan-agent admin serve`
- [x] `huan-agent admin set-password`（优先无回显终端输入）

## 7. 配置

- [x] `server.enable` / `session_ttl_minutes` / `metrics_enable` / `metrics_path`
- [x] `pricing.fallback` / `pricing.models`
- [x] `admin.username` / `admin.password_hash`
- [x] `config.example.yaml` 注释与示例价格表

## 8. Spec / 文档

- [x] openspec proposal（本目录）
- [x] `specs/usage-statistics/spec.md`
- [x] `specs/admin-ui/spec.md`
- [x] `specs/observability/spec.md`
- [x] 进度页更新

## 9. 质量门禁

- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test ./...`（含 `-race`）
- [x] 新增包覆盖率 ≥ 70%（pricing 100% / metrics 100% / store 86.9% / server 70.9%）
- [x] `gofmt` 干净（本次改动涉及的文件）
- [x] 真实进程端到端冒烟（登录、401、聚合、费用、UI、metrics、SPA 回退）
