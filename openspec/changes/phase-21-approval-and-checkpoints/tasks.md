# Phase 21 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

实施记录（2026-09-18）：**全部完成并验证**（后端、CLI、Web 控制台）。实施中发现并修复了一个
真实的设计缺陷（第五节 bug 1），它只有在真机跑一遍才会暴露；另有两个由测试抓到的缺陷
（bug 2 能力标签丢失、bug 3 409 分支是死代码）。

## 1. `internal/tool`：审批词汇表

- [x] `PreviewKind`（context / add / del / meta）与 `PreviewLine`
- [x] `Request{ ID, Tool, Capability, Summary, Preview, Default, Timeout }`
- [x] `DecisionKind`：`allow_once` / `allow_turn` / `deny` + `Valid()`
- [x] `Decision{ Kind, Reason, Source }` + `Allowed()`
- [x] `DecisionSource`：`human` / `policy` / `timeout`
- [x] `Approver` 接口 + `WithApprover` / `ApproverFrom`（注释写明与 `Asker` 的同构关系，以及唯一
      差异：**超时是拒绝**）
- [x] `WithApprover` 挡住 typed nil（`reflect` 判断）：一个非 nil 接口持有 nil 指针会通过所有
      `== nil` 判断，然后在工具调用里 panic
- [x] `DenialResult`：拒绝作为工具观察值，带理由，并明确要求"不要重复同一个请求"
- [x] 测试：`Allowed()` 全表（含零值与未知 kind 必须不放行）、`Valid()` 全表、typed nil

## 2. 闸门（`internal/tool/approval_gate.go`）

- [x] `ApprovalPolicy{ Mode, Allow, Timeout }` + `Requires(cap)` + `Allows(summary)`
- [x] `ParseApprovalMode`：off / writes / writes+exec / all，非法值报错（不是静默当作 off）
- [x] `Gate(t, opts)`：装饰器，**mode=off 时原样返回工具本身**（不为每个部署没打开的开关付
      接口间接寻址的代价）
- [x] 按能力判定，所以新增工具不需要改策略代码
- [x] 白名单在询问之前检查；**正则编译失败照常询问**（不能变成"全部放行"）
- [x] `allow_turn` 通过 context 上的集合实现（不是闸门自己的状态），所以"本轮"随轮次结束而结束
- [x] 没有 approver → 拒绝（防御性：正常路径下这种 surface 根本不该有这些工具）
- [x] approver 报错 → 拒绝，并把错误写进理由（与"人说不"区分开）
- [x] 每类工具的摘要与预览：`bash` 保留**完整命令行**（永不截断），`write_file` 报行数/字节/
      整文件覆盖，`edit_file` 显示删除与新增两侧，其它工具显示参数
- [x] 预览有行数与字节上限，截断如实说明
- [x] 每一次决定都写日志（含 `source`）：只记"人批准了什么"的审计回答不了"没人在看时跑了什么"
- [x] 测试 18 个：策略矩阵、off 不包装、允许/拒绝/超时/无 approver/approver 失败、
      `allow_turn` 的作用域与按工具隔离、白名单与非白名单、只读工具不受影响、摘要与预览
- [x] **`ApprovalGate` 显式声明 `Capability()`**（见第五节 bug 2）

## 3. 每类工具的动作摘要

- [x] `BuildRequest(toolName, cap, args)`，逐工具定义
- [x] `bash`/`bash_background`：`执行: <完整命令行>` + 完整命令、工作目录、超时的预览
- [x] `write_file`：`写入 <path>（N 行，M 字节，整文件覆盖）`
- [x] `edit_file`：`编辑 <path>（+N 行）` + 删/增两侧
- [x] `apply_patch`：文件清单形态已定义（Phase 24 落地时即可用）
- [x] 未知工具：显示工具名 + 参数（不能让人盲批）
- [x] 多字节内容按 rune 截断，不会切坏字符

## 4. `internal/server`：事件、端点、per-turn approver

- [x] `chat.EventApproval` 与 `Approval` / `ApprovalID` / `ApprovalDecision` / `ApprovalReason` /
      `ApprovalSource` 字段（注释写明与 `ask_user` 的超时语义相反）
