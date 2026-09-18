# Capability: agent-cli

## Purpose

让 huan-agent 在**没有人坐在终端前**的时候也能被使用：一次调用、一个 prompt、一份干净的
答案、一个可判断的退出码。它服务的场景是 git hook、CI 与 cron —— 也就是"提交前审一遍"
和"CI 挂了自动定位"这两类高频写代码用法。

## Scope

- `cmd/huan-agent` — `run` 子命令、输出契约、退出码、一次性 surface 的能力裁剪。
- `internal/config` — `run.*` 配置项。
- `internal/prompt` — 非交互式运行的系统提示词补充。
- `docs/` — 用法与退出码文档。

## Requirements (MUST)

1. **单一入口。** `huan-agent run "<prompt>"` MUST 在回答完成后退出。prompt 也 MUST 支持从
   stdin 读入（`run -`），使 `git diff --cached | huan-agent run -` 成立。prompt 缺失或参数
   多余时 MUST 以用法错误退出。
2. **stdout 只放答案。** stdout MUST 只包含模型的答案（`--output text`）或一个 JSON 对象
   （`--output json`）。日志、进度、用量、告警 MUST 全部写 stderr。这一条 MUST 由测试守卫：
   这是管道可用性的全部依据。
3. **答案流式输出。** `--output text` MUST 逐块写出并 flush，使 `run | tee` 能实时看到；
   MUST NOT 缓冲到结束才写。
4. **退出码是稳定契约。**

   | 码 | 含义 |
   |---|---|
   | 0 | 成功完成 |
   | 1 | 模型 / agent 运行失败 |
   | 2 | 用法错误（prompt、flag） |
   | 3 | 超时 |
   | 4 | 步数或 token 预算耗尽 |
   | 5 | 存在工具调用失败且启用了 `--fail-on-tool-error` |
   | 6 | 配置错误（缺 API key、工作区不存在、provider 不支持工具调用） |

   这些码 MUST 是常量而非配置项：它们是调用方脚本的分支依据，把它变成可配置值只会让
   "退出码 0 是什么意思"取决于部署。
5. **工具默认打开。** `run` MUST 默认注册文件、搜索与命令工具。`--no-tools` MUST 能关掉。
   理由：没有工具的一次性运行等于一次廉价补全调用，而本命令存在的意义正是让 agent 在
   无人值守时干活。
6. **非交互式裁剪。** 在 `run` 这个 surface 上，需要人在场的能力 MUST NOT 注册：
   `ask_user`（没有渲染卡片与提交答案的通道）以及 `bash_background` 系列（一次性运行退出后
   无人管理这些进程）。后者 MUST 只在显式 `--allow-background` 时注册，且进程退出前 MUST
   停掉本次运行启动的全部作业。
7. **非交互式提示词。** `run` 的系统提示词 MUST 声明：不能提问；遇到歧义选最保守方案并在答案
   里说明所选的假设；不要等待人工确认。
8. **可脚本化结果。** `--output json` MUST 输出恰好一个 JSON 对象，包含 `answer`、`steps`、
   `usage`、`provider`、`model`、`session_id`、`stop_reason`、`tool_failures`；
   `stop_reason` ∈ `done` / `steps` / `budget` / `deadline` / `error`。失败运行 MUST 同样产出
   合法对象（而不是半截 JSON），使调用方能解析失败原因。
9. **成本可见。** 用量（prompt / completion / total token 与费用估算）MUST 出现在 stderr 汇总
   与 `--output json` 里；价格表未匹配时 MUST 如实标注为未定价，MUST NOT 报 0。
10. **可在控制台回看。** 每次运行 MUST 关联一个会话并给出 `session_id`，使其能在控制台里
    看到他做了什么。`--session` 指定既有会话时 MUST 追加进去而不是新建。

## 输出示例（wire 契约）

```json
{
  "answer": "这个项目是基于 CloudWeGo Eino 的个人 AI Agent 平台……",
  "provider": "deepseek",
  "model": "deepseek-chat",
  "session_id": "9f1c…",
  "stop_reason": "done",
  "tool_failures": 0,
  "usage": { "prompt_tokens": 812, "completion_tokens": 143, "total_tokens": 955,
             "cost": { "total": 0.0004, "priced": true } },
  "steps": [
    { "step": 1, "tools": [ { "name": "read_file", "ok": true, "duration_ms": 3 } ] },
    { "step": 2, "tools": [] }
  ]
}
```

## Non-goals

- **不做交互式审批。** 无人值守时没有可批的时刻。`run` 的安全边界是既有的
  `deny_patterns`、`tools.read_only`，以及默认不给 `bash_background`。Phase 21 引入审批后，
  `run` 的对应语义是"策略为 `off` 或由配置预授权"，而**不是**在管道里弹一张卡。
- **不做后台/守护式调度。** `run` 是同步的。cron、队列、重启续跑需要一个持久化任务模型，
  属于另一个 change。
- **不做多轮续跑。** `--session` 是为了回看，不是为了"接着上一轮跑"。
- **不给 `chat` 加 `--print`。** 一次性运行是独立 surface，把两种模式塞进一个命令会污染
  第 2 条的 stdout 契约。
- **不做 TTY 检测后的行为分叉。** 输出模式由 `--output` 显式决定，不由"是不是终端"推断——
  推断会让同一行命令在本地与 CI 里行为不同。
