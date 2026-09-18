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
  # 压缩开启后一轮 60 步大约烧 1~2M token（每一步都要带工具结果和输出），
  # 所以 token 预算要明显高于这个量级，否则它会在正常收尾之前先把轮次掐掉。
  # 真要一劳永逸，填 0 = 不限，代价是只剩步数和墙钟两个闸（两者都不是成本上限）。
  turn_max_tokens: 6000000
  turn_deadline_seconds: 10800
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

## 中途断了怎么办：三层恢复

长任务的失败大多不是"想不明白"，而是"被打断"：模型调用被掐、流断在半路、预算用尽。
下面三层按代价从低到高排列，前一层能解决就不会惊动后一层。

### 第一层：单次模型调用重试（`llm.retry`）

对每一次模型调用生效，覆盖网页对话、飞书机器人、CLI 和摘要/标题这类内部调用：

```yaml
llm:
  retry:
    enable: true
    max_attempts: 3      # 总尝试次数（含首次）
    base_delay_ms: 800   # 第一次重试等多久
    max_delay_ms: 30000  # 指数退避的上限
    multiplier: 2.0      # 每多一次等待翻几倍
    jitter: true         # ±25%，避免多个轮次同时醒来又撞上限流
```

**重不重试看失败的含义，不看它是不是 error**：连接中断、超时、408/409/425/429、5xx 会重试；
400 / 401 / 403 / 404 / 422、余额不足**立刻失败**——再买一次只会得到同一句拒绝。
每次重试写一条 warn 日志（provider / model / attempt / delay / error）；重试成功的那次调用
除此之外不留痕迹。

### 第二层：单步重试（`chat.step_retry`）

管的是第一层**刻意不管**的那种失败：流已经吐出一半才断。此时模型已经开始回答，
重放会让读者看到两遍同一句话的开头。所以由 runner 从**同一份历史**重跑这一步
（失败的那次没有产生 assistant 消息，历史还是干净的），并先发一个 `step_retry` 事件：

```json
{ "type": "step_retry", "step": 3, "attempt": 2, "max_attempts": 3,
  "delay_ms": 3000, "error": "unexpected EOF" }
```

控制台收到它会把**这一步**已显示的文字与思考清掉，并在该步骤上写一行「第 2/3 次尝试，
3s 后重试」；整轮中其它步骤的思考保留（它们真的发生过）。

```yaml
chat:
  step_retry:
    enable: true
    max_attempts: 3      # 这一步最多跑几次（含第一次）
    base_delay_ms: 1500
    max_delay_ms: 20000
```

两层会**相乘**：最坏情况下一步的模型调用次数 = `llm.retry.max_attempts` ×
`step_retry.max_attempts`（默认 3 × 3 = 9），而且只在连续失败时才会发生。
重试的等待还被本轮墙钟预算卡着：等待时间会被截到"这一步开始时还剩多少时间"，不会睡过 deadline。

### 第三层：任务级续跑（点「继续执行」）

两层重试都失败之后，这一轮以错误结束——但**已完成的工作不会作废**。界面上失败的回答旁边
（以及任务看板上）会出现「继续执行」，它做的是：**带着上一轮的计划、已经做过的步骤和工具观察
开始新一轮**，而不是让你把原话再说一遍、让模型从头想。

服务端在这一轮的上下文里插入一段（不落库、不出现在界面上的）接续简报：

```
[接续执行] 上一轮任务没有跑完，现在从中断处继续。已经完成的工作不要重做。

【原始目标】<你最后说的一句话>
【中断原因】<错误 / 停因>
【计划现状】已完成 2/4 …（带 id 的清单）
【上一轮已经做过的步骤】
- 步骤 1：先读一下 runner.go
    · read_file({"path":"internal/chat/runner.go"}) → 完成：package chat ...
- 步骤 2：跑一下测试
    · bash({"command":"go test ./internal/chat/"}) → 失败：exit status 1
【工作区】<工作区名>（已经改动过的文件就在这里，不要从头重写）

请先调用 plan_read 确认计划现状，然后从第一个未完成的任务继续……
```

界面上只留一条可见的用户消息「继续执行（还有 N 项未完成）」；简报本身是给模型的。
工具输出按条截断（每条 800 字、整份约 6000 字）并**明文说明被截断**——模型需要知道自己
看到的不是全文，必要时重新跑一次只读命令确认。

相关性判据：上一轮**没跑完**（有 error 或停因），或者**计划里还有未完成的任务**。
两者都不满足时接口返回 400，并说明这个对话没有需要继续的任务。

