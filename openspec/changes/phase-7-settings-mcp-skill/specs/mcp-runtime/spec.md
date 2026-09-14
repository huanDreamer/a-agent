# Capability: mcp-runtime

## Purpose

让 MCP 服务器成为控制台可以管理、并且模型真的能调用的能力：配置持久化、连接可
测试、增删改立即生效、工具注册进对话读取的同一个注册表。

## Scope

- `internal/mcp` — 传输方式、运行时管理器。
- `internal/store` — `mcp_servers` 表与访问器。
- `internal/server` — `/api/mcp/*` 与 `SyncConfigServers`。
- `internal/tool` — `Unregister` 与并发安全的 `List`。
- `web/src` — 设置 → MCP。

## Requirements (MUST)

1. **传输方式。** 每个服务器 MUST 声明三种传输之一：`stdio`（默认）、`sse`、
   `http`。`stdio` 必须有 `command`，`sse` / `http` 必须有 `url`；缺失时 MUST 在
   拨号前报错并指明缺的是哪个字段。空的 transport MUST 按 `stdio` 处理（历史配置
   文件里没有这个字段）。
2. **持久化。** 服务器定义 MUST 存在 `mcp_servers` 表（不是配置文件），`args`、
   `env`、`headers` MUST 原样往返；保存 MUST 整行替换（空列表就是空列表）。
3. **热加载。** 保存、删除、启停 MUST 立刻反映到运行时：新增/变更的服务器被连接、
   其工具被注册进对话使用的注册表；删除或停用的服务器 MUST 注销其工具并关闭连接。
   不需要重启进程。
4. **不变就不重连。** 只改了显示名的服务器 MUST 保持现有连接（比较的是连接相关
   字段的指纹，不是整行）。
5. **可测试。** `POST /api/mcp/servers/{id}/test` MUST 连接、列出工具、断开，且
   MUST NOT 影响该服务器正在运行的连接。`POST /api/mcp/probe` MUST 接受一份未保存
   的定义并做同样的事，MUST NOT 写入数据库、MUST NOT 注册任何工具。
6. **失败是结果。** 连接失败 MUST 以 200 返回 `{ok:false,error}`（界面要显示原因），
   并把消息写进该服务器的 `last_error`，使重启后仍能看到；保存一份连不上的定义
   MUST 成功（定义合法、连接失败是两件事）。
7. **工具名冲突。** 与已注册工具撞名时 MUST 跳过该工具、保留其余工具、把冲突写进
   该服务器的错误信息，MUST NOT 让整台服务器不可用。
8. **配置来源只读。** `source=config` 的行（由 `mcp.servers` 同步而来）MUST 只允许
   切换 `enabled`；修改定义或删除 MUST 以 409 拒绝并指向配置文件。
9. **id 冲突保护。** 用一个已存在的 id 提交完整定义且未声明 `overwrite` 时 MUST 以
   409 拒绝（否则界面生成的重名 id 会静默覆盖一台正在工作的服务器）；只带
   `id`+`enabled` 的请求 MUST 视为启停并允许更新。
10. **重连。** 提供一个「重连」接口，强制重新连接全部已启用服务器——传输在运行中
    死掉（子进程崩溃、SSE 断开）时这是唯一的恢复方式。
11. **生命周期。** 进程退出时 MUST 关闭所有连接（`stdio` 是子进程，不关就会泄漏）。
    远端传输的连接 MUST NOT 因为触发它的那次 HTTP 请求结束而被取消。

## Non-goals

- OAuth / 动态客户端注册。
- MCP 的 prompts、resources、sampling（只做 tools）。
- 多用户隔离：控制台是单用户的本地控制台，`env` / `headers` 原样返回给界面以便编辑。
