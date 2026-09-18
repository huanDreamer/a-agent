# Phase 20 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

实施记录（2026-09-18）：全部完成，并用**真实 gopls**（本机 `gopls v0.23.0`）端到端验证通过。
实施中有两处与原计划的差异、以及三个只有跑真服务器才能发现的 bug，都记在第七、八节。

## 1. `internal/lsp`：分帧与 JSON-RPC

- [x] `Content-Length: N\r\n\r\n` 读帧（头部大小写不敏感、跳过未知头、拒绝超长头部块与超大帧）
- [x] 写帧按 **payload 字节数**算长度（不是 rune 数——满篇中文标识符的文件会让两者不同）
- [x] 请求/响应关联：id → 等待中的 channel；id 支持 number 与 string 两种（协议允许）
- [x] 通知分发：`textDocument/publishDiagnostics` 走回调，其余记录到 debug 日志
- [x] 服务器 → 客户端的请求统一回 `-32601`（method not found），不让服务器干等
- [x] 读循环退出时唤醒所有等待中的请求（否则调用方永久阻塞——「agent 卡住」的典型形状）
- [x] 测试：粘包/拆包（一帧分两次到）、非法 JSON 帧被跳过而不是终止会话、对端直接关闭

## 2. `internal/lsp`：位置与范围转换

- [x] `position.go`：UTF-16 code unit ↔ rune 的双向转换，1-based（工具）/ 0-based（LSP）互转
- [x] 表驱动测试：ASCII、CJK、emoji（代理对）、制表符、CRLF、空文件、超范围的行/列
- [x] **往返性质测试**：对 6 种内容（含中文注释、emoji、CRLF、无末尾换行）逐行逐列
      `FromLSP(ToLSP(x)) == x`
- [x] 越界一律报错而**不 clamp**：clamp 会把「你指的地方不存在」变成「这是另一行的答案」
- [x] 落在代理对中间的位置向下取整到该字符起点（服务器想指的就是那里）
- [x] `SnippetAt`：给工具结果带上所指那一行的文本，模型不用为判断相关性再开一次文件

## 3. `internal/lsp`：服务器进程生命周期

- [x] `ServerSpec`（name/command/args/env/languages/root_markers/init_options/enabled）
- [x] `Covers`：语言既能写成扩展名（`.go`）也能写成语言 id（`go`）——配置是人手写的
- [x] `RootFor`：最近的含 root marker 的祖先成为 root，且**不越过工作区上界**
      （越过就会给一个 agent 无法触及的目录启动服务器）
- [x] 进程启动：新会话 + 独立进程组；stderr 收进有界环形缓冲（服务器为啥死，答案在那里）
- [x] 关闭：`SIGTERM → 宽限 → SIGKILL` 打**整个进程组**（gopls 会派生 `go list`）
- [x] `sysproc.go` 作为平台缝，与 `internal/jobs/sysproc.go` 同一做法
- [x] 测试：命令不在 PATH 时的降级与安装提示、`RootFor` 的上界与回落、默认服务器表

## 4. `internal/lsp`：Manager（实例、单飞、回收、降级）

- [x] 一个（root，command）一个实例；首次使用时才启动（不预热）
- [x] 单飞：同一 key 的并发首次调用只启动一次服务器
- [x] 空闲回收（`Reap` 可被测试直接驱动）+ 定时回收循环 + 退出时统一关闭
- [x] 失败记忆：启动失败后**不重试**，只 warn 一次（每次调用都重试会变成日志洪水 + 每次失败的进程 spawn）
- [x] `ClientFor` 在「没有服务器管这个文件」时返回 `(nil, nil)`——不是错误
- [x] `AvailableCommand`：用 `exec.LookPath` 判断**是否真的装了**（见第七节的差异 2）
- [x] `FileDiagnostics`：先 `Open` 再等（不打开的话服务器会针对旧内容给出一个自信的答案）
- [x] 测试：缺失二进制只 warn 一次且不重试、无服务器不是错误、`HasEnabledServer` 的各种关闭形态

## 5. `internal/lsp`：诊断缓存与等待

