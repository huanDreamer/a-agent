# Phase 10 — 后台进程：让常驻命令活过一次工具调用

## Summary

今天 `bash` **有意**在每次调用结束时杀掉整个进程组：`internal/tool/builtin/bash.go`
的 `bashReapGroup` 在 `Wait` 返回后无条件再杀一次，超时看门狗也杀一次，工具描述里甚至直接
写着「不要把后台进程的存活当成可以依赖的事」。这条规则挡住了进程泄漏，也挡住了
**dev server、`--watch` 编译、需要常驻的 API 与数据库** —— 它们全都活不过启动它们的那一次
调用。

本次变更把「常驻」变成一件**显式的事**：

1. `bash` 语义**完全不动**（同步执行、超时、结束即清理进程组）。想常驻就得明说。
2. 新增四个工具：`bash_background`（启动）、`bash_jobs`（列表）、`bash_output`（看输出）、
   `bash_stop`（停止 / 删记录）。
3. 常驻进程的生命周期**跟随 agent 进程**：agent 退出时统一 SIGTERM → SIGKILL，
   不留下一个没人管的孤儿进程。
4. 控制台新增 设置 → **后台进程**：`/api/jobs` 一组接口 + 一个面板，能看状态、跟日志、停止。

## Why

- **这是 coding agent 的基本能力缺口。** 让 agent 干活，最常见的动作之一就是「起一个
  dev server 然后看它报什么错」。现在的行为是：起得来，日志拿不到（进程被杀了），
  模型只能一遍遍重跑 `npm run dev`，每次都被 120s 超时打断。
- **`bash` 的清理语义不能放宽。** 它现在的保证（一次调用不留残留）是审计和排障的基础，
  把它改成「有时会留进程」才是真正的坑。缺口要补在**另一个工具**上，而不是把 `bash` 变成
  两套语义。
- **常驻进程必须有人管。** 放任 `nohup ... &` 的结果是：进程在跑，日志在丢，模型不知道
  它在哪、控制台看不见它、想停只能靠 `pkill` 猜。因此本次变更的重点不是「能起」，
  而是「能起、能看、能停、退出时能收」。
- **进程组是唯一的停止单位。** `npm run dev` 会自己再 fork；只杀 shell 会留下子进程
  （`bash.go` 早就用 `kill(-pid)` 解决了这件事），常驻进程沿用同一套。

## What Changes

### ADDED

1. **`internal/jobs`**：常驻进程管理器（每个 agent 进程一个实例）
   - `Spec{Command, Cwd, Name, Workspace, WorkspaceRoot, RequestedBy, Env, Shell, ShellArgs}`。
   - `Start`：`setsid` 起一个新会话（脱离 agent 自己的进程组，agent 收到的信号不会误杀它），
     stdout/stderr 合流进一个管道，由父进程逐块写进**受限环形窗口**（默认 256 KiB）和
     **持久日志文件**（默认上限 8 MiB，超过后停止写入并留一行说明）。
   - 状态判定以**进程组**为准：shell 退出但组里还有活着的子孙进程（`kill(-pgid, 0)` 探活），
     作业仍算 `running` —— 用户问的是「我的服务还在吗」，不是「那个 sh 还在吗」。
   - `Output(id, from, max_bytes, wait)`：按**绝对字节偏移**读取，返回 `next` 供续读，
     `dropped_bytes` 说明有多少字节已经被窗口丢掉（不假装没发生）。`wait` 让「启动后等它
     打印 ready」变成一次调用而不是轮询。
   - `List` / `Get` / `Stop(signal, grace)` / `Forget`（删记录 + 删日志，只允许对已结束的作业）/
     `Close`（退出时 SIGTERM → 宽限 → SIGKILL）。
   - 上限：同时运行 `max_jobs`（默认 8），保留的已结束记录 `max_finished`（默认 20，淘汰记录
     但**不删日志文件**：删除是操作者的事，控制台也有显式入口）。
