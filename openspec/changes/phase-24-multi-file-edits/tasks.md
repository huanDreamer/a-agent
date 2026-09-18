# Phase 24 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。


## 实施记录（2026-09-18）

**已完成并验证**：第 1–4 节（共享匹配规则、两阶段应用器、`apply_patch` 结构化形态、接线与文档）
与**第 6 节 `rename_symbol`**（含真 gopls 集成测试与端到端）。**未完成**：第 5 节（unified diff
形态）、第 7 节的部分文档条目，以及重命名在超大仓库里的完整性说明 —— 见文末。

### 第 1 节：共享匹配规则

- [x] `edit.Match(content, Edit) (result, count, err)`：与 `edit_file` 逐条一致（空 old 拒绝、
      0 次 / 多次 / `replace_all`、多字节、空白敏感），有 7 个用例的表驱动测试
- [x] `edit_file` 的语义**未变**：本次没有改 `fileEdit` 的匹配代码（共享实现是新增的，
      `edit_file` 仍走自己那段），所以"错误信息与语义逐字一致"这一条以**不修改**满足；
      两者的规则由测试同时钉住（见下方"未完成"里的一条：真正让 `edit_file` 调 `Match` 还没做）

### 第 2 节：阶段一（纯计算）

- [x] `Operation` / `Edit` / `PlanBatch`：逐文件在内存里算出结果与 `+N -M`
- [x] 校验（任一不成立即整体失败且**不碰磁盘**）：路径越界、目录、二进制、没有 edits、
      `operations` 为空、同一路径两次、歧义、找不到、超过单文件上限、只读工作区、文件数上限
- [x] 错误里指明**哪个文件的哪一条 edit**，并给出下一步（"用 read_file 重新读一下这个文件…"）
- [x] `Plan.Preview(n)`：有界 diff，`dry_run` 与审批预览共用同一份实现
- [x] 测试：全部校验分支各一条；**关键断言**是每次拒绝后 `fingerprint(root)` 与调用前逐字节相同
- [x] 重复路径的检查放在 `os.Stat` **之前**：它是请求形状的问题，不是文件的问题

### 第 3 节：阶段二（落盘与回滚）

- [x] 落盘前为每个将改文件取检查点，**且在任何写入之前**（有测试断言捕获到的内容是改动前的）
- [x] 逐文件原子写：`workspace.WriteFileAtomic`（临时文件 + rename + 模式保留 + fsync）
- [x] 中途失败：用阶段一保存的原内容回滚已写入的文件，报告里 `RolledBack` 说明回滚了什么，
      错误信息明确写出"磁盘与你调用前一致，没有任何文件被改动"
- [x] **回滚本身失败**是最坏结果：错误里明说"磁盘现在处于混合状态，请用 git status 检查"，
      而不是假装成功
- [x] `Apply(ctx, ws, plan, opts) (Report, error)`，含 `Files` / `Added` / `Removed` / `RolledBack`
- [x] 测试：全成功（三文件）、**注入写失败**（只读父目录）后前两个文件被回滚且
      `fingerprint` 与调用前逐字节相同

### 四、`apply_patch` 结构化形态与接线

- [x] 输入 `operations: [{ path, edits: [{ old_string, new_string, replace_all? }] }]` + `dry_run`
- [x] 复用第 2/3 节的两阶段；`dry_run` 只返回计划与预览，不落盘（有测试）
- [x] 返回值：逐文件 `+N -M` + 总计 + `applied` + `summary`
- [x] 标 `CapWrite` + `Serial`，并**经过与其他写工具同一套装饰器**（审批闸门 → 检查点 guard →
      LSP 诊断反馈），所以一次多文件改动在审批卡片上有预览，在回滚里有前像，改完有诊断
- [x] 模型-facing 描述写明分工（`edit_file` / `apply_patch` / `rename_symbol`）与
      "不能创建/删除文件"
- [x] `workspaceBoundToolNames` 纳入 `apply_patch`（该列表的守卫测试直接抓到过遗漏）
- [x] 检查点按**调用所在轮次**归档：`ApplyOptions.TurnFrom` 从 context 读轮次，所以一个在启动时
      构建的工具也能把前像记到正确的轮次

### 测试与验证

- [x] `go test ./... -count=1` 全绿；`go vet ./...` 干净；`make build` 通过
- [x] `internal/edit` 新增 9 个测试、`internal/tool/builtin` 新增 6 个
- [x] 端到端（真模型）：`huan-agent run "把 Add 改名成 Sum，包括所有调用点和测试，用 apply_patch 一次完成"`
      → 模型先 `glob`+`grep`、**并行读 3 个文件**（Phase 22 的并发在真实运行里生效）、
      然后**一次 `apply_patch` 改 3 个文件（+5 −5）**、再用 `bash` 验证；
      改完 `/tmp/p24ws` 的 `go build ./...` 与 `go test ./...` 都通过，三个文件（含注释与测试）全部改名

### 实施中由守卫测试抓到的两个问题

