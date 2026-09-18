# 写代码方向的路线图：Phase 18–24

> 这份文档是 `CAPABILITY-GAPS.md` 那份缺口分析的**核实结果 + 落地计划**。
> 计划本体是 7 个 openspec change，每个都有 `proposal.md` / `tasks.md` / `specs/`。
> 本文只负责说明：哪条结论被核实、哪条被推翻、这 7 个 change 按什么顺序做、怎么算做完。

## 一、核实结论

核实方式：仓库内 grep / 读源码 / 实跑二进制，时间 2026-09。文中所有 `文件:行号` 引用都逐条
对过（绝大多数精确到行）。

### 属实的部分

| 原文结论 | 核实结果 |
|---|---|
| 无 LSP | 属实。`internal/` + `cmd/` 全量搜 `gopls\|lsp\|diagnostic` 仅 1 处命中，即文中指出的 `internal/store/trace.go:205` 注释 |
| 无 `web_search` / `fetch_url` | 属实。仅 `internal/metrics/metrics_test.go` 当字符串样本（另漏了第 490 行） |
| 工具串行、无子 agent | 属实，但理由需要更正（见下） |
| 只能 stop、不能 steer | 属实，且证据比原文更强：`internal/server/turns.go:384` 明说第二个消息 `reports as a conflict rather than silently queueing`——直接**拒绝**，不是排队 |
| 无审批、无检查点 | 属实。`approve\|approval` 在 Go 源码 **0 命中**；`snapshot\|checkpoint\|undo` 命中项全部无关（另漏列 `turns.go:576`、`stepretry_test.go:149`，但那些讲的是重试时撤销已流出的文本，结论不受影响） |
| 无 Dockerfile、无一次性执行入口 | 属实。子命令清单、`chat` 的 5 个 flag、`admin.go:300` 的注释都对得上 |
| 技能 `tools` 字段已接线、两份技能清单写错 | 属实。`internal/skill/skill.go:20`、`cmd/huan-agent/chat.go:405-422`、`internal/tool/registry.go:203-232` 都精确 |

### 必须更正的部分

**1. §6「全工作区同步…默认不同步」方向反了。**
`v.SetDefault("openviking.documents.sync_workspace", true)`（`internal/config/config.go:1102`）与
`sync_on_exit` 默认 `true`（:1108），而默认允许清单**包含源码**
（`internal/config/openviking.go:177-180` 的 `**/*.go`、`**/*.py`、`**/*.js`、`**/*.ts`、
`**/*.sh`、`**/*.sql`）。唯一的闸门是工作区目录为空时跳过，而 `tools.workspace` 恰是使用文件
工具的前提。实际结论是：**一旦配好工作区，整个代码仓库就会在每次 `chat` 退出时被上传并语义
索引**——这是默认行为。原文把它标为"未验证"，答案是对它不利的那一边。

**2. MCP 启动失败的归因错了：不是配置写错，是代码 bug（且可复现）。**

```
$ ./bin/huan-agent chat --tools
Error: build agent: connect mcp openviking: mcp: openviking: command is required for a stdio server
```

`configs/config.yaml:116-117` 是 `servers: []`，那条 openviking 是 `ApplyOpenVikingMCP()` 自动
生成的，并且**明确设成了 http**（`internal/config/openviking.go:383-388`，带 `Transport: "http"`、
`URL: .../mcp`、鉴权 `Headers`）。真正的原因在 `cmd/` 的两处手写转换丢字段：
`cmd/huan-agent/chat.go:382-387` 与 `cmd/huan-agent/workspace_tools.go:540-545` 只搬了
`Name/Command/Args/Env`，于是 `Transport` 为空、`ResolvedTransport()` 回落 stdio
（`internal/mcp/client.go:78-81`）、`Command` 为空 → 校验失败（:92）。对照 `internal/server/mcp.go`
的 `specFromStore`（:507）与 probe 构造（:380），它们**正确地传了** `Transport/URL/Headers`。

**这是一次实跑复现的启动阻塞**：按仓库自带配置，`chat --tools` 起不来，飞书那条路径同样。原文
只把它当成脚注里的"本机环境问题"，并把它误判为配置错误。→ Phase 18。

**3. 「gopls 已经装好」不成立。**
本机 `which gopls` **无输出**，`$(go env GOPATH)/bin` 里是 `dlv / goctls / protoc-gen-go /
wails / wire`，PATH 里唯一的语言服务器是 `/usr/bin/clangd`。所以这项改动的成本包含**安装与
外部进程管理**，而"语言服务器不存在"是必须被设计进去的常态。→ Phase 20。

