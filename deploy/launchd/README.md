# launchd: run the admin server as a managed service (macOS)

macOS 没有 systemd。同一件事由 **launchd** 负责：开机/登录自启、进程崩了自动拉起、
stdout/stderr 落盘。这里的 `com.huan-agent.admin.plist.tmpl` 就是那个"unit"。

## 为什么需要它

`make run-admin` 把服务跑在 SSH 会话的前台进程组里。SSH 一断，内核向该进程组发
`SIGHUP`，服务就死了——`nohup make run-admin &` 也救不了：make 在 exec 子进程前会把
信号处置重置回默认，所以真正干活的二进制照样收到 `SIGHUP`（症状是 `make: *** [run-admin] Hangup: 1`）。

交给 launchd 之后，进程由 launchd 直接拥有、没有控制终端，SSH 断线、关窗口、退出登录
都不影响它。

## 用法

```bash
make agent-install     # 编译 + 渲染 plist + 安装并启动（幂等，可重复执行）
make agent-status      # 看 PID / 状态 / 最近一次退出码
make agent-logs        # tail -f logs/admin.log logs/admin.err.log
make agent-restart     # 改了代码：make build 之后用它重启
make agent-uninstall   # 停掉并删除 LaunchAgent
```

手工等价命令（不想用 make 时）：

```bash
launchctl print gui/$(id -u)/com.huan-agent.admin     # 状态
launchctl kickstart -k gui/$(id -u)/com.huan-agent.admin   # 重启
launchctl bootout gui/$(id -u)/com.huan-agent.admin   # 停止并卸载
```

## 装了什么、装在哪

- plist：`~/Library/LaunchAgents/com.huan-agent.admin.plist`（由 `make agent-install`
  从模板渲染，`__PROJECT_DIR__` / `__HOME_DIR__` 会替换成实际绝对路径，所以换个目录
  checkout 也能装）
- 日志：`logs/admin.log`、`logs/admin.err.log`（`logs/` 已在 `.gitignore` 里）
- 服务地址：`http://<这台机器>:8080/`（`configs/config.yaml` 里 `admin.port`）

## 几个必须知道的行为

| 行为 | 说明 |
| --- | --- |
| 开机自启 | `RunAtLoad` + 登录时加载（LaunchAgent 属于 Aqua 会话）→ **登录后**才起来，不是开机即起 |
| 崩溃重启 | `KeepAlive` 为真，任何退出都会被拉起；`ThrottleInterval` 10s 防止崩溃热循环 |
| 停止方式 | 不能只 `kill`（会被立刻拉起）；用 `make agent-uninstall` 或 `launchctl bootout ...` |
| 工作目录 | plist 里显式设了 `WorkingDirectory` 与绝对 `--config`，否则 `data/`、checkpoints 会落到别处 |
| PATH | launchd 只给 `/usr/bin:/bin:/usr/sbin:/sbin`。agent 会 shell out 调 go/git/node，所以模板里把 PATH 显式写成了登录 shell 的一份；**装完新工具链记得同步这里** |
| 合盖休眠 | MacBook 睡眠时进程一起挂起，唤醒后继续；这是机器行为，不是服务配置问题 |
| 开机即起（无需登录） | 那要 LaunchDaemon（`/Library/LaunchDaemons` + root）。不推荐：服务会以 root 跑，写出来的文件属主、`~/.ssh`、用户密钥全都不对 |

## 想同时跑飞书机器人

`admin serve` 支持 `--with-feishu`（见 `cmd/huan-agent/admin.go`）。把模板里
`ProgramArguments` 的 `--config` 那条之后再加：

```xml
<string>--with-feishu</string>
```

然后 `make agent-install`。
