# Phase 12 — 交互卡片：模型问，人来选，回合接着跑

## Summary

Web 控制台的对话至今是**单向**的：模型说完就走，人在回答之后才能补充信息。真实使用里
反复出现的是另一种对话 —— 模型**不知道该怎么选**（用 Postgres 还是 SQLite？改哪个文件？
这个方案对不对？），于是它要么猜一个（猜错了，几步工具调用白跑），要么把问题写进正文然后
结束这一轮（人回答了，但上一轮的上下文与已经做了一半的工作都散了）。

本次变更给对话加一条**回合内的双向通道**：

1. 新工具 `ask_user`：模型调用它，人就在对话里看到一张卡片（标题 + 问题 + 若干选项 +
   自定义输入 + 提交按钮）。
2. runner **在这一步挂起**（不是结束回合），等人提交答案，把答案作为这次工具调用的结果
   交回模型，同一轮继续往下跑。
3. 答案与问题都按既有机制落盘（工具调用参数 + 工具结果），刷新页面后卡片仍在，且显示
   当时选了什么。

## Why

- **猜错比问一句贵得多。** 一次错误的分支通常要被 2–5 次工具调用、几十秒和几千 token
  才能发现；而问一句只需要人点一下。
- **回合结束式提问会丢上下文。** 把问题当回答发出去，人再回答时模型看到的是一条新的用户
  消息，之前那一步的工具结果、刚建立的假设、正在改的文件都要重新捡起来；回合内挂起则
  这些全在同一个窗口里。
- **卡片比散文可靠。** 「请告诉我用哪个数据库」这种句子，模型拿回来的是自由文本，还得自己
  解析；结构化选项让答案直接可用，也让人少打字。
- **能力必须只在有卡片的地方出现。** 飞书 / CLI 没有渲染这张卡片的前端，工具在那里注册
  只会换来一次必然失败的调用 —— 因此按 surface 注册。

## What Changes

### ADDED

1. **`internal/tool`：提问的词汇表**
   - `Question{ ID, Header, Text, Options []Option, MultiSelect, AllowCustom }`、
     `Option{ Label, Description }`、`Answer{ Status, Selected, Text }`。
   - `Answer.Status` ∈ `answered` / `timeout` / `cancelled`。
   - `Asker` 接口 + `WithAsker(ctx, a)` / `AskerFrom(ctx)`：回答者由**调用方**注入，
     所以工具本身不认识 HTTP、SSE 或任何前端。
2. **`internal/tool/builtin`：`ask_user` 工具**
   - 参数 `question`（必填）、`header`、`options[{label,description}]`、
     `multi_select`、`allow_custom`（默认 true）。
   - 校验：`question` 非空；选项 ≤ 8 个、label 非空且不重复；既没有选项又禁止自定义输入
     时拒绝调用（这三种情况都会把可读的错误交回模型，让它自己改调用）。
   - 结果 `{status, question, selected, text, answer, note}`：`answer` 是给人/模型直接读的
     一行答案，超时或取消时 `note` 说明发生了什么。
   - 上下文里没有 `Asker`（飞书 / CLI / 其它宿主）时返回明确错误，而不是假装问过了。
3. **`internal/chat`：一个事件**
   - `EventAsk`（`ask_user`），两种形态共用：`ask` 在场 = 新问题（`pending`）；
     `ask_id` + `ask_status`（+ `ask_answer`）在场 = 该问题的结局。
4. **`internal/server`：挂起与应答**
   - 每轮一个 `turnAsker`：分配问题 id、把问题作为事件推给当前 SSE 流、在
     `chat.ask_user_timeout_seconds` 内等待应答；超时 / 客户端断开 / 回合取消都返回带
     `status` 的答案，并推一条结局事件让卡片停止等待。
   - `POST /api/chat/sessions/{id}/questions/{qid}/answer`：`{selected:[], text}` →
     唤醒等待中的工具。校验：问题必须属于该会话并且仍在等待；`selected` 必须是卡片给出的
     选项；非多选最多一个；不允许自定义输入时拒绝 `text`；空答案拒绝。
5. **`internal/config`**：`chat.ask_user_timeout_seconds`（0 / 未配置 = 默认 600s；等待始终再被本轮 context 与墙钟预算封住）。
6. **Web 控制台**：`AskUserCard.vue`（选项 / 多选 / 自定义输入 / 提交 / 已提交 / 已超时
   五种状态）、`ask.js`（把「实时事件」与「历史工具调用」归一成同一个卡片模型）、
   `chatStore.submitAsk()`、`api.answerQuestion()`。
7. **按 surface 注册**：`ask_user` 只在 `Surface == "web"` 时进工具表。

### MODIFIED

- `registerBuiltinTools` 增加 web-only 工具；`ChatDeps` 增加 `AskTimeout`。
- `ChatMessage.vue`：`ask_user` 不再渲染成普通工具卡片，而是渲染问题卡片；回答后成为静态
  卡片（历史记录里也一样）。
- `web/README.md` 的接口表与 SSE 说明、`docs/admin.md`、`configs/config.example.yaml`
  同步。

## Impact

- **新依赖**：无。前端仍是 Vue 3 + 手写 SSE 解析，后端只用标准库（`crypto/rand` 生成
  问题 id）。
- **兼容性**：默认配置下行为不变（等待上限有默认值；`ask_user` 只对 web 注册）。飞书 / CLI
  的工具表不变。
- **已知边界**（写入文档，不假装支持）：页面刷新 / 关闭标签页 = 本轮请求被取消，等待中的
  问题随之作废（`status=cancelled`），模型收到"人没回答"后应自己收口；等待时间计入本轮
  墙钟预算（`chat.turn_deadline_seconds`），所以等待上限应当明显小于它，否则等到答案时
  本轮已经没有预算继续。
- **可观测**：等待、超时、取消都写日志（session / question id / 等待时长），
  `ask_user` 的参数与结果照旧进审计日志。
