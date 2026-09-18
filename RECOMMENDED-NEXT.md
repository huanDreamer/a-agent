# 还需要什么、且紧急：huan-agent 现状复核

> 视角：huan-agent 当作写代码的生产力工具，**在当前实况下**还缺什么、哪几件真的紧急。
>
> 核实时间：2026-09-18（同日 09:51 做了第二遍复核，数字已对齐，见 §P0-1 成因处的修正记录）。
> 方式：读源码 + 查数据库 + 实跑（`go build ./...` 通过、`go test ./...` 34 包 ok 无 FAIL，
> 第 35 个包 `internal/version` 无测试文件）。
> 每条结论标注依据（`文件:行号` 或命令输出）。「已验证」= 我亲自跑过或读过；推断会单独标出。
>
> **这份文档取代了早先的 `CAPABILITY-GAPS.md`**（该文件已删除，其内容并入本文文末的对照章节）。
> 那份写于 Phase 18–24 落地之前，7 条缺口里有 6 条已经实现
> （见文末「上一版为什么失效」）。请以本文为准。

## 结论速览

| # | 事项 | 性质 | 紧急度 | 依据 |
|---|------|------|--------|------|
| 1 | trace 数据无裁剪，数据库无界增长（**线性 + 平台期，不是 O(n²)**） | **已经在发生**，不是风险 | **P0** | `data/huan-agent.db` 191MB，observations 占 148MB，全仓无裁剪代码；`auto_vacuum=0` ⇒ 裁剪后还需 `VACUUM` |
| 2 | `run` 的写权限与保护措施强耦合（要么全信任、要么全残废） | 架构取舍，挡住 CI/hook 场景 | **P0** | `approval.go:28-41` + `run.go` 零 checkpoint |
| 3 | phase-5c 未完成（58 项：实时追踪 / 抽包 / CLI·飞书接入） | 功能未交付 | P1 | `openspec/changes/phase-5c-local-tracing/tasks.md` |
| 4 | `ask_user` 与审批只有 Web 有卡片 | 飞书路径能力残缺 | P1 | `chat.go:530`、`cmd/huan-agent/approval.go:36-40` |
| 5 | 无 MCP server 模式，无法被 IDE/别的 agent 调用 | 定位缺口 | P2 | 仅 `internal/mcp/` 客户端，无 `serve` 入口 |
| 6 | 无 Dockerfile / 部署外壳 | 交付缺口 | P2 | 仓库根目录无 `Dockerfile` |
| 7 | 文档滞后于实现 | 维护债 | P2 | `docs/status.html` 停在 Phase 9；`project.md` Capabilities 停在 Phase 7 |

前两条是「需要在写下一行代码之前处理」，后五条是「排期即可」。

**但这两条 P0 不是并列的，而是一条有序链**（依据见 P0-1 末的「与 P0-2 的顺序依赖」）：
trace 表先有界，`run` 才有资格接进 CI。反过来的话，`run` 每跑一次就往同一张无界表写一份
trace，P0-1 的增长速率会从「跟开发时长挂钩」变成「跟提交频率挂钩」。

---

## P0-1：trace 数据无裁剪，数据库已在无界增长

### 实测数据

```
$ ls -lh data/huan-agent.db
191M

$ sqlite3 data/huan-agent.db "SELECT COUNT(*), SUM(LENGTH(input))/1048576 FROM observations;"
2912|148       # 单 input 一列占 148MB

$ sqlite3 data/huan-agent.db "SELECT trace_id, COUNT(*), SUM(LENGTH(input))/1048576
                              FROM observations GROUP BY trace_id ORDER BY 3 DESC LIMIT 3;"
300d1903...|340|38    # 一条 340 节点的 trace，38MB
a6b0b4cc...|230|30
156134cc...|153|11
```

体积**全部**落在模型调用上，工具调用几乎不占：

```
$ sqlite3 data/huan-agent.db "SELECT type, COUNT(*), SUM(LENGTH(input))/1048576 FROM observations GROUP BY type;"
GENERATION|1216|148
SPAN|1696|0
```

