# Phase 19 — 一次性执行模式：`huan-agent run`

## Summary

今天 huan-agent 只有**交互式**入口。全部子命令是：`chat`（REPL，`cmd/huan-agent/chat.go:47`）、
`serve`、`admin serve|set-password`、`viking status|sync|save|remember|search`、`usage`、
`version`。`chat` 的 flag 只有 `--provider / --model / --system / --tools / --skill`
（`cmd/huan-agent/chat.go:79-83`），**没有 `--print`，也没有任何"给一个 prompt、跑完退出"的
入口**。

后果是它进不了任何一个自动化通道：

```bash
# 想做的（现在做不到）
huan-agent run "把 internal/chat/runner.go 的工具循环改成并发，跑通 go test ./..."
# git hook：提交前审一遍
huan-agent run "审一遍暂存区的 diff，只报 must-fix，没有就输出 OK" 
# CI：挂了自动定位
huan-agent run "npm test 失败了，找出根因并给出最小修复"
```

本变更新增 `run` 子命令：**给一个 prompt，跑完、把答案写 stdout、按结果给退出码**。它是一个
纯增量的入口——复用 `chat` 已有的 agent 构建路径（provider / model / system / skill / MCP /
workspace 工具），不引入第二套 agent 装配。

除了"能进 CI"这个直接价值，它还是**后续每个阶段的验收工具**：Phase 20 的诊断、Phase 21 的
审批、Phase 22 的并发，都需要"跑一轮真实 agent 并断言结果"。没有一次性入口时，这类验收只能
靠单元测试自证——而 Phase 18 那个漏改恰恰就是单元测试自证不了的那一类 bug。

## Why

- **stdout 是产品，stderr 是噪声。** 一次性模式下 `huan-agent run "..." > answer.md` 必须拿到
  **干净的答案**。所以答案走 stdout、日志/进度/用量走 stderr，二者不混。这也是与 `chat` 最
  大的区别：REPL 里人眼能容忍交错，管道不能。
- **退出码是它与 shell 的契约。** "模型回答完了"和"任务成功了"是两件事。CI 需要能区分
  "agent 正常回答" / "模型调用失败" / "超时" / "工具失败"，否则这个命令在流水线里只能当装饰。
  退出码是**接口**而不是可调参数，所以它是常量而不是 Viper 配置项（`CLAUDE.md` 的"参数走
  Viper"针对的是可调行为，把它做成配置项只会让每个调用点都要先读配置才知道 0 是什么意思）。
- **非交互式必须显式声明，不能靠运气。** `ask_user` 需要一个会渲染卡片、会提交答案的前端
  （`cmd/huan-agent/chat.go:471` 只在 `opts.Surface == "web"` 时注册它）。一次性运行没有这个
  通道：注册它只会换来一次必然失败或永久挂起的调用。同理 `bash_background` 系列的语义是
  "留下一个进程"，而一次性运行退出后就没人管它了——默认必须不给。
- **它让"这个 agent 到底能不能干活"变成可测问题。** 今天要验证任何行为都得开一个 REPL、
  手敲、肉眼读。有了 `run` + `--json`，同一件事变成一条断言。
- **成本可见。** CI 里跑 agent 必须能回答"这次花了多少"。所以用量汇总要出现在 `--json` 里，
  而不是只在控制台的用量页。

## What Changes

### ADDED

1. **`cmd/huan-agent/run.go`：`run` 子命令**
   - 用法：`huan-agent run "<prompt>"`，或 `huan-agent run -` 从 stdin 读 prompt（便于
     `git diff --cached | huan-agent run -`）。prompt 为空且不是 `-` 时报用法错误（退出码 2）。
   - flag：`--provider` / `--model` / `--system` / `--skill`（与 `chat` 同名同义）、
     `--workspace <dir>`（沙箱根，缺省为进程工作目录）、`--output text|json`、
     `--timeout <dur>`、`--max-steps <n>`、`--session <id>`（追加到既有会话，便于在控制台
     回看这一次运行）、`--no-tools`、`--allow-background`、`--fail-on-tool-error`、`--quiet`。
   - **工具默认打开**（与 `chat` 的 `--tools` 相反）：没有工具的一次性运行只是一次便宜的
     补全调用，而"进 CI 干活"正是它的目的。
   - 退出码常量（写进 `docs/`，是稳定契约）：`0` 成功；`1` 模型/agent 失败；`2` 用法错误；
     `3` 超时；`4` 步数或 token 预算耗尽；`5` 有工具调用失败且设了 `--fail-on-tool-error`；
     `6` 配置错误（缺 API key、工作区不存在、provider 不支持工具调用）。

