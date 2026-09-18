# Capability: edit-feedback

## Purpose

把"刚写完的文件有没有类型错误"这个问题的答案，**放进编辑工具自己的返回值里**。它改变的不是
agent 会做什么，而是它在做错之后多久知道——从"下一次跑测试"变成"这一次工具调用"。

## Scope

- `internal/lsp` — `Augmenter`：取某个文件的诊断并渲染成一段简短文本。
- `internal/tool/builtin` — `write_file` / `edit_file` 的成功路径接入，以及 Phase 24 的
  `apply_patch`。

## Requirements (MUST)

1. **挂在编辑动作上。** `write_file` 与 `edit_file` 成功返回后，若该文件被某个语言服务器覆盖，
   其诊断 MUST 被附在这次工具调用的结果里。MUST NOT 要求模型额外调用 `diagnostics`——
   一个需要主动调用的检查，在"我觉得改对了"的心态下不会被调用，而那正是最需要它的时候。
2. **只限被编辑的文件。** 反馈 MUST 只覆盖**本次写入的那个文件**。MUST NOT 做全仓扫描：
   一个中型仓库的提示类诊断有几千条，无差别反馈会把它变成噪声并挤掉真正的信号。
3. **有界。** 条数 MUST 有上限（`max_diagnostics`）；被截断时 MUST 说明总条数。默认只含
   `error` 与 `warning`。
4. **等待有硬上限。** 诊断是异步推送的，因此 MUST 等待其稳定，但 MUST 以
   `diagnostics_wait_ms`（默认 2000）为硬上限。超时 MUST 给出"诊断尚未就绪"的明确说明，
   **MUST NOT 让编辑变慢或失败**——一次编辑的成败与诊断无关。
5. **绝不改变编辑语义。** 附诊断 MUST NOT 把成功的编辑变成失败，MUST NOT 阻塞写入本身
   （写入先落盘，再取诊断）。MUST NOT 修改输入参数。
6. **没有语言服务器时零影响。** 无可用服务器、或文件类型无服务器时，工具结果 MUST 与引入本
   能力之前**逐字节一致**：不追加空行、不追加"无诊断"。
7. **有服务器但无诊断时要说清楚。** 服务器可用且文件已分析、但没有任何 error/warning 时，
   MUST 明确写出一行"无诊断"。留白会被模型读成"工具没返回这个字段"，或者被读成"没检查"。
8. **失败路径不附诊断。** 写入或编辑本身失败时 MUST NOT 附诊断：失败的结果必须只表达失败，
   混入诊断会稀释它的含义。
9. **默认打开，可关。** `tools.lsp.attach_diagnostics` 默认 `true`；置 `false` 时行为回到
   引入本能力之前。

## 输出形状

附在既有工具结果之后，一节带固定标题的文本（Go 侧用结构体渲染，不靠 map 拼字符串）：

```
写入完成：internal/foo/bar.go（+12 -3）

诊断（该文件，2 条 error/warning）：
  internal/foo/bar.go:42:9  error  undefined: BAZ  (compiler)
  internal/foo/bar.go:88:2  warning  unused parameter: ctx  (unusedparams)
```

- 无诊断时：`诊断（该文件）：无`
- 未就绪时：`诊断（该文件）：尚未就绪（语言服务器未在 2000ms 内返回）`
- 无服务器时：**不出现这一节**

## 行为

- 一次工具调用里同时写多个文件（Phase 24 的 `apply_patch`）时，MUST 按文件分组附诊断，并遵守
  同一个总条数上限。
- 诊断的渲染 MUST 用工作区相对路径与 1-based 行列，与 `read_file` 的行号一致。
- 语言服务器启动本身就是秒级操作：某个文件的**第一次**编辑可能因此等满
  `diagnostics_wait_ms`。这是可接受的（上限有界），但 MUST NOT 把它变成"每次编辑都等满"——
  服务器启动后 MUST 复用。

## Non-goals

- 不做自动修复、不自动回滚有问题的编辑、不因为诊断里有 error 就拒绝这次编辑。
- 不做"诊断数量变化"的追踪（例如"你这次的修改新增了 3 个警告"）——那需要保存上一次的诊断快照，
  属于未来可能的增强。
- 不把诊断进上下文之外的地方（不进数据库、不进 `plan`、不进系统提示词）。
- 不给 `bash` 或其它工具附诊断。