这一条很重要：上表那条「340」是**节点总数**，其中只有 **161 个**是 generation。
后面算增长率必须按 161 算，按 340 算会把每步体积低估一半、把成因误判成平方。

按天增长（`SUBSTR(started_at,1,10)` 分组）：

| 日期 | observations | input 体积 |
|---|---|---|
| 09-13 | 149 | 1.5 MB |
| 09-14 | 6 | 0 MB |
| 09-15 | 913 | 32.6 MB |
| 09-16 | 375 | 20.7 MB |
| 09-17 | 1098 | 80.0 MB |
| 09-18 | 371 | 13.0 MB |

**5 天 148MB。** 09-17 单日 80MB。写下这段时是 178MB / 140MB，38 分钟后复核已再涨 13MB
——数字只会更快变旧，这本身就是本条要给 P0 的理由。

### 成因：每步存完整历史，且没有任何长度上限

`internal/tracing/recorder.go:163`（`StartTrace`）、`:201`（`StartSpan`）、`:244`（`StartGeneration`）
三处都是 `Input: encodeJSON(info.Input)`——`info.Input` 是**该步的完整 message 列表**。取一条实证：

```
$ sqlite3 data/huan-agent.db "SELECT SUBSTR(input,1,300) FROM observations
                              WHERE LENGTH(input)>10000 ORDER BY LENGTH(input) DESC LIMIT 1;"
[{"role":"system","content":"你是 huan-agent，一个通用 AI Agent。…
```

system prompt 每步重存一遍——这句成立，但它是 **O(n) 的常数浪费，不是平方**（见下节修正）。
`encodeJSON`（`:418`）只做 JSON 序列化，**没有任何长度上限**；`clip`（`:450`）只用于
`Name`/`Model` 这类短标签（`maxNameLen = 200`，`:67`）。写入侧
（`internal/store/trace.go:120` `RecordObservation`）也不截断。

#### 修正：增长是「线性 + 平台期」，不是 O(n²)

本文件早先的版本把成因写成「第 n 步存 n 步历史，一个 340 步的 trace 就是 Σ(1..340) 倍」，
并据此把「加长度上限」当成能消掉 O(n²) 主体的第一步。**这个机制判断是错的**，第二遍复核时
用逐步骤体积把它打掉了：

```
$ sqlite3 data/huan-agent.db "SELECT step, LENGTH(input)/1024 FROM observations
     WHERE trace_id='300d1903...' AND type='GENERATION' AND step IS NOT NULL ORDER BY step;"

step  10:  75KB      step  80: 411KB
step  40: 248KB      step 100: 452KB
step  60: 339KB      step 120: 480KB
                     step 121:  18KB   ← 压缩触发，断崖回落
                     step 161: 108KB
```

分窗斜率是**递减**的：`+5.8 → +4.1 → +1.7 KB/step`，在 ~480KB 处饱和，而不是无限累积。
这条 trace 是 **161 个 generation 步**（不是 340 个节点）× 均值 240KB ≈ 38MB
——**对步数线性，不是平方**。

原因是仓库里有活的上下文压缩：`configs/config.yaml:153` 是 `context.max_tokens: 60000`，
`cmd/huan-agent/turnbudget.go:60` 据此构造 `context.Manager`，超预算就把旧消息折叠成摘要
（`internal/context/context.go` 的 `ShouldCompress` / `Compress`），于是每步重新发给模型的
message 列表**有上界**——上图 480KB 的平台和 step 121 的断崖就是它在工作。

「平方增长」这个说法确实存在于代码库里，就是 `cmd/huan-agent/turnbudget.go:47-49` 的告警文案
（守卫在 `:47`，文案在 `:49`）。但它只在**压缩被关闭**时才触发：

```go
if cfg.Context.MaxTokens <= 0 {          // ← 只有走这个分支才会 Warn
    logger.Warn("长任务未启用上下文压缩：……token 成本随步数近似平方增长")
    return nil, nil
}
```