- [x] 每个 URI 的最新诊断 + 「这次发布回应的是第几次本地修改」计数器
- [x] 空数组是**清空**（文件干净了），不是「没收到」
- [x] `WaitDiagnostics`：既要「服务器已经回应了我们的修改」，也要「静止窗口内没有新发布」
- [x] 两个条件缺一不可：只要前者会拿到「先发的空报告」，只要后者会在快速编辑时拿到上一版文件的诊断
- [x] 硬上限 + ctx 取消 + 连接关闭都能唤醒，超时返回 `settled=false` 而不是错误也不无限等
- [x] 干净文件的已发布结果是**非 nil 空切片**（nil 必须保留「从未发布」的含义，见第八节 bug 1）
- [x] `SortDiagnostics`：未知严重度与 error 同档排序（严重度不明的问题不该埋在警告下面）

## 6. 四个只读工具 + 编辑反馈

- [x] `diagnostics`：`path?` / `severity?`（默认 warning 档）；不带 path 时按 `Covers` 扫描工作区
- [x] `goto_definition` / `find_references`（含 `include_declaration`）/ `workspace_symbols`
- [x] 四个工具都返回 `summary` 字段（已渲染、可直接读）+ 结构化字段——与 plan 工具同一形状
- [x] 全部 `CapRead`，只读工作区下照常可用
- [x] 位置在工具层先用 `checkPosition` 校验：错误信息是工具自己的（「a.go: 行 99 超出范围（共 6 行）」）
- [x] 工作区之外的结果被丢弃（符号搜索会带出依赖里的结果，而 agent 打不开那个路径）
- [x] `lsp.Feedback`：工具装饰器（与 `CapabilityFunc` 同一模式），挂在 `write_file` / `edit_file` 上
- [x] 渲染：`诊断（该文件，2 条 error/warning）：` + `path:line:col  severity  message  (source)`
- [x] 无诊断 → 「诊断（该文件）：无」（留白会被读成「没检查」）
- [x] 未就绪 → 「尚未就绪（语言服务器未在 2s 内返回）」，编辑照常成功
- [x] 编辑失败 → 结果原样返回，不附诊断；服务器报错 → 记 warn、结果不变
- [x] 无服务器 → 结果**逐字节不变**（这类部署不该为这个特性付任何开销）
- [x] 测试（`internal/tool/builtin/codeintel_test.go` 16 个 + `internal/lsp/manager_test.go` 8 个）：
      严重度档位全表、工作区扫描遵守 `Covers` 并如实报告检查了几个文件、
      引用上限不静默截断、位置越界被拒、路径越界被拒、服务器错误透传、
      feedback 的五种分支（正常/干净/未就绪/无服务器/带错误）

## 7. 与原计划的两处差异

**差异 1：编辑反馈实现为工具装饰器，而不是改 `files.go`。**
原计划说「写/编辑成功后调 Augmenter」，实现时改成在注册处套一个装饰器
（`lsp.NewFeedback`，`cmd/huan-agent/workspace_tools.go`）。三个好处：`internal/tool/builtin`
完全不认识语言服务器；`write_file` / `edit_file`（以及将来的多文件编辑器）自动共享同一行为，
不会各自漂移；一处接线覆盖所有 surface。

**差异 2：注册门槛从「有配置」改成「真的装了」。**
原计划把「没有可用语言服务器时不注册」理解为看配置。实现时发现这不够：本机 `gopls` 装了、
但完全可以不装，而「配置了但没装」会让四个工具出现在菜单上、每次调用都失败。现在用
`exec.LookPath` 判断（不启动进程），日志给出安装命令。这正是规格里那句
「一个必然失败的工具比没有这个工具更糟」的实际含义。

## 8. 只有跑真服务器才发现的问题（本节是集成测试存在的理由）

**bug 1：空诊断报告与「没有服务器」不可区分（真实缺陷，已修）。**
`WaitDiagnostics` 返回 `(nil, true)` 同时表示「服务器说这个文件干净」和（经 `FileDiagnostics`
透传后）「没有服务器管这个文件」——因为 Go 里 `append([]T(nil))` 得到 nil。后果是
**修好之后的文件不会显示「无诊断」**，而是完全没有任何反馈，看起来像这个特性没生效。
单元测试抓不到：进程内假服务器是按同一个含糊形状写的，我自己也照着写了一遍。
修法是把两态布尔换成显式的三态 `lsp.Diagnosed`（`NoServer` / `NotReady` / `Ready`），
并让「干净的已发布结果」是非 nil 空切片。

