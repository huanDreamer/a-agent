# Phase 9 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

> 本变更有过一版设计：工作区曾带策略（只读 / 允许命令 / 说明）并在设置页管理，目录由
> 服务端在 `workspaces_dir` 下按名字自动创建。那一版被**撤销**：工作区只回答「在哪个目录
> 工作」，策略回到全局 `tools.*`，工作区改由控制台左侧的文件夹与目录选择器管理，且必须指向
> 一个**已存在**的目录。下面的清单反映现行设计。

## 1. 配置 `internal/config`

- [x] `ToolsConfig.Workspace` 语义改为「初始工作区目录」，作为首次启动的种子
- [x] 移除 `tools.workspaces_dir`（不再自动创建目录）
- [x] `FeishuConfig.EnableTools`（默认 true）
- [x] `SetDefaults` 与 `configs/config.example.yaml` 同步
- [x] 测试：种子目录默认值、env 覆盖、`feishu.enable_tools` 默认与关闭

## 2. 持久化 `internal/store`

- [x] migration 9：`workspaces` / `workspace_bindings` / 审计与附件的 workspace 列
- [x] migration 10：`workspaces` 收成 `name + root + 时间戳`（删除 `description` /
      `read_only` / `enable_bash`；`root` 不动，已注册的目录继续指向原处）
- [x] `store.Workspace`（带 `SessionCount`）+ Upsert / List / Get
- [x] `RenameWorkspace`：一个事务里改标签并搬走绑定
- [x] `DeleteWorkspace`（不删文件）/ `MoveWorkspaceBindings` / `ListScopesInWorkspace`
- [x] `ListUnboundWebSessions`：回填「没有工作区的历史会话」
- [x] `ChatSession.Workspace` + `ChatSessionFilter.Workspace`（SQL join，不在前端拼）
- [x] 测试：迁移幂等、重命名原子性、删除解绑、迁移绑定、会话列表带工作区 → 覆盖率 85%+

## 3. 注册表 `internal/tool`

- [x] `Registry.Clone()`：深拷工具表 + allow-list 状态
- [x] `Registry.Replace(t)`：换掉同名实现，保持 allow-list 成员关系
- [x] 测试：克隆互不影响、替换可见、allow-list 保持、运行期注册的 MCP 工具被带过去

## 4. 服务层 `internal/workspaces`（重写）