- [x] `DefaultApprovalTimeout = 5min`（**短于** `DefaultAskTimeout`，并有一个测试钉住这个关系）
- [x] `approvalHub`：注册 / peek / resolve / forget / count，与 questionHub 同构
- [x] `turnApprover`：公告 → 等待 → 结算；超时/取消都产出 `deny` + `source=timeout`
- [x] 每条分支都结算卡片（否则卡片会一直挂着提供按钮）
- [x] 端点 `POST /api/chat/sessions/:id/approvals/:aid`，形状与提问回答端点一致
- [x] 校验：未知决定 400、非法 JSON 400、未知 id 404、别的会话 404、没有 hub 404、
      重复提交 404（已决定的不可能被翻转）
- [x] **允许时丢弃理由**（"因为 X 拒绝"和"允许，另外 X"是两件事，模型会把理由当指令读）
- [x] 按会话隔离：猜到的 id 不能批准别人的命令
- [x] 在 `startTurn` 里安装 per-turn approver + allowance 集合
- [x] 测试 14 个：hub 语义、per-turn approver 的全部出口、HTTP 端点（走真实 harness + 登录）、
      **一整轮真跑**（脚本模型调用被闸门保护的工具 → 卡片上线 → 决策走 HTTP → 工具执行或
      被拒绝且理由回到模型）
- [x] 日志：批准 info、人的拒绝 info、超时拒绝 warn

## 5. 实施中发现的两个真实缺陷（本节是测试存在的理由）

**bug 1：闸门把"本轮自己的改动"误判成冲突（真实设计缺陷，已修）。**
检查点最初的冲突判定只有"当前内容 ≠ 前像"，于是**最常见的情况**——"模型刚改的，我想撤掉"——
每次都被报成冲突并要求 `--force`。这在单元测试里完全发现不了（测试是我按自己的假设写的），
只有在真机跑一遍 CLI 会话时才暴露：改完文件、`/rollback 1`，得到的是"1 个文件在检查点之后被
改动过，默认没有覆盖"。

修法是记录**写入结果的哈希**（`FileEntry.AfterSHA256`，每次成功写入后更新）：
- 当前内容 == 本轮最后写入的 → 是本轮自己的改动，直接恢复，不需要任何标志；
- 当前内容两者都不等 → 是别人的改动，拒绝并要求 `--force`。

一个总要加确认标志的操作会训练人闭眼加标志，那比没有确认更糟。新增两个测试分别钉住这两侧。

**bug 2：嵌入 `Tool` 接口不会提升 `Capability()`（真实 bug，已修）。**
`ApprovalGate` 嵌入 `tool.Tool`（一个只有 `Info` + `InvokableRun` 的接口），而 `Capability()` 是
另一个接口（`Capable`）的方法，**不会被提升**。后果是 `CapabilityOf(被闸门包过的工具)` 返回默认值
`CapRead`——而 `FilterByCapabilities` 正是给只读 surface 裁工具用的，一个自称只读的**写**工具恰好
是唯一能穿过那个过滤器的工具。

`lsp.Feedback` 有同一个陷阱（它由外层 `WithCapability` 兜住了，但那是巧合而不是设计），
两个装饰器都补上了显式声明，并加了一个测试：被闸门包过的写工具**不能**穿过只读过滤。

## 6. `internal/checkpoint`：检查点

- [x] `Checkpointer` + `Options`（Dir / Workspace / KeepTurns / MaxTotalMB / Logger / Enable）
- [x] **copy-on-write**：第一次改动某文件前捕获前像；同一轮第二次改不会覆盖（有测试钉住）
- [x] `existed=false` 语义（本轮新建 → 恢复即删除）与文件模式保留
- [x] 内容寻址存储（同内容只存一份）
- [x] `BeginTurn` / `EndTurn`：记录轮次开始与结束的 git HEAD 与 `status --porcelain`
      （非 git 仓库不报错，如实留空）
- [x] `RecordAfter`：每次成功写入后记录结果哈希（见第五节 bug 1）
- [x] `NextTurn`：从磁盘推导，重启后不会重号（重号会让恢复读错轮次）
- [x] session id 经过 `sanitizeSegment`，`..` 与分隔符无法逃出目录
- [x] manifest 损坏或版本不支持 → 报错而不是当成空（静默重来会把已改的文件当"前像"）
- [x] 超大文件/非普通文件/读取失败 → 记录 `skipped`，回退报告中列出（不静默丢）
- [x] 保留策略：每会话 `keep_turns` + 总量 `max_total_mb`，从最旧淘汰并写日志
- [x] **恢复**：先捕获当前状态（使回退可撤销）→ 阶段一纯计算（冲突只报告不写入）→ 阶段二逐文件
      原子写 / 删除