本机 `context.max_tokens` 是 60000 而非 0，**这个分支是死的**。早先那版把「未启用压缩」
时的机制套用到了「已启用压缩」的实况上，这是错的来源。

**这个修正改变了修复的权重**：`context.max_tokens` 只管住「每步多大」，**没人管「总共留多少」**。
所以增长是 `步数 × ~240KB` 且没有平台——加长度上限只是把 240KB 降到几十 KB 的**常数改善**，
真正让它有界的是裁剪。两件都要做，但别把幌子当骨架（见下面的建议）。

### 缺口：没有任何清理路径

全仓搜（排除测试）：

```bash
grep -rn "PruneTraces\|ClearTraces\|DELETE FROM traces\|DELETE FROM observations\|retention" --include=*.go .
# → 仅命中 internal/checkpoint/checkpoint.go（那是检查点目录的保留策略，与 trace 无关）
grep -rn "VACUUM" --include=*.go internal/
# → 零命中
```

`internal/store/trace.go` 的导出方法只有 `RecordTrace` / `RecordObservation` / `EndTrace`
/ `EndObservation` / `ListTraces` / `GetTrace`——**只写读，没有删**。

### 这不是「以后可能有用」的预防

phase-5c 的 `tasks.md` 已经设计好了这部分，正是没做的那批：

- `:140` `store.PruneTraces(policy)`：按 age 与 count 双阈值
- `:142` `store.ClearTraces`：显式删除 observations 再删 traces
- `:183` `tracing.retention_days`（默认 14）、`tracing.enable`（默认 true）
- `:187` 启动时跑一次裁剪，之后按 `prune_interval` 周期跑

也就是说：**设计已定，代码未写。** 而 `internal/config/config.go` 里搜不到任何 `tracing.*`
的 `SetDefault`（只有 `LangfuseConfig`，`:319`）——配置项本身也不存在。

### 影响

- 本机 5 天 191MB；这是单人日常使用的速率，不是压测数字。
- trace 面板列一个 40MB 的 trace 要把它整个读出来（`GetTrace`，`trace.go:272`）。
- `ListTraces` 的 `SELECT` **包含 `input, output` 列**（`trace.go:227`），列表页白白拖大字段。
  实测这 50 行查询的差距：

  ```
  $ time sqlite3 data/huan-agent.db "SELECT id,name,...,input,output,... FROM traces ORDER BY started_at DESC LIMIT 50;"
  real 0m0.051s
  $ time sqlite3 data/huan-agent.db "SELECT id,name FROM traces ORDER BY started_at DESC LIMIT 50;"
  real 0m0.006s
  ```

  **8.5 倍**。当前 traces 表大字段合计只 110KB（110 行），所以绝对值还不难听；
  但它是随 trace 数线性增长的（约 1KB/行），而列表页从来不显示这两个字段
  （`web/src/components/TraceView.vue` 只在详情里用 `detailTrace.input`，列表不渲染）。

### 建议

最小切片，1 天内可完成。**顺序按「谁让表有界」排，不按改动量排**：

1. **`store.PruneTraces(retention_days, max_traces)` + 启动时跑一次**（照 phase-5c 的设计），
   外加 `tracing.retention_days` / `tracing.max_traces` / `tracing.prune_interval` 走
   `viper.SetDefault`。**这一条是让表有界的唯一一件**：没有它，下面两条都只是把斜率改小。
2. 写入侧加长度上限（如单条 input 超 64KB 就只存尾部 N 条消息 + 一个 `truncated: true` 标记）。
   这是**常数改善，不是有界化**：按上面的实测，约 3.7 倍（480KB/步 → ~64KB/步）。
   仍值得做——system prompt 每步重存一遍纯属浪费——但要清楚它消不掉「无界」。