**bug 2：`textDocumentSync` 是数字，不是布尔也不是对象（已修）。**
gopls 发 `"textDocumentSync": 1`。原来的解码只处理 `true/false` 与对象形式，数字形式让
`SyncKind` 保持 0，等于悄悄关掉全文同步。

**bug 3：位置类请求没有先 `didOpen`（已修）。**
`Definition` / `References` 原本只读文件并换算位置，没有把内容告诉服务器。服务器会针对
它从未见过的文档给出一个自信的答案——这类错误不报错，只是答案错了。

## 9. 配置与文档

- [x] `LSPConfig` / `LSPServerConfig` + `DiagnosticsWait()` / `IdleTimeout()` / `MaxDiagnosticsOr()`
- [x] `SetDefaults`：enable=true、attach_diagnostics=true、wait=2000ms、idle=600s、max=20
- [x] `configs/config.example.yaml`：`tools.lsp` 段（含 gopls 与 typescript 两份注释示例、
      「没装就不注册」的说明与安装命令、root_markers 为什么决定「找不到引用」是不是错答案）
- [x] `docs/tools.md`：工具表补四行 + 新增「Code intelligence」一节
      （装什么、花多少、三个开关、诚实边界：位置口径 / 无诊断只在服务器说过时才算 /
      工作区外的结果丢弃 / 陈旧位置丢弃而非 clamp）
- [x] `docs/tools.md` 排障段补五条（工具为什么不在、为何「尚未就绪」、引用为空、
      定义行号不对）
- [x] `docs/roadmap-coding-agent.md` 记录 `CAPABILITY-GAPS.md` 的那处更正：本机 gopls
      当时并未安装，成本含安装与外部进程管理

## 10. 验证

- [x] `go test ./... -count=1` 全绿（含新增 30 个测试）；`go vet ./...` 干净
- [x] `make build` 通过
- [x] 集成测试（`-tags integration`，真实 gopls v0.23.0）**5 个全过**：
  - 诊断报出真实类型错误（`cannot use helper() (value of type string) as int value`）
  - 修好之后诊断被清空
  - `Definition` 给出正确的 1-based 行（gopls 的 0-based 7 → 我们的 8）
  - `References` 含定义与另一个文件里的调用点
  - `WorkspaceSymbols("Answer")` 找到符号且带 kind
  - `Close()` 之后进程组确实消失（轮询确认，不是「忘了它」）
- [x] 编辑反馈集成测试（`cmd/huan-agent`，真实 gopls + 真实 `edit_file`）**3 个全过**：
  - 一次 edit 引入类型错误 → 结果里出现 `lib.go:5:9  error  cannot use value()…`
  - 修好 → 结果里出现「诊断（该文件）：无」
  - `tools.lsp.enable=false` → 结果里没有诊断段，四个工具也没注册
- [x] 人工实跑：`chat --tools` 的 `/tools` 列出四个工具，日志 `code intelligence enabled
      {"tools": 4, "attach_diagnostics": true}`
- [x] 人工实跑：`huan-agent run --workspace /tmp/… "修好 lib.go 里的编译错误"` 真的修好了
      （退出码 0，`go build` 通过）
- [~] `golangci-lint run`：**工具已装好并跑过**（`go install` 需要 workspace 之外的写权限，已获准）。
      本阶段新增/改动的代码是**干净**的；仓库里还剩 20 条**既有**告警，在本次工作**没有创建**的文件里
      （用 `git status` 逐个核对过：`memory.go`、`agent/audit.go`、`auth.go`、`skillapi.go`、`skills.go`、
      `mcp/manager_test.go`、`ask_test.go`、`usage.go`、`llm/retry_test.go` 等）。另外 `internal/lsp/sysproc.go`
      的 `processAlive` 是**误报**：它被 `//go:build integration` 的测试用着，而 linter 默认不编译带 tag 的文件
      —— 就是那个在 Phase 24 藏了一个编译不过的集成测试文件的同一个盲点。
      本阶段引入的 5 处死代码已在最后一轮清掉（`registerStaticTools`、`Server.checkpoints`、
      `workspaceToolSet.checkpoints`、`approvalGate.timeout`、`errNoApprover`）与 1 处 errcheck
      （`run.go` 的用量记录）。
      `gofmt -l` 对本次改动为空。
