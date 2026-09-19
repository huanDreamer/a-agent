# ClaudeCode 兼容模式

兼容模式让 huan-agent 直接跑在 Claude Code 的配置上：读 `~/.claude/settings.json`，
把模型换成那里的端点与模型，把那五个事件上注册的 hook 挂在真实触发点上。
开关在 **设置 → ClaudeCode**，侧边栏「新建对话」下面会写明当前是哪一种模式。

一句话概括它解决的问题：一台机器上已经为 Claude Code 配好了端点、凭据、模型和 hook，
兼容模式让这些配置原样生效，而不是让你把它们再抄一份到 `config.yaml` 里、然后两边慢慢发散。

```
设置 → ClaudeCode  ┌───────────────────────────────┐
                  │ 本机模式  │  ClaudeCode 兼容    │
                  └───────────────────────────────┘
```

---

## 1. 开关与生效范围

| 项 | 说明 |
| :--- | :--- |
| 开在哪里 | 设置 → ClaudeCode；侧边栏徽章显示当前模式与生效模型，点它跳到这个 tab |
| 存在哪里 | 数据库 `app_settings.claudecode.mode`（`native` / `claudecode`）。没写过这个键时，用 `config.yaml` 的 `claudecode.enable` |
| 何时生效 | 下一条消息。切换会作废缓存的 runner，所以已建的对话也会跟着换 |
| 影响谁 | **所有对话**。兼容模式开着时，每轮都跑 `model.effective_model`，覆盖会话里单独选的模型；关掉后恢复本机模型 |
| 谁读 settings.json | 只有这个进程的 admin server（`huan-agent admin serve`）。`chat` / `run` / 飞书 bot 是各自独立的入口，不受这个开关影响 |

`config.yaml`：

```yaml
claudecode:
  enable: false            # 启动时的默认状态，控制台的开关会覆盖它
  settings_path: ""        # 空 = ~/.claude/settings.json
  hooks: true              # 关掉只切换模型，不跑 hook
  hook_timeout_seconds: 600  # 单个 handler 的上限，跟 Claude Code 的默认值一致
  reload_seconds: 15       # 多久检查一次 settings.json 的 mtime
```

编辑 `settings.json` 之后不用重启：文件 mtime 一变（最多滞后 `reload_seconds`），
或者点「重新读取 settings.json」，就会重新读一次并重建 hook 引擎。

---

## 2. 模型配置：`env` 块

按 Claude Code 自己的优先级读这些变量：

