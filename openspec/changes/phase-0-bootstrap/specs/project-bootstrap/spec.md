# project-bootstrap

## Purpose

为 `huan-agent` 提供可编译运行、可配置、可观测、可平滑关闭的工程骨架，是所有后续 phase 的依赖基础。

## Requirements

### R1: 命令行入口

系统 MUST 提供单一二进制 `huan-agent`，支持以下子命令：

- `huan-agent version` — 打印版本信息（版本号、commit、构建时间）
- `huan-agent serve` — 启动服务（MVP 为占位，后续 phase 扩展）
- `huan-agent --config <path> <subcommand>` — 全局 `--config` flag 指定配置文件

### R2: 配置文件加载

系统 MUST 支持 YAML 格式配置文件，并通过环境变量覆盖：

- 配置文件路径：`--config` flag > `./configs/config.yaml` > `~/.config/huan-agent/config.yaml`
- 环境变量格式：`HUAN_<SECTION>_<KEY>`，例如 `HUAN_LOGGING_LEVEL=debug`
- 环境变量 MUST 优先于配置文件
- 配置文件不存在 MUST 返回明确错误，不静默使用默认值
- 配置结构 MUST 包含 `server` / `logging` / `llm` 三个子段

### R3: 结构化日志

系统 MUST 使用 `zap` 作为日志库：

- 支持 level：`debug` / `info` / `warn` / `error`
- 支持 format：`json` / `console`
- 默认：`info` level + `console` format
- 全局 MUST 通过 `zap.L()` 访问，禁止业务代码 new 一个新 logger
- 业务代码 MUST 禁止使用 `fmt.Println` / `log.Println`

### R4: 优雅关闭

服务 MUST 支持 graceful shutdown：

- 监听 `SIGINT` / `SIGTERM`
- 收到信号后 MUST 取消顶层 context
- 必须在 5 秒内完成清理并退出
- 超过 5 秒未退出 SHOULD 强制退出码 1
- 退出时 MUST 记录关闭日志

### R5: 构建与发布

- `make build` MUST 产出 `bin/huan-agent` 单文件二进制
- version 信息 MUST 通过 ldflags 注入
- 二进制 MUST 支持 `-ldflags="-s -w"` 体积优化

### R6: 测试与质量

- `make test` MUST 全部通过
- `internal/config` + `internal/obs` 模块覆盖率 MUST ≥ 70%
- `make lint` MUST 零警告（golangci-lint: errcheck / govet / staticcheck）

## Out of Scope

- LLM 接入（phase 1）
- 飞书 / 任何 IM 接入（phase 4）
- 任何业务功能
- 性能优化（除 binary 大小）

## Dependencies

无外部服务依赖。
