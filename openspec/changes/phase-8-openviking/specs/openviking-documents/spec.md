# Capability: openviking-documents

## Purpose

让「文档」有确定归宿：会话产出的文档/报告与工作区文件都保存进 OpenViking 的
`viking://` 资源库，可被语义检索，也随时能从磁盘状态核对「哪些已入库」。

## Scope

- `internal/documents` — 同步引擎（会话文档 + 工作区增量同步 + 状态文件）。
- `internal/tool/builtin/openviking.go` — `save_document` 工具。
- `cmd/huan-agent` — CLI 与定时同步。
- `internal/server` + `web/src` — 状态、清单与一键同步。
- `internal/config` — `openviking.documents.*`。

## Requirements (MUST)

1. **确定性 URI。** 会话文档 MUST 写到
   `<root_uri>/documents/<YYYY>/<MM>/<slug>-<hash8>.md`，`slug` 由标题派生且 MUST
   只含安全字符（非 ASCII 标题 MUST 退化为 `doc`），`hash8` 取自内容哈希以保证同名
   不同内容不互相覆盖。
2. **可检索即可用。** 文档写入 MUST 使用 `content/write` 且等待索引完成
   （`wait=true`），使 `Save` 返回后 `find` MUST 已能命中；返回值 MUST 是写入的
   `viking://` URI。
3. **frontmatter。** 文档 MUST 带 YAML frontmatter：`title`、`tags`、`source`、
   `created_at`、`generator: huan-agent`，便于人工在 openviking 侧识别来源。
4. **工作区增量。** `SyncWorkspace` MUST 按 `include` / `exclude` glob 过滤，
   MUST 跳过 `max_file_kb` 以上的文件，MUST 只上传内容哈希（sha256）发生变化的文件；
   未变化 MUST 计为 `Unchanged` 且不产生远端写入。
5. **二进制。** 非文本文件 MUST 按 `binary_mode` 处理：`skip` 计入 `Skipped` 并给出
   原因；`upload` MUST 走 `temp_upload` + `add_resource`（HTTP 服务不允许直接使用
   宿主路径，MUST NOT 传 `path`）。
6. **失败不中断。** 单个文件失败 MUST NOT 终止整轮同步；MUST 收集进 `Report.Errors`
   （含相对路径与原因）并继续处理其余文件。
7. **状态文件。** 同步状态 MUST 落在 `state_path`（JSON：uri → path / hash / size /
   synced_at），损坏或缺失时 MUST 退化为全量同步而不是报错；`--full` MUST 忽略既有
   状态强制重传。
8. **清单。** MUST 提供 `Documents()` 列出已同步条目（路径、URI、大小、时间），供
   控制台与 CLI 展示。
9. **启用即降级。** openviking 未启用或不可用时：`SyncWorkspace` MUST 返回明确的
   可读错误/报告，MUST NOT panic，MUST NOT 破坏状态文件。
10. **可测。** 引擎 MUST 能用假客户端测试：增量、排除、超大跳过、二进制策略、
    失败收集、状态往返。

## Non-goals

- 双向同步（openviking → 本地）与删除传播。
- 项目知识库（`docs/`、`configs/skills`）的一次性导入。
- 飞书/Web 上传附件的自动入库。
- 文件级 watch 订阅（openviking 的 `list_watches` 能力本次不用）。
