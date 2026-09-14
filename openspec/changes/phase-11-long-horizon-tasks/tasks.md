# Phase 11 任务清单

## 1. `internal/context`：可钉住的压缩

- [x] `Pin{Head, LastUser}` + `CompressWith`，`Compress` 保持原语义（`Compress` = `CompressWith(Pin{})`）
- [x] `CompressKeeping(ctx, msgs, head, pinLastUser)` 薄封装（满足 `chat.Condenser`）
- [x] 输出顺序固定为 head → 摘要 → 钉住的 user → 最近 K 条；钉住的消息不进摘要；无可折叠段原样返回；`Head > len` 与负值都按边界处理
- [x] 顺带修掉真实缺陷：`sessionMemory.history()` 原来的 `Compress` 会把 system 提示一起折进摘要，压缩一次技能/提示就没了 —— 改用 `CompressKeeping(ctx, msgs, 1, true)`
- [x] 压缩不修改调用方的切片（runner 拥有自己的历史，会被复用）
- [x] 测试：头部保留、最后一条 user 保留、顺序正确、不 mutate、`Compress` 行为不变
- [x] **缺陷修复**：`ShouldCompress` / `CompressWith` 对 nil receiver 返回“无压缩”。真实运行发现：`*Manager(nil)` 装进 `chat.Condenser` 接口后接口非 nil，默认配置（`context.max_tokens: 0`）下每一轮对话都 nil dereference panic。`turnCondenser` 改成返回接口类型（off 即真 nil），并在 `internal/chat` 补了 typed-nil 回归测试

## 2. `internal/chat`：三重预算 + 逐步压缩 + 可见停因

- [x] `Request` / `Config` 增加 `MaxTokens`、`Deadline`、`Condenser`
- [x] `Condenser` 接口（不 import `internal/context`，理由与 `chatTracer` 一致）
- [x] 每步调用前检查时间/token 预算；循环边界管步数；检查顺序固定（deadline → tokens），同一输入同一停因
- [x] `Result.StopReason`（`steps` / `tokens` / `deadline`）+ `BudgetExhausted()`
- [x] 新事件 `EventBudgetStop` / `EventContextCompressed`（reason / step / tokens / elapsed_ms / text）
- [x] 收口文本按停因分别说明「停在哪、花了多少、下一步能做什么」；无 assistant 文本时走另一句，且不谎称工作区已改动
- [x] warn 日志：session / reason / steps / tokens / elapsed / tools
- [x] `MaxStepsCeiling = 500`：超限 `New` 直接报错（不静默截断）；负数预算同样报错
- [x] 测试：三种停因各自触发、事件顺序（budget_stop 先于 done）、文本内容、无 assistant 文本的兜底、压缩每步调用、压缩失败不致失败、typed-nil 不 panic、`New` 的边界
- [x] 集成测试（用真的 `context.Manager`）：40 步长任务窗口 ≤ 12 条、系统提示与本轮目标每轮都在；token 预算在越过的那一步之前收口

## 3. `internal/config`：预算与上限

- [x] `chat.turn_max_tokens`、`chat.turn_deadline_seconds`（默认都是 0 = 不限，升级后行为不变）
- [x] `chat.max_steps` 的校验放到 `chat.New`（> 500 报错）；`agent.max_steps` 由静默 clamp 改为 `MaxStepsCeiling = 200` + warn
- [x] `tools.bash_max_timeout_seconds`（默认 900；低于 `bash_timeout_seconds` 时抬到它）
- [x] 测试：`TurnDeadline()` / `BashMaxTimeout()` 的默认值、负数、上限边界

## 4. `bash`：单次超时可延长

- [x] `BashPolicy.MaxTimeout` + `DefaultBashMaxTimeout`（900s）
- [x] `timeout_ms` 在 `[1ms, MaxTimeout]` 内自由增减；超过则截到上限并在结果里说明
- [x] `BashOutput.timeout_ms` / `timeout_capped`，stderr 附上限说明；工具描述与 `timeout_ms` 的 jsonschema 文案同步（描述里无逗号陷阱，schema_test 仍通过）
- [x] 测试：延长生效（1s 命令在 2s override 下跑完）、上限截断并标记、缩短仍生效、`bashConfigFrom` 抬升低于默认的上限

## 5. 接线：server / feishu / cli

- [x] `server.Config{ChatMaxTokens, ChatTurnDeadline}` → `chat.Config`
- [x] `ChatDeps.Condenser` 由调用方注入（`internal/server` 不需要知道“用哪个模型做摘要”）
- [x] 飞书 `serve.go` 同样接线，并在预算停时写 warn（reason / steps / tokens / tools / session）
- [x] catalog 暴露 `turn_max_tokens` / `turn_deadline_seconds`
- [x] store 迁移 11：`chat_messages.stop_reason`，落库 + 读回（含“模型自己答完”必须为空）
- [x] `cmd/huan-agent/turnbudget.go`：一处构造 condenser，Web 与飞书共用；`context.max_tokens: 0` 且步数偏大时启动即 warn

## 6. 控制台

- [x] `budget_stop` 渲染成独立提示行（含「继续」按钮），`context_compressed` 渲染成低调的一行说明
- [x] 重新加载的会话仍显示停因（读 `stop_reason`），与 live 事件同一套文案
- [x] 新增 `clock` / `layers` 图标与 `.banner.notice` / `.turn-notices` 样式，沿用既有告警色而非错误色
- [x] `vite build` + `npm run check:ui` 通过，dist 已重建

## 7. 文档

- [x] `configs/config.example.yaml`：每个新键的说明 + 一段「长任务推荐档位」
- [x] `docs/long-tasks.md`（新）：三重预算、为什么光调步数不够、逐步压缩、停因语义、接着跑、排障表
- [x] `docs/tools.md`：`bash` 超时可延长与上限、排障条目
- [x] agent / context 段落的注释同步（25 → 200；context 说明“每轮一次 + 每步一次”）

## 8. 质量门禁

- [x] `gofmt` 干净（除工作区里既存的 `internal/tool/builtin/calc.go`、`internal/agent/agent_test.go`、`internal/mcp/*` 未提交改动）
- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test ./...` 全绿；`-race` 跑 chat / context / builtin 通过
      （`internal/server` 的 `-race` 在 `TestOpenVikingAPI_FlushCallsThrough` 报数据竞争：那是未提交的 `openviking_test.go` 里 stub 自身的读写竞争，与本次改动无关）
- [x] 真实端到端（deepseek-chat，临时 DB + 临时工作区）：
      1. `chat.max_steps: 2` 的创建三文件任务 → 2 步后 `budget_stop`（reason=steps, tokens=16240, elapsed=2113ms, tools=4），回答带说明与「继续」，`stop_reason: "steps"` 已落库，日志有 warn，三个文件确实写进了磁盘；
      2. `context.max_tokens: 1200` + `keep_recent: 4` 的长任务 → 第 4、5 步各触发一次 `context_compressed`（9 条 → 7 条），日志 `condensed the in-loop history`，窗口没有随步数增长；
      3. 同一会话再发「继续」可以接着跑。
