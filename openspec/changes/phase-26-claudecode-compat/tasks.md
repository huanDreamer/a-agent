# Phase 26 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

## 实施记录（2026-09-19）

**已完成并验证**：全部 8 节。除单元测试外，做了一次真实的端到端实测：真实
`~/.claude/settings.json` + 一个把 payload 落盘的探针 handler，五个事件全部实跑命中，
同一轮模型确实跑在 `api.deepseek.com/anthropic` 上并调用了 `bash` 工具。
**未完成 / 未验证**：`http` 与 `mcp_tool` 两种 handler 只有单元测试、没有真实打到过外部
服务；`prompt` / `agent` / `if` 按设计不接入；`transcript_path` 与 `mcp_server` 不下发
（前者本 agent 没有 JSONL 会话文件，后者需要把 MCP server 名带进工具调用）。
**改动未提交。**

### 第 1 节：第二种协议（`internal/llm`）

- [x] `Provider` 增加 `Kind` / `AuthStyle` / `MaxOutputTokens` / `AnthropicVersion`，
      `New()` 按 kind 分派；`kind: openai`（含空值）走原路径，行为不变
- [x] `anthropic.go`：`POST {base}/v1/messages`（`/v1` 后缀去重）、`anthropic-version`、
      `Authorization: Bearer` / `x-api-key` 两种凭据头
- [x] 消息转换：system 上提并合并；assistant 的 tool call → `tool_use`；
      `schema.Tool` → `tool_result`，且相邻结果**合并成同一条 user 消息**（API 要求交替）
- [x] 图片：base64 data URL 拆分、url source 透传；`reasoning_content` 丢弃
- [x] 非流式解析：text / tool_use / thinking / usage / stop_reason（`tool_use` 不算错）
- [x] 流式解析：`message_start` / `content_block_start` / `text_delta` /
      `input_json_delta`（分片累积）/ `content_block_stop` / `message_delta` /
      `message_stop` / `error`；tool call 只在 `content_block_stop` 发一次
      （eino 的 concat 对没有 Index 的 call 会重复计数，发两次会执行两次）
- [x] 非 2xx 复用 `LLMError` 并解包 `{"error":{"message":…}}`，body 有上限
- [x] 测试：URL 三种形状、两种凭据头（并断言另一个头不存在）、system 上提、
      tool_result 合并、图片、max_tokens 优先级、工具 schema、非流式/流式解析、
      错误表、重试包装、kind 分派

### 第 2 节：hook 运行时（`internal/claudehook`）

- [x] 配置模型：event → matcher group → handler 三层；未知 handler 类型保留不丢，
      坏条目 join 成错误但**不丢**能解析的部分
- [x] matcher：全匹配 / 精确（`,`、`|` 分隔）/ 非锚定正则；`FileChanged`、
      `StopFailure` 用更窄字符集
- [x] `command`：有 `args` = exec 形式，否则 `sh -c`；payload 走 stdin；stdout/stderr 有上限
- [x] `http`：POST payload；`headers` 的 `$VAR` 只解析 `allowedEnvVars` 里的；
      2xx 状态码本身不阻塞
- [x] `mcp_tool`：`${tool_input.file_path}` 替换后调用；只调已连接的 server
- [x] 输出契约：`exit 2` 阻塞（且仍读 JSON）、其他非零码不阻塞、stdout 的
      `{`…`}` 判定、`hookSpecificOutput` 必须带匹配的 `hookEventName`、
      `additionalContext`/`systemMessage`/`initialUserMessage` 各 10000 字符上限
- [x] 决策聚合：阻塞优先、第一个原因保留其余进 failures、`continue:false` 最高优先级
- [x] 并行执行 + 有界并发安全的事件日志（每个真正执行的 handler 一条记录）
- [x] `prompt` / `agent` / 带 `if` 的 handler：提前标注 + 运行时记为跳过并给出原因
- [x] 测试覆盖上面每一条，含 subprocess 形式的 exec/shell 差异与超时

### 第 3 节：设置读取与模式（`internal/claudecode`）

- [x] `Load()`：读 JSON、env 容错（数字/布尔也转成字符串）、`disableAllHooks`、
      文件缺失/JSON 坏/路径空都以 `Err` 汇报而不 panic
- [x] 模型投影按 Claude Code 优先级；凭据样式按 `AUTH_TOKEN` 优先；
      `Mask()` 短值全遮、长值首 6 尾 4
- [x] `Ready` / `Problem`：缺端点、缺凭据、缺模型名三种原因按请求会失败的顺序给出
- [x] `env` 表：已知变量按目录顺序 + 未知变量按字母序追加；`secret` 行**在 Go 侧掩码**；
      未使用的变量标 `used:false`（`CLAUDE_CODE_SUBAGENT_MODEL`、`..._AUTO_COMPACT_WINDOW`）
- [x] `Service`：模式取自 store（没写过则取 config）、`SetCompat` 落库、mtime 轮询（带节流）
- [x] provider 源：`Row` / `Models` / `Build`，模式关闭或不可用时一律不提供；模型缓存按
      base_url+model+auth 为键，`Reload` 时整个作废
- [x] 每次会话注入的 SessionStart 上下文：按会话存、有上限、`ForgetSession` 清空
- [x] 面板 payload：模式、路径/mtime/错误、模型映射、env 表、hook 表（含每条 handler
      能否运行的标注）、运行记录、边界说明；`token` 不出现、`env` 的 secret 已掩码
- [x] 测试：17 个用例，含「状态体序列化后不含明文 token」这条断言

### 第 4 节：turn 里的钩子接缝（`internal/chat`）

- [x] `Hooks` 接口（`PreToolUse` / `PostToolUse` / `Stop`）+ `HookCall` /
      `HookDecision` / `HookStop` / `HookStopDecision`；nil = 完全没有 hook 开销