- [x] 冲突默认拒绝覆盖，`Force` 才覆盖；报告含冲突清单与跳过清单
- [x] `Guard` 装饰器：写工具执行**前**捕获，执行**后**记录结果哈希；没有 turn 上下文时什么都不做
- [x] 捕获失败不阻断写入（记 warn），因为"备份失败就不让编辑"会把降级的安全性变成坏掉的工具
- [x] 测试 20 个：copy-on-write、新建/删除/模式、冲突两侧、回退可撤销、并发捕获不丢更新、
      路径越界、损坏 manifest、版本、保留策略两侧、guard 的顺序与无 turn 行为

## 7. 接线

- [x] `registerBuiltinTools` 一处解析策略并做**注册表级**后处理（基础工具分散在四处注册，
      后处理是唯一能让覆盖结构性成立的做法）
- [x] `workspaceToolSet.build` 在唯一的出口应用闸门（该函数有四个 return，gate 覆盖三个比没有更糟）
- [x] `newWorkspaceToolSet` 自己解析闸门，避免控制台的 per-turn 绑定漏掉
- [x] `approvalSurfaceCanAsk`：web/cli 能问，feishu/run 不能（默认值对未知 surface 是"不能问"）
- [x] CLI approver 复用 REPL **自己的 scanner**（两个 reader 抢一个终端时，谁先拿到文件描述符
      谁说了算，那正是"答案对不上问题"的来源）
- [x] CLI 非 TTY 时不尝试交互：按"没有 approver"处理，启动时记一条说明
- [x] CLI 的 `y` / `t` / `n <理由>`；**空行与 EOF 都是拒绝**（在确认提示上按回车不等于同意）
- [x] CLI 的 `/checkpoints` 与 `/rollback <轮次> [--force]`
- [x] 检查点 guard 挂在写工具上，顺序是 **guard（前像）→ LSP 反馈（后像）**，顺序不可交换
- [x] REPL 每轮 `beginCheckpointTurn`（开始 + 结束 + 淘汰三件事绑在一个函数里，两半不会漂移）
- [x] 服务端 turn 的检查点接线：`Server.checkpoints` 设置、`checkpointFor(ws)` 按工作区构建、
      `beginCheckpointTurn` 在 `startTurn` 里开一轮（`BeginTurn` → 装 Turn 到 ctx → 结束时
      `EndTurn` + `Prune`）
- [x] 两条 HTTP 路由：`GET /chat/sessions/:id/turns/:tid/checkpoint` 与
      `POST .../rollback`（`force` 支持）；未启用时**不注册**（一个总是失败的按钮比没有更糟）
- [x] 检查点按**工作区**构建而不是进程级：控制台一个进程服务多个工作区，进程级的一个
      checkpointer 只能对其中一个正确
- [x] `CheckpointSettings` 接到 `server.Config` 与 `ChatDeps`（`cmd/huan-agent/admin.go`）

## 7b. Web 控制台：审批卡片

- [x] `web/src/approval.js`：`approvalFromEvent` / `applyApprovalOutcome` / `pendingApprovals` /
      `approvalOutcomeLine` / `approvalSecondsLeft`（倒计时**不会变负**——负数会暗示请求还活着）
- [x] `ApprovalCard.vue`：摘要 + 预览（按 add/del/meta 分色）+ 三个按钮 + 拒绝理由输入 +
      倒计时 + 已决定后变成一行结论
- [x] 卡片贴在**输入框正上方**（与任务看板同层）：这一轮正卡在这个决定上，一个需要滚动的
      决定会拖慢这一轮
- [x] `chatStore`：`approval` 事件（公告与结算两种形状）、`submitApproval`、`findApproval`
- [x] `submitApproval` 的 404 处理与 ask 相反：**按拒绝结算**（服务端已经这样做了），
      而不是把卡片放回可编辑状态诱使第二次点击
- [x] `api.js` 的 `answerApproval`
- [x] `styles.css`：`.approval-card` / `.approval-stack` 等（exec 与 write 不同色，已决定后停止醒目）
- [x] ssr-probe 新增 25 条断言：公告/结算/超时三种形态、按钮只在 pending 时出现、
      "已决定"断言的是 action row 而不是某个词（结论行也会出现那个词）、倒计时边界、
      请求 URL 与 body 形状
- [x] `npm run check:ui` 全绿（**198 PASS**，含既有 173）
- [x] `npm run build` 重建 `internal/server/webui/dist`（确认 bundle 里有卡片标记与 CSS）