我们**不**自动续跑：接着花的是你的钱，所以这件事由你点。

## 任务计划与看板

模型通过四个工具维护一份任务清单（`plan_create` / `plan_add` / `plan_update` / `plan_read`），
控制台把它画在**输入框上方**：

- **执行中**（`in_progress`）· **待执行**（`pending`，`failed` 排最前并标红写出原因）· **已完成**（`done`）
- 头部一行是目标 + `n/m` + 进度条，可折叠；没有计划时整块不渲染
- 计划落库在 `chat_plans`，随会话级联删除；刷新页面、重启服务都还在
- 每次变更发一个 `plan` 事件，所以看板是**随执行推进**的，不是等这一轮结束才更新
- **计划属于一次请求**：全部任务做完（或明确跳过）之后，你再发一条新消息，服务端会在
  新一轮开始前把这份计划清掉并广播一个空计划，看板随之消失——它已经是一张回执，不是
  待办。**没做完的计划会被保留**（它是「继续执行」的接续点），所以看板停在那儿不动，
  通常意味着"这一轮没跑完"，而不是"界面卡了"。

系统提示要求：多步任务先建计划；一次只推进一条；**每完成一步立刻更新**（不要攒到最后）；
发现新必做事项用 `plan_add` 追加而不是 `plan_create` 覆盖（覆盖会丢掉已有进度）。
计划有变更时，prompt 里也会要求它先 `plan_read`——续跑时这条尤其重要。

计划只在网页控制台注册（与 `ask_user` 同理）：没有计划存储的通道（CLI、飞书）装上一个
必然失败的工具比没有这个工具更糟。不想要它就把 `chat.plan.enable` 设为 false。

## 和"说一句继续"的区别

没有计划、直接说「继续」时，模型只拿到上一轮的回答文本：它知道自己说了什么，但不知道
每一步调用了什么工具、拿到了什么观察值。续跑（以及计划）补上的正是这一段——所以如果
这类任务要反复中断，值得让它先建计划。

## 排障

| 现象 | 先看什么 |
|---|---|
| 跑几步就停，回答里写「已达到本轮最大工具调用步数」 | `chat.max_steps`；再看这一轮是否在同一个工具上打转 |
| 停在第 3 步却烧了几十万 token | `chat.turn_max_tokens` 与工具输出大小（`max_read_kb`、`bash` 的输出上限） |
| 一直不停直到时间上限 | 某个命令在等 stdin / 等网络；看审计日志里耗时最长的那次调用 |
| 报上下文长度超限 | `context.max_tokens` 没设或设得比模型窗口还大 |
| 卡了很久才继续（日志里一串 retrying） | 上游在限流或抖动；看 `llm.retry` 的 delay 与 provider 的错误码 |
| 同一段话出现了两遍 | 客户端没处理 `step_retry`（旧的静态资源）——重试的那一步必须丢掉上半截 |
| 看板停在某个任务上不动 | 模型没有在每一步之后调 `plan_update`；这是提示词问题，也可能是它没建计划 |
| 「继续执行」点了报 400 | 那一轮其实正常收尾了、计划也没有未完成项（接口如实说明，不是 bug） |
| 回答写着 `timeout_capped` | `timeout_ms` 超过了 `bash_max_timeout_seconds`，按需要调大后者 |


## Subagents: context isolation, not compute

A long task is often long because of *reading*, not because of writing. Exploring a
subsystem to answer one question means twenty greps and thirty file reads, and in
the parent's window those tool results stay for the rest of the turn, crowding out
the code being changed.

`spawn_agent` buys a window that is allowed to get dirty. What crosses back is one
bounded report (truncated at `subagent.max_report_chars`, with a footer saying how
it was produced), and the nested run's own transcript stays in its own trace.

Three properties matter for a long task:

- **The cost is the parent turn's.** Tokens and wall-clock count against
  `chat.turn_max_tokens` and share `chat.turn_deadline_seconds`, so a turn that
  spawns four explorations is still bounded by the numbers on the 对话预算 page.
- **The concurrency bound is process-wide** (`subagent.max_concurrent`, default 2):
  "explore three things at once" is the intended use, and four parents each doing it
  is how a fan-out becomes a bill.
- **A subagent is read-only unless asked otherwise**, and asking still goes through
  the parent's approval gate, checkpoints and sandbox. A delegated write is a write
  the user approved.

See `docs/tools.md` for the tool's own contract.
