# Phase 10 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

## 1. 进程管理 `internal/jobs`（新包）

- [x] `Manager` / `Options` / `Spec` / `Job` / `Status` / `Read` / `ReadOptions`
- [x] `Start`：`setsid` 新会话 + 新进程组；stdout/stderr 合流一个管道；不用 `CommandContext`
- [x] 启动宽限（默认 500ms）：进程在这段时间内就退了，直接带退出码与输出返回
- [x] 输出捕获：有界环形窗口（默认 256 KiB）+ 封顶日志文件（默认 8 MiB，到顶写说明后停止）
- [x] 状态以**进程组**为准：shell 退出但组里还有成员时仍是 `running`（`kill(-pgid, 0)` 探活）
- [x] 逃逸进程兜底：组空但管道仍被占用时，宽限后仍判为结束并标记 `output_open`
- [x] `Output(from, max_bytes, wait)`：绝对字节偏移 + `next` + `discarded_bytes` / `skipped_bytes`，`wait` 到新输出/结束/超时为止
- [x] `Stop`：SIGTERM → 宽限 → SIGKILL（`kill` 直接 SIGKILL）；已结束的作业返回 `ErrNotRunning`
- [x] `Forget`：只允许已结束的作业，删记录 + 删日志
- [x] `Close`：并行 SIGTERM → SIGKILL，强制收敛记录，可重复调用
- [x] 上限：`max_jobs`（运行中）、`max_finished`（保留记录，不删日志）
- [x] 不重复的随机 id（`job-xxxxxx`）：会话比进程活得久，顺序 id 会指到另一个进程
- [x] 测试：立即失败上报、调用结束仍在跑、fork 子进程仍在组里、SIGTERM→SIGKILL、已结束冲突、未知 id、拒绝非法 spec、上限、关闭后拒绝启动、Close 收敛、Forget 语义、日志封顶、偏移与丢弃上报、wait 生效与超时、记录淘汰、id 不复用、`output_open` → 覆盖率 84%
- [x] `-race` 通过

## 2. 环形窗口 `internal/jobs/window.go`

- [x] 保留尾部、统计总字节与已丢弃字节、偏移切片（跨环连续）
- [x] 测试：绕环、超大块、空写、越界与负 max、容量默认、持续写入下有界

## 3. 四个工具 `internal/tool/builtin/background.go`

- [x] `bash_background` / `bash_jobs` / `bash_output` / `bash_stop`，能力都是 `exec`
- [x] 工具描述如实说明：无 PTY / stdin 为 `/dev/null`、stdout 与 stderr 合并、无资源限制、无沙箱、生命周期止于 agent
- [x] 描述里跟上"那该怎么办"：写明启动时就要带非交互参数（`-y` / `--yes` / `--no-input` / `CI=1`），而不是等运行起来再回答提示；测试固定住这几句
- [x] 与 `bash` 共用前置校验（只读工作区、`deny_patterns`、`cwd` 解析、shell 选择），拒绝时什么都没跑
- [x] 工作区归属：`bash_jobs` 只列本工作区，跨工作区的 `bash_output` / `bash_stop` 明确拒绝；别的工作区有多少在跑会说明
- [x] `bash_stop` 对已结束的作业返回结果而不是错误，并在 `forget` 时拒绝删除仍在运行的记录
- [x] 测试：构造拒绝（nil 工作区 / nil manager / 坏正则）、名字集合、调用后仍在干活（心跳文件）、立即失败、五类拒绝零副作用、跨工作区不可管理、forget 与二次 stop、running_only、偏移续读、未知 id、上限上报
- [x] `bash.go` 抽出 `bashGuard` / `compileDenyPatterns` 共用，原有错误文案不变

## 4. 配置 `internal/config`

- [x] `enable_background`（默认 true）、`background_dir`、`background_max_jobs`、`background_log_max_mb`、`background_window_kb`、`background_stop_grace_seconds`
- [x] `JobsDirOrDefault`（默认与数据库同级的 `jobs/`）、`BackgroundStopGrace`、`dirBesideDatabase` 与工作区共用
- [x] 测试：默认值、YAML 解析、目录解析、优雅停止时长
- [x] `configs/config.example.yaml` 同步