**4. 「工具串行执行」属实，但两个循环的理由不同。**
- Web 循环 `internal/chat/runner.go:377` 是手写 `for`——串行只是没人写调度；
- CLI / 飞书走 Eino `react.Agent`，Eino 的 `ToolsNodeConfig.ExecuteSequentially` **默认 false 即
  并行**（`compose/tool_node.go:171-174`、`parallelRunToolCall`），而
  `internal/agent/agent.go:107` 显式设成 `true`，注释写着 `deterministic; helps audit ordering`。

所以并行改造是**两处**，其中一处要推翻一个写在代码里的理由。→ Phase 22。

**5. 并行有一个没人检查过的前提：能力标签不等于并发安全。**
`ask_user`（`chat.go:477`）、`plan_*`（`chat.go:498`）、`save_document`（`chat.go:522`）今天全带
`CapRead`。按标签推断并发安全，会把"挂住整轮的提问"和"对同一份计划做 read-modify-write 的更新"
一起并发掉。并发安全必须是**显式声明且缺省串行**。→ Phase 22。

**6. 补两条原文漏掉的缺口。**
- **没有 MCP server 模式**：只有客户端（`internal/mcp/`）与管理 API（`internal/server/mcp.go`），
  无法把 huan-agent 本身暴露给 IDE / 别的 agent 调用。对"生产力工具"这个定位是硬缺口，本路线图
  暂不排期（见"未排期的缺口"）。
- **`ask_user` 只在 Web surface 注册**：`cmd/huan-agent/chat.go:471` 是 `if opts.Surface == "web"`，
  CLI REPL 与飞书都拿不到。原文把它当作通用能力介绍，偏乐观。

### 其他小偏差（不影响结论）

- 「无 healthz/readyz」不完整：管理 API 确实没有，但飞书回调服务器有
  （`internal/platform/feishu/callback.go:135`）。
- 文中 `docs/tools.md:52-55`（SPEED BUMP）实际在 54-55；`docs/tools.md:230-231`（Ask for
  approval）精确；`docs/openviking.md:44-52` 的三个配置项实际在 48-51。都属于区间内含，可接受。
- `internal/metrics/metrics_test.go` 的 `web_search` 样本另有一处第 490 行。

## 二、7 个 change 与执行顺序

```
18
 └─→ 19
      ├─→ 20 ─────────────────┐
      └─→ 21 ─→ 22 ─┬─────────┴─→ 24
                    └───────────→ 23
```

**依赖关系**（`→` 读作"之后"）：

- `18 → 19`：19 的验收要靠 agent 能启动。
- `19 → 20`、`19 → 21`：这两者的端到端验收都要靠 `run`。
- `21 → 22`：子 agent 的能力收窄需要审批闸门先到位。
- `22 → 23`：联网工具注册时要声明并发安全（Phase 23 只做 `fetch_url`，这条依赖很轻）。
- `20 + 21 + 22 → 24`：`rename_symbol` 需要 20，检查点需要 21，并发声明需要 22。

推荐顺序：**18 → 19 → 20 → 21 → 22 → 23 → 24**。其中 20 与 21 之间没有依赖，可以互换；
21 必须在 22 与 24 之前（检查点是 24 唯一的退路），22 必须在 23 之前。

- **18 必须先做**：按自带配置 `chat --tools` 起不来，后续任何阶段的端到端验收都无从谈起。
- **19 紧随其后**：它把 agent 变成可脚本化的东西，从而让 20–24 的验收可以从"开个 REPL 手敲肉眼读"
  变成一条断言。这是让后面几个阶段可验证的前提，不是顺手的功能。
- **20 与 21 互不依赖**：先做 20（收益/成本比最高）还是先做 21（让人敢放手）是判断题，本路线图
  按"先改得对、再敢放手"排序。

