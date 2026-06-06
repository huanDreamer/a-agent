# huan-agent 项目计划书

> 个人 AI 助手 / 多 IM 平台接入 / 多 LLM provider / 面向运维自动化与日常对话

## 0. 项目定位

**huan-agent** 是一个基于 [CloudWeGo Eino](https://github.com/cloudwego/eino) 的个人 AI Agent 平台，对外通过即时通讯软件（IM）与人对话，对内通过 MCP (Model Context Protocol) 协议和 Skills 机制扩展能力。

- **使用对象**：单用户（自己），但支持多 IM 账号 / 多用户隔离
- **部署形态**：服务器多用户模式
- **LLM 接入**：OpenAI 兼容协议（DeepSeek / Qwen / GLM / OpenAI / Ollama 等），通过配置切换
- **通讯平台**：MVP 阶段只做**飞书**（WebSocket 长连接 + 事件订阅），微信（个人微信 hook 协议）作为后期可选扩展
- **核心能力**：通用 MCP 工具调用、Skill 机制（提示词 + 工具组合）、上下文管理、用量统计、Agent Harness

## 1. 技术栈

| 维度 | 选型 | 备注 |
|---|---|---|
| 语言 | Go 1.22+ | eino 强依赖 |
| LLM 框架 | CloudWeGo Eino | ChatModel / Tool / Agent / Compose |
| HTTP 框架 | CloudWeGo Hertz | 与 eino 同源 |
| 配置 | Viper + env | 支持热加载（viper.WatchConfig） |
| 日志 | Uber Zap | 结构化日志 |
| 数据库 | SQLite (MVP) / PostgreSQL (生产) | 抽象 GORM |
| IM 协议 | 飞书 OpenAPI (lark-oapi-go) | WebSocket + 回调 |
| MCP | 自实现 stdio + SSE 客户端 | eino 已有工具抽象 |
| 向量库 (可选) | sqlite-vss / chromem-go | MVP 用文件型 |
| Metrics | Prometheus + Grafana | 用量统计 |
| Admin UI | Vue 3 + Vite | 用户/会话/用量/技能管理 |
| Spec | @fission-ai/openspec | 规范驱动开发 |

## 2. 顶层架构

```
┌─────────────────────────────────────────────────────────┐
│                   IM Adapters (飞书)                      │
│   inbound: 文本/图片/文件/卡片交互  outbound: 文本/富文本   │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────┐
│                  Platform Router                         │
│   用户隔离 / 会话路由 / 限流 / 消息分发                     │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────┐
│                  Agent Harness                           │
│   ReAct 循环 / 工具调用 / 流式响应 / 错误恢复                │
└──────┬───────────────┬─────────────┬────────────────────┘
       │               │             │
┌──────▼──────┐  ┌─────▼─────┐  ┌────▼─────┐
│   Skills    │  │ MCP Tools │  │ Built-in │
│  (提示词+    │  │ (stdio /  │  │  Tools   │
│   工具组合)  │  │  sse)     │  │ (时间/    │
└─────────────┘  └───────────┘  │  算/搜索) │
                                 └──────────┘
┌─────────────────────────────────────────────────────────┐
│                 Memory & Context                          │
│   短期: 会话窗口  长期: 摘要/文件/向量(可选)  Token预算       │
└─────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────┐
│               LLM Providers (OpenAI 兼容)                 │
│   DeepSeek / Qwen / GLM / OpenAI / Ollama / 自定义         │
└─────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────┐
│            Observability & Admin                         │
│   用量统计 / 审计日志 / 技能市场 / Web 控制台               │
└─────────────────────────────────────────────────────────┘
```

## 3. 目录结构

```
huan-agent/
├── cmd/
│   └── huan-agent/         # 主入口（单二进制）
├── internal/
│   ├── agent/              # Agent 核心 (ReAct 循环)
│   ├── llm/                # LLM provider 抽象与实现
│   ├── tool/               # 工具注册中心
│   ├── mcp/                # MCP stdio/sse 客户端
│   ├── skill/              # Skill 加载/匹配/执行
│   ├── memory/             # 短期/长期记忆
│   ├── context/            # 上下文窗口管理 + 摘要
│   ├── usage/              # 用量统计
│   ├── platform/           # IM 适配层
│   │   └── feishu/         # 飞书实现
│   ├── router/             # 用户/会话路由
│   ├── harness/            # Agent Harness（会话、工具、Skill 编排）
│   ├── config/             # 配置加载
│   ├── store/              # 数据库抽象 + 实现
│   ├── obs/                # 日志 / metrics / tracing
│   └── server/             # Admin HTTP server
├── pkg/                    # 可导出的公共包
├── web/                    # Admin 前端 (Vue)
├── configs/                # 配置文件模板
│   ├── config.example.yaml
│   └── skills/             # 内置 Skills
├── docs/
├── openspec/
│   ├── specs/              # 当前 spec
│   │   ├── project.md
│   │   └── <capability>/spec.md
│   └── changes/            # 变更提案
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile
├── docker-compose.yml
├── README.md
├── CLAUDE.md
└── PLAN.md
```

## 4. 阶段化开发计划

> 每个阶段对应一个 openspec `change proposal`，完成后归档到 `openspec/changes/archive/`。
> 阶段依赖关系：上层依赖下层，建议严格按阶段推进。

---

### Phase 0 — 项目骨架与 openspec 接入 (1-2 天)

**目标**：让仓库可编译、可运行 `huan-agent --version`，openspec 流程就位。

**交付物**：
- Go module（`github.com/huan/huan-agent`）
- 目录结构见 §3
- `Makefile`（`build` / `test` / `lint` / `run`）
- 配置加载（Viper + env 优先）
- 结构化日志（Zap）
- 优雅关闭
- `openspec init` 完成，目录规范就绪
- `CLAUDE.md` 写入项目背景与开发约定
- GitHub Actions: lint + test

**openspec capabilities**: `project-bootstrap`

**关键决策**：
- 选用 `lark-oapi-go` 官方 SDK 还是自己封装 WebSocket？→ **官方 SDK**（稳定 + 减少维护）
- 配置格式：YAML + env 覆盖（env 优先于文件）

---

### Phase 1 — LLM 接入与基础对话 (3-5 天)

**目标**：CLI 里能跟 LLM 流式对话，多 provider 切换，token 用量统计入库。

**交付物**：
- `internal/llm/` 多 provider 注册表（DeepSeek / Qwen / GLM / OpenAI / Ollama 至少 3 家实现）
- 基于 eino `ChatModel` 抽象
- 流式输出到 stdout（CLI 模式）
- Token 用量埋点 + SQLite 持久化（`usage_logs` 表）
- 单元测试：provider 切换、prompt 拼装、错误处理
- 简单的交互式 REPL（`huan-agent chat`）

**openspec capabilities**: `llm-provider`, `usage-tracking`

**关键决策**：
- eino 是否已经支持所有目标 provider？→ 需要逐个验证；EinoExt 有 `deepseek` / `openai` 适配
- 流式输出的 backpressure 怎么处理？→ 用 channel + ctx 控制
- Token 计数：靠 provider 返回还是本地 tiktoken-go？→ 优先 provider 返回，本地兜底

---

### Phase 2 — Agent 核心 + MCP + Skill (5-7 天)

**目标**：实现 ReAct Agent，支持 MCP stdio/sse 工具调用和 Skill 机制。

**交付物**：
- `internal/agent/`：ReAct 主循环（think → tool call → observe → ...）
- `internal/tool/`：Tool 接口 + 注册中心 + 权限/白名单
- `internal/mcp/`：stdio + sse 客户端（用 `mark3labs/mcp-go` 或自实现）
- `internal/skill/`：Skill 加载（markdown + YAML frontmatter），Skill 匹配（关键词/向量可选）
- 内置工具：`time` / `calc` / `web_search` / `fetch_url`
- 内置 Skill 模板：`daily-summary` / `code-review` / `git-commit`
- 工具调用审计日志

**openspec capabilities**: `agent-core`, `tool-registry`, `mcp-client`, `skill-system`

**关键决策**：
- 工具权限模型：白名单 / 黑名单 / 用户授权？→ **白名单 + 用户级 override**
- Skill 存储：文件系统 / 数据库 / 远程仓库？→ 文件系统为主，可热加载
- Skill 匹配：关键词 / LLM 路由 / embedding？→ MVP 关键词 + LLM 路由（避免引入向量库）

---

### Phase 3 — Memory & Context 管理 (3-4 天)

**目标**：让 Agent 有"记忆"，长会话不会爆 context window。

**交付物**：
- `internal/memory/`：短期（最近 N 轮消息）+ 长期（摘要 + 关键事实）
- `internal/context/`：Token 预算管理，自动摘要（用 LLM 自己）
- 会话级 / 用户级记忆隔离
- 长期记忆读写 API
- 上下文压缩策略可配置（"始终保留最近 K 条 + 摘要历史"）

**openspec capabilities**: `memory-system`, `context-management`

**关键决策**：
- 长期记忆用向量库还是文件？→ MVP 用 **JSONL 文件 + 关键词索引**，后期可换 chromem-go
- 摘要触发阈值：固定 token 数 vs 动态比例？→ **动态比例**（默认 70% 触发摘要）

---

### Phase 4 — 飞书接入 (5-7 天)

**目标**：用户可以在飞书里给机器人发消息，机器人能回复（文本 + 富文本卡片）。

**交付物**：
- `internal/platform/feishu/`：WebSocket 长连接 + 事件回调双模式
- 入站：文本消息、图片、文件
- 出站：文本消息、富文本卡片（`interactive`）
- 用户身份识别（飞书 open_id / user_id）
- 多用户隔离 + 路由到对应会话
- 飞书机器人后台配置文档（自建应用 / 权限申请）
- 失败重试 + 限流

**openspec capabilities**: `platform-feishu`, `user-routing`

**关键决策**：
- 长连接 vs 回调？→ 默认 **WebSocket 长连接**（无需公网回调地址），回调作为可选
- 流式输出：飞书不支持流式，**等 LLM 完全生成后一次性发**；长任务用"正在思考..."占位
- 富文本 vs 纯文本：默认纯文本，**支持 markdown 转 interactive 卡片**（表格 / 代码块高亮）

---

### Phase 5 — 用量统计与可观测性 (3-4 天)

**目标**：能看到每个用户、每个会话、每个模型的 token / 费用消耗。

**交付物**：
- Admin HTTP server（`internal/server/`）：REST API
  - `GET /api/usage/summary` - 总体用量
  - `GET /api/usage/by-user` - 按用户
  - `GET /api/usage/by-model` - 按模型
  - `GET /api/usage/by-day` - 趋势
- Web Admin UI（Vue 3）：登录、用量仪表盘、用户列表、会话列表、技能开关
- Prometheus 指标（`/metrics`）：调用次数、延迟、错误率、token 用量
- 审计日志（谁在什么时候调了哪个工具）

**openspec capabilities**: `usage-statistics`, `admin-ui`, `observability`

**关键决策**：
- Admin UI 鉴权：单用户密码 / OAuth？→ MVP **单用户密码**（bcrypt 存库），多用户后期再说
- 时序数据用 Prometheus 还是直接查 SQL？→ **直接 SQL + 简单图表**，Prometheus 作为补充
- 费用计算：硬编码 model → price 表？→ **YAML 配置文件**（按 1k token 计费）

---

### Phase 6 — 生产加固 (3-4 天)

**目标**：可对外提供服务。

**交付物**：
- Dockerfile（多阶段构建，镜像 < 50MB）
- `docker-compose.yml`（含可选 Postgres / Prometheus / Grafana）
- 限流（per-user / global）
- 重试 / 熔断（针对 LLM 调用）
- 健康检查 `/healthz` / `/readyz`
- 优雅关闭
- 部署文档（systemd / Docker / k8s 简版）
- 备份脚本（SQLite 定时备份到对象存储）

**openspec capabilities**: `deployment`, `reliability`

**关键决策**：
- 镜像基础：distroless / alpine / scratch？→ **distroless static**（最小攻击面）
- 配置管理：配置文件 / 全部环境变量 / Vault？→ **YAML + env 覆盖**

---

### Phase 7 (未来扩展，不在 MVP 内)

- **语音支持**：飞书语音消息转文本（ASR），回复可发语音（TTS）
- **更多 IM**：企业微信 / 钉钉 / Slack / Telegram
- **个人微信 hook 协议**（需要权衡合规风险）
- **Skill Marketplace**：从远端仓库拉取 Skill
- **多 Agent 协作**：主 Agent 调度多个子 Agent
- **定时任务**：Cron 触发 Agent 执行（如"每天早上 8 点总结昨天的 commit"）
- **RAG 增强**：内置向量库 + 文档索引
- **A2A 协议**：Agent-to-Agent 通信

## 5. 阶段依赖图

```
Phase 0  ──▶  Phase 1  ──▶  Phase 2  ──▶  Phase 3
                  │            │            │
                  └────────────┴────────────┴──▶ Phase 4 (飞书)
                                                   │
                                                   ▼
                                             Phase 5 (用量/UI)
                                                   │
                                                   ▼
                                             Phase 6 (生产加固)
                                                   │
                                                   ▼
                                             Phase 7 (扩展)
```

## 6. 与 openspec 的配合

### 6.1 初始化

```bash
# 在项目根目录
npx @fission-ai/openspec init
# 选择项目语言: zh-CN, 工具类型: Go
```

### 6.2 工作流

每个 Phase 对应一个 `change proposal`：

```bash
# 1. 起草提案
openspec/changes/phase-1-llm-provider/
├── proposal.md         # 为什么 / 做什么 / 影响
├── tasks.md            # TODO 清单
└── specs/
    └── llm-provider/
        └── spec.md     # delta spec (ADDED/MODIFIED/REMOVED)
```

### 6.3 提议 → 实施 → 归档

```bash
# 起草
/openspec:proposal "phase 1: llm provider 多 provider 接入"

# 审核 + 实施（Claude 会读取 openspec/ 目录）
/openspec:apply phase-1-llm-provider

# 实施完成后归档
/openspec:archive phase-1-llm-provider
```

### 6.4 项目级 spec

`openspec/specs/project.md` 维护项目级约定（不变的事实）：
- 技术栈、目录结构、命名规范
- 测试覆盖率要求（建议 ≥ 70%）
- 错误处理规范（error wrapping + sentry/alert 兜底）
- API 风格（REST + JSON）

## 7. 待办事项（按阶段）

### Phase 0 — 项目骨架
- [ ] `go mod init github.com/huan/huan-agent`
- [ ] 创建目录结构
- [ ] 接入 Viper + Zap
- [ ] 编写 `Makefile`
- [ ] 编写 `CLAUDE.md`（项目背景 / 开发约定 / 常用命令）
- [ ] 编写 `README.md`（快速开始）
- [ ] GitHub Actions：lint + test
- [ ] `openspec init`

### Phase 1 — LLM 接入
- [ ] 实现 `internal/llm/provider.go` 抽象
- [ ] 实现 DeepSeek / Qwen / GLM provider
- [ ] 实现 Ollama（本地）
- [ ] 流式输出 CLI
- [ ] Token 用量埋点 + SQLite 表
- [ ] `huan-agent chat` 交互式命令
- [ ] 单元测试

### Phase 2 — Agent 核心
- [ ] Tool 接口 + 注册中心
- [ ] ReAct 主循环
- [ ] MCP stdio 客户端
- [ ] MCP sse 客户端
- [ ] Skill 加载器（markdown + frontmatter）
- [ ] Skill 匹配器
- [ ] 内置工具：time / calc / web_search / fetch_url
- [ ] 内置 Skill 模板
- [ ] 工具调用审计

### Phase 3 — Memory & Context
- [ ] 短期记忆：最近 N 轮
- [ ] 长期记忆：摘要 + 关键事实
- [ ] Token 预算管理
- [ ] 自动摘要（动态阈值）
- [ ] 会话隔离

### Phase 4 — 飞书
- [ ] WebSocket 长连接
- [ ] 文本消息收发
- [ ] 富文本卡片
- [ ] 用户身份识别 + 路由
- [ ] 长任务占位 + 完成通知
- [ ] 飞书后台配置文档

### Phase 5 — 用量与 UI
- [ ] Admin REST API
- [ ] Vue 3 Admin UI
- [ ] 登录鉴权
- [ ] 用量仪表盘
- [ ] Prometheus 指标
- [ ] 审计日志

### Phase 6 — 生产
- [ ] Dockerfile（多阶段）
- [ ] docker-compose
- [ ] 限流
- [ ] 熔断
- [ ] 健康检查
- [ ] 部署文档

## 8. 关键风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| eino API 不稳定 | 高 | 锁定 eino 版本，封装自有抽象层 |
| LLM 流式 + 飞书不兼容 | 中 | 占位消息 + 完整生成后替换 |
| 上下文爆炸 | 高 | 动态摘要 + 严格 Token 预算 |
| 飞书 SDK 升级破坏 | 中 | 固定 lark-oapi-go 版本，CI 跑通 |
| Token 费用失控 | 中 | 用户级限流 + 告警阈值 |
| MCP 工具误用 | 中 | 工具白名单 + 用户确认（可选） |
| SQLite 写入瓶颈 | 低 | MVP 用 SQLite，量大时换 Postgres |

## 9. 开发约定（建议写入 CLAUDE.md）

- **Go 版本**：1.22+
- **Lint**：golangci-lint run（启用 errcheck / govet / staticcheck）
- **测试覆盖率**：≥ 70%
- **错误处理**：使用 `fmt.Errorf("...: %w", err)` 包裹错误
- **日志**：用 Zap，禁止 `fmt.Println`
- **配置**：所有可调参数走 Viper，禁止硬编码
- **提交信息**：Conventional Commits（feat / fix / refactor / docs / test）
- **分支策略**：trunk-based，feature 分支 → 短 PR → squash merge

## 10. 后续讨论

- 飞书机器人形态：单租户 vs 多租户？单机器人 vs 多机器人？
- 是否需要 Web 对话界面（与 IM 互补）？
- Skill 是否需要支持从 GitHub 仓库同步？
- 是否需要"定时任务"（Scheduler）能力？
- Agent 的 system prompt 是否需要支持多语言（中/英）切换？

---

**下一步行动建议**：
1. 跑 `npx @fission-ai/openspec init` 完成 openspec 接入
2. 起草 Phase 0 的 change proposal（`openspec/changes/phase-0-bootstrap/`）
3. 开始实施 Phase 0：搭骨架 + Makefile + 基础配置
4. 完成后归档，开始 Phase 1
