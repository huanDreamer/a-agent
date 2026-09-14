# agent-interaction

## 目标

让一轮 turn 能在**中途**向人提问并拿到结构化答案：模型提出问题（可带选项、可让人自由
输入），人在 Web 控制台的卡片上回答，答案作为工具结果回到模型，同一轮继续跑完。
提问通道由宿主注入，工具本身不认识 HTTP / SSE / 任何前端；没有宿主的 surface（飞书、CLI）
不注册这个工具。

## API

- `tool.Question{ ID, Header, Text, Options []tool.Option, MultiSelect, AllowCustom }`
  - `ID` 由 `Asker` 分配（空着传进来），是前端提交答案时的句柄。
  - `AllowCustom` 默认 true：卡片始终允许"自己输入"。
- `tool.Option{ Label, Description }` —— `Label` 是答案，`Description` 是给人看的一行解释。
- `tool.Answer{ Status, Selected []string, Text string }`
  - `Status` ∈ `answered`（人提交了）/ `timeout`（等待超时）/ `cancelled`（回合被取消）
- `tool.Asker`：`Ask(ctx context.Context, q Question) (Answer, error)`
- `tool.WithAsker(ctx, Asker) context.Context` / `tool.AskerFrom(ctx) (Asker, bool)`
- 工具 `ask_user`，参数：
  `question`（必填）、`header`、`options[{label,description}]`、`multi_select`、
  `allow_custom`（`*bool`，默认 true）
- 工具结果：`{status, question, selected, text, answer, note}`

## 行为

- **挂起而非结束**：`ask_user` 的 `InvokableRun` 阻塞到人回答、等待超时或回合被取消；
  runner 的这一步不结束，拿到答案后把结果放进同一条工具消息里继续循环。
- **注册**：只在 Web 控制台（`Surface == "web"`）注册。其它 surface 不注册，因此模型不会
  调用一个必然失败的"问人"工具。
- **无 Asker 时的兜底**：上下文里没有 `Asker`（例如工具被别处复用）时，返回错误：说明当前
  通道不能提问、应当直接回答或把问题写进回答里。
- **校验在调用侧**：`question` 为空、选项超过 8 个、选项 label 为空或重复、既无选项又
  `allow_custom=false` → 返回可读错误让模型改调用，而不是把坏问题推到人面前。
- **答案归一**：`answer` = 选中的 label（多选用 `、` 连接）与自定义输入按同样规则拼成的
  一行；两者都有时自定义输入在后并加「（补充：…）」。
- **超时/取消不是失败**：返回 `status=timeout` / `cancelled`，附 `note` 说明"人没有回答，
  请基于最合理的假设继续，或把问题留到最后一起问"，`error` 为 nil —— 模型仍有完整的判断
  空间，而不是收到一个异常。
- **等待上限**：由宿主决定（Web 控制台用 `chat.ask_user_timeout_seconds`，0 / 未配置 = 默认
  600s）。等待始终再被本轮 context 封住（停止 / 关页面 / 进程退出），并且等待时间计入本轮
  墙钟预算 —— 所以等待上限要明显小于 `chat.turn_deadline_seconds`。
- **落盘**：问题在工具参数里、答案在工具结果里，随该轮 assistant 消息一起持久化；刷新后
  卡片可复原。
- **同一个问题只被回答一次**：第一个提交者生效，之后对该 id 的提交返回 404（"问题已不在
  等待中"）。

## 接入

- `internal/tool`（词汇表 + ctx 注入）← `internal/tool/builtin`（工具）→ `internal/chat`
  （`EventAsk`）→ `internal/server`（`turnAsker` + 应答接口）→ `web/src`（卡片）。
- 事件 `ask_user`：
  - `{type:"ask_user", ask:{id,header,text,options,multi_select,allow_custom}}` —— 新问题；
  - `{type:"ask_user", ask_id, ask_status, ask_answer}` —— 结局（answered/timeout/cancelled）。
- 后端不得把答案直接写进消息正文：人看到的是卡片，模型看到的是工具结果。
