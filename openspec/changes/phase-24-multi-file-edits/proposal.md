# Phase 24 — 跨文件编辑：`apply_patch` 与符号重命名

## Summary

今天写代码的工具只有两个：`write_file`（整文件）与 `edit_file`（单文件、字符串级）。

`edit_file` 要求 `old_string` 在文件内唯一出现，歧义时直接报错
（`internal/tool/builtin/files.go:450-455`）：

```
edit_file: old_string appears %d times in %s, so this edit is ambiguous;
include more surrounding context to make it unique or set replace_all=true ...
```

这个设计本身是对的（拒绝猜测比猜错好）。问题在于它是**单文件、字符串级**的：重命名一个符号要
N 个文件逐个 `edit_file`，而每一次调用都是一次独立的模型往返。更糟的是**中间态**：改到第三个
文件时如果发现漏了一个调用点，仓库正处在半个编译不过的状态，而前两个文件的改动已经落盘。

本变更新增 `apply_patch`（多文件、**全有或全无**的批量编辑）与 `rename_symbol`（用 Phase 20 的
语言服务器算出 `WorkspaceEdit`，再用同一个批量应用器落地）。

## Why

- **全有或全无是这个工具的全部意义。** 一次跨文件重构要么整体成立，要么完全不动。做不到这件事的
  批量编辑只是"循环调用 `edit_file`"，那已经把问题留在了原地：半途失败留下一个损坏的仓库，而且
  模型得自己记得回滚。
- **两种输入形态服务的其实是两件事。** 模型**自己写** diff 时，`@@ -a,b +c,d @@` 的行数是它最容易
  数错的东西（错了就是整个 hunk 被拒或者被贴到错位置）。所以主形态是**结构化操作列表**：每个
  `{path, edits:[{old_string, new_string, replace_all?}]}` 直接复用 `edit_file` 已经被验证过的
  "唯一匹配"规则，没有行数算术，天然原子。而**应用别人给的 diff**（issue 里的补丁、`git diff`
  的输出、用户贴的一段 patch）是另一个真实场景——这种情况下 diff 是输入而不是创作，模型只需要
  按行匹配，出错的余地小得多。两种都支持，但主次分明。
- **重命名的正确答案不是正则。** `grep` 会把注释、字符串、另一个包里的同名方法一起改掉。
  `textDocument/rename` 返回的是编译器眼里的引用集合，那才是"跨文件符号重命名"该有的依据。
  这也是 `CAPABILITY-GAPS.md` 里与 Claude Code / Cursor 类工具差距最大的一项。
- **在检查点之后落地。** 多文件编辑是目前最可能"改到一半发现方向不对"的操作，而它此前是唯一
  没有回退路径的写操作。Phase 21 的检查点必须先到位，否则这个工具会放大它要解决的问题。
- **审批需要看到它要做什么。** 一个"将修改 7 个文件"的审批卡片如果只显示一个数字，人就无法判断。
  所以批量编辑 MUST 能**无副作用地**算出计划（文件清单 + 行数增减 + 有界预览），供 `dry_run`
  与 Phase 21 的审批预览共用同一份实现。

## What Changes

### ADDED

1. **`internal/edit`：两阶段批量应用器**
   - **阶段一（纯计算，无副作用）**：对每个操作在内存里用当前磁盘内容做匹配，算出每个文件的
     结果内容与行数增减。任何一条不成立（匹配 0 次、多次且未 `replace_all`、路径越界、超出
     `max_write_mb`、目标不是文本文件）→ 整个请求失败，**磁盘一字未动**，错误里指明是哪个文件
     的哪一条。
   - **阶段二（落盘）**：逐文件原子写（临时文件 + rename，与既有 `write_file` 相同）。若某次
     写入失败，MUST 用阶段一保存的原内容把**已经写过的文件回滚**，并在报告里说明回滚了什么。
   - 这是本变更的核心不变量：**要么全部生效，要么磁盘与本轮开始时逐字节一致。**
   - 落盘之前 MUST 为每个将被修改的文件取一次检查点（Phase 21），使这次批量编辑本身也可以被
     回退——两阶段保证的是"这次调用内部"的一致性，检查点保证的是"跨调用"的可回退性，两者不是
     同一件事。