3. `ListTraces` 的 `SELECT` 去掉 `input, output`（列表本来就不显示它们）。
4. **裁剪必须配 `VACUUM`，否则文件不会变小**——这一条 phase-5c 的设计漏了，仓库里
   任何地方都没提（`grep -rn -i "vacuum\|auto_vacuum\|incremental" openspec/` 零命中）。
   实测本库：

   ```
   $ sqlite3 data/huan-agent.db "PRAGMA page_count; PRAGMA freelist_count; PRAGMA page_size; PRAGMA auto_vacuum;"
   49119 0 4096 0        # 191MB 全是活页，空闲页 0，auto_vacuum 关闭
   ```

   两个后果：
   - **现在跑 `VACUUM` 一点用没有**（`freelist_count = 0`，没有可回收的空闲页）。
     所以别指望它在裁剪之前救急。
   - **但裁剪落地后不配 `VACUUM` 就白做**：`auto_vacuum = 0` 时 `DELETE` 只把页标为空闲，
     文件大小不降——库会一直停在历史最高水位（现在就是 191MB，且峰值还在涨）。
     那时会看到「`PruneTraces` 明明在跑、行数明明少了，文件却还是 191MB」。
     要么定时 `VACUUM`（需库文件大小的额外临时空间并短暂锁库），
     要么一次性 `VACUUM` 转成 `auto_vacuum=INCREMENTAL` 后用 `incremental_vacuum` 增量回收。
     **这条应作为 P0-1 切片的一部分，而不是留给以后。**

### 与 P0-2 的顺序依赖：先裁剪，再让 `run` 进 CI

`run` **已经在往这张无界表写 trace**：

```go
cmd/huan-agent/run.go:263   tracer := tracing.NewRecorder(st, logger)
cmd/huan-agent/run.go:394   Tracer:      tracer,
```

而 P0-2 要做的正是把 `run` 接进 git hook / CI。这两条 P0 因此不能并列推进——一旦 `run`
挂上 pre-commit，trace 写入速率就从「跟开发对话时长挂钩」变成「跟提交频率挂钩」，
P0-1 的增长率上一个台阶。**裁剪必须先落地**，否则 P0-2 是在给一个漏的桶接更大的水管。

---

## P0-2：`run` 的写权限与保护措施强耦合

`huan-agent run` 是接进 CI / git hook / cron 的唯一入口（`cmd/huan-agent/run.go:110`，
`Short` 写着 "for scripts, hooks and CI"）。它现在只有两个档位，没有中间态。

### 档位一（默认）：完全信任

- `tools.read_only` 默认 **false**（`internal/config/config.go:1459`）
- `tools.enable_bash` 默认 **true**（`:1460`）
- `tools.approval.mode` 默认 **off**（`:1482`）

`run.go` 里没有任何 `checkpoint` 引用（`grep -n checkpoint cmd/huan-agent/run.go` 零命中）。
所以无人值守的 run：**有完整写权限 + 无审批 + 无回滚**。改坏了只能靠它自己先 commit。

### 档位二（配了审批）：完全残废

`cmd/huan-agent/approval.go:28` 的 `approvalSurfaceCanAsk`：

```go
case prompt.SurfaceWeb:
    return true
case prompt.SurfaceCLI:
    return true
default:
    // Feishu has no card for this yet, and a one-shot run has nobody watching
    // by definition. Both withhold the gated tools instead.
    return false
}
```

`SurfaceRun`（`internal/prompt/prompt.go:38`）落进 `default`（`approval.go:36`）→ `canAsk = false`。
而 `wrap`（`approval.go:80-87`）：

```go
func (g approvalGate) wrap(t agenttool.Tool) (agenttool.Tool, bool) {
    if !g.active() || !g.policy.Requires(agenttool.CapabilityOf(t)) {
        return t, true
    }
    if !g.canAsk {
        return nil, false        // ← 不是拒绝，是从清单里整个摘掉
    }
    return agenttool.Gate(t, ...), true
}
```

于是配了 `--approval writes+exec` 的 CI 任务里，`write_file` / `edit_file` / `bash` /
`apply_patch` / `rename_symbol` **根本不在工具清单里**。代码注释自己承认了这个取舍
（`approval.go:73-78`：「The withheld case is not a refusal: the tool is left out of the
list entirely... a tool that was never offered」），理由是「refused 比 absent 更糟」——
这个理由对**交互式**场景成立，对 CI 场景不成立：