- [x] `runTool`：调用前问 PreToolUse（阻塞即跳过并把原因当工具结果、`updatedInput`
      在工具看到之前替换入参），调用后问 PostToolUse（可改写结果、可追加上下文）
- [x] `runToolCalls` 返回值携带追加的上下文（而不是共享字段 + 锁），由 `Run` 按模型
      自己的调用顺序追加
- [x] `Run`：最后一步（模型不再要工具）先问 Stop；被阻止则把原因作为 system 消息
      交回模型并继续，受步数预算约束，`AlreadyBlocked` 让 hook 自己防死循环
- [x] 单元测试：阻塞、改写入参、改写结果、追加上下文、并行调用下上下文不串

### 第 5 节：服务端接线（`internal/server`）

- [x] `/api/claudecode`、`/mode`、`/reload`、`/events`；未启用时 `available:false` 而非 404
- [x] 切换成功即作废 runner 缓存（否则已用的模型会继续跑旧目标）
- [x] 派发点：`SessionStart`（create/resume/clear，含 title 与上下文注入）、
      `UserPromptSubmit`（入库前阻止）、`PreToolUse`/`PostToolUse`/`Stop`（经 hookAdapter）
- [x] `insertHookContext`：钩子注入的文本插在最后一条 user 消息**之前**（system 角色）
- [x] `permissionMode()`：`off` → `bypassPermissions`，其余 → `default`；
      `effort` 取自 settings.json
- [x] 工具名翻译表（bash→Bash 等）与"没有对应关系就原样透传"的规则
- [x] `HookMCPCaller`：`mcp_tool` handler 调已连接 server；无 MCP 运行时时返回 nil
- [x] catalogbuilder 的 `ExtraProvider`：在 catalog 读取时追加、在 `Build` 时解析、
      模式打开时把它的模型标为 default；存储型 provider 也允许 `anthropic-messages`
- [x] 测试：路由、切换落库、目录随模式增删、坏 body 拒绝、reload/events、
      SessionStart 命名、UserPromptSubmit 阻止后不留消息、payload 形状、工具名翻译

### 第 6 节：配置与持久化

- [x] `config.ClaudeCodeConfig`（enable / settings_path / hooks / hook_timeout_seconds /
      reload_seconds）+ `SetDefaults` + `Default()` + `config.example.yaml` 注释
- [x] `store.GetClaudeCodeMode`（区分"没写过"与"写成了 native"）/ `SetClaudeCodeMode`
      + `Store` 接口

### 第 7 节：控制台（`web/`）

- [x] `SETTINGS_TABS` 增加 `ClaudeCode`，`SettingsView` 注册面板与副标题
- [x] `ClaudeCodePanel.vue`：开关（不可用时锁定并给出原因）、模型卡、env 表（secret
      再加一层浏览器侧掩码）、hook 表（matcher 组 + 每条 handler 的目标/超时/async/
      为何不会运行）、五个事件的含义、未接入事件单列、运行记录（可刷新）、边界说明
- [x] 侧边栏「新建对话」下方的模式徽章：探针未回答时**什么都不渲染**（不闪"本机模式"），
      native / compatible / unknown 三态，点击跳到 设置 → ClaudeCode
- [x] `state.js` 单一状态源（徽章与面板读同一个对象）、`api.js` 四个方法、
      `bootstrap()` 并行加载
- [x] `npm run build` + `npm run check:ui`（311 PASS / 0 FAIL，含为本面板新增的 40 条断言）

### 第 7.5 节：payload 与参考文档实测样例对齐（实测后修正）

真实实例跑过一轮之后，把 hook 收到的 payload 与参考文档 §4/§10 的实测样例逐字段比对，
修掉四处不符合的地方：

- [x] `stop_hook_active` 在 `Stop` 上**恒下发**（`false` 也发）。原实现 `omitempty` 掉了 `false`，
      而 §10 的实测样例是显式 `false`：hook 用它做防死循环判断，分不清"假"和"运行时忘了发"
- [x] `tool_response` 改成**工具自己的输出对象**（内置工具都返回 JSON 对象），
      不再包一层 `result` 字符串，`jq -r '.tool_response.stdout'` 直接可用；
      纯文本结果才放 `result`，并附 `is_error`
- [x] 补 `prompt_id`（同一轮四个事件共用一个 id）、`PostToolUse` 的 `duration_ms`、
      `SessionStart` 的 `session_title`
- [x] 补 `Stop` 的 `background_tasks`（取自真实作业表、只列本会话在跑的进程、按 §8 的 1000 字符上限
      与 `… [+N chars]` 标记）与 `session_crons`（恒空数组）；两者永不省略，空数组 ≠ 字段缺失
- [x] 测试 8 个新用例钉住这些规则（含 `stop_hook_active` 恒在、空数组 vs 缺失、
      `duration_ms` 只属于 PostToolUse、`tool_response` 内联、prompt_id 共用、作业数组过滤、
      截断标记）
- [x] 端到端复验：重启后真实跑一轮，用**用户自己的** `~/.claude/hooks/summary.jq` 解析五个 payload，
      字段全部能取到

### 第 8 节：文档

- [x] `docs/claudecode.md`：开关与生效范围、`env` 映射表、五个事件与三种 handler、
      协议细节、payload 与工具名翻译、可观测的三个地方、API、边界、验证步骤、排查表
- [x] `docs/admin.md`：端点表 4 行 + 「ClaudeCode 兼容模式」章节
- [x] `openspec/changes/phase-26-claudecode-compat/`：本目录
- [ ] 归档到 `openspec/specs/`（归档命令：`/openspec:archive phase-26-claudecode-compat`）
