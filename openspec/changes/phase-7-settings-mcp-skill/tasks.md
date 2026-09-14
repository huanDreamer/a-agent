# Phase 7 — 任务清单

## 1. 数据层

- [x] migration 8：`mcp_servers`（transport / command / args / env / url / headers /
      source / enabled / last_error）
- [x] `store.MCPServer` + `ResolveTransport` / `Remote` / `Validate`
- [x] `Upsert`（整行替换）/ `Get` / `List`（config 优先）/ `Delete` / `SetError`
- [x] `args` / `env` / `headers` 的 JSON 数组编解码，坏值降级为空列表而不是让整行不可用
- [x] 测试：往返、整行替换、删除报 `ErrNotFound`、列表顺序、校验表

## 2. MCP 运行时（`internal/mcp`）

- [x] `Transport` 三值 + `ParseTransport`（空 = stdio，兼容旧配置）
- [x] `ServerSpec.Validate` 与 `Fingerprint`（改名不重连）
- [x] `Connect` 按传输方式分支；远端用 `context.WithoutCancel` 建立长连接
- [x] `WrapClient`（接 in-process 传输，使整条链路可测）
- [x] `Manager.Apply` 对齐期望与实际：不变保持、变化重连、消失注销并关闭
- [x] `Manager.Refresh`（重连）与 `Manager.Inspect`（只拨号一次）
- [x] 工具名冲突：跳过 + 记录，不拖垮整台服务器
- [x] `ServerID` slug 派生 + 表驱动测试
- [x] 管理器测试：幂等、改名不重连、改参数重连、删除注销、失败上报、部分注册、重连恢复
- [x] `tool.Registry.Unregister`；`List` 在锁内快照（并发安全）
- [x] `RegisterMCPTools` 的失败回滚改成真的回滚

## 3. MCP 接口（`internal/server/mcp.go`）

- [x] `GET /api/mcp/servers`（定义 + `runtime` 实时状态，`runtime:false` 表示没有注册表）
- [x] `POST /api/mcp/servers`：创建 / 编辑 / 启停三种语义
- [x] id 冲突 409（未声明 `overwrite` 时不允许覆盖定义）；`id`+`enabled` 视为启停
- [x] config 来源只读：非 enabled 的改动与删除都 409 并指向配置文件
- [x] `DELETE`、`POST {id}/test`、`POST /api/mcp/probe`、`POST /api/mcp/reload`
- [x] `SyncConfigServers`：镜像配置 + 清理消失的 config 行（用户行不动）
- [x] 失败写进 `last_error`（后台 context，不随请求取消）
- [x] `Server.Start` 关闭时 `mcp.Close()`；`admin.go` 启动后台 `SyncMCP`
- [x] 测试：列表、保存并注册、校验、覆盖保护、停用注销、删除、失败上报并持久化、
      probe 不落盘不注册、reload、config 只读、无注册表时仍可用、草稿、sync

## 4. 技能（文件 + 工具 + 提示）

- [x] `skill.Loader` 记录文件路径（`Path`）与 `Dir`，名字与文件名不一致时能改对文件
- [x] `skillState`：`loadedLocked` 每次重扫、`detail` / `write` / `remove` /
      `enabledSkills` / `enabledSkill`
- [x] `PUT /api/skills/{name}`：原子写、服务端渲染 frontmatter、空正文拒绝、名字白名单
- [x] `GET` / `DELETE /api/skills/{name}`；删除清掉停用记录
- [x] `tools` 白名单清洗 + `dropped_tools` 回报
- [x] `skillState` 构造时 `LoadAll`（此前 `Get` 依赖先调用过列表）
- [x] `skill` 工具（描述静态、可用性由系统提示表达）+ 可用技能段 `skillsPromptSection`
- [x] `buildHistory` 改用 `systemPrompt()`；停用即从提示与工具里消失
- [x] `POST /api/skills-draft` 写作助手（未知工具剔除 + 名字可作文件名校验 + 警告）
- [x] 测试：列表含文件信息、正文、保存往返、原地替换、校验（含穿越）、剔除未知工具、
      删除、保存清停用、工具加载/停用、提示段、渲染往返、草稿

## 5. 前端

- [x] `state.js`：`SETTINGS_TABS` / `state.settings` / `setSettings`
- [x] `SettingsView.vue`：sub-tab 外壳（`ViewHead` + `.subtabs` + `.page-scroll`）
- [x] `AppearancePanel`（主题）、`ModelPanel`（模型目录 + 模型管理）、
      `ServicePanel`（服务信息 + 工作区与工具，进入时重读工具列表）
- [x] `McpPanel`：配置助手 + 服务器列表（实时状态、工具、错误）+ 表单（测试连接 /
      保存并连接）+ 启停 / 编辑 / 两步删除 / 重连
- [x] `SkillPanel`：写作助手 + 技能列表（文件、字节数、工具）+ 编辑器 + 启停 / 删除
- [x] `api.js`：`/api/mcp/*`、技能文件接口、两个草稿接口
- [x] `icons.js`：`sparkles`
- [x] `styles.css`：MCP 列表 / 助手输出 / 原始回复 / 技能操作条
- [x] 删除旧 `SkillsPanel.vue`

## 6. 验证

- [x] `go test ./...` 全绿；`internal/{server,mcp,store,tool}` `-race` 全绿
- [x] 真实浏览器验证（`web/.verify/set-run.sh`，4 个阶段 50 项断言 0 失败）：
  - `tabs`：五个 tab、切走回来仍在同一分区、380px 不换行不溢出、暗色对比度、
    各面板无裁剪；文档本身不滚动
  - `mcp`：助手填表 → 测试连接连上**真的 stdio 子进程** → 保存连接 → 行显示已连接
    与工具 → 工具出现在服务与工具 → 停用后消失 → 重新启用再连
  - `skill`：写作助手填编辑器 → 未知工具被剔除并提示 → 保存出文件 → 启停并跨刷新持久
  - `chat`：模型真的调用了 MCP 桩的 `now`，回答里带着桩自己写下的标记
- [x] `openspec` 变更提案、文档（`web/README.md`、`docs/admin.md`、
      `configs/config.example.yaml`）
