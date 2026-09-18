# Phase 22b — 子 agent 的可见性与并行

## Why

Phase 22 让模型可以派子 agent，但两件事没做：

1. **看不见。** 控制台的顶部状态栏能列后台进程（`JobsDrawer`），却不能列子 agent。一个跑了
   一分钟的子 agent 只在那张工具卡片里留一行"运行中"，读者不知道这一轮到底派了几个、各自什么
   状态、花了多少 —— 而"委派"恰恰是最需要可见的操作，因为它把工作交给了看不见的东西。
2. **不好并行。** `spawn_agent` 已经声明 `ParallelSafe`，Web/`run` 那条路的调度器也确实会重叠
   同一条回复里的多次调用；但 CLI 与飞书走的 Eino ReAct 循环**按设计是串行的**，所以那三个
   surface 上"并行派三个"做不到。而且模型的调用习惯由描述决定：原来的描述根本没有提"可以同时
   派几个"。

## What changes

- `internal/subagent` 新增 `Tracker`：一次派生一条记录（名字、任务摘要、状态、起止时间、步数、
  token、失败原因、所属会话、父调用 id），**运行中的记录永不被淘汰**，已结束的按会话保留最近
  `DefaultTrackerKeep` 条。`Spawn` 在开始前登记、结束时结算。
- `spawn_agent` 新增 `tasks` 形态：**一次调用并行跑多个子任务**，各返回一份报告。这样即使循环
  本身串行（CLI、飞书），"同时探三条路"也能成立；扇出在工具内部，走同一个全进程闸门。
- 工具描述写明两种并行方式（`tasks` 形态、或在同一回复里多次调用），并说明该并行的场景是
  "问题之间互不依赖"。
- `internal/server`：`GET /api/chat/sessions/:id/subagents`，形状与 `/api/jobs` 对齐
  （`{subagents, running, total, max_concurrent, running_all, tokens}`）。未启用子 agent 时
  **不注册**该路由，所以状态栏不会出现一个永远说"0"的按钮。
- Web：`subagentsStore.js`（照 `jobsStore` 的样子：运行中才轮询、切换会话即清空）、
  `subagentsChip.js`（顶部 chip 的措辞，单独成文件以便测试）、`SubagentsDrawer.vue`（正在跑 /
  已结束两组，各自显示状态、步数、token、耗时、失败原因）。

## 一个必须说清的取舍

**`steps: -1` 表示"不知道"，不是"0"。** Eino 的 ReAct 循环只返回最终消息，所以那条路上的 runner
数不出步数；失败的运行更是从来没拿到过计数。抽屉把它显示为"步数不可得"，**绝不显示"0 步"** ——
一个干了一分钟的子 agent 显示"0 步"读起来是"它什么也没做"，而这是读者用来判断"这活儿到底做没做"
的唯一位置。这个坑在 Phase 22 第一次真机跑的时候就踩到过：父模型据此怀疑了一个其实干得不错的
子 agent。