> CI 里没人看，但**执行者（CI 脚本）可以事先声明权限**。
> 「按策略需要确认，但无人在场 → 那就别做」是正确行为；
> 但「于是连读带写的工具全摘掉，任务整个无法完成」不是。

### 为什么这挡住 CI 场景

git hook / CI 里的典型用法是「读 diff、跑测试、（可选）改代码」。今天：

- 不配审批 → 它可以在 CI 里跑 `git push`、改 CI 配置，无人拦。风险不可接受。
- 配审批 → 它连 `read_file`（`CapRead` 在 `all` 模式下要审批，default 的 `writes+exec` 下读还能用）
  之外什么也做不了，任务必然失败。

两者都不是「可托付」。

### 建议

给 `run` 一个**非交互的权限档位**——显式声明，而不是「配了审批」的副作用。
**边界用档位，便利性才用正则**，这个顺序不能反：

1. **档位式声明（这才是边界）**：`run --read-only` / `run --allow-write`，
   把「无人值守时允许动什么」变成命令行里一句明说的事。被拒时**返回失败**而不是把工具
   从清单里摘掉——CI 需要的是「按策略需要确认但无人在场 → 失败并给出非零退出码」，
   而不是「工具消失、任务以另一种方式失败」。
2. **`allow` 正则只当便利项，不要拿它撑安全诉求**：`ApprovalPolicy.Allow` 确实已存在
   （`internal/tool/approval_gate.go:45`，匹配在 `:99` 的 `Allows`），但**它自己声明了不是边界**：

   > `approval_gate.go:41` — Like deny_patterns, this is a convenience and **NOT a boundary**:
   > an equivalent action that does not match still runs.

   `tools.deny_patterns` 的注释（`internal/config/config.go:574-576`）更直白：
   「A speed bump … NOT a security boundary: an LLM can trivially write an equivalent command
   that does not match.」 所以「用 allow 正则做命令级白名单来防 CI 里 `git push`」是站不住的
   ——它挡不住等价改写。要边界就用第 1 条的档位。

3. **`run` 接 checkpointer**（`internal/checkpoint/` 已就绪，Web 路径在用，
   `internal/server/checkpoint.go:144` 有 `rollback` 端点）。接线量很小，但它决定了
   「CI 里改坏了能不能恢复」。

> 早先的版本把「`run --allow-write` + `allow` 正则白名单」放在首位推荐——那是在用
> 一个自称非边界的机制去承担边界职责。第 1 条的档位才是有边界的那个。

**执行前提**：本条落地后 `run` 才会被接进 hook / CI，而 `run` 已经在写 trace
（见 P0-1「与 P0-2 的顺序依赖」）。**先做完 P0-1 的裁剪再动本条**。

---

## P1-3：phase-5c 未完成（58 项）

`openspec/changes/phase-5c-local-tracing/tasks.md` 是真未提交的工作，不是过时文档：

```
phase-5c-local-tracing: 50 done / 58 todo
```

已完成的是 §0「最小切片」+ §2 本地存储 + §3 写入路径（`internal/tracing/` 存在，
`cmd/huan-agent/admin.go:397-400` 装配了 `Recorder` + `Multi` 镜像），且 `langfuse` 类型**尚未抽包**——
`internal/tracing/recorder.go:42` 仍 import `internal/langfuse`（`tasks.md:113` 标注为
「当前为临时状态：`internal/tracing` 反向 import `internal/langfuse` 取类型」）。

未做的部分：

- §1 抽包（观测词汇中立化）
- §5b 实时推送（`tracing.Hub` + `GET /api/traces/stream` SSE）
- §6a 前端实时（`useTraceStream`、进行中渲染）
- §7 CLI / 飞书接入（`grep Tracer cmd/huan-agent/chat.go cmd/huan-agent/serve.go` 零命中）。
  注意 `run` **已经接了**（`cmd/huan-agent/run.go:263`、`:394`），所以缺的是 CLI REPL 与飞书这两条。
- §5 `GET /api/traces` 新增 `backend` 字段、`DELETE /api/traces`

