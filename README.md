# huan-agent

基于 CloudWeGo Eino 的个人 AI Agent 平台。

## 快速开始

```bash
# 安装依赖
go mod download

# 复制配置
cp configs/config.example.yaml configs/config.yaml
# 编辑 configs/config.yaml，填入 LLM API key

# 运行（CLI 对话模式）
make run

# 或直接
go run ./cmd/huan-agent chat
```

## 非交互式运行（git hook / CI / cron）

`huan-agent run` 给一个任务、跑完、把答案打到 stdout 并退出。答案走 stdout、日志与进度走
stderr，所以可以直接接管道：

```bash
huan-agent run "读一下 README.md，用一句话说这个项目是什么"
git diff --cached | huan-agent run - "审一遍下面这段 diff，只报 must-fix"
huan-agent run "找出 npm test 的根因" --output json | jq -r .answer
```

退出码是稳定契约：`0` 跑完 / `1` 模型或循环失败 / `2` 用法错误 / `3` 超时 / `4` 预算耗尽 /
`5` 工具失败（配合 `--fail-on-tool-error`）/ `6` 配置问题。详见
[docs/run.md](./docs/run.md)，其中有 pre-commit、GitHub Actions 与 cron 的可复制示例。

## 功能特性

- 多 LLM provider（DeepSeek / Qwen / GLM / OpenAI / Ollama）
- 飞书 IM 接入（WebSocket 长连接）
- 通用 MCP 工具调用
- Skill 机制（提示词 + 工具组合）
- 短期 / 长期记忆 + 上下文自动压缩（Phase 3）
- Token 用量统计
- Web Admin UI

详细规划见 [PLAN.md](./PLAN.md)。

## 开发

```bash
make build      # 编译
make test       # 测试
make lint       # Lint
make fmt        # 格式化
make run        # 运行
```

## License

MIT
