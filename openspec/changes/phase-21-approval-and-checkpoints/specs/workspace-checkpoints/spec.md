# Capability: workspace-checkpoints

## Purpose

让一轮里被 agent 改过的文件**回得去**。它不追求"存下所有变化"，追求的是：当一次长任务跑歪了，
人能把它对工作区造成的影响撤销掉，而不用依赖 git、也不用指望模型自己记得先 commit。

## Scope

- `internal/checkpoint` — 前像捕获、manifest、恢复、保留策略。
- `internal/tool/builtin` — 文件工具在改动之前留前像。
- `internal/server` — 列出与回滚端点、Web 上的「回退这一轮」。
- `cmd/huan-agent` — `checkpoint list|show|rollback`。

## Requirements (MUST)

1. **copy-on-write，不是轮次开始时猜。** 前像 MUST 在某个文件**第一次被本轮修改之前**捕获。
   轮次开始时并不知道哪些文件会被改，因此"轮次开头做一次快照"是不可能的；同一轮内对同一文件的
   后续修改 MUST NOT 覆盖已记录的前像（第一次的内容才是一轮开始时的内容）。
2. **覆盖范围必须诚实。** 检查点 MUST 覆盖经由文件工具（`write_file` / `edit_file` /
   `apply_patch`）的改动，MUST NOT 声称覆盖 `bash` 造成的改动（`rm`、`sed -i`、构建产物等）。
   文档 MUST 写明这一点。为弥补，轮次开始时 MUST 记录 `git HEAD` 与 `git status --porcelain`
   （工作区是 git 仓库时），使人能看到 bash 动了什么。
3. **新建与删除都要能还原。** 前像 MUST 记录"该路径当时不存在"这一状态，使恢复时能删除本轮新建
   的文件。文件模式 MUST 一并记录与恢复。
4. **恢复是原子的。** 每个文件的写回 MUST 用临时文件 + rename，MUST NOT 留下半写的文件。
   路径 MUST 经工作区沙箱解析，越界路径 MUST 一律拒绝。
5. **冲突默认拒绝覆盖。** 若磁盘上的文件在检查点记录之后又被改动（哈希不符）且不是本 agent 的
   写入所致，恢复 MUST 默认拒绝该文件并列出冲突清单，MUST 需要显式 `force` 才覆盖。理由：一次
   "回滚"把人的新工作冲掉，比不提供回滚更糟。
6. **回滚本身可撤销。** 恢复开始之前 MUST 对当前状态再捕获一次检查点，使刚刚的回滚也能被回滚。
7. **有界。** 检查点 MUST 有保留上限（轮数默认 20、总字节默认 512 MB），超出时按时间从旧到新
   淘汰并记日志说明淘汰了什么。MUST NOT 无限增长。
8. **报告而非沉默。** 恢复 MUST 返回报告：恢复的文件数、删除的文件数、跳过的文件数、冲突清单，
   以及被撤销的那次检查点的位置。CLI 与 Web 都 MUST 显示它。
9. **不干扰正常路径。** 捕获前像 MUST NOT 让写入失败或变慢到可感知（前像是读取 + 复制，不是
   扫描整个仓库）。只读工作区下 MUST NOT 产生检查点（那里没有写工具）。
10. **并发安全。** 同一轮内多个文件被并发修改时 MUST NOT 丢失任何一条前像记录（Phase 22 的工具
    并发会让这一点从"理论问题"变成"必然发生"）。

## 存储布局

```
data/checkpoints/<session_id>/<turn_index>/
  manifest.json          # 版本号 + 轮次元信息 + 文件条目
  files/<hash>           # 前像内容，按 sha256 命名（同一内容只存一份）
```

`manifest.json` 条目：

```json
{ "path": "internal/foo/bar.go", "existed": true, "mode": 420,
  "sha256": "…", "size": 1234, "captured_at": "2026-09-17T10:00:00Z" }
```

轮次元信息含 `turn_index`、`started_at`、`git_head`（可为 `null`）、`git_status`（可为 `null`）。

## API

- `GET /api/chat/sessions/{id}/turns/{tid}/checkpoint` — 文件清单与统计；不存在 → 404。
- `POST /api/chat/sessions/{id}/turns/{tid}/rollback` — 恢复；成功 200 带报告；存在冲突且未
  `force` → 409 带冲突清单。
- CLI：`huan-agent checkpoint list`、`checkpoint show <session> <turn>`、
  `checkpoint rollback <session> <turn> [--force] [--json]`，与 HTTP 走同一个 `Restore`。

## 配置

```yaml
tools:
  checkpoint:
    enable: true
    dir: ""                # 空 = 数据库文件旁的 checkpoints 目录
    keep_turns: 20
    max_total_mb: 512
```

## Non-goals

- **bash 的文件级检查点。** 一条 `rm -rf` 绕过文件工具直接改盘；要覆盖它就得整仓快照，成本与
  收益不成比例。诚实的替代是记录轮次前后的 `git status`。
- 检查点的"分支 / 命名 / 手工管理"（它不是版本控制，只是最近几轮的回退点）。
- 跨会话或跨机器的恢复（检查点属于一个会话的一个轮次，随会话删除而删除）。
- 自动回滚：什么时候该退回去由人决定，本能力只把这件事变成一次点击。
- 二进制大文件的高效存储（有 `max_total_mb` 兜底；单个超大文件按上限跳过并记录）。
