# CLAUDE.md

## 项目背景

`huan-agent` 是基于 CloudWeGo [Eino](https://github.com/cloudwego/eino) 的个人 AI Agent 平台。
通过飞书（IM）与人对话，通过 MCP 协议和 Skills 机制扩展能力。
完整规划见 `PLAN.md`。

## 技术栈

- **语言**：Go 1.22+
- **LLM 框架**：CloudWeGo Eino
- **HTTP 框架**：CloudWeGo Hertz
- **配置**：Viper
- **日志**：Uber Zap
- **数据库**：SQLite (MVP) / PostgreSQL (生产)
- **IM**：飞书 OpenAPI (lark-oapi-go)
- **MCP**：自实现 stdio + sse 客户端
- **前端**：Vue 3 + Vite (Admin UI)
- **Spec**：@fission-ai/openspec

## 开发约定

- **Lint**：`golangci-lint run`（启用 errcheck / govet / staticcheck）
- **测试覆盖率**：核心模块 ≥ 70%
- **错误处理**：使用 `fmt.Errorf("...: %w", err)` 包裹错误
- **日志**：用 Zap，禁止 `fmt.Println`
- **配置**：所有可调参数走 Viper，禁止硬编码
- **提交信息**：Conventional Commits（feat / fix / refactor / docs / test）
- **分支策略**：trunk-based，feature 分支 → 短 PR → squash merge

## 常用命令

```bash
# 编译
make build
# 测试
make test
# Lint
make lint
# 本地运行
make run
# 格式化
make fmt
```

## 目录约定

```
cmd/                # 主入口
internal/           # 业务代码（不可被外部 import）
  agent/            # Agent 核心
  llm/              # LLM provider
  tool/             # 工具注册
  mcp/              # MCP 客户端
  skill/            # Skill 系统
  memory/           # 记忆系统
  context/          # 上下文管理
  usage/            # 用量统计
  platform/         # IM 适配层
    feishu/         # 飞书实现
  router/           # 路由
  harness/          # Agent Harness
  config/           # 配置
  store/            # 数据库
  obs/              # 观测（日志/metrics）
  server/           # Admin HTTP
pkg/                # 公共包（可被外部 import）
web/                # Admin 前端
configs/            # 配置文件 + 内置 skills
openspec/           # openspec 规范
```

## openspec 工作流

每个 Phase 对应一个 change proposal：

```bash
# 起草
/openspec:proposal "phase X: ..."

# 实施
/openspec:apply <change-id>

# 归档
/openspec:archive <change-id>
```

详见 `openspec/specs/project.md`（项目级约定）和 `PLAN.md` §6。

## 阶段依赖

```
Phase 0 → Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 → Phase 6 → Phase 7
```

Phase 0 是脚手架，Phase 1 之后可独立使用 CLI 对话，Phase 4 接入 IM 后才"能用"。

## 不要做

- 不要在 `internal/` 里写可被外部 import 的包
- 不要在代码里硬编码 API key、密码、token
- 不要绕过 Viper 直接读环境变量（用 `viper.GetString`）
- 不要用 `fmt.Println` / `log.Println`（用 Zap）
- 不要在 `main()` 里写业务逻辑
- 不要在没有 spec 的情况下添加新 capability