| # | change | 内容 | 依赖 | 规模 | 完成定义（可验证） |
|---|---|---|---|---|---|
| 18 | `phase-18-mcp-transport-fidelity` | 收敛配置→运行态转换为一处、补齐 `Transport/URL/Headers`、修两份技能 `tools` 清单 | — | 1–2 小时 | `./bin/huan-agent chat --tools` 进入 REPL；`--skill code-review` 能读到文件 |
| 19 | `phase-19-one-shot-run` | `huan-agent run`：clean stdout、稳定退出码、`--output json`、一次性 surface 裁剪 | 18 | 0.5–1 天 | `run "…" --output json \| jq .` 成功；`git diff --cached \| run -` 可用 |
| 20 | `phase-20-code-intelligence` | `internal/lsp`（长驻 gopls）+ 4 个只读工具 + **编辑后自动附诊断** | 18, 19 | 3–5 天 | 制造一个类型错误，编辑结果里直接出现该错误；无 gopls 时启动不受影响 |
| 21 | `phase-21-approval-and-checkpoints` | 审批装饰器 + Web 卡片 + copy-on-write 检查点与回滚 | 18, 19 | 4–6 天 | `mode=writes` 下拒绝一次写入后模型改口；一轮改动可逐字节回退 |
| 22 | `phase-22-parallel-tools-and-subagents` | 并发声明（缺省串行）+ 带屏障调度 + `spawn_agent` | 18, 21 | 4–6 天 | `[read,write,read]` 严格保序而纯读并发；子 agent 的中间过程不入主上下文 |
| 23 | `phase-23-web-retrieval` | `fetch_url`（正文抽取、分页、缓存、SSRF 边界）；**搜索不自研**，走 MCP | 22 | 1.5–3 天 | 抓一个文档页得到正文与完整代码块；`169.254.169.254` 与 `file://` 被拒 |
| 24 | `phase-24-multi-file-edits` | `apply_patch`（两阶段原子）+ `rename_symbol`（LSP） | 20, 21, 22 | 3–5 天 | 含错误操作的批量编辑被整体拒绝且磁盘未变；重命名改到所有调用点且可编译 |

总计约 3–5 周（一个人、每阶段一个 change、每阶段都有 `go test ./...` + `golangci-lint` 作为
门槛）。这一列是量级估算，不是承诺；20、21、22 三个阶段的规模主要取决于测试要做到多细。

> 注：`openspec/specs/project.md` 的 Capabilities 表停在 Phase 7（`deployment`），8–17 都没进去，
> 所以本路线图的 7 个新 capability 也没有加到那张表里——加进去只会让它与现状更不一致。
> 需要在归档各 change 时统一补一次。

## 三、一句话的优先级理由

- **先让它能跑**（18）：一个在自带配置下启动就失败的 agent，讨论它的能力缺口没有意义。
- **再让它可测**（19）：`run` 的存在把后面每个阶段的验收成本降一个量级。
- **再让它改得对**（20）：把"改完 → 跑测试 → 发现类型错 → 再改"这个循环从"一次测试运行"缩到
  "一次工具调用"，这是收益/成本比最高的一项。
- **再让人敢放手**（21）：现在缺的不是能力，是让人敢把写权限交出去的底气。这是"交互式聊天"与
  "可托付的工程工具"的分界线。
- **然后才是省钱省时间**（22、23）：并发与联网省的是墙钟与上下文，不是正确性——但子 agent 对
  长任务的上下文预算影响很大。
- **最后是最大的一块**（24）：跨文件符号重命名与 Claude Code / Cursor 类工具差距最大，也最贵，
  且它必须有检查点做退路。

## 四、怎么执行

按仓库既有的 openspec 工作流（`CLAUDE.md` §openspec）：

```bash
/openspec:apply phase-18-mcp-transport-fidelity    # 实施
/openspec:archive phase-18-mcp-transport-fidelity  # 归档
```

每个 change 的 `tasks.md` 里每一节都对应一次可独立验证的提交，`- [ ]` 逐条勾掉。
`proposal.md` 的「明确不做（本阶段）」是有意写死的范围，要扩范围应该开新 change。

## 五、未排期的缺口（已知但不在这 7 个里）

- **`web_search`——不自研，走 MCP。** 搜索需要一个后端（托管 API 要 key 与额度，自建 SearXNG 要
  部署），而"哪家搜索、用哪个 key、额度怎么算"是部署问题，不是这个 agent 该自己实现的能力。
  仓库已有完整的 MCP 客户端（`internal/mcp`，stdio / sse / http 三种传输）与控制台的 MCP 管理面
  （`internal/server/mcp.go` 的 `/api/mcp/*`，含连接测试与热加载），所以搜索能力的正确来源是
  **装一个搜索类的 MCP server**。Phase 23 因此只做 `fetch_url`——它读内容，搜索结果本身只是索引，
  两块拼起来才是完整的"能查到且读得到"。这是一条路线决定，不是漏掉的一项。
- **把 huan-agent 暴露成 MCP server**，供 IDE / 别的 agent 调用。需要自己的 capability。
- **`ask_user` 的 CLI / 飞书支持**（今天只有 Web 有卡片）。
- **Dockerfile / 部署外壳**：`PLAN.md` 的 Phase 6 计划过，仓库里没有。它不影响"写好代码"这件事，
  所以排在这些之后。
- **飞书的审批卡片**（Phase 21 的明确不做）。
- **API 层的 `/healthz` / `/readyz` 命名**（`/api/health` 已存在且是未鉴权探针，够用）。
- **递归子 agent（深度 > 1）**：需要全局预算树。
