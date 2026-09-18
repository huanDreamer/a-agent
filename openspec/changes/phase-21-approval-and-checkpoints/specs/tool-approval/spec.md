# Capability: tool-approval

## Purpose

在写/执行类工具调用落地**之前**放一道人能批、能拒的闸门，并把拒绝的理由送回模型，使它下一步
改对方向而不是换个写法再撞一次。它存在的意义是让人敢把写权限交出去。

## Scope

- `internal/tool` — 审批的词汇表（`Request` / `Decision` / `Approver`），与 `Asker` 同构。
- `internal/agent` — 工具执行前的闸门。
- `internal/server` — 事件、回答端点、Web 卡片。
- `cmd/huan-agent` — CLI 的 approver 与各 surface 的注册决策。
- `internal/config` — `tools.approval.*`。

## Requirements (MUST)

1. **闸门是工具装饰器。** 审批 MUST 实现为一个包裹 `Tool` 的装饰器，在注册时套上（与既有的
   `tool.WithCapability` / `CapabilityFunc` 同一个模式），MUST NOT 由各个工具自行实现（一个
   工具内部的审批会在第五个工具出现时被忘记），也 MUST NOT 只挂在某一个执行循环里——仓库里有
   **两个**循环（`internal/chat/runner.go` 的 Web 循环与 `internal/agent` 的 Eino ReAct，
   后者服务 CLI 与飞书），只改一个就等于让一半 surface 没有闸门。装饰 MUST 加在
   `cmd/huan-agent` 的注册辅助函数里（三个 surface 都经由它们建注册表），使覆盖是结构性的
   而不是靠人记得。
2. **按能力而非按工具名生效。** 策略 MUST 依据工具的能力标签（`CapWrite` / `CapExec`）判定：
   `writes` 覆盖写、`writes+exec` 再覆盖执行、`all` 覆盖全部。新增工具不需要改审批代码。
3. **超时默认拒绝（fail closed）。** 无人应答、轮次被停止、approver 报错时 MUST 判定为**拒绝**。
   MUST NOT 默认放行。这与 `ask_user` 的超时语义（一种合法答案）**刻意相反**，两者 MUST NOT
   共用同一个默认值：审批在没人看的时候，恰恰是最需要生效的时候。
4. **拒绝是有内容的回答。** 拒绝 MUST 作为工具结果交回模型（MUST NOT 终止本轮），MUST 带上拒绝
   理由（若人给了）与"你可以怎么做"的可操作提示。模型 MUST 有权改用别的方案继续。
5. **被拒绝的调用零副作用。** 拒绝 MUST NOT 产生任何磁盘写入、任何进程启动、任何远端调用。
6. **粒度只有两种。** `allow_once`（只放行这一次）与 `allow_turn`（本轮内该工具名免审批）。
   MUST NOT 提供"永久允许"：一次点击变成一条不再被复查的规则，需要它自己的管理界面，属于未来。
   `allow_turn` MUST 在轮次结束时失效。
7. **请求必须可判断。** 请求 MUST 包含：工具名、能力、一行动作摘要、有界预览、建议决定、超时。
   - 摘要 MUST 逐工具定义（写/编辑是路径 + 行数增减；`bash` 是**完整命令行**；多文件是文件清单）。
   - `bash` 的**命令行本身 MUST NOT 被截断**——被截断的命令行使人无法判断自己在批准什么，
     这比不显示更糟。
   - 预览 MUST 有字节上限，超限时截断并说明。
8. **没有 approver 就没有工具。** 一个 surface 无法提问时（例如无人值守的管道、尚未接入卡片的
   通道），`mode != off` 的情况下需要审批的工具 MUST NOT 注册，MUST 在启动时记一条说明。
   MUST NOT 注册一个"调用必然被拒"的工具。
9. **审计。** 每一次决定 MUST 随轮次落盘（工具调用参数中记录决定与理由），并记一条日志，
   含工具名、参数摘要、决定、来源（`human` / `policy` / `timeout`）。MUST 能回答"这一轮里人批准
   过什么"。
10. **顺序。** `deny_patterns` MUST 先于审批生效（它更便宜，且被它拒绝的调用根本不该占用人的
    注意力）。
11. **默认关闭。** `tools.approval.mode` MUST 默认为 `off`，使升级不改变既有无人值守用法（`run`、
    飞书、cron）的行为。打开它是部署方的显式选择。
12. **白名单仍是减速带。** `tools.approval.allow` 是按摘要匹配的正则白名单，命中即免审批。文档
    MUST 明确它是**减速带不是安全边界**，与 `deny_patterns` 的措辞一致——一个等价的、不被匹配的
    动作仍会执行。

## 事件与 API

```json
{ "type": "approval_request", "step": 4, "id": "ap-3f9c", "tool": "bash",
  "capability": "exec", "summary": "执行: git push origin main",
  "preview": [ { "kind": "meta", "text": "cwd: /Users/me/project" } ],
  "default": "deny", "timeout_ms": 300000 }

{ "type": "approval_result", "step": 4, "id": "ap-3f9c",
  "decision": "deny", "reason": "不要动主分支", "source": "human", "auto": false }
```

- `POST /api/chat/sessions/{id}/approvals/{aid}`，请求体 `{ "decision": "allow_once|allow_turn|deny",
  "reason": "…" }`。形状与既有的 `.../questions/{qid}/answer` 对齐：问题 id 是连接请求与回答的钥匙。
- 未知 id → 404；已作答 → 409；轮次已结束 → 409 并说明原因。
- 轮次因 stop / deadline / 进程退出而结束时，挂起的审批 MUST 以 `deny`（原因写明轮次已结束）
  收敛，MUST NOT 留下永远 pending 的卡片。

## 配置

```yaml
tools:
  approval:
    mode: "off"            # off | writes | writes+exec | all
    allow: []              # 按动作摘要匹配的正则；命中免审批（减速带，不是安全边界）
    timeout_seconds: 300
```

## 各 surface 的行为

| surface | approver | `mode=off` | `mode!=off` |
|---|---|---|---|
| Web 控制台 | 卡片 | 正常 | 正常，写/执行前弹卡片 |
| 交互式 CLI | stdin 的 `y` / `t` / `n <理由>` | 正常 | 正常；stdin 不是 TTY 时按"无 approver"处理 |
| 飞书 | 无（本阶段） | 正常 | **不注册**写/执行工具，启动时记一条说明 |
| `run`（一次性） | 无 | 正常 | **不注册**写/执行工具，启动时记一条说明 |

## Non-goals

- 永久允许 / 按路径记忆 / 按风险等级打分的策略引擎。
- 独立的审批审计表（决定随轮次落盘 + 日志已足够回溯）。
- 飞书的审批卡片（独立集成面）。
- `run` 的交互式审批：无人值守时没有可批的时刻。
- 自动提交或自动 stash：检查点管"这一轮"，git 管"这个项目"。
- 把审批当成安全边界：它防的是"人不在时的坏决定"，不是恶意进程。真正的隔离需要容器与
  seccomp，见 `docs/tools.md` 的既有说明。