**问题 1：`workspaceBoundToolNames` 漏了 `apply_patch`。** 那个列表决定哪些工具在工作区切换时
被重新绑定/撤销，漏掉意味着换工作区之后 `apply_patch` 还指向旧工作区。守卫测试
`TestWorkspaceBoundNamesCoverTheBuilders` 直接抓到（"the builder produces \"apply_patch\", which
workspaceBoundToolNames does not list"）。

**问题 2（我自己的操作失误，值得记下）**：把 `fileWriteAtomic` 迁到 `internal/workspace` 时，
我的脚本按"注释在前、函数在后"的顺序切分，但那个文件里 `fileEdit` 在 `fileRealPath` **之前**，
于是 `src[end:]` 把一大段重复插了回来（7174 字节）。`go build` 立刻报
`fileEdit redeclared`，我按第二个副本的首尾把重复段删掉后 `go vet`/测试全绿。
教训：**用两个独立锚点切分文件时要先确认它们的先后关系**，否则 `src[:a] + x + src[b:]` 会悄悄
复制一整段。

### 未完成的部分（下一轮）

1. **第 5 节 unified diff 形态**：`diff --git` / `@@` 解析、上下文行定位、新文件/删除文件 diff 的
   明确拒绝。`ApplyPatchInput.Patch` 字段已在 schema 里，且拒绝"两者都为空"——但传 patch 时目前
   不会解析，需要补上解析器（或在解析器落地前明确拒绝）。
2. ~~第 6 节 `rename_symbol`~~ **已完成**：`PrepareRename`/`Rename`、两种形状的 `WorkspaceEdit`、
   位置化编辑、越界整体拒绝、集成测试与端到端，见下方第 6 节记录。
3. **让 `edit_file` 真正调用 `edit.Match`**：本次以"不修改 `edit_file`"满足"语义不变"，但共享实现
   的意义在于只有一份，所以下一步应该把 `fileEdit` 的匹配段换成 `Match` 调用，并保留一个
   `edit_file` 层端到端测试。
4. 文档：`internal/prompt/` 的工具指引（改符号用 `rename_symbol`）、`docs/tools.md` 里
   `apply_patch` 的 diff 形态一段。


## 第 5 节实施记录：unified diff 形态

（说明：这一节的原始条目在早先追加记录时被误删，本节把它们补回并逐条勾选。）

- [x] 输入 `patch: "<unified diff>"`（`ApplyPatchInput.Patch`）
- [x] 解析 `diff --git` / `---` / `+++` / `@@` 三种头；容忍 `\ No newline at end of file`
      与 `index` / `new file mode` / `similarity index` 等元数据行
- [x] **用上下文行匹配定位 hunk**：每个 hunk 转成 `Edit{Old: 上下文+删除行, New: 上下文+新增行}`，
      所以定位靠内容而不是行号。`@@` 里声明的行数**只作提示**，与实际不符时记一条 warning
      （随返回值一起给模型看），MUST NOT 静默贴错位置
- [x] 上下文不匹配 → 整个补丁失败（由 `PlanBatch` 报错，错误里指明文件与第几条 edit）
- [x] 新文件 / 删除文件 / 重命名 MUST 被明确拒绝，并说明用 `write_file` / `bash`（本阶段不支持）
- [x] 与结构化形态共用阶段一/阶段二（解析后转成 `[]Operation`），**所以原子性保证不因输入形态而变**
- [x] 解析不出任何改动 / 完全不是 diff → 明确报错，而不是当成"没有改动"静默通过
- [x] 测试 11 个：真 `git diff` 往返、**行号故意写错仍按上下文匹配**、行数声明不符只 warning、
      上下文不匹配被拒、新建/删除/重命名/改路径四种拒绝、多 hunk、文件末尾无换行、
      路径穿越被拒、六种非 diff 输入被拒、**diff 形态同样是全有或全无**（磁盘指纹不变），
      工具层 3 个（接受 diff、diff 的 dry_run、拒绝创建文件且磁盘不变）

设计上值得记下的一点：**解析器故意不信 `@@` 的行号**。理由是行号会漂移（有人在上面加两行就变了），
而按行号落点的错法是**静默**的——差三行贴上去，文件编译不过，而模型看不出为什么。按上下文定位的
代价是"上下文对不上就拒绝"，这对 agent 是正确的取舍：被拒的补丁是模型能处理的消息，贴错的补丁是
一次调试。

## 第 6 节实施记录：`rename_symbol`

- [x] `internal/lsp/rename.go`：`PrepareRename`（可重命名性，兼容 range 与带 placeholder 两种返回）
      与 `Rename`；`WorkspaceEdit` 支持 **`documentChanges` 与 `changes` 两种形状**
      （同时出现时以 `documentChanges` 为准，否则每次改动会被应用两次）
- [x] `WorkspaceEdit.Flatten()` 按路径排序，所以报告与检查点顺序可复现（map 迭代是随机的）
- [x] `edit.FromWorkspaceEdit`：**越界即整体拒绝**，错误里列出**所有**越界路径，并说明为什么
      不能只应用工作区内的一部分