| 变量 | 用途 |
| :--- | :--- |
| `ANTHROPIC_BASE_URL` | 端点。协议按 **Anthropic Messages** 走（`{base}/v1/messages`），空则用 `https://api.anthropic.com` |
| `ANTHROPIC_AUTH_TOKEN` | 凭据，以 `Authorization: Bearer` 发送 |
| `ANTHROPIC_API_KEY` | 凭据，以 `x-api-key` 发送（两个都有时 `AUTH_TOKEN` 优先，与 Claude Code 一致） |
| `ANTHROPIC_MODEL` | 主模型（优先级最高） |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` / `..._OPUS_...` / `..._HAIKU_...` | 各档模型。主模型为空时按 Sonnet → Opus → Haiku 取第一个 |
| `ANTHROPIC_SMALL_FAST_MODEL` | 小模型旧名，只在 Haiku 档为空时使用 |
| `CLAUDE_CODE_EFFORT_LEVEL` | 随每个 hook payload 作为 `effort.level` 传出，同时导出成 `$CLAUDE_EFFORT` |
| `CLAUDE_CODE_MAX_OUTPUT_TOKENS` | 单次回复的 `max_tokens`，空则 8192 |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | **仅展示**。本 agent 的窗口按模型目录推导，不读这个值 |
| `CLAUDE_CODE_SUBAGENT_MODEL` | **仅展示**。本 agent 的子 agent 与主 agent 同模型运行 |

兼容模式带来的 provider 叫 `claudecode`（`kind: anthropic-messages`），它**不写数据库**：
`settings.json` 是唯一事实来源，数据库里再存一份就是第二个会过期的答案。
开关关掉，它就从模型目录里消失。

顺带说明两件事：

* Anthropic Messages 是本项目支持的第二种协议（`internal/llm/anthropic.go`）。
  在 模型管理 里手建一个 `kind: anthropic-messages` 的 provider 也能用，但那种行没有
  `auth_style` 列，凭据走 `x-api-key`；兼容模式则严格照 Claude Code 的方式发头。
* 端点不可用（缺 token、缺模型名）时，开关**仍然可以打开**：此时每一轮继续跑本机模型，
  面板上写着原因。一个打开了却什么都不做的开关，比一个拒绝移动的开关好排查。

---

## 3. Hooks

### 3.1 支持范围

配置文件按 Claude Code 的三层结构完整解析（event → matcher group → handler），
但**只有五个事件在本 agent 里有触发点**：

| 事件 | 在本 agent 里什么时候触发 | 能阻止吗 |
| :--- | :--- | :--- |
| `SessionStart` | 新建对话（`startup`）、继续上一轮（`resume`）、清空对话（`clear`） | 不能 |
| `UserPromptSubmit` | 用户消息提交前 | **能**（这条消息不入库、不下发） |
| `PreToolUse` | 每次工具调用前 | **能**（该次调用被跳过，原因作为工具结果给模型） |
| `PostToolUse` | 每次工具调用后（失败也触发） | 不能（但可改写工具结果、可追加上下文） |
| `Stop` | 一轮回答结束时 | **能**（原因作为 system 消息交回模型，循环继续，受步数预算约束） |

其余 28 个事件（`Notification`、`PreCompact`、`SubagentStart`…）会被读取并在面板上列出，
标成「本 agent 没有这个事件的触发点」——不运行，也不假装运行。

### 3.2 handler 类型

| 类型 | 状态 |
| :--- | :--- |
| `command` | 完整支持：有 `args` 走 exec 形式（直接 spawn，不过 shell），没有 `args` 走 `sh -c`；stdin 是 payload JSON；`timeout` 秒 |
| `http` | 完整支持：POST payload，`headers` 里的 `$VAR`/`${VAR}` 只解析 `allowedEnvVars` 里列出的变量；**2xx 状态码本身不能阻塞**，要阻塞得在响应体里给决策字段 |
| `mcp_tool` | 支持：调用已连接的 MCP server 上的工具（`server` + `tool`），`input` 支持 `${tool_input.file_path}` 这类替换。不会为了一个 hook 去触发连接或 OAuth |
| `prompt` / `agent` | **未接入**（需要 LLM 判定）。面板上提前标注，运行时记为「跳过」并写明原因 |
| 带 `if` 的 handler | **未求值**（权限规则语法是另一套子系统）。同样提前标注、运行时跳过 |

### 3.3 协议细节

实现严格照 `~/.claude/hooks/HOOKS-REFERENCE.md`（该文档就是为写兼容工具而写的）：

* **matcher 语义**：`""` / `*` / 省略 = 全匹配；只含字母数字 `_ - 空格 , |` = 精确匹配（`,` 或 `|` 分隔的列表）；含其他字符 = 非锚定正则。
* **exit code**：`0` 成功；`2` 在可阻止的事件上阻止（同时仍会读 stdout 上合法的 JSON）；**其他任何值（包括 1）都不阻塞**，只记为非阻塞错误。
* **stdout**：首尾去空白后「以 `{` 开头且以 `}` 结尾」才按 JSON 解析，否则按纯文本；`SessionStart` / `UserPromptSubmit` 的纯文本会进模型上下文。
* **JSON 输出**：`continue` / `stopReason` / `systemMessage` / `terminalSequence`（接受并忽略）/ 顶层 `decision: "block"` + `reason` / `hookSpecificOutput`（**必须带 `hookEventName`**，不匹配就整块拒收并记错误）里的 `permissionDecision`、`permissionDecisionReason`、`updatedInput`、`updatedToolOutput`、`additionalContext`、`sessionTitle`、`initialUserMessage`、`watchPaths`、`reloadSkills`、`retry`。
* **并行**：同一事件下命中的 handler 并行执行；`async: true` 的 command handler 后台启动，不参与决策。
* **上限**：`additionalContext` / `systemMessage` / `initialUserMessage` 各 10,000 字符，超长截断并记录。
* **超时**：默认 600s，受 `claudecode.hook_timeout_seconds` 约束。
* **`disableAllHooks: true`** 被尊重：配置保留，一个都不跑，面板写明原因。

### 3.4 payload 与工具名翻译

命令/HTTP handler 收到的 JSON 是协议里的公共字段加事件专属字段：

```json
{
  "session_id": "9c1a9abe-…",
  "cwd": "/Users/huan/project/a-agent",
  "permission_mode": "bypassPermissions",
  "hook_event_name": "PreToolUse",
  "tool_name": "Bash",
  "tool_input": {"command": "ls .verify-cc", "description": "…"},
  "tool_use_id": "call_1",
  "model": "deepseek-flash[1m]",
  "effort": {"level": "max"}
}
```

字段与 Claude Code 实测的一致（对照参考文档 §4 与 §10）：

| 事件 | 本 agent 下发的字段 |
| :--- | :--- |
| `SessionStart` | `session_id` `cwd` `permission_mode` `hook_event_name` `source` `model` `effort`，以及会话已有标题时的 `session_title` |
| `UserPromptSubmit` | 公共字段 + `prompt` `prompt_id` |
| `PreToolUse` | 公共字段 + `tool_name` `tool_input` `tool_use_id` `prompt_id` |
| `PostToolUse` | 上一行全部 + `tool_response` `duration_ms` |
| `Stop` | 公共字段 + `prompt_id` `stop_hook_active`（**显式 false，不省略**）`last_assistant_message` `background_tasks` `session_crons` |

几点说明：

* `tool_response` 是**工具自己的输出对象**（本 agent 的内置工具都返回 JSON 对象），
  所以 `jq -r '.tool_response.stdout'` / `.exit_code` 能直接取到值，不会被包一层 `result` 字符串；
  纯文本结果才放在 `tool_response.result` 下。另外附上 `is_error`（§8 没有这个字段，
  但 hook 需要一个判据，靠 stderr 猜是错的）。
* `background_tasks` 来自本进程真实的作业表，**只列本会话正在跑的进程**，
  形状按 §8（`id` / `type: "shell"` / `status` / `description` / `command`，字段 1000 字符上限带 `… [+N chars]` 标记）；
  `session_crons` 恒为空数组（本 agent 没有定时唤醒）。两者在 `Stop` 上**永不省略**——
  §8 明确说"空数组"与"字段缺失"含义不同。
* `prompt_id` 每一轮生成一次，同一轮的 `UserPromptSubmit` / `PreToolUse` / `PostToolUse` / `Stop` 共用一个值，
  hook 可以据此把一次提问的所有事件串起来。`/clear` 会清掉它。
* `stop_hook_active` 只在 `Stop` 上出现，且 `false` 也会真的下发：
  hook 用它做防死循环判断，把 `false` 省略掉会让 hook 分不清"假"和"运行时忘了发"。

其余差异值得知道：

* **工具名按 Claude Code 的叫法翻译**：`bash → Bash`、`read_file → Read`、`write_file → Write`、
  `edit_file → Edit`、`glob → Glob`、`grep → Grep`、`list_dir → LS`、`fetch_url → WebFetch`、
  `skill → Skill`、`spawn_agent → Task`、`ask_user → AskUserQuestion`。
  没有对应关系的工具（`describe_image`、`save_artifact`、`apply_patch`、`plan_*`、`mcp__*`）原样透传。
  翻译是必须的：matcher 对工具名做的是大小写敏感的精确比较，`"matcher": "Bash"` 若原样比对
  本 agent 的 `bash`，那条 hook 就永远不会触发——而这是兼容模式唯一不能有的失败方式。
* **`tool_input` 保持本 agent 自己的字段**：`Bash` 的 `command`/`description` 两边一致，
  但只有 Claude Code 特有的字段（例如 `Edit` 的 `file_path` 写法）不保证相同。
* `transcript_path` 不下发：本 agent 的会话存在数据库里，没有对应的 JSONL 文件。
  需要「本轮助手最后说了什么」的 hook 请用 `Stop` 的 `last_assistant_message`。
* `mcp_server`（MCP 工具额外携带）不下发；`model` 与 `effort` 会比 Claude Code 多带几个事件，
  是超集而非缺失，hook 按字段名读取不受影响。

### 3.5 观察 hook 有没有被调用

三个地方能看到：

1. **设置 → ClaudeCode → 运行记录**：每次 handler 执行一行（时间、事件、命中的 matcher、
   被匹配的值、命令、exit code、耗时、是否阻止、原因/错误），`async` 的行 exit code 显示 `-1`。
   等价接口：`GET /api/claudecode/events?limit=50`。
2. **服务端日志**：`exit 0` 的 stderr 与 hook 返回的 `systemMessage` 都会记进 zap 日志。
3. **hook 自己写的文件**：Claude Code 的常规做法（例如参考文档里的 `~/.claude/hooks/events.log`）。

> 注意：如果 huan-agent 进程本身跑在受限的沙箱里（例如由某个 harness 启动），
> 它的子进程写 `~/.claude/` 之外的路径可能失败，而 hook 脚本常常把重定向错误吞掉
> （`>> "$LOG" 2>/dev/null`），于是表现为「hook 跑了但没日志」。先确认 hook 自己的写权限。

---

## 4. API

| Endpoint | 用途 |
| :--- | :--- |
| `GET /api/claudecode` | 面板渲染的全部内容：模式、settings.json 路径/mtime/错误、模型映射（凭据只给掩码）、env 表、hook 配置表、运行记录、当前模式的边界说明 |
| `POST /api/claudecode/mode` | `{"compat": true}` 或 `{"mode": "claudecode"｜"native"}`，返回同一个状态体 |
| `POST /api/claudecode/reload` | 立刻重读 `settings.json` |
| `GET /api/claudecode/events?limit=50` | 单独的运行记录，供面板刷新 |

凭据**不会**出现在任何响应里：`model.token_masked` 是 `sk-4f9…2f10` 这种形式，
`env` 表里标记为 secret 的值在 Go 侧就已掩码（不是在浏览器里），所以任何客户端、日志或代理都拿不到明文。

---

## 5. 边界（面板底部也写着同样的内容）

1. 只读 `~/.claude/settings.json`。Claude Code 还会合并项目级 `.claude/settings.json`
   与 `.claude/settings.local.json`，那是每个目录一份的事实，本 agent 未接入。
2. 只派发那五个事件。
3. `prompt` / `agent` 类型的 handler 与 `if` 条件未接入，一律「标注 + 运行时跳过」。
4. `CLAUDE_CODE_SUBAGENT_MODEL` 与 `CLAUDE_CODE_AUTO_COMPACT_WINDOW` 只展示。
5. 兼容模式不写 `settings.json`：那是 Claude Code 的文件，一个兼容模式不该成为它的第二个作者。

---

## 6. 怎么验证

```bash
# 1. 看服务端读到了什么
curl -s localhost:8080/api/claudecode | python3 -m json.tool | head -40