2. **输出契约**
   - `--output text`（默认）：答案流式写 stdout 并逐块 flush（`run | tee` 要能实时看到）；
     每一步的工具名、耗时、用量写 stderr。`--quiet` 只留错误。
   - `--output json`：stdout 只有**一个 JSON 对象**，包含 `answer`、`steps`（每步的工具调用与
     观察摘要）、`usage`（prompt/completion/total token 与费用估算）、`provider`、`model`、
     `session_id`、`stop_reason`（`done` / `steps` / `budget` / `deadline` / `error`）、
     `tool_failures`。这个形状是 wire 契约，Go 侧用结构体定义并测试，不靠 map 拼。
   - 两种模式下 stdout MUST NOT 出现 Zap 日志或进度行；实现方式是把日志 sink 指向 stderr、
     把 stdout 包在一个 writer 里独占给答案。

3. **`cmd/huan-agent`：一次性的能力裁剪**
   - 新增 surface 名 `run`（与既有 `cli` / `feishu` / `web` 并列），传给
     `registerBuiltinTools` 的 `toolSetOptions.Surface`。
   - `Surface == "run"` 时**不注册** `ask_user`（没有答案通道）与 `bash_background` 系列
     （`--allow-background` 才注册，且退出前必须停掉本进程启动的全部作业）。
   - `Surface == "run"` 时系统提示词追加一段非交互约束：不能提问、遇到歧义选最保守方案并在
     答案里说明假设、不要等待人工确认。

4. **配置**
   - `run.timeout_seconds`（默认 300，0 = 不限制）、`run.max_steps`（默认继承 `chat.max_steps`，
     当前 12）、`run.default_output`（默认 `text`）。全部进 `configs/config.example.yaml`
     与 `SetDefaults`。

5. **测试**
   - `cmd/huan-agent/run_test.go`：prompt 为空 → 退出码 2；`-` 从 stdin 读；stdout 里不含日志
     与进度（这是最容易回归的契约，用伪造的 stderr/stdout 缓冲区断言）；`--output json` 只
     产生一个可解析的 JSON 对象；超时 → 3；步数耗尽 → 4；工具失败 + `--fail-on-tool-error` → 5；
     缺 API key → 6。
   - 断言 `Surface == "run"` 时 `ask_user` 与 `bash_background` 不在注册表里，
     `--allow-background` 时才出现。
   - 端到端（可 `//go:build integration`）：一个真实但廉价的 prompt，断言退出码 0 且答案非空。

6. **文档**
   - `docs/tools.md` 或新 `docs/run.md`：用法、退出码表、三个真实场景（git hook / CI / cron）的
     可复制示例、`--json` 形状。
   - `README.md` 补一行"非交互式用法"。
   - 明确记录 stdout/stderr 分工，以及在 CLI 里为 stdout 写答案是对
     `CLAUDE.md`"禁止 `fmt.Println`"的**有意豁免**：答案必须落在 stdout，而日志仍走 Zap→stderr；
     豁免范围限于一个独占的 output writer，不允许散落的 `fmt.Println`。

### 明确不做（本阶段）

- **不做独立的 `--print` 模式给 `chat`。** 一个"混合了 REPL 与管道"的命令会把上面那条
  stdout/stderr 契约弄脏；一次性运行是一个独立 surface，不是 `chat` 的一个 flag。
- **不做后台/守护式任务执行。** `run` 是同步的：调用方拿到退出码才算完。真正的调度器
  （cron、队列、重启续跑）是另一个 change，它需要的是持久化任务而不是一个 flag。
- **不做交互式审批。** `run` 里没有人工可批的时刻；本阶段的安全边界仍是既有的
  `deny_patterns` + `tools.read_only`，以及**默认不给** `bash_background`。审批属于 Phase 21，
  届时 `run` 的语义是"审批策略为 `off` 或按配置预授权"，见那里的 Non-goals。
- **不做多轮会话续跑。** `--session` 只是把这次运行挂到一个既有会话上便于回看，不提供
  "接着上一轮跑"的语义（那是控制台的 `resume`）。
- **不改 `chat` 的任何行为。**

## Impact

- 新增文件：`cmd/huan-agent/run.go`、`cmd/huan-agent/run_test.go`、`docs/run.md`。
- 改动：`cmd/huan-agent/main.go`（注册子命令）、`cmd/huan-agent/chat.go`（把 surface 与输出
  writer 的装配抽成两处共用，避免复制整段 agent 构建）、`internal/config/config.go`
  （`run.*` 默认值）、`configs/config.example.yaml`、`README.md`。
- 依赖：Phase 18（否则 CLI 路径在默认配置下起不来，`run` 无法验收）。
- 兼容性：纯增量。既有子命令、既有 flag、既有配置全部不变；`run.*` 有默认值，不配置也能用。
- 验收：`huan-agent run "读一下 README.md，用一句话说这个项目是什么" --output json` 返回退出码
  0 且 stdout 是一个可 `jq` 解析的对象；把它接进一条 `git diff --cached | huan-agent run -`
  的本地 hook 能正常工作。
