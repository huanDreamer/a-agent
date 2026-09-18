# 能力缺口分析：huan-agent 作为写代码的生产力工具

> 视角：把 huan-agent 当作日常写代码的助手用，它现在缺什么。
> 每条结论都标注了核实依据（`文件:行号`）。文末单独列出上一轮口述中被推翻的两条结论。
>
> 核实时间：2026-09-17。核实方式：仓库内 grep / 读源码 / 实跑二进制。下文「已验证」指我亲自跑过的命令或读过的代码，「未验证」会单独标出。

## 结论速览

| # | 缺口 | 对写代码的影响 | 现状 |
|---|------|----------------|------|
| 1 | 无 LSP：诊断 / 跳转 / 跨文件重命名 | 改完不知道有没有类型错误；重命名要逐文件 edit | 完全缺失 |
| 2 | 无联网检索工具 | 新库 API、报错原文只能靠训练数据猜 | 只能用 `bash curl` 顶着 |
| 3 | 工具串行执行、无子 agent | 多文件探索延迟线性叠加、上下文被脏数据占满 | 架构层面 |
| 4 | 只能 stop，不能中途补指令 | 发现方向错了只能整轮作废 | 部分缓解 |
| 5 | 无审批、无检查点 / 回滚 | 不敢无人值守交出写权限 | 刻意缺失 |
| 6 | 仓库级语义索引只覆盖记忆与文档 | 「哪段代码管鉴权」类问题只能 grep 关键词 | 部分具备 |
| 7 | 无 Dockerfile、无一次性执行模式 | 进不了 CI / git hook / cron | 缺失 |

按「对写代码的阻塞程度」排序，1 最阻塞。

---

## 一、代码智能：完全没有（最核心缺口）

### 现状

只有纯文本检索工具。工作区工具注册在 `cmd/huan-agent/workspace_tools.go:127-135`：

```go
readers := []struct{...}{
    {"read_file", builtin.NewReadFileTool},
    {"list_dir", builtin.NewListDirTool},
    {"glob", builtin.NewGlobTool},
    {"grep", builtin.NewGrepTool},
}
```

**无 LSP 接入（已验证）**：全仓搜 `gopls|lsp|diagnostic`，源码中零命中，唯一命中是无关的 `internal/store/trace.go:205`（注释里 "only a diagnostic"）和 `web/node_modules` 里的压缩产物。

### 缺的三件事

**1. 诊断（diagnostics）**

改完代码，模型不知道自己有没有制造类型错误，只能靠跑 `go build` / `go vet` 才知道——对动态语言更无从判断。IDE 里这一步是实时的。

**2. 跳转与引用**

`grep` 是字符串匹配。「这个方法被谁实现了」「这个接口有几个实现」「改这个签名要动哪些调用点」只能靠猜关键词，漏掉一个调用点就留下半个仓库编译不过。

**3. 跨文件符号重命名**

`edit_file` 要求 `old_string` 在文件内唯一出现：`internal/tool/builtin/files.go:450-455` 在歧义时直接报错——

```
edit_file: old_string appears %d times in %s, so this edit is ambiguous;
include more surrounding context to make it unique or set replace_all=true ...
```

且它是单文件、字符串级替换。重命名一个符号要 N 个文件逐个 edit，没有 patch、没有多文件原子编辑（**已修复**：Phase 24 的 `apply_patch` 提供两阶段、全有或全无的多文件编辑）。**这是与 Claude Code / Cursor 类工具的主要差距。**

### 建议

优先级最高。**先接 `gopls` 的诊断**，成本只是包一层已经装好的工具，直接把「改完 → 跑测试 → 发现类型错 → 再改」的循环砍掉一半。跳转/引用可作为第二步。

---

## 二、联网检索：没有专门工具

### 现状

