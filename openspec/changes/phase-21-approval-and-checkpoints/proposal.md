# Phase 21 — 敢把写权限交出去：审批闸门与文件级检查点

## Summary

今天这个 agent 拿到写权限之后是**没有刹车**的。这不是疏漏，是刻意缺的，而且写在文档里：

`docs/tools.md:230`：

```
- **Ask for approval.** A write or a command runs when the model calls it. There
  is no confirmation step to accept or reject.
```

`openspec/changes/phase-6-agent-tools/proposal.md:65` 与 `specs/agent-tools/spec.md:75` 也都把
交互式审批记成 "a separate change"。代码层面，`internal/tool/`、`internal/chat/`、
`internal/agent/`、`cmd/` 里搜 `approve|approval|require_confirm|needs_confirm` **零命中**。

唯一的拦截是 `deny_patterns`，而 `docs/tools.md:54` 自己承认它是：

```
# A SPEED BUMP, not a security boundary: an equivalent command that does not
# match will run.
```

于是 `rm`、`git reset --hard`、`git push`、改 CI 配置这类动作没有任何确认环节；改到一半发现
方向不对，唯一的退路是 git——而且得模型自己记得先 commit。缺的检查点也一样：搜
`snapshot|checkpoint|undo` 命中的全是无关快照（Langfuse 计数器、LLM 目录同步、内存镜像计数、
预算渲染），**没有一个是文件级的**。

本变更补上这两件事，它们是同一个目的的两半：

- **审批**：写/执行类工具调用落地之前挂一道闸门，人能批、能拒，拒绝的理由回到模型那里让它改；
- **检查点**：一轮里被文件工具改过的文件，在改动之前留有原样副本；任何时候能把这一轮回退掉。

合起来，把"交互式聊天"变成"可托付的工程工具"。

> **对 `CAPABILITY-GAPS.md` 的一处修正。** 原文的建议是"每轮开始前对改动文件做一次快照"。
> 轮次开始时**不知道**哪些文件会被改，所以正确的形态是**在第一次改动某个文件之前**为它留下
> 前像（copy-on-write），而不是轮次开头做一次猜谜式快照。另外，`bash` 造成的改动不在检查点
> 覆盖范围内（见"明确不做"），这一点必须写清楚——否则用户会以为它保住了自己的工作。

## Why

- **现在的风险不是"模型会做坏事"，是"一个人不敢离开键盘"。** 无人值守跑几十分钟的前提，不是
  模型更聪明，而是**任何一步坏决定都能被拦下或撤销**。这是从"玩具"到"工具"的分界线。
- **拒绝必须是一种有内容的回答。** 审批如果只能返回一个"否"，模型的下一步几乎是随机的：它会
  换个写法再撞一次。所以拒绝要能带上理由（"不要动 CI 配置"），作为工具结果回到模型那里——这与
  `ask_user` 的答案机制是同一件事。
- **审批机制不该自己发明一套通道。** 仓库里已经有一条完整的"回合内双向通道"：`tool.Asker`
  的词汇表（`internal/tool/ask.go`）、`WithAsker`/`AskerFrom` 的上下文注入、SSE 事件、以及
  `POST /api/chat/sessions/:id/questions/:qid/answer` 这条回答端点
  （`internal/server/chat.go:288`）。审批就是同一条通道上的另一种问题，唯一不同的是
  **超时的含义**。
- **超时必须默认拒绝（fail closed），这与 `ask_user` 恰好相反。** `ask_user` 超时是一种合法
  答案（`AnswerTimeout`，模型自己决定下一步），因为它只是在收集信息；而审批超时如果默认放行，
  这道闸门在最需要它的时候——没人在看的时候——等于不存在。
- **检查点的价值在于"回得去"，不在于"存得下"。** 快照本身没有意义，能可靠地回退才有。所以恢复
  必须原子、必须拒绝越界路径、必须能撤销自己，并且在文件已被人工改过时**默认拒绝覆盖**——
  一次"回滚"把人的新工作冲掉，比不提供回滚更糟。
- **这两件事一起才成立。** 只有审批：模型被拦下后就僵住了，方向性的错误仍然要靠 git 收场。
  只有检查点：一场跑歪的长任务要先跑完才能回退，浪费的是钱。

## What Changes

### ADDED

1. **`internal/tool`：审批的词汇表（与 `Asker` 同构）**
   - `Request{ ID, Tool, Capability, Summary, Preview []PreviewLine, Default }`：
     - `Summary`：一行人类可读的动作描述（`写入 internal/foo/bar.go`、`执行: git push origin main`）；
     - `Preview`：**有界**的细节——写/编辑是路径 + 行数增减 + 前若干行 diff，`bash` 是**完整
       命令行**（不截断命令行本身，只截断附带上下文），多文件是文件清单；
     - `Default`：无人回应时的建议决定（用于前端显示，不改变超时语义）。
   - `Decision{ Kind, Reason }`，`Kind` ∈ `allow_once` / `allow_turn` / `deny`；
     `Reason` 只在 `deny` 时有值，且它**会作为工具结果回到模型那里**。
   - `Approver` 接口 + `WithApprover(ctx, a)` / `ApproverFrom(ctx)`——注释里写明它与 `Asker`
     共用上下文注入这套做法，以及它唯一的不同点：超时是拒绝。
   - 一条 `Decision` 记录为审计所需的最小信息（工具名、参数摘要、决定、来源、时间），进日志。

