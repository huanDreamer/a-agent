# Phase 0 — Tasks

## 1. 工程初始化

- [ ] 1.1 `go mod init github.com/huan/huan-agent`
- [ ] 1.2 添加依赖到 `go.mod`：
  - `github.com/spf13/cobra` (CLI)
  - `github.com/spf13/viper` (config)
  - `go.uber.org/zap` (logging)
- [ ] 1.3 创建目录结构（见 §3 of PLAN.md）：
  - `cmd/huan-agent/`
  - `internal/config/`
  - `internal/obs/` (logger)
  - `configs/`
- [ ] 1.4 编写 `Makefile`（build / test / lint / fmt / run / clean / tidy）

## 2. 配置层

- [ ] 2.1 实现 `internal/config/config.go`：
  - `type Config struct` 包含 Server、Logging、LLM 三个子段
  - `Load(path string) (*Config, error)` 加载 YAML + env 覆盖
  - `Default() *Config` 提供默认值
- [ ] 2.2 编写 `configs/config.example.yaml` 模板
- [ ] 2.3 单元测试 `internal/config/config_test.go`：
  - 默认值
  - YAML 加载
  - env 覆盖
  - 不存在的路径报错

## 3. 日志层

- [ ] 3.1 实现 `internal/obs/logger.go`：
  - `New(level, format string) (*zap.Logger, error)`
  - 支持 `level=debug|info|warn|error`
  - 支持 `format=json|console`
- [ ] 3.2 单元测试 `internal/obs/logger_test.go`：
  - 各 level 启用/禁用
  - JSON / console 格式输出可解析

## 4. 入口与关闭

- [ ] 4.1 实现 `cmd/huan-agent/main.go`：
  - 解析 `--config` 全局 flag
  - 子命令：`version`、`serve`
  - `version` 打印 version / commit / build time
  - `serve` 占位实现：启动后等待 signal
  - signal handler：`SIGINT` / `SIGTERM` 触发 context cancel
  - 5s 超时强制退出
- [ ] 4.2 注入 ldflags 注入 version 信息到 `internal/version` 包

## 5. CI

- [ ] 5.1 创建 `.github/workflows/ci.yml`：
  - `go-version: '1.22'`
  - jobs: `lint`（golangci-lint）、`test`（go test + race + coverage）
  - 触发：push 到 main / PR

## 6. 验证

- [ ] 6.1 `make build && ./bin/huan-agent version` 验证 version 输出
- [ ] 6.2 `./bin/huan-agent serve` + Ctrl+C 验证 graceful shutdown
- [ ] 6.3 `make test` 全绿
- [ ] 6.4 `make lint` 零警告
- [ ] 6.5 `git commit` + push，验证 CI 通过

## 7. 收尾

- [ ] 7.1 更新 `CLAUDE.md` 添加 Phase 0 完成的说明
- [ ] 7.2 起草 Phase 1 change proposal
- [ ] 7.3 归档 Phase 0：`mv openspec/changes/phase-0-bootstrap openspec/changes/archive/`