**为什么紧急度只给 P1**：这些是「看 trace 的体验」，不是「写代码的能力」。
但注意 P0-1 的裁剪设计（§4「本地存储」的 `:140`-`:142`）也在这一批里——
**做 P0-1 时应当顺手把这个 change 归档掉**，否则两边会各写一份裁剪逻辑。

---

## P1-4：`ask_user` 与审批卡片只有 Web 有

```go
// cmd/huan-agent/chat.go:530
if opts.Surface == "web" {
    // ask_user 注册
}
// cmd/huan-agent/chat.go:551
if opts.Surface == "web" && cfg.Chat.Plan.Enable {
    // plan_* 注册
}
```

`cmd/huan-agent/approval.go:36-40` 同样：飞书 `canAsk = false`（注意不是 `internal/tool/approval.go`，后者是闸门实现本身）。

后果：飞书 bot 上的写操作要么无审批（如果没配 `tools.approval.mode`），要么被静默摘掉
（配了的话）。飞书审批卡片是 Phase 21 明确的「不做」项，所以这是**已知取舍**，
但结合 P0-2，它意味着**三个非 Web 路径（CLI 的 plan/ask、飞书、run）能力显著更弱**。

CLI 有一条补偿：REPL 支持 `/rollback`（`cmd/huan-agent/chat.go:342`），
所以 CLI 的检查点是通的。飞书与 run 没有。

---

## P2-5：没有 MCP server 模式

`go.mod:12` 是 `github.com/mark3labs/mcp-go v0.54.1`，但全仓搜 `mcp-go/server`
只在**测试**里命中（`cmd/huan-agent/mcp_test.go:15`，用来起一个假 server 验客户端）。
生产代码里没有把 huan-agent 暴露成 MCP server 的入口，子命令只有
`admin` / `serve` / `chat` / `run` / `usage` / `viking` / `version`。

对「生产力工具」定位的影响：Cursor / Claude Code 这类工具生态正在用 MCP 做互操作，
不能被调用 = 不进别人的工作流。

需要自己的 capability（roadmap §五 已记，未排期）。**不紧急**，因为它不改善本机写代码。

---

## P2-6：无 Dockerfile / 部署外壳

仓库根目录无 `Dockerfile` / `docker-compose.yml` / `.dockerignore`（`ls` 无输出）。
`PLAN.md` Phase 6 计划过。

本机单二进制 32MB（`bin/huan-agent` 实测）+ SQLite 单文件 + 前端 embed，
容器化成本很低，但它不影响「写好代码」。**不紧急。**

## P2-7：文档滞后

- `docs/status.html`：`grep -o 'Phase [0-9]\+[a-z]*' | sort -u` 得到
  `0 1 2 3 4 5 5b 6 7 9 10`——**缺 8 与 11–24**，且 10 是唯一进入两位数的。
- `openspec/specs/project.md`：Capabilities 表停在 `deployment`（:69），8–24 全没进。
- 早先的 `CAPABILITY-GAPS.md`（已删除）：7 条缺口 6 条已失效，对照见下节。
- `openspec/changes/` 下 27 个 change（`ls -1d openspec/changes/*/` 计 28，含 `archive/` 本身）
  里只有 4 个归入 `archive/`（phase-1 到 phase-4）。其余 23 个——含 phase-0、phase-4-feishu-completion、
  phase-5*、phase-6 到 phase-24——全部还挂在 `changes/`，已完成也未归档。

这是维护债，不是能力缺口。**建议在归档 P0 改动时一并清一轮。**

---

## 附：上一版缺口分析为什么失效（`CAPABILITY-GAPS.md`，已删除）

那份写于 Phase 18–24 **落地之前**。现在它们已实现并提交（`453e07a` "编码 agent 能力补齐
（Phase 18–24）"），逐条对照：

