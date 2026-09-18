# Phase 18 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

实施记录（2026-09-17）：全部完成。实施中在同一个位置发现**第二个同形 bug**（第七节），
已一并修复——只修传输字段的话，`chat --tools` 仍然起不来。

## 1. 唯一的配置→运行态转换 `cmd/huan-agent/mcp.go`

- [x] 转换函数落在 `cmd/huan-agent/mcp.go`
      （**与原计划的差异**：实现为 `mcpSpecFromConfig(config.MCPServer) (mcp.ServerSpec, error)`，
      不含 `IsEnabled` 返回值——启停判断移进了共享的连接循环 `connectConfiguredMCP`，因为
      "是否连接"是循环的职责，转换函数保持纯粹）
- [x] 用 `mcp.ParseTransport` 解析并**保留解析结果**（不再让空 transport 漏到运行态）
- [x] 逐字段搬运：`ID`（`mcp.ServerID(name)`，与 `internal/server.SyncConfigServers:543` 同源）、
      `Name`、`Transport`、`Command`、`Args`、`Env`、`URL`、`Headers`
- [x] 非法 transport 的报错指明服务器名与取值（`mcp server "weird": mcp: unsupported transport
      "websocket" (want stdio, sse or http)`）
- [x] `headers` 顺序与内容原样保留（鉴权头顺序是契约）
- [x] 注释写清根因（三处构造、两处漏字段、控制台为什么没这个问题），并写明"不要再内联"

## 2. 两个调用点收敛

- [x] `cmd/huan-agent/chat.go` 的循环改为调用共享的 `connectConfiguredMCP`，并补上
      `IsEnabled()` 跳过（原本是漏的）
- [x] `cmd/huan-agent/workspace_tools.go` 的 `buildFeishuTooling` 同样收敛到 `connectConfiguredMCP`
- [x] 连接/注册失败的错误消息带上传输方式：`connect mcp %s (%s): %w`
- [x] `mcp.ServerSpec{` 在 `cmd/` 只剩 `mcp.go` 一处——**用测试固化**而非人工 grep：
      `TestOneMCPSpecBuilderInCmd` 直接扫 `cmd/**/*.go`，出现手写构造即失败
- [x] 顺带收敛重复的日志：连接成功一条 info（含 `surface` / `transport` / `tools` / `skipped`）

## 3. 测试

- [x] `cmd/huan-agent/mcp_test.go`：http / sse / stdio 三态经转换后逐字段断言保真
- [x] 断言每种输入的 spec 都能通过 `Validate()`（an 空 `Transport` 会回落 stdio 并在 `Validate`
      报错，这正是 bug 的形状）
- [x] 断言 `enabled: false` 被跳过（`TestConnectConfiguredMCP_SkipsDisabled`）
- [x] 断言非法 transport 返回错误而不是回落 stdio
- [x] 断言 `args` / `env` / `headers` 的数量与顺序
- [x] 构建级测试：加载 `configs/config.example.yaml`（并按其文档打开 OpenViking），断言其中每个
      服务器条目都能转换出可拨号的 spec（`TestShippedExampleConfig_ServersAreDialable`）
- [x] 真实连接的端到端测试：`httptest` + `mcpserver.NewStreamableHTTPServer` 起一个真的 http
      MCP 服务器，断言 `transport: http` 的配置条目**真的连上并注册了工具**
      （`TestConnectConfiguredMCP_HTTPEntryReachesTheServer`）
- [x] 飞书路径：`TestFeishuToolingConnectsHTTPEntry` 用同一个 http 服务器断言
      `buildFeishuTooling` 成功构建并注册工具（这条路径原本和 CLI 一样坏）
- [x] 回滚测试：转换失败时不返回任何 client（`TestConnectConfiguredMCP_RollsBackOnFailure`），
      以及 nil config / nil registry 不是错误

## 4. 技能清单修正

- [x] `configs/skills/code-review.md`：`tools: [read_file, grep, glob, list_dir, calc]`
- [x] `code-review.md` 正文"可用工具"一段按新清单改写，删掉 `echo`，并补一句"你没有写权限"
- [x] `configs/skills/daily-summary.md`：`tools: [time, read_file, grep]`
- [x] `daily-summary.md` 正文"工具使用建议"一段按新清单改写
- [x] 测试：`cmd/huan-agent/skills_test.go` 遍历 `configs/skills/*.md`，对每个技能断言
      `SetAllowList` 成功、结果不为空、且每个声明的名字都还在
