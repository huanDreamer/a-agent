# Phase 19 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

实施记录（2026-09-18）：全部完成，并已用真模型端到端跑通（text / json / stdin 三种输入）。
有两处与原计划不同，都记在下面。

## 1. `run` 子命令骨架 `cmd/huan-agent/run.go`

- [x] `runCmd`（`Use: "run <prompt|->"`），注册进 `rootCmd`
- [x] 退出码常量集中定义（0/1/2/3/4/5/6）并注释
- [x] `exitError` + `withExitCode` + `exitCodeFor`，`main()` 用 `exitCodeFor(err)` 退出；
      其它子命令的行为不变（无类型的错误仍然是 1）
- [x] 三种输入形式：`run "<prompt>"`、`run - "<instruction>"`（stdin 作为材料追加）、`run -`（stdin 即任务）
- [x] prompt 为空 / 参数非法 / 两个参数时第一个不是 `-` → 用法错误（退出码 2）
- [x] SIGINT / SIGTERM 取消本轮（`signal.NotifyContext`）→ 退出码 1
- [x] flag：`--provider` `--model` `--system` `--skill` `--workspace` `--output` `--timeout`
      `--max-steps` `--session` `--no-tools` `--allow-background` `--fail-on-tool-error` `--quiet`

**与原计划的差异 1（输入形式）**：原计划写的是 `huan-agent run - "审一遍这段 diff"` 这类用法，
但最初实现按"最多一个位置参数"写，于是它其实是**不可用的**——`-` 与指令是两个参数。现改为
`MaximumNArgs(2)`：第一个必须是 `-` 时才读 stdin，并把它围栏 + 标注后追加在指令后面。

`-` 是显式的，**不从"stdin 不是终端"推断**：git hook 的 stdin 通常不是空的（`pre-push` 会收到
待推送的 refs），嗅探管道会把一串 refs 悄悄拼到指令后面。这条写在了函数的注释里。

## 2. stdout / stderr 分工（本阶段最容易回归的契约）

- [x] 新增 `obs.NewLoggerTo(w, level, format)`，`NewLogger` 委托给它并保持签名不变
- [x] `run` 用 `obs.NewLoggerTo(os.Stderr, ...)`：日志不再可能落进答案
- [x] stdout 只由 `runWriter` 写（答案增量、最终答案、JSON 对象各一处）
- [x] `--output text` 逐块 flush（`run | tee` 实时可见）
- [x] `--quiet` 去掉进度与汇总行，**错误仍然打印**（`notice` 不受 `--quiet` 影响）
- [x] 测试 `TestRunWriterKeepsStdoutClean`：断言 stdout 恰好是 `答案 + \n`，且不含工具名、
      日志级别、汇总字样；同时断言 stderr 非空且含工具名
- [x] 测试 `TestRunWriterReasoningNeverReachesStdout`：思考内容不得进 stdout
- [x] 实测：`./bin/huan-agent run "..." --quiet > out.txt` 的 stdout 只有一句答案

## 3. `--output json` 契约

- [x] `runResult` 结构体：`answer` `provider` `model` `session_id` `stop_reason` `tool_failures`
      `usage` `steps` `failures` `error`
- [x] `stop_reason` ∈ `done` / `steps` / `tokens` / `deadline`
- [x] stdout 恰好一个 JSON 对象（`json.Encoder` + `SetEscapeHTML(false)`）
- [x] 用量进 `usage`；费用走 `pricingTable(cfg)` 的 `CostOf`，**未匹配价格表时报 `priced:false`
      而不是 0**（报 0 会被读成免费）
- [x] 失败运行同样产出合法对象（`error` 字段），`steps`/`failures` 永远是数组
- [x] 测试：往返解析、字段值、`steps`/`failures` 形状、失败运行可解析、`stop_reason` 保留

## 4. 一次性 surface 的能力裁剪

- [x] 新增 `prompt.SurfaceRun` + `internal/prompt/surface_run.md`
      （不能提问；歧义选最保守方案并在答案里说明假设；不等人工确认；答案自成一体；先验证再收尾）
- [x] `toolSetOptions.AllowBackground`；`workspaceToolSet` 在 `surface == run` 且未开启时
      **不注册**后台进程工具
- [x] `--allow-background` 才注册；无论是否开启，退出前 `defer jobMgr.Close()` 停掉本次作业
- [x] `ask_user` / `plan_*` 由既有的 `Surface == "web"` 条件自然排除（无需额外改动）
- [x] 测试 `TestRunSurfaceWithholdsInteractiveTools`：同一个 config 下 run 面没有
      `ask_user`/`plan_*`，web 面有（反面断言防止测试空转），而
      `read_file`/`grep`/`write_file`/`bash` 都在
- [x] 测试 `TestRunSurfaceWithholdsBackgroundTools`：默认没有 `bash_background`，开 flag 后有
- [x] 测试 `TestPromptSurfaceRunExists`：run 面确实有自己的提示词段落

## 5. 预算、时长与 `--session`