2. **四个工具**（`internal/tool/builtin/background.go`，能力都是 `exec`）
   - `bash_background`：启动，返回 `id / pid / status / log_path`；启动后有一小段
     **启动宽限**（默认 500ms），进程在这段时间内就自己退了（端口占用、命令不存在）时
     直接把退出码和日志尾部一起返回 —— 这类失败是常态，让模型一次调用就知道原因。
   - `bash_jobs`：列出本工作区的作业（状态、pid、命令、运行时长、日志大小、退出码）。
   - `bash_output`：读输出（`from` / `max_bytes` / `wait_ms`）。
   - `bash_stop`：`signal`（term/kill）+ `forget`。
   - 四个工具都走 `bash` 同一套前置校验：只读工作区拒绝、`tools.enable_bash: false` 时**不注册**、
     `deny_patterns` 先拒绝再执行、`cwd` 必须落在工作区内。
3. **`/api/jobs`**（管理台已认证）
   - `GET /api/jobs`：`enabled / dir / max_jobs / running / total / jobs[]`。
   - `GET /api/jobs/:id?from=&max_bytes=`：一个作业的输出窗口。
   - `POST /api/jobs/:id/stop`：停止（正在运行的作业）。
   - `DELETE /api/jobs/:id`：删记录 + 删日志（已结束的作业；运行中 409）。
4. **设置 → 后台进程**（`web/src/components/JobsPanel.vue`）：状态、命令、工作区、pid、
   运行时长、日志大小；查看日志（跟随 `from=next` 增量读取）、停止（两步确认）、删除记录
   （仅已结束）。仅在面板可见且有作业运行时轮询。

### MODIFIED

- `internal/config`：新增 `tools.enable_background`（默认 true）、`background_dir`
  （默认与数据库同级的 `jobs/`）、`background_max_jobs`、`background_log_max_mb`、
  `background_window_kb`、`background_stop_grace_seconds`。
- `cmd/huan-agent`：`admin` / `chat` / `serve` 三个入口各建一个 `jobs.Manager`，
  退出时 `Close()`；`workspaceToolSet` 持有它，按工作区构建工具时注册四个新工具。
- `docs/tools.md`、`docs/admin.md`、`configs/config.example.yaml`、`docs/status.html`。

### REMOVED

无。`bash` 的行为、超时、进程组清理、拒绝语义都保持不变。

## Non-goals

- **跨 agent 重启存活**：作业跟随起它的进程，agent 退出即收。要跨重启的常驻服务应该用
  launchd / systemd / 容器来管，而不是让 agent 记住 pid。
- **PTY 与交互输入**：stdin 仍是 `/dev/null`，一个会等待输入的命令仍然会失败 —— 不是挂住。
- **调度与依赖**：没有 cron、没有「A 起来了再起 B」、没有并发编排。
- **资源限制**：不做 CPU / 内存 / 磁盘配额，也不做 cgroup。真正的隔离仍然要靠容器。
- **容器级隔离**：常驻进程与 `bash` 一样，以 agent 用户的身份运行，能访问该用户能访问的一切。
- **跨进程视图**：飞书 bot 与 admin 服务是两个进程，各自管各自的作业；控制台显示的是
  admin 进程的作业（与此前工具行为的可见性一致）。

## Risks

| 风险 | 缓解 |
| --- | --- |
| 常驻进程泄漏（agent 退出后没人管） | 生命周期跟随 agent：`Close()` 收敛所有运行中的作业；进程组 `kill(-pgid)` 覆盖 fork 出来的子孙；`setsid` 让它们不受 agent 自身信号影响，因此收回动作只发生在我们主动执行时 |
| 日志写满磁盘 | 文件上限 `background_log_max_mb`（默认 8 MiB）到达后停止写入并留标记；内存窗口上限 256 KiB；两者都不随运行时间增长 |
| 同时起太多服务把机器拖垮 | `background_max_jobs`（默认 8）在 `Start` 时就拒绝，错误信息说明当前有几个在跑 |
| 模型把 `bash_background` 当 `bash` 用（该同步的也丢后台） | 工具描述明确分工：跑完就退的命令用 `bash`，常驻服务用 `bash_background`；启动宽限让「一启动就失败」在同一个调用里暴露 |
| 面板显示「运行中」但进程其实是残留孤儿子孙 | 状态以进程组探活为准，且这正是用户想知道的答案；`bash_stop` 杀整个组 |
| 已结束记录的日志文件在淘汰后仍留在 `background_dir` | 淘汰记录不删日志（删除是显式操作）；`docs/admin.md` 写明该目录归运维清理 |