2. **策略与闸门位置**
   - 闸门实现为一个**工具装饰器**（与既有的 `tool.CapabilityFunc` / `WithCapability` 同一个
     模式，`internal/tool/capability.go:41-56`），在注册时套在每个工具外面。**不是**放在某个
     循环里，也**不是**放在各个工具内部：
     - 不放在循环里，是因为仓库里有**两个**独立的执行循环——`internal/chat/runner.go` 的 Web
       循环，与 `internal/agent/agent.go` 的 Eino `react.Agent`（服务 CLI 与飞书）。写进任一
       循环都只覆盖一半 surface，而这个分叉正是 Phase 18 那个 bug 的形状。
     - 不放在工具内部，是因为一个只在某个工具里实现的审批，会在第五个工具出现时被忘记。
   - **装饰只加在一处**：`cmd/huan-agent` 的注册辅助函数（`chat.go:448` 的
     `registerBuiltinTools` 与 `workspace_tools.go:389` 的 `registerWorkspaceTools`）。已核实
     三个 surface 都从这里建注册表——`admin.go:108`（控制台）、`chat.go:375`（CLI）、
     `workspace_tools.go:532`（飞书）——所以一处覆盖全部 surface 与两个循环。
   - 装饰器在 `Invoke` 时从 ctx 取 approver（`ApproverFrom`），因此装饰发生在注册期、而决定
     发生在调用期，两者不冲突。
   - `tools.approval.mode` ∈ `off`（默认） / `writes` / `writes+exec` / `all`，按工具的能力标签
     （`CapWrite` / `CapExec`）决定是否需要审批。`read_only` 工作区下本闸门无意义（没有写工具）。
   - `tools.approval.allow`：正则白名单，命中的工具调用免审批（例如 `**/*.md` 下的写入）。
     匹配对象是动作摘要，与 `deny_patterns` 一样，**它也是减速带不是安全边界**——文档要直说。
   - `tools.approval.timeout_seconds`（默认 300）。
   - **`allow_turn` 的语义**：本轮内对该工具的后续调用免审批（不是"对该文件"，也不是"永久"）。
     最容易理解、也最不容易被误当成安全承诺。
   - **没有 approver 的 surface**：`mode != off` 时，需要审批的工具**不注册**（不是"注册了但
     每次都拒"）——沿用 `cmd/huan-agent/workspace_tools.go` 里已经写明的原则："一个必然失败的
     调用比一个不在菜单上的工具更糟"。启动时记一条 info 说明原因。

3. **`internal/server`：Web 上的审批卡片**
   - 新事件 `approval_request{ id, step, tool, capability, summary, preview, default, timeout_ms }`
     与 `approval_result{ id, decision, reason, source, auto }`。
   - 回答端点 `POST /api/chat/sessions/:id/approvals/:aid`，形状与既有的
     `.../questions/:qid/answer` 一致（问题 id 是连接的钥匙）。
   - 决定 MUST 随轮次落盘（与 `ask_user` 的答案同样机制），所以刷新页面后卡片仍在、并且显示
     当时选了什么。
   - `GET /api/chat/sessions/:id` 的轮次步骤里带出审批结果，使"这一轮里人批过什么"可回溯。
   - Web：`ApprovalCard.vue`——动作摘要 + 预览（diff 用既有样式，不引入 diff 库）+ 三个按钮
     （允许一次 / 本轮允许 / 拒绝）+ 拒绝理由输入框 + 剩余时间提示。

4. **CLI 的审批（第二个 surface）**
   - 交互式 REPL 上，审批渲染成 stderr 上的一段提示 + stdin 的 `y / t / n <理由>`。
   - 目的不只是可用性：它让 Phase 19/22 的端到端测试可以在**无人值守但可脚本化**的条件下验证
     审批路径（把答案从 stdin 喂进去）。
   - **飞书不在本阶段范围内**（见"明确不做"）。

5. **`internal/checkpoint`：文件级检查点**
   - **copy-on-write 语义**：文件工具（`write_file` / `edit_file` / Phase 24 的 `apply_patch`）
     在**第一次修改**某个文件之前，把它当时的内容（前像）与元信息记入本轮检查点。同一轮内第二次
     修改同一文件不再重复记。
   - 落盘位置：`data/checkpoints/<session>/<turn>/`，目录按 `dirBesideDatabase` 的做法派生
     （与 `tools.background_dir`、`tools.workspaces_dir` 一致），可用
     `tools.checkpoint.dir` 覆盖。
   - 每个检查点一份 manifest：`path`（工作区相对）、`existed`（新建的文件前像为"不存在"）、
     `mode`、`sha256`、`size`、`captured_at`。
   - 检查点也记录轮次开始时的 `git HEAD` 与 `git status --porcelain`（若工作区是 git 仓库），
     使用户能看清 **bash 造成的**改动——它们不在前像覆盖范围内。
   - **恢复**：`internal/checkpoint.Restore(ctx, session, turn, opts)`——逐文件校验 sha256、
     原子写回（temp + rename）、把"前像不存在"的文件删除、恢复文件模式；恢复**之前**对当前状态
     再做一次检查点，使"回滚"本身可以撤销。
   - **冲突默认拒绝**：磁盘上的文件与检查点记录之后又被改过（哈希不符）、且不是本 agent 的写入
     所致时，MUST 默认拒绝覆盖并列出冲突文件；`--force` 才覆盖。
   - 保留策略：`tools.checkpoint.keep_turns`（默认 20）与 `tools.checkpoint.max_total_mb`
     （默认 512），超出按时间从旧到新淘汰，并记日志说明淘汰了什么。
   - 只读工作区下不产生检查点（没有写工具）。