## 5. 接线 `cmd/huan-agent`

- [x] `newJobManager`：每个进程一个，创建失败只告警（后台工具因此被摘除）
- [x] `admin` / `chat` / `serve` 三个入口各自创建，退出时 `Close()`
- [x] `workspaceToolSet` 持有 manager 与 surface，按策略**摘除**而不是注册后拒绝
- [x] `workspaceBoundToolNames` 纳入四个名字（每轮克隆按工作区重新绑定）
- [x] 测试：按策略摘除/注册、注册名都在 workspace-bound 列表里、作业记录带工作区名字、root 与 surface

## 6. 控制台 `internal/server` + `web/`

- [x] `GET /api/jobs`、`GET /api/jobs/:id`、`POST /api/jobs/:id/stop`、`DELETE /api/jobs/:id`
- [x] 未接线时 `enabled: false` + 说明，而不是 404（与 `/traces` 一致）
- [x] 错误映射：未知 404、运行中删记录 409、已结束再停止 409
- [x] 测试：列表 / 偏移续读 / 停止 / 删记录 / 404 / 409 / 未接线
- [x] 设置 → 后台进程面板（`JobsPanel.vue`）：状态、命令、工作区、pid、运行时长、日志大小；查看日志（按 `from=next` 跟读）、停止（两步确认）、删除记录（仅已结束）
- [x] `state.js` 增加设置子页、`api.js` 增加四个调用、`SettingsView.vue` 注册面板
- [x] `npm run check:ui` 通过；`npm run build` 重新生成内嵌包（`internal/server/webui/dist`）
- [x] 真实控制台验证：`admin serve` 起服务，`/api/jobs` 返回 `enabled: true` 与作业目录；内嵌 JS 含「后台进程」面板

## 7. 文档与规范

- [x] `docs/tools.md`：后台进程一节（能做什么 / 不做什么）+ 四个工具进表 + 配置块
- [x] `docs/admin.md`：接口表 + 设置 → 后台进程一节
- [x] `docs/status.html`：Phase 10 卡片与能力卡
- [x] `openspec/changes/phase-10-background-jobs/`：proposal + capability spec + 本清单

## 8. 端到端验证

- [x] `go test ./...` 全绿（`internal/mcp` 的 `TestManager_ToolNameCollisionIsReported` 在负载下偶发顺序抖动，单独重跑 3 次稳定通过，且与本次改动无关）
- [x] 真实二进制：`chat --tools` 列出 `bash_background` / `bash_jobs` / `bash_output` / `bash_stop`，并打印 `background jobs ready {dir, max_jobs, log_cap_bytes}`
- [x] 真实二进制 + 脚本化 provider：模型依次调用 `bash_background`（起 `python3 -u -m http.server`）→ `bash_jobs` → `bash_output` → `bash_stop`；会话期间外部 TCP 探活 `OPEN`、`GET /` 返回 200；`bash_output` 读到服务自己的启动横幅与请求日志；停止后端口关闭
- [x] agent 进程退出后端口关闭、无残留进程（生命周期跟随 agent）；控制台进程同样：退出日志 `stopping background jobs {"jobs": 1}`，端口随即关闭
- [x] 控制台真实数据：网页对话里让模型起 `http.server` → `/api/jobs` 显示 `workspace: project` / `requested_by: web` / running；读日志窗口、运行中删记录 409、停止 200、删记录 200、日志文件消失、端口关闭
- [x] 本轮验证抓到并修掉一个接线缺陷：`Options.StartGrace` 留空时被当成 0，启动宽限实际不生效（该缺陷已补回归测试）

## 后续（本变更不做）

- [ ] 跨 agent 重启存活（交给 launchd / systemd / 容器）
- [ ] PTY 与交互式输入
- [ ] 每个作业的资源配额（CPU / 内存 / 磁盘）
- [ ] 控制台里手动起作业（会引入一条没有模型参与的 exec 通道）