- [x] `--max-steps` 缺省继承 `chat.max_steps`（经 `RunConfig.MaxStepsOr`）
- [x] `--timeout` 缺省取 `run.timeout_seconds`；超时 → 退出码 3
- [x] 预算耗尽（`res.BudgetExhausted()`）→ 退出码 4，并在 stderr 说明是哪个预算
- [x] 工具调用失败 + `--fail-on-tool-error` → 退出码 5
- [x] `--session` 复用既有会话（不存在则退出码 6，**不新建**：打错字不该看起来像成功了）
- [x] 缺省新建会话并写进 stderr 日志与 JSON
- [x] 会话落库 + 工作区绑定，所以这次运行在控制台能看到
- [x] 测试 `TestResolveRunSession`、`TestRunConfigDefaults`

## 6. 配置

- [x] `RunConfig{ TimeoutSeconds, MaxSteps, DefaultOutput }` + `Timeout()` / `MaxStepsOr()` / `OutputOr()`
- [x] `SetDefaults`：`run.timeout_seconds=0`、`run.max_steps=0`、`run.default_output="text"`
- [x] `configs/config.example.yaml` 的 `run` 段 + 注释（含退出码指向 `docs/run.md`、
      以及裁剪了哪两个工具）
- [x] 测试：默认值、"0 = 继承 chat.max_steps"、大小写不敏感的 `json`、示例配置能加载

## 7. 文档

- [x] 新 `docs/run.md`：stdout/stderr 契约、三种输入形式、退出码表、flag 表、工具裁剪、
      `--output json` 示例与字段语义、问题排查
- [x] `docs/run.md` 三个可复制示例：pre-commit hook、GitHub Actions 失败定位、cron 日报
- [x] `docs/run.md` 记录 stdout 写入是对"禁止 `fmt.Println`"的**有意豁免**及其边界
      （只有 `runWriter` 写 stdout，日志仍走 Zap→stderr）
- [x] `README.md` 增加"非交互式运行"一节并指向 `docs/run.md`
- [x] `docs/run.md` 写明与审批的关系：`tools.approval.mode != off` 时 `run` 拿不到写/执行工具

## 8. 验证

- [x] `go test ./... -count=1` 全绿；`go vet ./...` 干净
- [x] `make build` 通过
- [x] 真模型端到端（text）：`run "读一下 README.md…" --quiet` → 退出码 0，stdout 只有一句答案
- [x] 真模型端到端（json）：`--output json --quiet` → 单个可 `json.load` 的对象，
      含 `steps`（bash/list_dir/read_file）、`usage`、`stop_reason=done`
- [x] 真模型端到端（stdin + 指令）：`printf 'FROM alpine…' | run - "指出一处问题"` → 退出码 0，
      答案正确利用了 stdin 的内容（识别出 alpine 没有 apt-get）
- [x] 退出码实测：无 prompt=2、空 stdin=2、非法 `--output`=2、不存在的 `--session`=6、
      不存在的 provider=6
- [~] `golangci-lint run`：**工具已装好并跑过**（`go install` 需要 workspace 之外的写权限，已获准）。
      本阶段新增/改动的代码是**干净**的；仓库里还剩 20 条**既有**告警，在本次工作**没有创建**的文件里
      （用 `git status` 逐个核对过：`memory.go`、`agent/audit.go`、`auth.go`、`skillapi.go`、`skills.go`、
      `mcp/manager_test.go`、`ask_test.go`、`usage.go`、`llm/retry_test.go` 等）。另外 `internal/lsp/sysproc.go`
      的 `processAlive` 是**误报**：它被 `//go:build integration` 的测试用着，而 linter 默认不编译带 tag 的文件
      —— 就是那个在 Phase 24 藏了一个编译不过的集成测试文件的同一个盲点。
      本阶段引入的 5 处死代码已在最后一轮清掉（`registerStaticTools`、`Server.checkpoints`、
      `workspaceToolSet.checkpoints`、`approvalGate.timeout`、`errNoApprover`）与 1 处 errcheck
      （`run.go` 的用量记录）。
      `gofmt -l` 对本阶段改动的文件为空。

## 9. 实施中的一处设计决定（与原计划的差异 2）

原计划说 `run` 复用 `chat` 的 agent 构建路径。实现时改用 **`internal/chat.Runner`**（Web 循环）
而不是 `internal/agent` 的 Eino ReAct，理由是 Phase 19 的规格要求 `--output json` 给出
`steps` / `usage` / `tool_failures`，而 `agent.Agent.Generate` 只返回最后一条消息。Runner 的
`Result` 恰好有 `Plan`（每步的工具调用）、`Usage`、`StopReason`，并且顺带带来了既有的
步骤级重试、上下文压缩与预算控制。

复用的仍然是同一套「装配」：provider/model 解析、`registerBuiltinTools`、MCP 连接
（`connectConfiguredMCP`）、skill 与 allow-list、`tracing.NewRecorder`、usage recorder。
Eino 那条路以后仍可作为一个 `Runner` 实现接进来。
