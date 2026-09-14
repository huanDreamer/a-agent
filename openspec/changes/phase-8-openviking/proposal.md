# Phase 8 — 接入 OpenViking：记忆与文档统一落到上下文数据库

## Summary

把本机运行的 [OpenViking](https://github.com/volcengine/OpenViking)（`ov` 0.4.19，
HTTP 服务默认 `http://127.0.0.1:1933`）作为 huan-agent 的**长期记忆与文档底座**接入，
用两条通道：

1. **原生 REST 客户端**（`internal/openviking`）—— 记忆与文档由代码**自动**落库，
   不依赖模型自觉调用工具：对话轮次与关键事实进 openviking 长期记忆，会话产出的
   文档与工作区文件进 `viking://` 资源库。
2. **MCP 通道** —— 把 openviking 自带的 MCP server（streamable HTTP，`/mcp`）注册
   进工具注册表，模型可以主动 `find` / `search` / `read` / `write` / `add_resource`。

本地 JSONL 仍然保留为**真源与兜底**：openviking 不可用时只告警，不影响对话。

## Why

- **记忆目前只有关键词索引。** `internal/memory` 是 JSONL + 内存倒排表
  （`fileStore`），`SearchFacts` 只做「查询词命中倒排表」的字面匹配：换个说法、
  跨语言、近义表达全都检索不到。`PLAN.md` 里那条 "RAG 增强：内置向量库 + 文档索引"
  一直是待办，而本机已经装好了带向量库与 VLM 抽取的 openviking —— 自己造向量库没有
  意义。
- **文档没有归宿。** agent 现在能写工作区文件（`write_file`）和媒体产物，但这些
  文件停在 `data/` 与工作区里：下一轮对话看不见它们，换一个会话也检索不到。用户要
  的是「记忆和文档都保存到 openviking」。
- **两条通道各有不可替代的用途。** 只有 MCP 的话，「记忆/文档一定被保存」这件事
  取决于模型有没有调用工具；只有原生客户端的话，模型无法用它最擅长的语义检索主动
  探索资源库。两者都要。

## 现状核查（2026-09-13，本机实测）

| 能力 | 端点 | 结论 |
| --- | --- | --- |
| 健康检查 | `GET /health` | `{"status":"ok","auth_mode":"dev"}` |
| 记忆写入 | `POST /api/v1/sessions/{id}/messages/batch` + `/commit` | 可用；会话不存在时自动创建；`commit` 返回 `task_id`（异步） |
| 语义检索 | `POST /api/v1/search/find` | 可用，实测命中 score 0.70 |
| 文档写入 | `POST /api/v1/content/write` | 可用，`wait=true` 时同步返回索引状态 |
| 文件入库 | `POST /api/v1/resources/temp_upload` → `POST /api/v1/resources` | 可用；**HTTP 服务拒绝 host 落地路径**（`path` 只对 CLI 有效），本地文件必须先传 `temp_upload` |
| MCP | `POST /mcp`（streamable HTTP） | 15 个工具：`find` `search` `read` `write` `edit` `remember` `add_resource` `list` `tree` `glob` `grep` `forget` `health` … |
| 认证 | `X-API-Key` / `Authorization: Bearer`，身份头 `X-OpenViking-Account` / `X-OpenViking-User` | dev 模式免认证；api_key 模式必须带 key |

**已知环境问题（不阻塞本次接入，但会被本次接入暴露）：** 本机 `ov.conf` 的 VLM
`doubao-seed-2-0-code-preview-260215` 在 Ark 账号下**未开通**（`ModelNotOpen`），
因此 `session_commit` 的记忆抽取任务会失败（`ov task list` 可见 failed），
`add_resource` 也会带一条 `Memory linking failed` 警告。Embedding 模型正常，所以
语义检索不受影响。为此本变更的记忆写入**不把 VLM 抽取当作唯一路径**：除 session
提交外，另有一条不依赖 VLM 的直写通道（见 ADDED-3）。

## What Changes

### ADDED

1. **`internal/openviking`（原生客户端）**
   - `Client` 覆盖接入需要的 REST 面：`Health`、`Remember`（批量消息 + 可选 commit）、
     `Find`、`WriteContent`、`ReadContent`、`Stat`、`UploadTemp`、`AddResource`、
     `TaskStatus`、`Forget`。
   - 认证：`X-API-Key`（或 `Authorization: Bearer`）+ `X-OpenViking-Account` /
     `X-OpenViking-User` 身份头；dev 模式留空即可。
   - 统一错误类型 `*APIError{Status, Code, Message, RequestID}`，把 openviking 的
     `{"status":"error","error":{...}}` 翻成 Go 错误；**超时与网络错误必须可识别**
     （`IsUnavailable`），调用方据此降级。
   - `Find` 结果按 `memories` / `resources` / `skills` 分类返回，含 `uri`/`score`/
     `abstract`，供记忆召回与文档检索共用。

2. **配置节 `openviking`（Viper，全部可调）**
   ```yaml
   openviking:
     enable: true
     base_url: "http://127.0.0.1:1933"
     api_key: ""
     account: "default"
     user: "default"
     timeout_seconds: 15
     memory:
       enable: true
       commit: true            # 提交给 VLM 抽取长期记忆
       flush_every: 4          # 攒够 N 条消息提交一次（0 = 每轮）
       recall_enable: true     # 用语义检索召回事实
       recall_limit: 5
       recall_target: ""       # 默认 viking://user/<user>
       journal_enable: true    # 不依赖 VLM 的直写日志
     documents:
       enable: true
       root_uri: "viking://user/default/huan-agent"
       sync_workspace: true
       workspace_dir: ""       # 空 = tools.workspace
       include: ["**/*.md", "**/*.txt", "**/*.json", "**/*.yaml", "**/*.yml", "**/*.go"]
       exclude: [".git/**", "node_modules/**", "data/**", "*.db", "*.lock"]
       max_file_kb: 512
       binary_mode: "skip"     # skip | upload（upload 走 temp_upload + add_resource）
       sync_interval_seconds: 0 # >0 时后台定时同步（serve）
       state_path: "./data/openviking-docs.json"
     mcp:
       register: true          # 自动把 openviking 注册成 MCP server
       name: "openviking"
   ```

3. **记忆双写（`internal/memory`）**
   - `NewOpenVikingStore(local Store, ov *openviking.Client, cfg)`：装饰器实现
     `memory.Store`。
   - `Append`：先写本地 JSONL（真源），再把 `user`/`assistant` 轮次**攒批**推到
     openviking（`messages/batch` + `commit`）。
   - `AddFact`：本地 + openviking 两条路并行 —— (a) `Remember` 让 VLM 抽取成结构化
     记忆；(b) **直写**一条 `viking://user/<user>/huan-agent/memory/journal.md`
     （append 模式，不依赖 VLM，立即可被 `find` 检索）。VLM 未开通时 (a) 会失败，
     此时 (b) 保证关键事实不会丢。
   - `SearchFacts`：**先语义检索**（`Find`，限定 `recall_target`），无结果或
     openviking 不可用时回落本地关键词索引。
   - 全链路降级：openviking 故障只记 WARN，本地写入与对话不受影响；错误与失败计数
     暴露给状态接口。
   - `Flush` / `Close`：把攒批的轮次提交掉。

4. **文档同步（`internal/documents`）**
   - `Save(ctx, Document)`：会话产出的文档/报告 → `content/write` 到
     `<root_uri>/documents/<YYYY>/<MM>/<slug>-<hash8>.md`（带 frontmatter：
     title / tags / source / created_at），`wait=true` 确保写完即可检索。
   - `SyncWorkspace(ctx)`：遍历工作区（include/exclude glob、大小上限、跳过二进制），
     文本文件按 sha256 与本地状态文件比对，**只上传变化的**；每个文件
     `content/write` 到 `<root_uri>/workspace/<relpath>`。`binary_mode: upload` 时改走
     `temp_upload` + `add_resource`。
   - 状态文件 `state_path`（JSON：uri → {path, hash, size, synced_at}）驱动增量同步，
     并提供 `Documents()` 给控制台列出已同步清单。
   - 结果类型 `Report{Scanned, Uploaded, Unchanged, Skipped, Failed, Errors[]}`。
   - 定时同步：`sync_interval_seconds > 0` 时由 `serve` 起后台 ticker。

5. **Agent 内置工具 `save_document`（`internal/tool/builtin`）**
   - 仅在 `openviking.documents.enable` 为真时注册；参数 `title` / `content` /
     `tags[]` / `source`，返回写入的 `viking://` URI。这是「会话产出的文档」被保存的
     确定性路径（模型不必知道 URI 规则）。

6. **MCP 自动注册**
   - `openviking.mcp.register: true` 且 `openviking.enable: true` 时，配置加载阶段把
     一条 `transport: http`、`url: <base_url>/mcp`、带认证/身份头的服务器条目并入
     `mcp.servers`（同名条目已存在时不重复添加，用户显式声明优先）。因此 CLI
     `chat --tools`、飞书 `serve`、控制台 Web 对话三处**同时**获得 openviking 工具。

7. **CLI `huan-agent viking`**
   - `viking status`：连通性 + 记忆/文档统计。
   - `viking sync [--full]`：手动触发工作区同步（`--full` 忽略状态文件全量重传）。
   - `viking save --title T (--file F | --content C) [--tags a,b]`：保存一份文档。
   - `viking remember "文本"`：写入一条关键事实。

8. **Admin API + 控制台面板**
   - `GET /api/openviking/status`、`POST /api/openviking/sync`、
     `POST /api/openviking/save`、`GET /api/openviking/documents`、
     `POST /api/openviking/flush`。
   - 设置新增 sub-tab「OpenViking」：连接状态、记忆/文档计数、最近同步报告与错误、
     一键同步、文档清单。

### MODIFIED

- `internal/config`：新增 `OpenVikingConfig`（含子配置与默认值）＋ `mcp.servers`
  合并逻辑。
- `cmd/huan-agent/memory.go`：`newSessionMemory` 在 openviking 启用时用镜像商店包装
  本地商店；`recallFacts` 走语义检索。
- `cmd/huan-agent/serve.go`：启动时按配置起文档同步 ticker，退出时 flush。
- `configs/config.yaml`：新增带注释的 `openviking` 示例段（默认 `enable: false`，
  不改变现有行为）。
- `docs/`：新增 `docs/openviking.md`（部署、配置、故障处理、VLM 未开通的表现）。

## Non-goals

- 不做 openviking 的账户/用户/ACL 管理（控制台只读展示状态）。
- 不改本地 JSONL 的存储格式，不做历史数据迁移（历史记忆可继续用本地检索）。
- 不实现 openviking 的 sessions 会话管理、snapshot、ovpack 导入导出。
- 不引入 openviking 的 Go SDK（无官方 Go SDK，直接走 REST + 现有的 mcp-go 通道）。
- 不修 openviking 侧的 VLM 开通问题（文档说明如何自查）。

## Risks

- **VLM 未开通** → 记忆抽取失败。缓解：直写 journal 通道 + 状态接口暴露
  `last_error` 与失败计数 + `docs/openviking.md` 写明 `ov task list` 自查方法。
- **语义检索召回污染**：openviking 里已有其它项目资料（本机实测有 `viking://resources/README`）。
  缓解：召回默认限定 `viking://user/<user>/huan-agent` 子树，配置可改。
- **同步放大**：工作区大目录全量入库会消耗 embedding 配额。缓解：glob 过滤、大小上限、
  sha256 增量、二进制默认 skip、默认关闭定时同步。
