# Capability: subagent-observability

## Purpose

让"委派出去的工作"和"本地的后台进程"在控制台上一样可见：读者一眼能看到这一轮派了几个子 agent、
各自在做什么、结果如何；同时让并行派发在**所有** surface 上都成立，而不是只在自带调度器的那两个上。

## Requirements (MUST)

1. **每次派生一条可读记录。** MUST 记录：显示名、任务摘要（有界）、状态（running / ok / failed）、
   开始与结束时间、耗时、步数、token、失败原因、所属会话、发起它的工具调用 id。
2. **运行中的记录不可淘汰。** 保留策略 MUST 只作用于已结束的记录；把仍在工作的子 agent 从列表里
   丢掉，比列表长更糟。
3. **步数不可知与步数为零必须可区分。** 无法计数时 MUST 用哨兵值（`StepsUnknown = -1`），
   界面 MUST 显示"不可得"而不是"0 步"。
4. **按会话隔离。** 一个会话的列表 MUST NOT 出现另一个会话的派生记录。
5. **HTTP 形状与后台任务对齐。** `GET /api/chat/sessions/:id/subagents` MUST 返回
   `{subagents, running, total, max_concurrent, running_all, tokens}`；未启用时 MUST NOT 注册该路由。
6. **一次调用可并行多任务。** `spawn_agent` MUST 支持 `tasks: [...]`，扇出 MUST 在工具内部完成
   （因而与循环是否串行无关），且 MUST 走同一个全进程并发闸门；一份任务的失败 MUST NOT 取消兄弟。
7. **描述必须教会模型怎么并行。** 工具描述 MUST 说明 `tasks` 形态与"同一回复多次调用"两种方式，
   并说明适用场景是互不依赖的问题。
8. **界面措辞可测。** chip 的措辞 MUST 单独成模块（不渲染整个视图即可断言），因为"在新会话标题下
   显示上一个会话的计数"正是这个缝隙要防的错误。
