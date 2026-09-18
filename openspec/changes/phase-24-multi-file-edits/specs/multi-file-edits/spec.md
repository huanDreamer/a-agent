# Capability: multi-file-edits

## Purpose

让跨文件的编辑**要么整体成立，要么完全不动**。一次重命名或一次重构涉及 N 个文件时，中间态
（"改了三个，第四个发现漏了"）是最坏的结果：仓库编译不过，而前几个文件的改动已经落盘。本能力
把这类操作变成一个原子动作，并且把"改一个符号的所有引用"从正则匹配换成编译器的引用集合。

## Scope

- `internal/edit` — 两阶段批量应用器（纯计算 + 落盘回滚）、unified diff 解析、`WorkspaceEdit` 转换。
- `internal/tool/builtin` — `apply_patch` 与 `rename_symbol`。
- `internal/lsp` — rename 相关的请求（`prepareRename` / `rename`）。
- `docs/` — 三种编辑工具的分工与原子性保证。

## Requirements (MUST)

1. **两阶段，且第一阶段绝不落盘。** 批量编辑 MUST 先在一个纯计算阶段对所有操作做完整校验并算出
   每个文件的结果内容，任何一条不成立就整体失败，此时磁盘 MUST 与请求前逐字节一致。MUST NOT
   边校验边写。
2. **失败即回滚。** 落盘阶段若某个文件写失败，MUST 用第一阶段保存的原内容回滚**已经写入**的文件，
   并在报告里说明回滚了什么。这是核心不变量：**要么全部生效，要么磁盘与本轮开始时逐字节一致。**
3. **落盘前取检查点。** 在修改任何文件之前 MUST 为每个将改文件取一次检查点（`workspace-checkpoints`）。
   两阶段保证的是"这次调用内部"的一致性，检查点保证的是"跨调用"的可回退性，两者缺一不可。
4. **主形态复用 `edit_file` 的匹配规则。** 结构化形态（`operations[].edits[]`）的匹配语义 MUST 与
   `edit_file` 完全一致（唯一出现，或显式 `replace_all`），MUST 从同一份实现出发——模型不需要
   学第二套心智模型，两个工具的失败信息也必须同风格。
5. **明确分工。** 改一处用 `edit_file`；改多处且要求全有或全无用 `apply_patch`；改一个符号的
   全部引用用 `rename_symbol`。工具描述 MUST 写明这个分工，否则模型会用 `grep` + 批量替换去
   做重命名——那正是会改到注释与字符串里的做法。
6. **dry_run 无副作用。** MUST 支持只返回计划（文件清单、每文件 `+N -M`、有界预览）而不落盘。
   返回值 MUST 与真实执行使用同一份结构，使"先给人看再执行"成立。
7. **审批预览共用同一实现。** `apply_patch` MUST 暴露一个无副作用的计划计算，供 `dry_run` 与
   审批卡片（`tool-approval`）的预览共用。一个"将修改 7 个文件"的卡片如果不显示是哪些文件、
   改了多少行，人无法判断自己在批什么。
8. **重命名用引用集合，不用正则。** `rename_symbol` MUST 通过语言服务器的 `prepareRename` +
   `rename` 得到 `WorkspaceEdit`（支持 `documentChanges` 与 `changes` 两种形状），MUST NOT 用
   字符串搜索模拟。
9. **越界即整体拒绝。** `WorkspaceEdit`（或补丁）中只要有一个目标路径落在工作区之外，MUST 整个
   拒绝并列出越界路径，MUST NOT 只应用工作区内的一部分——半次重命名比失败更糟。
10. **有界。** 路径 MUST 经工作区沙箱解析；非文本文件与超过 `max_write_mb` 的文件 MUST 被拒绝；
    同一路径在一次操作里出现两次 MUST 被拒绝。
11. **能力与顺序。** `apply_patch` 与 `rename_symbol` MUST 标 `CapWrite` + `Serial`（写即屏障）
    并受审批闸门约束。只读工作区下 `rename_symbol` MUST NOT 注册；没有可用语言服务器时
    `rename_symbol` MUST NOT 注册。
12. **改完给反馈。** 成功后 MUST 对每个被改文件附上诊断（有界、总条数上限），与 `write_file` /
    `edit_file` 走同一个 `edit-feedback` 实现。

## `apply_patch` 的两种输入形态

**主形态：结构化操作列表**（模型自己创作改动时的形态）

```json
{ "operations": [
  { "path": "internal/a.go", "edits": [ { "old_string": "func Foo(", "new_string": "func Bar(" } ] },
  { "path": "internal/b.go", "edits": [ { "old_string": "Foo(", "new_string": "Bar(", "replace_all": true } ] }
] }
```

理由是**行数算术**：模型自己写 unified diff 时，`@@ -a,b +c,d @@` 里最容易错的就是 `b` / `d`。
结构化形态没有这个数字，匹配规则又和 `edit_file` 一致，所以它更可靠。

**次形态：unified diff**（应用别人给的补丁时的形态）

```json
{ "patch": "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,3 @@\n…" }
```

用于 issue 里的补丁、`git diff` 的输出、用户贴的一段 patch。这种情况下 diff 是**输入**而不是
创作，模型只需按行匹配，出错余地小得多。MUST 用上下文行定位 hunk；`@@` 声明的行数只作提示，
与实际不符时以上下文为准并记一条 warn，MUST NOT 静默贴到错位置。上下文不匹配则整个补丁失败。

**本阶段不支持的形态**：创建新文件、删除文件的 diff。它们 MUST 被明确拒绝并指出替代
（`write_file` / `bash` + 审批）——把增删折进来会让"全有或全无"的语义变复杂（删文件的回滚是
恢复内容，建文件的回滚是删除文件，两者需要各自的检查点语义）。

## 返回值

```
应用 3 个文件，+42 -17

internal/a.go   +12 -4
internal/b.go   +30 -13
internal/c.go    +0 -0    （内容未变，已跳过）
```

- 失败时 MUST 明确写出"没有任何文件被改动"。
- 失败信息 MUST 指明文件与第几条 edit（或 diff 的 hunk 序号）以及下一步怎么做。

## `rename_symbol`

| 参数 | 语义 |
|---|---|
| `path` | 符号所在文件（工作区相对） |
| `line` / `column` | 1-based，与 `read_file` 的行号口径一致 |
| `new_name` | 新名字 |

流程：`prepareRename` 校验位置可重命名 → `rename` 取 `WorkspaceEdit` → 转成 `internal/edit`
的操作列表 → **复用同一个两阶段应用器与同一个检查点**（MUST NOT 新写一套落盘逻辑）。

失败原因 MUST 可读并作为工具观察值交回模型：位置不可重命名、新名字与已有符号冲突、语言服务器
不支持 rename、目标路径越界。

## Non-goals

- 废弃 `edit_file`：单文件单处修改仍是最常见、参数面最小的操作。
- "尽力而为"的补丁应用（部分 hunk 失败就跳过）：与第 1、2 条直接冲突。
- 创建 / 删除文件的操作形态。
- 二进制与超大文件。
- 依赖 git（`git apply`）：工作区不是仓库时本能力也必须可用。
- 跨工作区编辑。
- 重命名的自动重试或自动改名：冲突就是冲突，换个名字是模型的判断。