# 2. 打开开关（这一步会写库，下一条消息就生效）
curl -s -X POST -H 'Content-Type: application/json' \
     -d '{"compat":true}' localhost:8080/api/claudecode/mode | head -c 300

# 3. 模型目录里应该出现 claudecode provider，并且它是 default
curl -s localhost:8080/api/chat/models | python3 -m json.tool | grep -A 3 claudecode

# 4. 新建一个对话 → SessionStart 的 hook 应该出现在运行记录里
curl -s -X POST -H 'Content-Type: application/json' -d '{}' localhost:8080/api/chat/sessions
curl -s 'localhost:8080/api/claudecode/events?limit=20' | python3 -m json.tool
```

一次完整的实测（本机 `~/.claude/settings.json` + 一个把 payload 落盘的探针 handler）：

```
SessionStart      matcher=startup|resume|clear|compact|fork  value=startup  exit=0   993ms
UserPromptSubmit  matcher=*                                  value=         exit=0   112ms
PreToolUse        matcher=Bash                               value=Bash     exit=0    83ms
PreToolUse        matcher=*                                  value=Bash     exit=0    61ms
PostToolUse       matcher=Bash                               value=Bash     exit=0    77ms
PostToolUse       matcher=*                                  value=Bash     exit=0    54ms
Stop              matcher=*                                  value=         exit=0    81ms
```

同一轮里模型确实跑在 cc 的端点上（`api.deepseek.com/anthropic`），调用了 `bash` 工具，
并且读到了 `SessionStart` 注入的 `additionalContext`。

---

## 7. 出问题怎么查

| 症状 | 原因 |
| :--- | :--- |
| 侧边栏一直显示「模式未知」 | `GET /api/claudecode` 没通（服务未起、或版本里没有这个路由） |
| 开关点不动 / 显示不可用 | `model.problem`：settings.json 缺 `ANTHROPIC_BASE_URL`、缺凭据、或没有任何模型名 |
| 开关打开了但模型没变 | `model.ready` 为 false 时每轮仍跑本机模型，面板上写着原因；修好 settings.json 后点「重新读取」 |
| hook 一个都没跑 | 兼容模式没开、`claudecode.hooks` 为 false、`disableAllHooks: true`、或 settings.json 里没配 hook —— 面板的 `hooks.reason` 按这个顺序给出第一个原因 |
| 某个 matcher 命中了但另一个没有 | matcher 是大小写敏感的精确匹配；工具名按 §3.4 的表格翻译后再比。`Edit|Write` 只在 `edit_file`/`write_file` 上命中 |
| 运行记录里出现 `prompt` / `agent` 的「跳过」 | 这两类 handler 需要 LLM 判定，本 agent 未接入 |
| hook 跑成功但「什么都没有发生」 | `exit 0` 的 stderr 不会进对话；要影响模型得用 `additionalContext`，要阻止得 `exit 2` |