`web_search` / `fetch_url` 在 `PLAN.md:174` 和 `PLAN.md:374` 的 Phase 2 里列了，但**代码里不存在（已验证）**。全仓搜这两个名字，除 `PLAN.md` 外只命中 `internal/metrics/metrics_test.go` 里把它当字符串样本用的测试（第 36、252-254、259、262、269、330-331、423 行）。

`PLAN.md:374` 该条目仍是未勾选的 `- [ ]`。

### 严格说不是完全不能联网

`bash` 不限制网络，`curl` 能抓到东西。但后果是**裸 HTML 直接进上下文**：

- 一个现代文档页转成文本动辄几百 KB
- 撞 `max_read_kb: 512` 上限被截断（`docs/tools.md:49`）
- 剩下的还以导航栏、CSS 为主

### 对写代码的影响

遇到新版本库的 API、报错信息、RFC、GitHub issue，模型只能靠训练数据猜。**猜错的结果是「看起来完全合理的错误代码」**，比明显报错更难发现。

### 建议

`fetch_url` 要做成「抓取 + 正文抽取（readability 一类）」再进上下文，而不是原样灌 HTML。`web_search` 需要一个搜索后端。

---

## 三、并行与分工：串行执行、没有子 agent

### 工具调用是串行的（已验证）

`internal/chat/runner.go:375-385`（循环起始在第 377 行）：

```go
for _, tc := range msg.ToolCalls {
    run := r.runTool(ctx, reg, tc, req, step, traceID, emit)
    ...
}
```

一次模型回复里的多个工具调用**顺序执行**（不是 `errgroup`、不是 channel）。模型要同时读 5 个文件、跑 2 条命令，就是 7 倍延迟。长任务里这个开销持续累积。

### 没有子 agent（已验证）

全仓搜 `subagent|sub-agent|delegate`，源码零命中（命中的是 `internal/memory/openviking.go:246,252` 的 `Delegates to the local store`、`internal/memory/openviking_test.go:474`，以及 `openspec/changes/archive/phase-4-platform-feishu/proposal.md:35` 描述 Eino agent 委派——都不是子 agent 机制）。

**后果是探索污染上下文**：为了改一个函数，先 grep 20 次、读 30 个文件摸清结构，这 50 次工具结果全部留在这轮上下文里（只能靠 `context.max_tokens` 压缩挽回）。

理想形态：子 agent 带着脏上下文去调研，只回一份结论，主 agent 的窗口留给代码本身。

---

## 四、中途插话：有 ask_user，但没有 steer

### 现状（这一条比上一轮口述的要好）

已经有的：

- **取消整轮**：`POST /api/chat/sessions/:id/turn/stop`（`internal/server/chat.go:278`）
- **模型主动提问**：`ask_user` 是已注册的工具（`internal/tool/builtin/ask.go:18` 定义 `AskUserToolName = "ask_user"`，注册点 `cmd/huan-agent/chat.go:472`），回答端点 `internal/server/chat.go:288`，等答超时 600s（`internal/config/config.go:181`）
- **中断后接续**：`POST /api/chat/sessions/:id/resume`（`internal/server/resume.go:56`），会生成一份 resume brief，把「计划 + 已执行步骤 + 工作区现状」交给新的一轮，而不是让用户从头重说

### 仍然缺的

**用户无法在轮次进行中主动补一句新指令。** `ask_user` 是模型问、用户答，方向是单向的；`resume` 是轮次已经死后接续，不是活着的时候转向。

发现模型理解错了方向（比如它准备大改而你想要小修），选项只有：停掉整轮 → 重说 → 前面已完成的工作作废。长任务里这个代价很高。

---

## 五、信任门槛：没有审批、没有回滚

两件事都是**刻意**缺的，不是疏漏。

### 审批：没有

`docs/tools.md:230-231`：

```
- **Ask for approval.** A write or a command runs when the model calls it. There
  is no confirmation step to accept or reject.
```

`openspec/changes/phase-6-agent-tools/proposal.md:65` 与 `specs/agent-tools/spec.md:75` 都写着交互式审批是 "a separate change"。

