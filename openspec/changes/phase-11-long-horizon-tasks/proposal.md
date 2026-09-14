# Phase 11 — 长任务：让一轮 turn 撑得住几十步

## Summary

「复杂任务」在今天跑不完，而且不是一处卡住：

1. 一轮 `model → tool → model` 循环默认 **12 步**（`chat.DefaultMaxSteps`），CLI 的
   `agent.max_steps` 还被硬 clamp 到 25（`internal/agent/agent.go:66`，静默）。
2. **一轮之内没有任何上下文管理**。`internal/context` 只在 `sessionMemory.history()`
   里被调用一次 —— 那是**每轮开始时压一次**，CLI 与飞书享受得到，Web 控制台的
   `buildHistory` 完全不经过它。`runner.Run` 的循环里，每一步都把「原始历史 + 之前
   所有 assistant/tool 消息」整体重发：12 步能忍，40 步时 token 成本近似平方增长，
   最后撞模型上下文上限**报错**，而不是优雅收口。
3. **停下来是静默的**：`exhaustedMessage` 只往回答文本里塞一句中文，没有事件、没有
   日志、没有可查询的字段，`res.Steps == maxSteps` 是唯一机器可读信号。
4. **单次命令超时只能缩短**：`bash` 的 `timeout_ms` 上限就是配置的默认值，一次十分钟的
   `go test ./...` / `npm install` 必然被杀。
5. **没有第二道闸**：步数是唯一的预算，token 和墙钟时间都没有上限，跑到第 200 步的
   成本无人预知。

本次变更给一轮 turn 装上**三重预算 + 逐步压缩 + 可见停因**，让几十步的长任务在成本
可预期、原因可解释的前提下跑完。

## Why

- **步数只是最钝的那把刀。** 把 12 调成 120 而不做压缩，等于用平方级的 token 成本换
  步数，最后仍然是报错收场 —— 只是账单先到。要跑得久，必须同时让窗口有界。
- **长任务必须能解释自己为什么停。** 现在用户看到的是一句混在回答里的中文，无法区分
  「模型答完了」与「预算用尽了」，也无法在页面关闭后再看到原因。审计与排障都需要一个
  字段，而不是一段散文。
- **用户已经在用小时级任务思考。** 一轮跑 30 分钟、60 步，是「大重构 / 全量测试 /
  批量改文件」的常态。此时 token 与时间的上限比步数更贴近真实成本。
- **`timeout_ms` 只能缩短这条规则把长命令逼进死角。** 想跑久只能改全局默认，而那会影响
  所有调用；正确做法是给一个**上限**，单次调用可以在上限内自由延长。

## What Changes

### ADDED

1. **单轮预算（`internal/chat`）**
   - `Request` / `Config` 新增 `MaxTokens int`、`Deadline time.Duration`（与既有
     `MaxSteps` 一起构成三重预算；0 表示不限）。
   - 预算在**每一步的模型调用之前**检查，超了就当轮收口，`Result.StopReason` 取
     `steps` / `tokens` / `deadline` 之一。
   - 新事件 `EventBudgetStop`（带 `reason` / `step` / `tokens` / `elapsed_ms`）与
     `EventContextCompressed`，让 UI 与飞书能看到「第几步、为什么停、压了多少」。
   - 停因写 warn 日志（session / reason / steps / tokens / elapsed）。
   - 收口文本按停因分别说明，并如实告知下一步（工作区改动已落盘，可以接着说「继续」，
     或调大配置）。
2. **循环内的窗口压缩（`internal/context`）**
   - `Manager.CompressWith(ctx, msgs, Pin{Head, LastUser})`：保留开头 `Head` 条
     （system 提示）与**最后一条 user 消息**（本轮到底要干什么），中间折叠成一条摘要；
     `Compress` 保持原语义（`Pin{}`）。
   - runner 每一步调用前压缩（`internal/chat` 只依赖一个 `Condenser` 接口，不 import
     `internal/context`）。
   - 顺带修掉一个真实缺陷：`sessionMemory.history()` 现在会把 system 提示一起折进摘要
     —— 压缩一次，技能/系统提示就没了。改为 `Pin{Head: 1}`。
3. **配置（`internal/config`）**
   - `chat.turn_max_tokens`（默认 0 = 不限）、`chat.turn_deadline_seconds`（默认 0 = 不限）。
   - `chat.max_steps` 与 `agent.max_steps` 增加校验与**命名上限**，超限报错而不是静默
     截断；`internal/agent` 的 25 硬 clamp 变成 `MaxStepsCeiling = 200` 并写 warn。
   - `tools.bash_max_timeout_seconds`（默认 900）：单次 `timeout_ms` 的上限。
   - `context.*` 的压缩配置对三条路径（CLI / 飞书 / Web）统一生效。
4. **`bash` 超时可延长**
   - `BashPolicy.MaxTimeout`：`timeout_ms` 可以在 `[1ms, MaxTimeout]` 内自由增减，
     超过上限则**截到上限并在结果里说明**（不报错浪费一步）；`BashOutput` 增加
     `timeout_ms` 与 `timeout_capped` 字段。
5. **停因可查**
   - store 迁移 11：`chat_messages.stop_reason`，Web 重新打开对话后仍能看到「这一轮是
     预算停的」。
   - 控制台：SSE 的两个新事件渲染成独立提示行（不混进回答正文），catalog 暴露
     `turn_max_tokens` / `turn_deadline_seconds`。

### 明确不做（本阶段）

- **跨轮/跨重启的持久化任务与自动续跑**：「继续」目前靠历史里的回答文本 + 工作区落盘
  的文件接续，工具观察值（tool result）不跨轮重放。小时级任务在重启后接着跑，需要一张
  任务表和 resume 调度，那是下一个 change（见 Non-goals）。
- 飞书进度卡片的实时更新（仍旧是"思考中"占位 + 完成后的结果卡）。
- 每轮金额预算（只做 token；换算成钱交给用量统计那一层）。

## Impact

- `internal/chat`：`runner.go`、`events.go`（新事件、`Result.StopReason`、`Condenser` 接口）。
- `internal/context`：`CompressWith` + `Pin`（`Compress` 行为不变）。
- `internal/config`：chat / agent / tools 三段新增字段与校验。
- `internal/server`：预算接线、catalog 暴露、`stop_reason` 落库与渲染。
- `internal/store`：迁移 11。
- `internal/tool/builtin`：`bash` 的 `MaxTimeout`、`timeout_ms` schema 与结果字段。
- `cmd/huan-agent`：`bashPolicy`、飞书与 admin 的 runner 构造。
- `web/`：SSE 两个新事件的渲染。
- 文档：`configs/config.example.yaml`、`docs/tools.md`、新增 `docs/long-tasks.md`。
