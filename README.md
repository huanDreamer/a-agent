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

## 功能特性

- 多 LLM provider（DeepSeek / Qwen / GLM / OpenAI / Ollama）
- 飞书 IM 接入（WebSocket 长连接）
- 通用 MCP 工具调用
- Skill 机制（提示词 + 工具组合）
- 短期 / 长期记忆
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