- [x] `edit.Workspace.PlanWorkspaceEdit`：转换与规划一体，工具不必自己持有沙箱
- [x] `rename_symbol` 工具：参数校验（1-based 位置）、`dry_run`、引用计数与摘要、
      模型-facing 描述写明分工
- [x] 接线：只读工作区或没有语言服务器时**不注册**（有理由，见下）；经同一套装饰器
      （审批闸门 → 检查点 → 诊断反馈）
- [x] `workspaceBoundToolNames` 纳入 `rename_symbol`（守卫测试直接抓到遗漏）
- [x] 测试：`internal/edit` 新增 8 个（两种形状、越界整体拒绝、非 file URI、越界区间、空区间、
      结束在开始之前、UTF-16 列、跨行 span、位置化编辑保整文件、过期位置被拒、底部向上批量应用），
      `internal/tool/builtin` 新增 5 个（跨文件、dry_run、参数校验、服务器拒绝、描述分工）
- [x] 集成测试（真 gopls，`//go:build integration`）：重命名覆盖定义与调用点、
      **不碰含同名字符串的字面量**、改完 `go build ./...` 通过；关键字被 `PrepareRename` 拒绝
- [x] 端到端（真模型 + 真 gopls）：一次 `rename_symbol` 改两个文件、字符串字面量保持原样、
      `go build ./...` 通过、`tool_failures: 0`

### 实施中由测试与真机抓到的三个真实问题

**问题 1：重命名的多条 edit 老 old_string 相同，文本匹配会判为"有歧义"（设计问题，已修）。**
一次重命名会产出 N 条 old_string 都是同一个标识符的编辑，按文本匹配处理时第二条就会因
"old_string 出现了 2 次"被拒——而**改在哪里恰恰是语言服务器已经告诉我们、而文本搜索不可能知道的事**。
修法是给 `Edit` 增加 `Position *Span`：位置化编辑按坐标落点，并在落点校验文本（文件在服务器
编辑的版本之后变过就报错而不是覆盖）。位置化编辑**从文件底部往上应用**，所以一条编辑不会让后面
还没用的坐标失效——这是"一串位置化编辑"能成为良定义批次而不是排序谜题的原因。

**问题 2：`editError` 把所有非歧义错误都报成"old_string 没有找到"（真实缺陷，已修）。**
位置化编辑失败时（过期位置、区间越界）得到的是"用 read_file 重新读一下这个文件"——把模型指向
一个不是问题的地方。改成只对两个哨兵错误用那两段文案，其余原样透传并包裹。
它掩盖了问题 3，所以这条本身也是有价值的。

**问题 3：位置化编辑把文件开头截掉了（真实缺陷，已修）。**
同一行内的替换我漏了 `lines[:StartLine]`，于是第一次编辑之后文件只剩后半段——而它暴露出来的
现象是**下一条**编辑报"位置超出文件范围（文件有 3 行）"，离原因很远。修好后专门加了一个测试
直接钉住"位置化编辑必须保留整个文件"。

**问题 4（真机发现，不是测试）：工具传工作区相对路径，语言服务器要绝对路径。**
第一次端到端跑出来的错误是 `lsp: read calc.go: open calc.go: no such file or directory`——
模型按 schema 传 `path: "calc.go"`，而 LSP 客户端直接 `os.ReadFile`。修法是在
`workspaceRenamer.Rename` 里用 `ws.Resolve` 转换（顺带完成沙箱约束：语言服务器的请求不能成为
读取工作区之外的路）。修完 `tool_failures` 从 1 变成 0。

**问题 5（真机发现）：语言服务器只知道它被告知过的文档。**
第一次重命名只改了定义所在的文件，调用点没动——因为只有目标文件被 `didOpen` 过。
"静默漏掉其他文件"是这个工具最坏的失败方式：仓库编译不过，而它看起来成功了。
修法是在提问之前把同目录下的同扩展名文件（上限 64）都打开，并在注释里写明这个界限——
超大仓库里仍可能需要别的工具先把相关文件打开。

**问题 6（顺带修掉的既有缺陷）：`gopls_integration_test.go` 自 Phase 20 起就无法编译。**
Phase 20 把 `FileDiagnostics` 的第二个返回值从 `bool` 改成三值 `Diagnosed` 时，没有更新
`//go:build integration` 那个文件——`!settled` 对一个 int 取反，只在带 tag 时才编译，所以常规
`go test` 一直没发现。修好后那 5 个既有集成测试重新可跑且全部通过。**教训：带 build tag 的测试
不会在常规 `go test ./...` 里编译，所以它们需要在改动接口时被显式检查**（本次是因为新增集成测试
才碰到）。

- [~] `golangci-lint run`：工具已装好并跑过，**本阶段新增的 `internal/edit`、`internal/lsp/rename.go`、
      `internal/tool/builtin/rename.go` 全部干净**；仓库里剩下的 20 条告警是既有的（见其他 phase 的说明）。
- [x] `make build` 通过
