# OpenViking 接入（记忆与文档）

huan-agent 把 [OpenViking](https://github.com/volcengine/OpenViking) 作为长期记忆与
文档的底座。本文说明它做什么、怎么配、怎么验证、以及出问题怎么查。

- 一次性方案与验收标准见 `openspec/changes/phase-8-openviking/`。
- 所有开关都在 `openviking` 配置节，**默认关闭**：不配就不改变任何原有行为。

## 1. 它做什么

两条通道同时工作，各有不可替代的用途：

| 通道 | 谁用它 | 做什么 |
| --- | --- | --- |
| 原生 REST 客户端（`internal/openviking`） | 代码自动调用 | 记忆双写、文档落库、语义召回、工作区同步 |
| MCP（`<base_url>/mcp`） | 模型主动调用 | `find` / `search` / `read` / `write` / `add_resource` 等 OpenViking 自带工具 |

具体行为：

- **记忆双写**。对话轮次与事实仍然先写本地 JSONL（真源），同时提交给 OpenViking
  抽取长期记忆；事实另有一条不依赖 VLM 的直写日志
  （`viking://user/<user>/huan-agent/memory/journal.md`），保证 VLM 未开通时关键
  事实照样可检索。召回优先语义检索（`find`），失败或为空时回落本地关键词索引。
- **文档**。会话产出的报告/笔记通过 `save_document` 工具（模型可调用）、
  `huan-agent viking save`、控制台「OpenViking」面板保存到
  `<root_uri>/documents/<年>/<月>/<标题>-<hash8>.md`。
- **工作区同步**。工作区里的文本文件按内容哈希增量同步到
  `<root_uri>/workspace/<相对路径>`；二进制默认跳过（可切成 `upload`，走
  `temp_upload` + `add_resource`）。
- **降级**。OpenViking 不可用时只记 WARN 并计数，对话与本地记忆不受影响；失败原因
  在 `huan-agent viking status` 与设置 → OpenViking 里可见。

## 2. 配置

最小可用配置（本机 `ov` 默认端口 1933、dev 认证）：

```yaml
openviking:
  enable: true
  base_url: "http://127.0.0.1:1933"
```

完整字段见 `configs/config.example.yaml` 的 `openviking` 段。几个值得注意的点：

- `user` / `account` 决定数据落在 `viking://user/<user>/...`；默认 `default`。
- `memory.flush_every`：攒够多少条消息提交一次（`0` = 每轮）。提交会触发服务端
  抽取，攒批能显著减少抽取次数。
- `documents.workspace_dir`：要同步的目录。空则用 `tools.workspace`；两者都空时
  **不同步**（而不是去爬进程工作目录）。
- `documents.sync_on_exit`：交互式 `chat` 结束时做一次增量同步（默认开）。
- `documents.sync_interval_seconds`：`serve` 的后台定时同步（默认 0 = 关）。
- `documents.wait_index`：批量同步是否等待索引完成（默认关；显式保存的文档总是等）。
- `index_wait_seconds`：写操作允许服务端索引队列跑多久（默认 30）。服务端在
  `content/write` 的 `wait=true` 时按这个预算等待，**实测本机一篇小文档约需
  15–17 秒**（语义抽取 + embedding），所以 30 秒够覆盖一个慢周期。
- `timeout_seconds`：HTTP 客户端单次请求超时（`0` = 自动 = `index_wait_seconds + 15s`）。
  它**永远不会低于** `index_wait_seconds + 15s`：客户端必须先于服务端放弃，否则
  你看到的是「context deadline exceeded」，而不是服务端自己的 504，就分不清
  「索引慢」和「服务不可达」。详见 §5。
- `mcp.register`：自动把 `<base_url>/mcp` 注册成 MCP server，让模型拿到 OpenViking
  自己的工具。若 `mcp.servers` 里已有同名条目，以你声明的为准。

环境变量一律可用：`HUAN_OPENVIKING_ENABLE`、`HUAN_OPENVIKING_BASE_URL`、
`HUAN_OPENVIKING_MEMORY_FLUSH_EVERY`、`HUAN_OPENVIKING_DOCUMENTS_SYNC_WORKSPACE` 等
（Viper 的 `.` → `_` 规则）。

## 3. 使用

```bash
# 连接状态、记忆计数、文档计数、最近一次同步报告
huan-agent viking status
huan-agent viking status --json      # 给脚本用

# 手动同步工作区（--full 忽略状态文件全量重传）
huan-agent viking sync
huan-agent viking sync --full

# 保存一份文档（返回写入的 viking:// URI）
huan-agent viking save --title "设计笔记" --file notes.md --tags design,review

# 记一条关键事实 / 语义检索
huan-agent viking remember "部署负责人是 Alice"
huan-agent viking search "部署负责人" --limit 5
```

控制台的**设置 → OpenViking** 面板提供同样的信息与操作：连接状态、记忆/文档计数、
最近同步报告与错误、一键同步（增量/全量）、已同步文档清单、手动保存文档。

模型侧的工具：`save_document`（框架内置，只在 `openviking.documents.enable` 时注册）
与 OpenViking 自己的 MCP 工具（`find` / `search` / `read` / `write` / `add_resource`
/ `remember` / `forget` 等）。

## 4. 数据落在哪

```
viking://user/<user>/huan-agent/
├── documents/2026/09/<标题>-<hash8>.md   # viking save / save_document / 控制台
├── memory/journal.md                     # 关键事实（append，不依赖 VLM）
└── workspace/<相对路径>                   # 工作区同步
```

本地状态：

- 记忆真源：`memory.dir`（默认 `./data/memory`）的 JSONL 文件。
- 同步状态：`openviking.documents.state_path`（默认 `./data/openviking-docs.json`），
  记录 `路径 → 内容哈希`，是增量同步的依据；损坏或版本不匹配时退化为全量同步。

## 5. 排障

**记忆「写了但检索不到」** — 先看是不是 VLM 抽取失败：

```bash
ov task list              # 看 session_commit 任务的状态
ov observer models        # 看模型可用性
```

如果 `error` 里出现 `ModelNotOpen`，说明你账号下没有开通 `~/.openviking/ov.conf`
里配的 VLM 模型（默认 `doubao-seed-2-0-code-preview-260215`）。这不是 huan-agent
的问题，两条路：

1. 在火山方舟控制台开通该模型；
2. 或把 `ov.conf` 的 `vlm.model` 换成一个已开通的模型。

在修好之前，**关键事实仍然可用**：它们写在 `memory/journal.md` 里，只依赖
embedding 即可被 `find` 命中（embedding 模型正常时）。对话轮次的结构化抽取则要等
VLM 可用。

**文档同步后立刻检索不到** — OpenViking 的索引是异步的，写入后几秒内
`find` 可能还看不到。`huan-agent viking sync` 结束时会提示这一点；
把 `documents.wait_index` 设为 `true` 可以让同步等索引完成（每个文件多一次往返）。

**保存文档报 `context deadline exceeded (Client.Timeout exceeded while awaiting headers)`**
— 这是客户端超时预算配错了，不是文档丢了。成因：`content/write` 在 `wait=true`
时先落盘、再等索引，而索引要走完服务端的语义抽取（一次约 15–17 秒）。如果
HTTP 客户端超时和「服务端等待预算」一样长（旧默认都是 15s），两者会在同一瞬间
到期，客户端必然先放弃，于是拿到一个什么都说明不了的传输层错误。

现在两者被强制拉开：`index_wait_seconds`（服务端预算，默认 30）严格小于
`timeout_seconds`（客户端超时，自动 = 前者 + 15s）。所以你会看到服务端自己的
`HTTP 504: DEADLINE_EXCEEDED`，而不是传输层超时。

**更重要的是：等待超时不再算保存失败。** 内容在等待之前就已落盘，所以
「等待超时」会被判为「已保存，索引稍后就绪」：打一条 WARN、写进本地同步状态、
正常返回 URI。只有**连内容都确认读不到**时才报错。想确认服务端在忙什么：

```bash
ov observer queue      # Semantic / Embedding 队列的 pending / in-progress
ov status              # queue 组件的健康与处理中数量
```

如果语义队列长期堆积，把 `index_wait_seconds` 调大即可——它只影响等多久，
不影响写入是否成功。

**同步把不该传的文件传上去了** — 用 `include` / `exclude` 收敛：
`**` 跨目录，不含 `/` 的模式（如 `*.log`）也会按文件名在任意层级匹配。
另外 `max_file_kb` 限制单文件大小，二进制由 `binary_mode` 决定。

**连接失败** — `huan-agent viking status` 会直接把原因打出来；`ov health` 与
`ov status` 是服务端的自检。配置 `api_key`（api_key 模式下 `X-API-Key` 必填）
并确认 `account` / `user` 指向的子树有权限。

服务端绑定非 loopback 地址时**必须**用 api_key 模式：`~/.openviking/ov.conf` 的
`server.root_api_key` 一旦非空，认证模式自动从 `dev` 切到 `api_key`，此时
没有 key 的请求一律 401。key 分两层：`root_api_key` 只能调 admin/system/reindex，
数据接口（`ls` / `find` / `write`）要用 Admin API 建出来的 **user key**：

```bash
# 用 root key 建账户与用户，返回 user_key
curl -X POST http://127.0.0.1:1933/api/v1/admin/accounts/default/users \
  -H "X-API-Key: $ROOT_KEY" -H "Content-Type: application/json" \
  -d '{"user_id": "default", "role": "admin"}'
```

把返回的 `user_key` 填进 `openviking.api_key`（或用 `HUAN_OPENVIKING_API_KEY`）。
api_key 模式下服务端从 key 推导身份，`X-OpenViking-Account` / `X-OpenViking-User`
会被**忽略**（不报错），但 `openviking.user` 仍决定写入的
`viking://user/<user>/huan-agent` 路径，两者要保持一致。

## 6. 设计取舍

- **本地优先**：本地 JSONL 始终是真源，远端是镜像。这样 OpenViking 故障不会让对话
  丢记忆，代价是两边可能短暂不一致（以本地为准）。
- **提交与直写并行**：VLM 抽取质量更高（结构化的 preferences / entities 等），但它
  依赖模型开通；直写日志保证「一定存下来」。两者都失败才会真的丢，而那时本地仍有。
- **只写一个子树**：所有写入收敛在 `viking://user/<user>/huan-agent` 下，这样同一台
  OpenViking 上多个项目的检索不会互相污染。
- **不用 CLI**：直接走 HTTP。CLI 是给人用的，进程里起子进程解析文本输出既慢又脆；
  HTTP 之外还复用同一份认证与错误分类（`openviking.IsUnavailable`）。