代码层面，`internal/tool/`、`internal/chat/`、`internal/agent/`、`cmd/` 里搜 `approve|approval|require_confirm|needs_confirm` **零命中（已验证）**。

意味着 `rm`、`git reset --hard`、`git push`、改 CI 配置这类动作没有任何确认环节。唯一的拦截是 `deny_patterns`，而 `docs/tools.md:52-55` 自己承认：

```
# A SPEED BUMP, not a security boundary: an equivalent command that does not
# match will run.
```

### 检查点 / 回滚：没有文件级实现

搜 `snapshot|checkpoint|undo`（排除 `openspec/changes/archive`），命中的全是无关的：Langfuse 计数器快照（`internal/langfuse/types.go:97`）、LLM 目录同步快照（`internal/llm/catalog.go:279`、`catalog_test.go` 的 `snapshotCatalog`）、内存镜像计数（`internal/memory/openviking.go:120`）、SSE 测试辅助（`internal/server/ask_test.go`）、预算快照（`internal/server/budget.go:145`）。**没有一个是文件级快照/回滚。**

`step_retry`（`docs/long-tasks.md`）重跑的是模型调用，不回滚磁盘。

### 合起来的后果

**不太敢让它在自己仓库里无人值守地跑几十分钟。** 大范围重构改到一半发现方向不对，只能靠 git——而 git 也得模型自己记得先 commit。

### 建议

如果要投产出，这一条比第一条更值得先做。现在缺的不是能力，是让人敢把写权限交出去的底气。最小可用形态：写/执行类工具调用前挂一个可批准/拒绝的钩子（Web 上就是一张卡，机制可复用 `ask_user` 的事件通道），加上「每轮开始前对改动文件做一次快照」。

---

## 六、仓库级语义索引：只覆盖记忆和文档

OpenViking 有语义检索（`find` / `search` 工具），但**工作区文本文件的同步是为了进记忆/文档库，不是为代码建语义索引**。

`docs/openviking.md:44-52` 的配置项：

- `documents.workspace_dir`：要同步的目录，空则用 `tools.workspace`；两者都空则**不同步**
- `documents.sync_on_exit`：交互式 `chat` 退出时增量同步（默认开）
- `documents.sync_interval_seconds`：`serve` 后台定时同步（默认 0 = 关）

`internal/viking/` 下只有 `service.go` / `service_test.go` 两个文件，搜 `embedding|vector` **零命中（已验证）**——embedding 是 OpenViking 服务端做的。全工作区同步（含代码，不只文档）是一个显式开关，默认不同步。

后果：「哪段代码负责处理鉴权」这种概念级问题，只能靠 grep 关键词试。

> 未验证：我没有实跑 `documents.sync_workspace: true` 看它是否会把整个代码仓库灌进文档库。如果会，那它带来的是「文档检索」而不是「代码索引」——仍不是 IDE 那种符号级索引。

---

## 七、工程外壳：几样基础设施缺位

- **无 `Dockerfile` / `docker-compose.yml`（已验证）**。根目录 ls 无此文件；`PLAN.md` 的 Phase 6 计划了，仓库里没有。
- **无 `healthz` / `readyz`（已验证）**。但注意：**这不影响 API 探活**——`/api/health` 是存在的未鉴权 liveness 探针（路由 `internal/server/server.go:300`，实现 `internal/server/api.go:88-95`，返回 `{ok, version, uptime_s}`）。另有 `/metrics`（`server.go:293`，受 `MetricsEnable` 控制）。缺的只是容器化场景惯用的 `/healthz` 命名。
- **无一次性执行模式（已验证）**。所有子命令：`chat`（`cmd/huan-agent/chat.go:47`，交互式 REPL）、`serve`、`admin serve|set-password`、`viking status|sync|save|remember|search`、`usage`、`version`。**没有 `huan-agent run "<prompt>"` / `--print` 这类「给一个 prompt、跑完退出」的入口**，`chat` 也没有 `--print` flag（`chat.go:79-83` 只有 provider/model/system/tools/skill）。

  `cmd/huan-agent/admin.go:300` 那处 `non-interactive` 注释讲的是 admin 密码 flag，与此无关。

  这意味着**没法把它放进 git hook、CI、cron**——而「提交前帮我审一遍」「CI 挂了自动定位」正是写代码场景的高频用法。