## 8. 配置与文档

- [x] `ApprovalConfig`（mode / allow / timeout_seconds）+ `ModeOr()` / `Timeout()`
- [x] `CheckpointConfig`（enable / dir / keep_turns / max_total_mb）+ `*Or()` + `CheckpointDirOrDefault`
- [x] `SetDefaults`：approval off / 300s；checkpoint on / 20 轮 / 512MB
- [x] `configs/config.example.yaml`：approval 与 checkpoint 两段（含各 surface 的行为差异、
      "超时=拒绝"、bash 不在覆盖范围）
- [x] 新 `docs/approvals-and-checkpoints.md`：为什么默认关、各 surface 表、超时语义对比表、
      卡片显示什么（含"命令行永不截断"的理由）、检查点的三件保命设计、覆盖与不覆盖范围、
      保留策略、位置、排障
- [x] `docs/tools.md`：补一句"审批是闸门、deny_patterns 仍是减速带"
- [x] `docs/tools.md` 里那句"没有确认环节"已改写：默认 `off` 时成立，打开后按 surface 不同
      （能问的弹卡片，不能问的**不注册**工具），并指向 `docs/approvals-and-checkpoints.md`
- [x] `docs/tools.md` 的 `deny_patterns` 注释旁补一句：审批是闸门，它是减速带
- [x] `docs/approvals-and-checkpoints.md` 补"控制台里的审批卡片"一节（位置、三态、不可翻转、
      以及为什么刷新后卡片不再出现）

## 9. 验证

- [x] `go test ./... -count=1` 全绿（新增 39 个测试）；`go vet ./...` 干净
- [x] `make build` 通过
- [x] 真机端到端（CLI + 真模型 + 真 gopls）：
  - 模型用 `edit_file` 改 `hello.txt` → `/checkpoints` 列出第 1 轮 1 个文件
  - `/rollback 1` → **不需要 `--force`** 恢复到原文，并提示"这次回退本身也可以撤销：/rollback 2"
  - 会话结束后文件内容确认是 `original content`
- [x] 一整轮真跑（`internal/server`，脚本模型 + 真 HTTP）：允许 → 工具执行一次；
      拒绝 → 工具**没有**执行，且理由出现在工具结果里回到模型
- [x] Web 审批卡片与 `npm run check:ui`（198 PASS；`npm run build` 已重建内嵌 UI）
- [~] `golangci-lint run`：**工具已装好并跑过**（`go install` 需要 workspace 之外的写权限，已获准）。
      本阶段新增/改动的代码是**干净**的；仓库里还剩 20 条**既有**告警，在本次工作**没有创建**的文件里
      （用 `git status` 逐个核对过：`memory.go`、`agent/audit.go`、`auth.go`、`skillapi.go`、`skills.go`、
      `mcp/manager_test.go`、`ask_test.go`、`usage.go`、`llm/retry_test.go` 等）。另外 `internal/lsp/sysproc.go`
      的 `processAlive` 是**误报**：它被 `//go:build integration` 的测试用着，而 linter 默认不编译带 tag 的文件
      —— 就是那个在 Phase 24 藏了一个编译不过的集成测试文件的同一个盲点。
      本阶段引入的 5 处死代码已在最后一轮清掉（`registerStaticTools`、`Server.checkpoints`、
      `workspaceToolSet.checkpoints`、`approvalGate.timeout`、`errNoApprover`）与 1 处 errcheck
      （`run.go` 的用量记录）。

## 10. 本阶段完成后的状态与已知边界

全部条目已完成。仍然成立的两个边界（都是有意的，不是缺口）：

- **飞书没有审批卡片**，所以 `mode != off` 时它拿不到写/执行工具。这是"一个必然失败的工具比
  没有更糟"的直接结果，日志里说明。给飞书做卡片是另一个集成面（要走它自己的交互卡片 API 与回调）。
- **`huan-agent run` 同样没有审批**（无人值守时没有可批的时刻），`mode != off` 时它不注册写/执行
  工具。`docs/run.md` 与 `docs/approvals-and-checkpoints.md` 都写明了。

以及一个实现上的边界：**审批卡片只在运行中的那一轮存在**。刷新之后看到的是工具卡片与它的结果，
因为审批发生在工具调用**之前**，服务器没有把它存成一条工具调用。回看时该看到的是"问了什么、
结果如何"，而不是一个已经不能再点的卡片。
