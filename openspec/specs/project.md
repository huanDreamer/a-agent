# project

huan-agent 是基于 CloudWeGo Eino 的个人 AI Agent 平台。

## Purpose

为个人用户提供一个可扩展的 AI Agent 框架，通过即时通讯软件（飞书）作为主要交互界面，通过 MCP 协议和 Skill 机制扩展 Agent 能力。

## Tech Stack

- **语言**: Go 1.22+
- **LLM 框架**: [CloudWeGo Eino](https://github.com/cloudwego/eino)
- **HTTP 框架**: CloudWeGo Hertz
- **配置**: Viper
- **日志**: Uber Zap
- **数据库**: SQLite (MVP) / PostgreSQL (生产)
- **IM**: 飞书 OpenAPI (lark-oapi-go)
- **MCP**: 自实现 stdio + sse 客户端
- **前端**: Vue 3 + Vite (Admin UI)
- **Spec**: @fission-ai/openspec

## Conventions

### Code Style
- Go 1.22+ 语法
- `golangci-lint run` 必须通过
- 错误用 `fmt.Errorf("...: %w", err)` 包裹
- 禁止 `fmt.Println` / `log.Println`，统一用 Zap
- 配置走 Viper，禁止硬编码

### Testing
- 核心模块测试覆盖率 ≥ 70%
- 集成测试用 `//go:build integration` tag
- Mock 用 `gomock` 或手写 stub（视场景）

### Git
- Conventional Commits（feat / fix / refactor / docs / test / chore）
- 主分支: `main`
- Feature 分支: `feat/<scope>`
- Squash merge

### Directory Layout
- `cmd/` 主入口
- `internal/` 业务代码（不可被外部 import）
- `pkg/` 公共包
- `web/` 前端
- `configs/` 配置文件 + 内置 skills
- `openspec/` openspec 规范

## Capabilities

| Capability | Phase | Status |
|---|---|---|
| `project-bootstrap` | 0 | proposed |
| `llm-provider` | 1 | proposed |
| `agent-core` | 2 | proposed |
| `mcp-client` | 2 | proposed |
| `skill-system` | 2 | proposed |
| `memory-system` | 3 | proposed |
| `context-management` | 3 | proposed |
| `platform-feishu` | 4 | proposed |
| `usage-tracking` | 5 | proposed |
| `admin-ui` | 5 | proposed |
| `deployment` | 6 | proposed |

## Out of Scope (MVP)

- 个人微信 hook 协议（合规风险）
- 多 Agent 协作
- 定时任务调度
- RAG / 向量库
- 语音（ASR / TTS）

## Open Questions

- 飞书机器人形态：单租户 vs 多租户？单机器人 vs 多机器人？
- 是否需要 Web 对话界面（与 IM 互补）？
- Skill 是否需要支持从 GitHub 仓库同步？
- Agent system prompt 是否需要多语言切换？
