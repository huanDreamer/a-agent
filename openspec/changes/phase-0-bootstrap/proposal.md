# Phase 0 — 项目骨架与 openspec 接入

## Why

`huan-agent` 仓库目前是空白的（只有 PLAN.md 和 openspec 目录）。在写任何业务代码之前，需要一个可编译、可运行、约定明确的工程骨架：

1. **可编译运行的二进制** — Phase 1 之后才能写 LLM provider
2. **统一的配置 / 日志 / 关闭规范** — 后续所有模块都依赖
3. **openspec 流程跑通** — 让每个 phase 都有 proposal → apply → archive 的标准动作
4. **CI 基础** — 避免代码质量雪球

不做 Phase 0 的话，后续每个 phase 都要重新折腾一遍工程基础。

## What Changes

### ADDED

- Go module `github.com/huan/huan-agent`（Go 1.22+）
- 单二进制入口 `cmd/huan-agent/main.go`，子命令 `version` / `serve`（占位）
- 配置加载（Viper + YAML + 环境变量覆盖）
- 结构化日志（Zap）
- 优雅关闭（signal handler + context）
- Makefile（`build` / `test` / `lint` / `fmt` / `run` / `clean`）
- 配置文件模板 `configs/config.example.yaml`
- GitHub Actions：lint + test
- 单元测试：config + logger

### NOT in this change

- LLM provider（Phase 1）
- Agent 核心（Phase 2）
- MCP / Skill（Phase 2）
- 飞书（Phase 4）
- Admin UI（Phase 5）

## Impact

| 影响面 | 影响 |
|---|---|
| 仓库结构 | 一次性创建，约定长期生效 |
| 构建系统 | 引入 Makefile，所有后续命令以它为入口 |
| 依赖 | Viper + Zap + cobra（极简依赖集合） |
| CI | GitHub Actions 增加 lint + test job |
| 文档 | CLAUDE.md / README.md / PLAN.md 补完 |

## Success Criteria

- [ ] `make build` 成功产出 `bin/huan-agent` 二进制
- [ ] `./bin/huan-agent version` 输出 `huan-agent v0.1.0 (commit xxx, built yyy)`
- [ ] `./bin/huan-agent serve` 启动后 Ctrl+C 在 5s 内退出，退出码 0
- [ ] `./bin/huan-agent --config /tmp/nonexistent.yaml serve` 退出码非 0，打印清晰错误
- [ ] `make test` 全部通过，覆盖率 ≥ 70% in `internal/config` + `internal/obs`
- [ ] `make lint` 零警告
- [ ] GitHub Actions 在 PR 上跑通

## Risks

| 风险 | 缓解 |
|---|---|
| 选错 CLI 框架（cobra/urfave/cli/kong） | 用 cobra（生态最广，eino 项目也用） |
| Viper 性能/复杂度争议 | MVP 用 Viper，后期如果成为瓶颈再换 |
| openspec CLI 在团队内不一致 | CLI 只用来初始化 + archive，业务开发不依赖 CLI |

## Rollback

无破坏性，纯新增。