- [x] 测试：`TestCodeReviewSkillCanReadFiles` 断言 `read_file` / `grep` 可用，且
      `write_file` / `edit_file` / `bash` **不可**用（评审技能不该能改它评审的东西）

## 5. 文档

- [x] `docs/tools.md` 排障段新增"技能的 `tools` 是 allow-list 而不是提示"一条，写明
      `SetAllowList` 只拒绝**未知**名字，所以"合法但无用"的清单会静默通过，并给出
      `--skill <name>` + `tools available:` 的查看方法
- [x] `docs/tools.md` 排障段新增"MCP 工具没出现"（撞名被跳过、builtin 优先）与
      "配置是 http 却报 stdio"（转换丢字段的定位方法）两条
- [x] 复核 `docs/tools.md` 的 "Ask for approval"（第 230 行）与 "SPEED BUMP"（第 54 行）引用
      仍然准确——两者都还对；前者由 Phase 21 改写

## 6. 验证

- [x] `go test ./... -count=1` 全绿（含新增测试）
- [x] `make build` 通过
- [x] `./bin/huan-agent chat --tools` 在 `configs/config.yaml` 下**进入 REPL**（人工实跑）
- [x] `./bin/huan-agent chat --tools --skill code-review` 的允许清单为
      `calc, glob, grep, list_dir, read_file`（人工实跑）
- [x] `--skill daily-summary` 的允许清单为 `grep, read_file, time`（人工实跑）
- [x] `go vet ./...` 通过
- [~] `golangci-lint run`：**工具已装好并跑过**（`go install` 需要 workspace 之外的写权限，已获准）。
      本阶段新增/改动的代码是**干净**的；仓库里还剩 20 条**既有**告警，在本次工作**没有创建**的文件里
      （用 `git status` 逐个核对过：`memory.go`、`agent/audit.go`、`auth.go`、`skillapi.go`、`skills.go`、
      `mcp/manager_test.go`、`ask_test.go`、`usage.go`、`llm/retry_test.go` 等）。另外 `internal/lsp/sysproc.go`
      的 `processAlive` 是**误报**：它被 `//go:build integration` 的测试用着，而 linter 默认不编译带 tag 的文件
      —— 就是那个在 Phase 24 藏了一个编译不过的集成测试文件的同一个盲点。
      本阶段引入的 5 处死代码已在最后一轮清掉（`registerStaticTools`、`Server.checkpoints`、
      `workspaceToolSet.checkpoints`、`approvalGate.timeout`、`errNoApprover`）与 1 处 errcheck
      （`run.go` 的用量记录）。
      替代：`go vet ./...` 干净 + `gofmt -l` 对本次改动的文件为空。装好后需补跑。

## 7. 实施中发现并修复的第二个 bug（传输修复之后才暴露出来）

修好传输字段后 `chat --tools` 前进了一步，随即失败在：

```
Error: build agent: register mcp tools openviking (http): mcp: register openviking/grep: tool: "grep" already registered
```

OpenViking 的 MCP 服务器自己也暴露了 `grep` 与 `glob`。而 `internal/mcp/bridge.go` 的
`RegisterMCPTools` 在**任何**注册错误上都会回滚并返回错误，让整个 agent 构建失败——直接违反
`mcp-runtime` 规格第 11 条（"撞名 MUST 跳过该工具、保留其余工具、MUST NOT 让整台服务器不可用"）。

这是**同一个 bug 形状的第二次出现**：两份实现，一份对（`internal/mcp/manager.go` 的
`registerTools` 一直正确地跳过并上报），一份错（CLI / 飞书走的那份）。控制台从来不受影响，
所以这个 bug 与传输那个一样，只在 CLI 和飞书侧可见。

- [x] 抽出唯一的实现 `registerToolSpecs`（`internal/mcp/bridge.go`），管理器与一次性桥接共用
- [x] `RegisterMCPTools` 改为返回 `(names, skipped []string, error)`：撞名是跳过，只有"列不出工具"
      （未连接 / 不可达）才是硬错误
- [x] `Manager.registerTools` 改为调用同一个实现（顺带修掉它原本按 `specs[:registered]` 回滚的
      索引错位隐患）
- [x] 测试：`TestRegisterMCPTools_SkipsCollidingNames`（包内）与
      `TestConnectConfiguredMCP_NameCollisionIsNotFatal`（端到端）都断言"撞名的被跳过、其余注册、
      已存在的工具不被注销"
- [x] 人工实跑确认：`mcp server connected {"tools": 13, "skipped": 2}` 且 REPL 正常进入
- [x] 更新 `internal/mcp/manager.go` 的注释，指向共享实现，避免第四个副本出现