| 上一版结论 | 现在 |
|---|---|
| 无 LSP | **已实现**：`internal/lsp/` + 5 个工具（`workspace_symbols` / `goto_definition` / `find_references` / `rename_symbol` / `diagnostics`），底层 gopls。实测 `diagnostics` 能报到真实编译错误 |
| 无 `web_search` / `fetch_url` | **`fetch_url` 已实现**（`internal/tool/builtin/web.go`，含分页、缓存、SSRF 边界）；`web_search` 走 MCP（有意不自研） |
| 工具串行、无子 agent | **已实现**：`spawn_agent`（`internal/subagent/`）+ 并发声明（`internal/tool/concurrency.go`）。深度上限 1（`subagent.go:485`） |
| 只能 stop 不能 steer | **已实现**：`plan_*` + resume 回合（`internal/server/turns.go:366`「continues an interrupted turn」） |
| 无审批、无检查点 | **已实现**：`internal/tool/approval_gate.go` + `internal/checkpoint/`（`tools.checkpoint.enable` 默认 true）+ Web 回滚端点 |
| 无一次性执行 | **已实现**：`huan-agent run`，稳定退出码 0–6、`--output json`、`run -` 读 stdin |
| 无 Dockerfile | 仍缺（本文 P2-6） |

另外**修正上一版的一处误判**：OpenViking 工作区同步。上一版说「整个代码仓库会在每次 chat 退出时
被上传并语义索引」——本机数据不支持这个断言：

```
$ python3 -c "... data/openviking-docs.json ..."
documents/2026/09/openviking-接入验收-95aa5ca8.md
f1.go  f2.go  f3.go  f4.go  f5.go  f6.go      # 测试残留
hello.txt
```

**只有 8 个文件**，因为 `configs/config.yaml` 没有配 `tools.workspace`
（同步的闸门是「工作区目录为空则跳过」，`openviking.go:177-186` 的允许清单确实包含
`**/*.go` 等，`documents.enable: true` 与 `sync_workspace: true` 也确实开着）。
准确说法是：**机制已就绪且默认开启，本机因未配工作区而未触发**——一旦配上工作区，
允许清单里的源码就会在每次 `chat` 退出时上传。这是需要知道的默认行为，但不是「已经发生」。

---

## 附：核实方法

```bash
# 构建与测试
go build ./... && go test ./...          # 34 包 ok，无 FAIL（第 35 个包 internal/version 无测试文件）

# 数据库体积与增长
ls -lh data/huan-agent.db                # 191M
sqlite3 data/huan-agent.db "SELECT COUNT(*), SUM(LENGTH(input))/1048576 FROM observations;"
# 体积落在哪一类节点上（这一条是判定成因的关键）
sqlite3 data/huan-agent.db "SELECT type, COUNT(*), SUM(LENGTH(input))/1048576 FROM observations GROUP BY type;"

# 每步体积的曲线（判定「无界累积」还是「饱和 + 断崖」）
sqlite3 data/huan-agent.db "SELECT step, LENGTH(input)/1024 FROM observations
  WHERE trace_id='<id>' AND type='GENERATION' AND step IS NOT NULL ORDER BY step;"

# 裁剪代码是否存在
grep -rn "PruneTraces\|ClearTraces\|VACUUM\|retention" --include=*.go . | grep -v _test.go
grep -rn -i "vacuum\|auto_vacuum\|incremental" openspec/     # 设计里有没有这一步（零命中）

# 裁剪之后文件能不能变小（决定要不要配 VACUUM）
sqlite3 data/huan-agent.db "PRAGMA freelist_count; PRAGMA auto_vacuum;"
# → 0 / 0：现在没有可回收的空闲页，且 DELETE 不会自动还空间给文件系统

# run 的写能力与保护
grep -rn 'SetDefault("tools.read_only"\|SetDefault("tools.enable_bash"' internal/config/config.go
grep -n checkpoint cmd/huan-agent/run.go
grep -n "tracing.NewRecorder" cmd/huan-agent/*.go     # run 也在写 trace（P0-1/P0-2 依赖）

# 各路径的能力差异
sed -n '28,41p' cmd/huan-agent/approval.go

# allow 正则是不是边界（读注释，不要只看字段名）
sed -n '37,45p' internal/tool/approval_gate.go

# 各 change 完成度
for d in openspec/changes/*/; do grep -c '^\s*- \[x\]' $d/tasks.md; done
```

