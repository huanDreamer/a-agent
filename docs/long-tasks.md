# 长任务：让一轮 turn 撑得住几十步

huan-agent 的一轮 turn 是一个循环：模型要工具 → 工具执行 → 把结果交回模型 → 模型再要工具。
「复杂任务跑不完」几乎总是这个循环被三件事中的一件截断，而它们的修法完全不同。

## 三重预算

| 预算 | 配置 | 默认 | 它挡住的事 |
|---|---|---|---|
| 步数 | `chat.max_steps` | 12 | 模型不停地要工具调用 |
| token | `chat.turn_max_tokens` | 0（不限） | 步骤不多但每步很贵（长工具输出） |
| 时间 | `chat.turn_deadline_seconds` | 0（不限） | 不靠步数也不靠 token 的慢：卡住的构建、等网络超时的命令 |

三个都要看，因为它们的失败方式不一样：**只有步数上限时，一个把大文件反复读进上下文的模型会用十步烧掉一整天的额度**；
只有 token 上限时，一个每步都很便宜但永远在打转的模型会烧掉一整晚。

步数上限超过 500 会被 `chat.New` **拒绝**（不是截断）—— 那几乎总是笔误，而启动时发现是免费的。
CLI 的 ReAct 路径（`huan-agent chat --tools`）用的是 `agent.max_steps`，上限 200 并会告警：
它没有 token 与时间预算，步数就是唯一的闸门。

## 为什么光调大步数不够

runner 的循环里，每一步都把「历史 + 到目前为止的所有 assistant/tool 消息」整体发给模型。
12 步还能忍；60 步就是几百条消息，token 成本随步数近似平方增长，最后不是优雅收口，
而是撞模型上下文上限**报错**。

所以长任务需要第二半：`context.max_tokens` 开启**循环内压缩**。
每一步调用模型之前，runner 会：

- 逐字保留开头的 system 提示（规则）与**最后一条 user 消息**（这一轮到底要干什么）；
- 把中间那段（前面若干步的工具往返）折叠成一条摘要；
- 于是窗口有界，成本随步数**线性**而不是平方增长。

`context.keep_recent` 决定最近多少条逐字保留。压缩本身要花一次模型调用，这是有意的取舍：
一次小调用换窗口有界，远比撞上下文上限便宜。

> 想跑长任务但 `context.max_tokens: 0` 时，启动日志里会有一条 warn 提醒这件事。

## 推荐档位（几十步 / 十几分钟）

```yaml
chat:
  max_steps: 60
  turn_max_tokens: 400000
  turn_deadline_seconds: 3600
context:
  max_tokens: 60000        # 模型窗口的 60%~70%
  keep_recent: 12
  summarize: true
tools:
  bash_timeout_seconds: 300       # 单条命令默认给多久
  bash_max_timeout_seconds: 1800  # 模型用 timeout_ms 最多能要到多久
```

`bash` 的单次超时可以在 `[1ms, bash_max_timeout_seconds]` 内自由增减：
一次十分钟的 `go test ./...` 用 `timeout_ms` 要时间即可，不必把全局默认调大；
超出上限会被截到上限，并在结果里用 `timeout_capped` 与 stderr 说明。
常驻进程（dev server、数据库）仍然走 `bash_background` + `bash_output`，不要用 `bash` 硬撑。
它们**属于启动它的那次对话**：对话标题栏上的 `N 个后台进程` 点开就是列表、日志和停止按钮
（同一工作区里别的会话起的进程列在下面一组，标注来源）。模型那边仍然可以用 `bash_jobs` /
`bash_output` / `bash_stop` 自己看和停 —— 它看到的范围是**本工作区**，与界面一致。

## 停下来的时候会发生什么

预算用尽不是错误：`runner.Run` 返回 `err == nil`，最后一个事件是 `done`，
前面几十步写的文件都还在磁盘上。但**它必须说出来**，所以：

- `Result.StopReason` 取 `steps` / `tokens` / `deadline` 之一（模型自己答完时为空）；
- 事件 `budget_stop`（`reason` / `step` / `tokens` / `elapsed_ms`）先于 `done` 发出；
- 回答末尾附一句按停因定制的话，说明停在第几步、花了多少 token、跑了多久，以及下一步能做什么；
- 日志里有一条 warn，带 session / reason / steps / tokens / elapsed / tools；
- `chat_messages.stop_reason` 落库，所以**刷新页面之后**这一轮仍然带标记；
- 控制台把 `budget_stop` 渲染成一条独立提示（不混进回答正文），并给「继续」按钮。

窗口被压缩时另有一条 `context_compressed` 事件，控制台渲染成一行低调的说明：

```
上下文已压缩：37 条消息 → 14 条（保留系统提示与本轮目标）
```

## 接着跑

本阶段**没有**跨轮的自动续跑：预算用尽后由你来发起下一轮。

- 说「继续」通常就够了：下一轮会带上这一轮的回答文本，而工作区的文件已经在磁盘上。
- 但**上一轮的工具输出不会跨轮重放**（store 只保留文本，重放工具往返正是让 provider 拒绝请求的原因）。
  所以如果下一步需要某个上一轮才知道的细节，直接问它，或让它重新读一次。
- 一小时级、且中途可能重启的任务，需要一张持久化的任务表与 resume 调度 —— 那是下一个 change
  （见 `openspec/changes/phase-11-long-horizon-tasks/proposal.md` 的 Non-goals）。

## 排障

| 现象 | 先看什么 |
|---|---|
| 跑几步就停，回答里写「已达到本轮最大工具调用步数」 | `chat.max_steps`；再看这一轮是否在同一个工具上打转 |
| 停在第 3 步却烧了几十万 token | `chat.turn_max_tokens` 与工具输出大小（`max_read_kb`、`bash` 的输出上限） |
| 一直不停直到时间上限 | 某个命令在等 stdin / 等网络；看审计日志里耗时最长的那次调用 |
| 报上下文长度超限 | `context.max_tokens` 没设或设得比模型窗口还大 |
| 回答写着 `timeout_capped` | `timeout_ms` 超过了 `bash_max_timeout_seconds`，按需要调大后者 |