- [x] `Spec{Name, Root, SessionCount, Missing}`（**无策略字段**）
- [x] `ResolveDir`：必须是已存在的目录；解析符号链接；拒绝文件、不存在的路径、`/`
- [x] `ValidateName`：标签规则（非空、去空白、长度、禁 `/` `\`、禁控制字符），允许中文与空格
- [x] `Create`：校验目录 → 校验名字 → 唯一性 → 落库；**不创建目录**
- [x] `Rename`：只改标签，目录不动，绑定跟着走
- [x] `Delete`：迁走会话后删除注册（永不删文件）；只剩一个时拒绝
- [x] `EnsureSeed`：空库时按 `tools.workspace` 种下第一个工作区，并回填无绑定的会话
- [x] `Default`：优先「最近一次对话所在的工作区」
- [x] `Active` / `Select` / `Resolve` / `Open`（按 scope，按 root 缓存沙箱）
- [x] `Browse`：目录浏览（只列目录、跟随符号链接、含 parent/home/截断标记）
- [x] `ParseSwitchIntent` / `ResolveName`：飞书自然语言切换（保守，不误判）
- [x] 测试：目录校验、标签规则、两个工作区互不可达、按 scope 隔离、重命名不丢会话、
      删除不删文件且会话迁移、种子与回填、默认工作区、浏览拒绝与符号链接 → 覆盖率 88%

## 5. 回合契约 `internal/chat`

- [x] `Request.Scope` + `WithScope` / `ScopeFrom`
- [x] `Config.ToolsFor`（每轮取注册表；为空退回静态 `Tools`）
- [x] Runner 在开跑前把 scope 盖进 ctx；每轮只解析一次注册表
- [x] 测试：`ToolsFor` 每轮生效、工具 ctx 里能读到 scope、失败即回合失败、nil 即无工具

## 6. 装配 `cmd/huan-agent`

- [x] `workspaceTools` binder：Clone 基础表 + 按 root 重建 workspace 类工具
- [x] 策略回到全局：`tools.read_only` / `tools.enable_bash`（不再按工作区摘除）
- [x] `buildChatDeps` 与 `runServe` 共用同一 binder，并在启动时 `EnsureSeed`
- [x] 测试：每轮注册表来自当轮 scope、全局只读不注册写工具、克隆含运行期注册的工具

## 7. 控制台 API `internal/server`

- [x] `GET /api/workspaces`（列表 + 会话数 + 默认 + home + 目录缺失警告）
- [x] `POST /api/workspaces`（`{root,name}`，目录必须已存在）
- [x] `PATCH /api/workspaces/:name`（重命名）/ `DELETE /api/workspaces/:name`（删除）
- [x] `GET /api/fs/dirs`（目录选择器；只列目录，坏路径 200 + ok=false）
- [x] `PUT /api/chat/sessions/:id/workspace`（按会话切换）
- [x] 会话创建即绑定工作区（可显式指定，否则沿用最近使用的工作区）
- [x] 会话列表每条带 `workspace`，并支持 `?workspace=` 过滤
- [x] 附件上传/回读按会话工作区；审计行写入 workspace
- [x] 测试：CRUD 校验、最后一个不可删、浏览、会话必属工作区、按会话切换互不影响

## 8. 飞书渠道

- [x] `botHandler` 走 `chat.Runner`（scope = `feishu:<open_id>`），启动时 `EnsureSeed`
- [x] `/workspace`、`/workspaces`、`/workspace <name>`
- [x] 自然语言切换（精确 → 唯一前缀 → 一次模型调用 → 列出候选）
- [x] 回复只讲名字与目录（工作区没有策略可讲）
- [x] `feishu.enable_tools=false` 时退回纯聊天
- [x] 测试：命令、NL 用例集（含「在工作区里建个文件」这类反例）、切换按 open_id 隔离

## 9. Web 控制台

- [x] 删除 设置 → 工作区 面板与相关 api/store 代码
- [x] 侧边栏按工作区分组（文件夹）：折叠、会话数、目录缺失提示
- [x] 文件夹头：在该工作区新建会话 / 重命名 / 删除（两步确认，文案写明会话会被迁走）
- [x] 侧边栏底部「新建工作区（选择目录）」入口
- [x] `DirPicker.vue`：macOS 式分栏目录选择器 —— 点击文件夹在右侧相邻栏显示其内容、
      整条链高亮、再点已选中的文件夹收起其右侧栏、上一级 / 主目录 / 路径跳转 / 名字预填
- [x] 对话页顶部选择器：显示当前工作区，title 显示完整目录
- [x] 设置页六个 sub-tab（外观 / 模型 / MCP / OpenViking / 技能 / 服务与工具）
- [x] `npm run build` 产出到 `internal/server/webui/dist`
- [x] `npm run check:ui`：组件绑定守卫（prop 被同名 setup 绑定遮蔽 / 模板把 prop 当函数调用）
      + 渲染探针（关闭时不渲染对话框等）
- [x] `bash web/.verify/ws-run.sh`：真实 Chrome/CDP 的 26 项断言 —— 进页面无弹窗、打开/取消、
      选目录后关闭、重名时保持打开并显示原因、刷新后仍在、会话挂在文件夹下、折叠、
      + 在该工作区新建会话、重命名、删除（文件仍在、会话迁移）、无意外控制台报错，
      以及分栏交互（点击文件夹→右侧出现新栏并列出内容、点击链上的文件夹→收起其右侧栏且
      保持自身选中、点击最深层文件夹无变化）

## 10. 文档

- [x] `docs/tools.md`：工作区章节（选目录、无策略、隔离边界、排错）
- [x] `docs/feishu.md`：命令与自然语言示例、`feishu.enable_tools` 警告
- [x] `docs/admin.md`：工作区 API 与侧边栏交互
- [x] `web/README.md`：侧边栏文件夹与目录选择器
- [x] `docs/status.html`：阶段卡片与能力卡片
- [x] `configs/config.example.yaml`

## 11. 验收

- [x] `go build ./...`
- [x] `go test ./...`（核心模块 ≥ 70%）
- [ ] `golangci-lint run`（本机未安装；`go vet ./...` 已通过）
- [x] `cd web && npm run build`
- [x] 端到端：两个工作区（两个真实目录）、两个会话分别写各自的 `note.txt`；只读时
      provider 收到的工具里没有 `write_file` / `bash`；重命名后会话跟着走；
      删除工作区后文件仍在且会话迁移；只剩一个时拒绝删除；审计带工作区名