**未验证项**（不写成结论）：

- 配了 `tools.workspace` 后 OpenViking 实际上传全部源码的行为——我只是读出了机制
  （允许清单 + `sync_on_exit: true`），没有开一次真实同步来观察。
- `run` 配 `tools.approval.mode` 后工具被摘掉导致的失败率——机制确定（`wrap` 返回
  `nil, false` → `applyToTools` 丢弃，且 `writes+exec` 下读工具确实保留），
  但没有实跑一个 CI 任务统计失败形态。
- phase-5c 的 `PruneTraces` 若按设计实现，对 40MB 单 trace 的回收效果——未实现，无法测。
- 压缩触发点为什么落在 ~480KB/121 步（`context.max_tokens: 60000` 配 `≈chars/4` 的估算器，
  理论上该更早触发）——峰值 480KB 明显高于 60k token 的估算值，我只观察到了现象，
  没有去读压缩在循环里的确切调用时机，**不写成结论**。
- 手工 `VACUUM` 对现网 191MB 的实际回收量——没跑（会锁库），但已实测 `freelist_count = 0`，
  所以**现在跑回收量约为零**这一点是确定的；裁剪后能回收多少，要等裁剪落地才知道。

---

## 附：本次修订记录（2026-09-18 09:51 第二遍复核）

第一版（09:44）的结论方向是对的，但有**一处机制错误**和**两处排序/选型问题**，
这里逐条记下改了什么、为什么，方便下次判断这份文档还能不能信：

| 改动 | 改动前 | 改动后 | 为什么 |
|---|---|---|---|
| P0-1 成因 | 「第 n 步存 n 步历史，O(n²)，Σ(1..340)」 | 实测为**线性 + 平台期**（斜率 +5.8→+1.7 KB/step，~480KB 饱和，step 121 压缩断崖） | 压缩是**开着**的（`config.yaml:153` 的 `context.max_tokens: 60000`）。「平方增长」是 `turnbudget.go:47-49` 的告警文案，只在 `MaxTokens <= 0` 的死分支里触发 |
| P0-1 建议 | 把「加长度上限」列第一步，称其「消掉 O(n²) 主体」 | 把「`PruneTraces` + 周期裁剪」提到第一步，并明说长度上限是**常数改善、不是有界化** | 原排序会让人以为加了上限就不用裁剪了；而让表有界的只有裁剪 |
| P0-1 新增 | — | 「与 P0-2 的顺序依赖」一节 | `run.go:263` 已在写 trace，P0-2 把 `run` 接进 CI 会**放大** P0-1 |
| P0-2 建议 | 首选「`--allow-write` + `allow` 正则白名单」 | 首选**档位式声明**（`--read-only` / `--allow-write`），`allow` 正则降为便利项 | `approval_gate.go:41` 与 `config.go:574-576` 都明写这**不是边界**，不能用它承担边界职责 |
| 数字 | 178MB / 140MB / 33MB / 104KB | 191MB / 148MB / 32MB / 110KB | 实测对齐；`observations` 按 type 拆开后确认体积**全在 generation** 上。测试包数「34 包 ok」原文是对的，未改（第 35 个包无测试文件） |
| P0-1 新增 | — | 「裁剪必须配 `VACUUM`」（`freelist_count = 0` / `auto_vacuum = 0`） | 第二遍复核时我原本写了句「先手工 VACUUM 就能回收」，**实测自己把它推翻了**：没删过行就没有空闲页可回收。真正的价值在反面——`auto_vacuum=0` 时裁剪后文件也不会变小，而 phase-5c 的设计缺这一条 |

**没有改动的部分**：P0-2 的机制描述（`approval.go:28-41`、`wrap` 摘工具、三个默认值）、
P1-3 的完成度（50/58）、P1-4、P2-5、P2-6、P2-7，以及 OpenViking 那条自我修正
——第二遍逐条复核后**全部成立**，行号引用无一错位。
