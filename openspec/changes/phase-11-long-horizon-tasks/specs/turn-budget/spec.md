# turn-budget

## 目标

让一轮 agent turn 能跑几十步而不失控：步数、token、墙钟时间三重预算共同界定这一轮能花多少，
超预算时**优雅收口**并把原因交给调用方与用户，而不是把成本变成一次报错。

## API

- `chat.Request{ MaxSteps, MaxTokens, Deadline }` —— 单轮覆盖
- `chat.Config{ MaxSteps, MaxTokens, Deadline, Condenser }` —— runner 默认值
- `chat.Result{ Steps, Usage, Tools, StopReason, TraceID }`
  - `StopReason` ∈ `""`（模型给出了回答）/ `"steps"` / `"tokens"` / `"deadline"`
- `chat.Condenser`：
  `CompressKeeping(ctx, msgs, head int, pinLastUser bool) ([]*schema.Message, string, error)`
  （由 `internal/context.Manager` 满足；runner 只依赖这个接口，不 import `internal/context`，
  于是它可以在没有压缩配置的部署里为 nil，也能在测试里被替换）
- 事件：`EventBudgetStop{ Reason, Step, Tokens, ElapsedMs }`、
  `EventContextCompressed{ Step, Tokens, Text }`

## 行为

- 预算在**每一次模型调用之前**检查；已经花掉的调用不会被撤销，但不会再多花一次。
- 检查顺序固定：步数（由循环边界天然保证）→ 时间 → token。三者同时超限时，报告最先
  命中的那一个（固定顺序让同一个输入得到同一个原因）。
- `MaxSteps <= 0` → `DefaultMaxSteps`；`MaxTokens <= 0` / `Deadline == 0` → 该维度不限。
- 超预算时：`Result.StopReason` 置位，`EventBudgetStop` 先于 `EventDone` 发出，回答文本
  用**最后一条有内容的 assistant 消息**加一段按停因定制的说明；一条都没有时给出
  "未能得出最终回答"。
- 停因说明必须**如实**：说明停在第几步 / 用了多少 token / 跑了多久，并说明下一步能做什么
  （工作区改动已落盘；可以接着说「继续」；或调整对应配置）。不承诺本阶段做不到的事
  （跨重启自动恢复）。
- 收口是正常结束：`Run` 返回 `err == nil`，最后一个事件是 `EventDone`。
- 每次都写一条 warn 日志，带 session、reason、steps、tokens、elapsed。静默停步是缺陷。
- token 预算依赖 provider 上报的 usage：provider 不上报时该维度**不生效**（如实记录，
  不假装算过）。
- 墙钟预算在步与步之间生效（不在飞行中打断模型调用）；硬取消仍然由调用方的 context 负责。

## 接入

- `internal/server`：Web 与飞书两条路径都从配置取三重预算并落到 `chat.Config`，
  Web 的 catalog 暴露 `turn_max_tokens` / `turn_deadline_seconds`。
- `internal/server`：`stop_reason` 随 assistant 消息落库（迁移 11），刷新页面后仍可见。
- `cmd/huan-agent`：`chat.max_steps` / `chat.turn_max_tokens` /
  `chat.turn_deadline_seconds` 三条配置的默认值见 `configs/config.example.yaml` 的
  "长任务" 段落。

## 边界

- 不负责跨轮的任务状态：预算用尽后如何"接着跑"由用户发起下一轮，runner 不自动续跑。
- 不做金额预算；token 与时间已足以界定一次失控。