2. **`apply_patch` 工具**
   - 主形态（结构化）：
     ```json
     { "operations": [
         { "path": "internal/a.go", "edits": [ { "old_string": "…", "new_string": "…" } ] },
         { "path": "internal/b.go", "edits": [ { "old_string": "…", "new_string": "…", "replace_all": true } ] }
     ] }
     ```
     匹配规则**与 `edit_file` 完全一致**（唯一出现，或 `replace_all`）——模型不需要学第二套
     心智模型，且两个工具的失败信息可以一致。
   - 次形态（unified diff）：`{ "patch": "diff --git …" }`，用于应用别人给的补丁。MUST 支持
     `diff --git`、`---/+++`、`@@` 三种头，MUST 做上下文校验（上下文不匹配则整个补丁失败），
     MUST NOT 把行数错误当成"尽力而为"的理由悄悄贴错位置。
   - `dry_run: true`：只返回计划，不落盘。返回值与真实执行**同一份结构**（文件清单、
     `+N -M`、有界预览、以及"如果没有干这步会怎样"的一致性说明），使模型可以先给人看再执行。
   - 返回值：逐文件 `+N -M` 与总计数；失败时 MUST 说明**没有任何文件被改动**。
   - 标 `CapWrite` + `Serial`（写即屏障）。
   - 与 Phase 20 的联动：成功后对每个被改文件附诊断（有界），与 `write_file` / `edit_file` 同一
     个 `Augmenter`。

3. **`rename_symbol` 工具**
   - 参数：`path`、`line`、`column`、`new_name`。
   - 流程：`textDocument/prepareRename` 校验可重命名 → `textDocument/rename` 拿 `WorkspaceEdit`
     → 转成 `internal/edit` 的操作列表 → 用**同一个两阶段应用器与同一个检查点**落地。
   - MUST 支持 `documentChanges`（带版本的编辑）与朴素的 `changes` 两种形状。
   - **越界即拒绝**：`WorkspaceEdit` 里只要有一个目标路径落在工作区之外（例如指向模块缓存或
     vendored 依赖），MUST 整个拒绝并列出越界路径，MUST NOT 只改工作区内的一部分——半次重命名
     比重命名失败更糟。
   - 标 `CapWrite` + `Serial`；只读工作区下 MUST NOT 注册。
   - 没有可用语言服务器时 MUST NOT 注册（与 Phase 20 的工具同一条件）。
   - 失败时返回可读原因（"新名字与已有符号冲突"、"该位置不可重命名"），作为工具观察值交回模型。

4. **提示词与文档**
   - 工具描述写明分工：改一处 → `edit_file`；改多处且要么全成要么全不动 → `apply_patch`；
     改一个符号的所有引用 → `rename_symbol`（而不是 `grep` + 批量 `apply_patch`）。
   - `docs/tools.md`：两个工具、两种输入形态的主次与理由、原子性保证、与检查点的关系。
   - `CAPABILITY-GAPS.md` 第一条里"没有 patch、没有多文件原子编辑"这一句在本变更后不再成立。

### 明确不做（本阶段）

- **不废弃 `edit_file`。** 单文件单处修改是最常见的操作，它的参数面最小、最容易用对。批量工具
  不是它的替代，而是它的超集场景。
- **不做"尽力而为"的补丁应用**（部分 hunk 失败就跳过）。这与本变更的核心不变量直接冲突。
- **不做创建/删除文件的操作形态。** 本阶段的批量编辑只改**已存在的文本文件**；新建用
  `write_file`、删除用 `bash`（走审批）。把增删折进同一个工具会让"全有或全无"的语义变复杂
  （删除文件的"回滚"是恢复内容，创建文件的"回滚"是删除文件，两者都可行但需要单独的检查点
  语义）。这是有意的范围收窄。
- **不做二进制或超大文件。** 超出 `max_write_mb` 或非文本 MUST 被拒绝。
- **不做 `git apply` / git 集成。** 补丁应用不依赖 git；工作区不是仓库时也必须能用。
- **不做跨工作区的编辑。** 多个工作区（`tools.workspaces_dir`）之间不互相修改。
- **不做重命名的自动重试。** 语言服务器报冲突就是冲突，模型应该换个名字，而不是让工具猜。

## Impact

- 新增包：`internal/edit`。
- 新增文件：`internal/edit/{apply,plan,patch,workspaceedit}.go`、
  `internal/tool/builtin/patch.go`（`apply_patch` + `rename_symbol`）。
- 改动：`cmd/huan-agent/chat.go`（注册、并发声明）、`internal/tool/builtin/files.go`（把 `edit_file`
  的匹配规则抽成 `internal/edit` 的共享实现，避免两套规则漂移）、`internal/lsp`（暴露 rename 相关
  请求）、`docs/tools.md`。
- 依赖：**Phase 21 必须先落地**（检查点是这里唯一的回退路径）；`rename_symbol` 依赖 Phase 20；
  并发声明依赖 Phase 22 的机制。
- 兼容性：纯增量，`edit_file` 行为不变（它的匹配规则被抽出去复用，但语义与错误信息保持一致——
  这一点 MUST 有测试守住，否则抽共享实现会静默改变既有工具的行为）。
- 风险与对策：批量写是最容易造成数据损坏的操作。对策是"阶段一绝不落盘 + 阶段二失败即回滚 +
  落盘前取检查点"三层，外加一个测试直接断言"阶段一失败后磁盘哈希不变"。