---

## 更正上一轮口述中的两处错误

这两条我上一轮说错了，在此更正：

**1. ~~技能的 `tools` 字段没有接线~~ —— 错了，它接线了。**

`internal/skill/skill.go:20` 定义 `Tools []string`，消费点在 `cmd/huan-agent/chat.go:406-422`：

```go
// Apply allow-list: skill wins, then config, else "allow all".
switch {
case sk != nil && len(sk.Frontmatter.Tools) > 0:
    if err := reg.SetAllowList(sk.Frontmatter.Tools); err != nil { ... }
case len(cfg.Agent.AllowedTools) > 0:
    if err := reg.SetAllowList(cfg.Agent.AllowedTools); err != nil { ... }
}
```

`SetAllowList` 实现在 `internal/tool/registry.go:203-232`，且会校验名字是否已注册，未知工具名直接报错。`--skill` 的 flag 描述也写明了 "system prompt + tool allow-list"（`chat.go:83`）。

**但发现了一个真实问题**：`configs/skills/code-review.md` 的 frontmatter 是 `tools: [calc, echo]`，而它是个**代码评审**技能——给了 `calc` 和 `echo`，**没给 `read_file` / `grep` / `glob`**，模型连 diff 都读不到。`daily-summary.md` 是 `tools: [time, echo]`，同样读不到文件。

这不是机制缺失，是配置写错了。另外注意 `SetAllowList` 的语义是**收窄**：skill 的 tools 一旦非空，其它工具全部不可用。所以这套「技能声明工具」的设计要配一份准确的清单才安全。

> 未验证：我没跑通 `--skill code-review` 的端到端（本机 `configs/config.yaml` 的 openviking MCP 是 stdio 模式但缺 `command`，首次工具初始化就报 `mcp: openviking: command is required for a stdio server` 并退出）。allow-list 的接线结论来自源码阅读，不是运行验证。

**2. ~~没有 healthz / readyz~~ —— 部分错了。**

`/api/health` 存在且是未鉴权探针（见第七节）。我上一轮把它误判为「没有」，原因是 grep 时用了 `| head` 截断，输出被前几行占用而漏掉了 `internal/server/server.go:300` 那一行。

---

## 建议的优先级

**若只做一件：接 `gopls` 诊断。** 收益/成本比最高——把「改完 → 跑测试 → 发现类型错 → 再改」的循环次数砍掉一半，成本只是包一层已装好的工具。

**若要投产出：审批 + 检查点。** 现在缺的不是能力，是让人敢把写权限交出去的底气。这也是把「交互式聊天」变成「可托付的工程工具」的分界线。

**顺手的低成本项：**

1. 修 `configs/skills/code-review.md` 和 `daily-summary.md` 的 `tools` 清单（配置问题，不是代码问题）
2. `huan-agent run "<prompt>"` 一次性执行入口——让工具能进 CI / git hook
3. `internal/chat/runner.go:377` 的工具循环改并发（注意：同一轮里对同一文件的写操作需要保序）

---

## 附录：核实方法与范围

- 核实方式：`grep -rn --exclude-dir={.git,node_modules,.gocache,.idea}` + 读源码 + 实跑 `bin/huan-agent`
- 排除范围：`web/node_modules`、`.git`、`.gocache`、`openspec/changes/archive`（归档提案不算现状）
- 明确未验证的项：OpenViking 全工作区同步的实际行为、`--skill` 的端到端运行（被本机 MCP 配置阻塞）
- 本文档只做缺口分析，不含实现方案设计
