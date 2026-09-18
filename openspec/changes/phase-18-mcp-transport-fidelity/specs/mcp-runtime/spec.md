# Capability: mcp-runtime

## Purpose

让 MCP 服务器成为控制台可以管理、**每一个 surface 都能真的连上**、并且模型真的能调用的
能力：配置持久化、连接可测试、增删改立即生效、工具注册进对话读取的同一个注册表。

## Scope

- `internal/mcp` — 传输方式、运行时管理器。
- `internal/store` — `mcp_servers` 表与访问器。
- `internal/server` — `/api/mcp/*` 与 `SyncConfigServers`。
- `cmd/huan-agent` — CLI 与飞书两个 surface 的配置→运行态转换。
- `internal/tool` — `Unregister` 与并发安全的 `List`。
- `web/src` — 设置 → MCP。

## Requirements (MUST)

1. **传输方式。** 每个服务器 MUST 声明三种传输之一：`stdio`（默认）、`sse`、
   `http`。`stdio` 必须有 `command`，`sse` / `http` 必须有 `url`；缺失时 MUST 在
   拨号前报错并指明缺的是哪个字段。空的 transport MUST 按 `stdio` 处理（历史配置
   文件里没有这个字段）。
2. **配置到运行态必须保真。** 从 `config.MCPServer` / `store.MCPServer` 构造
   `mcp.ServerSpec` 时，MUST 逐字段搬运 `Transport`、`Command`、`Args`、`Env`、`URL`、
   `Headers`。**每一个 surface（控制台、CLI、飞书）MUST 走同一个转换函数**，MUST NOT
   各自手写字段列表——一个只搬了 `Name/Command/Args/Env` 的转换会把 `http` 声明变成
   一个缺 `command` 的 `stdio` 声明，而报错会指向配置文件，指向错误的方向。
3. **自动注册的服务器与控制台一致。** 由 `ApplyOpenVikingMCP` 之类机制自动追加的条目
   （声明 `transport: http` + `url` + 鉴权 `headers`）MUST 在 CLI 与飞书路径上以同样的
   传输方式连上；MUST NOT 出现"控制台能连、CLI 不能连"的分叉。
4. **非法传输不静默回落。** 遇到无法解析的 transport 取值时 MUST 报错并指出取值与可选值，
   MUST NOT 当作 `stdio` 继续尝试连接。
5. **停用即不连。** `enabled: false` 的服务器 MUST 被所有 surface 跳过（缺省视为启用）。
6. **持久化。** 服务器定义 MUST 存在 `mcp_servers` 表（不是配置文件），`args`、
   `env`、`headers` MUST 原样往返；保存 MUST 整行替换（空列表就是空列表）。
7. **热加载。** 保存、删除、启停 MUST 立刻反映到运行时：新增/变更的服务器被连接、
   其工具被注册进对话使用的注册表；删除或停用的服务器 MUST 注销其工具并关闭连接。
   不需要重启进程。
8. **不变就不重连。** 只改了显示名的服务器 MUST 保持现有连接（比较的是连接相关
   字段的指纹，不是整行）。
9. **可测试。** `POST /api/mcp/servers/{id}/test` MUST 连接、列出工具、断开，且
   MUST NOT 影响该服务器正在运行的连接。`POST /api/mcp/probe` MUST 接受一份未保存
   的定义并做同样的事，MUST NOT 写入数据库、MUST NOT 注册任何工具。
10. **失败是结果。** 连接失败 MUST 以 200 返回 `{ok:false,error}`（界面要显示原因），
    并把消息写进该服务器的 `last_error`，使重启后仍能看到；保存一份连不上的定义
    MUST 成功（定义合法、连接失败是两件事）。失败信息 MUST 包含实际使用的传输方式。
11. **工具名冲突。** 与已注册工具撞名时 MUST 跳过该工具、保留其余工具、把冲突写进
    该服务器的错误信息，MUST NOT 让整台服务器不可用。
12. **配置来源只读。** `source=config` 的行（由 `mcp.servers` 同步而来）MUST 只允许
    切换 `enabled`；修改定义或删除 MUST 以 409 拒绝并指向配置文件。
13. **id 冲突保护。** 用一个已存在的 id 提交完整定义且未声明 `overwrite` 时 MUST 以
    409 拒绝（否则界面生成的重名 id 会静默覆盖一台正在工作的服务器）；只带
    `id`+`enabled` 的请求 MUST 视为启停并允许更新。
14. **重连。** 提供一个「重连」接口，强制重新连接全部已启用服务器——传输在运行中
    死掉（子进程崩溃、SSE 断开）时这是唯一的恢复方式。
15. **生命周期。** 进程退出时 MUST 关闭所有连接（`stdio` 是子进程，不关就会泄漏）。
    远端传输的连接 MUST NOT 因为触发它的那次 HTTP 请求结束而被取消。

## Non-goals

- OAuth / 动态客户端注册。
- MCP 的 prompts、resources、sampling（只做 tools）。
- 把 huan-agent 自身暴露成 MCP server（见 Open Questions）。
- 多用户隔离：控制台是单用户的本地控制台，`env` / `headers` 原样返回给界面以便编辑。

## Open Questions

- 反过来把 huan-agent 变成 MCP server（供 IDE / 别的 agent 调用）是一个独立 change；
  它需要自己的 capability，不属于 `mcp-runtime` 的客户端职责。