6. **入口**
   - HTTP：`GET /api/chat/sessions/:id/turns/:tid/checkpoint`（列出文件与统计）、
     `POST .../rollback`（恢复；冲突时 409 并带冲突清单）。
   - CLI：`huan-agent checkpoint list|show|rollback`（便于脚本化与故障恢复）。
   - Web：轮次卡片上出现「回退这一轮」，二次确认后显示恢复报告。

7. **文档**
   - `docs/tools.md:230` 那段"没有确认环节"的说明 MUST 被改写，并指明 `tools.approval.mode`
     与它的默认值；`docs/tools.md:54` 的 `deny_patterns` 说明旁边补一句"审批是闸门，
     `deny_patterns` 仍是减速带"。
   - 新 `docs/approvals-and-checkpoints.md`：策略取值、卡片交互、检查点覆盖范围与**不覆盖范围**
     （bash 的改动）、回滚的冲突语义、保留策略。
   - `openspec/changes/phase-6-agent-tools/` 里那两处 "a separate change" MUST 被指向本 change。

### 明确不做（本阶段）

- **不做 bash 的文件级检查点。** 一条 `rm -rf` 或 `sed -i` 绕过文件工具直接改盘。要做就得整仓
  快照，成本与收益不成比例。诚实的做法是：检查点只覆盖文件工具，同时把轮次开始与结束的
  `git status --porcelain` 记下来让人看清 bash 动了什么。文档必须写明这一点。
- **不做飞书的审批卡片。** 飞书需要走自己的交互卡片 API 与回调，是一个独立的集成面。本阶段飞书
  在没有 approver 时的行为是**不注册写/执行工具**（安全的一侧），并在启动时说明。
- **不做 `run`（Phase 19）的审批。** 无人值守的管道里没有可批的时刻。`mode != off` 时 `run`
  拿不到写/执行工具——这是有意的，也必须在 `docs/run.md` 里说清。
- **不做自动提交 / 自动 stash。** 检查点与 git 是两套东西：前者管"这一轮"，后者管"这个项目"。
  让 agent 自动 commit 会污染用户的历史。
- **不做审批的"记住这个决定"（永久允许）。** 只有一次性与本轮两种粒度。永久允许会把一次点击
  变成一条不再被复查的规则，而这类规则需要自己的管理界面。
- **不做审批的独立审计表。** 决定随轮次落盘 + 日志已经足够回溯；专门的审计表需要保留策略、
  导出、查询界面，是另一个 change。
- **不做审批的差异化策略引擎**（按路径 / 按命令前缀 / 按风险等级打分）。一张白名单正则 +
  四种 mode 是能被人真正理解的上限。

## Impact

- 新增包：`internal/checkpoint`。
- 新增文件：`internal/tool/approval.go`、`internal/checkpoint/{checkpoint,manifest,restore}.go`、
  `internal/server/approval.go`、`cmd/huan-agent/checkpoint.go`、
  `web/src/components/ApprovalCard.vue`、`docs/approvals-and-checkpoints.md`。
- 改动：`internal/agent`（注册时套装饰器）、`internal/tool/builtin/files.go`（改前留前像）、
  `internal/chat/{runner,events}.go`（两个新事件）、`internal/server/{chat,turns}.go`、
  `internal/config/config.go`、`cmd/huan-agent/{chat,workspace_tools,serve}.go`（注入 approver 与
  检查点）、`web/src/{api.js,chatStore.js,styles.css}`、`docs/tools.md`。
- 依赖：Phase 18（启动）、Phase 19（可脚本化的端到端验收）。与 Phase 20 的顺序无关，但
  `apply_patch`（Phase 24）必须在检查点之后落地，否则多文件编辑是唯一没有回退路径的写操作。
- 兼容性：**`tools.approval.mode` 默认 `off`**，所以默认行为与本变更之前逐字节相同。这是一个
  刻意的选择：默认打开会让既有的无人值守用法（`run`、飞书、cron）在不打招呼的情况下失去写
  权限——那是比"没有审批"更糟的意外。要拿到"敢交付"的收益，就得显式打开它，并在
  `docs/` 里注明随之而来的 surface 差异。
- 存储：新增 `data/checkpoints/` 目录，有保留上限（默认 20 轮 / 512 MB），不影响数据库。
