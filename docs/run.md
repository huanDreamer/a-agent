# 非交互式运行：`huan-agent run`

`huan-agent run` 给一个任务的描述，它做到底，然后把答案打到 stdout 并退出。它是给 **git hook、
CI、cron** 用的入口：没有终端、没有人坐在前面、没有人在中途回答任何问题。

```bash
huan-agent run "读一下 README.md，用一句话说这个项目是什么"
huan-agent run "审一遍这个 diff，只报 must-fix" --output json | jq -r .answer
git diff --cached | huan-agent run - "审一遍下面这段 diff"
cat error.log | huan-agent run -
```

## stdout 是产品，stderr 是噪声

这是这个命令最重要的一条约定：

| 流 | 内容 |
|---|---|
| **stdout** | 只有答案（`--output text`），或只有一个 JSON 对象（`--output json`） |
| **stderr** | 日志（Zap）、每一步的工具进度、结束时的用量与耗时汇总、以及失败原因 |

所以下面这些是可用的：

```bash
huan-agent run "..." > answer.md          # answer.md 就是答案，没有别的东西
huan-agent run "..." | tee log.txt        # 边跑边看，因为答案是逐块 flush 的
huan-agent run "..." 2>/dev/null | pbcopy # 只要答案
```

`--quiet` 只去掉 stderr 上的进度与汇总行，**错误仍然会打印**（一条解释为什么退出码非零的信息
不能省）。日志级别由 `logging.level` 控制，与其它命令共用同一个配置。

> 实现上，Zap 的 sink 被指向 stderr（`obs.NewLoggerTo`），stdout 只通过一个 `runWriter` 写入。
> 这是这个命令唯一允许自己破坏的"禁止 `fmt.Println`"边界——答案必须落在 stdout，而日志不是答案。

## 输入形式

| 形式 | 含义 |
|---|---|
| `run "<prompt>"` | 参数就是任务 |
| `run - "<instruction>"` | 参数是指令，**stdin 作为材料追加在后面**（围栏 + 标注，让模型分得清哪句是指令） |
| `run -` | stdin 整个就是任务 |

`-` 是**显式**的，不从"stdin 不是终端"推断。理由很具体：git hook 的 stdin 通常不是空的
（`pre-push` 会收到将要推送的 refs），嗅探管道会把一串 refs 悄悄拼到指令后面。只在调用方明确
要求时才读 stdin，是"功能"和"灵异事件"的区别。

## 退出码

退出码是它与 shell 的契约，**是常量而不是配置项**——如果部署可以重映射它，"0 表示成功"就取决于
部署了。

| 码 | 含义 |
|---|---|
| 0 | 跑完了（**不保证答案是对的**，只保证这一轮正常结束） |
| 1 | 模型调用或 agent 循环失败 |
| 2 | 用法错误（prompt 为空、flag 非法） |
| 3 | 超时（`--timeout` 或 `run.timeout_seconds`） |
| 4 | 步数或 token 预算耗尽，没等到答案 |
| 5 | 有工具调用失败，且设了 `--fail-on-tool-error` |
| 6 | 配置问题（缺 API key、工作区不存在、provider 不支持工具调用、`--session` 不存在） |

它们描述的是**这一轮跑得怎么样**，不是活干得好不好。"模型答了但答案是错的"是 0；判断答案好坏
是调用方的事。需要区分时用 `--fail-on-tool-error`，或读 `--output json` 的 `stop_reason`。

## flag

| flag | 说明 |
|---|---|
| `--provider` / `--model` | 覆盖默认 provider / 模型 |
| `--system` | 替换内置系统提示词 |
| `--skill <name>` | 加载技能（技能正文成为系统提示词，`tools` 成为允许清单，且隐含开启工具） |
| `--workspace <dir>` | 文件与命令工具被限制在这个目录（缺省用 `tools.workspace`） |
| `--output text\|json` | 输出格式，缺省取 `run.default_output` |
| `--timeout <dur>` | 整轮的墙钟预算 |
| `--max-steps <n>` | 工具调用轮数上限，缺省 `run.max_steps` → `chat.max_steps` |
| `--session <id>` | 把这次运行挂到既有会话上（**必须存在**，打错不会新建一个"看起来成功了"的会话） |
| `--no-tools` | 不带工具跑，退化成一次普通补全 |
| `--allow-background` | 允许留下后台进程的工具（默认不给，见下） |
| `--fail-on-tool-error` | 有工具调用失败就以 5 退出 |
| `--quiet` | stderr 只留错误 |

## 工具默认打开，但少两个

`run` **默认带工具**（与 `chat` 的 `--tools` 相反）：没有工具的一次性运行只是一次便宜的补全调用，
而"进 CI 干活"正是它的目的。

按 surface 裁剪掉的是需要人在场的能力——沿用"注册了但必然失败，不如不在菜单上"的原则：

- **`ask_user` 不给。** 没有卡片、没有答案通道，注册它只会换来一次必然失败或永久挂起的调用。
- **后台进程工具（`bash_background` 等）默认不给。** 一次性运行退出之后就没人管它们了；`run`
  留下的开发服务器会活过监督它的进程，下一次运行可能撞上一个它叫不出名字的进程占用的端口。
  `--allow-background` 是显式选择，且无论是否开启，**退出前都会停掉本次启动的作业**。
- **计划工具（`plan_*`）不给**（它们本来就只在 Web 面注册）：一次性运行没有看板要维护。

系统提示词里对应的是 `internal/prompt/surface_run.md`：不能提问、歧义时选最保守的方案并在答案里
说明假设、不要等人工确认、答案必须自成一体。

## `--output json`

一个对象，字段就是 Go 里的结构体（`cmd/huan-agent/run.go` 的 `runResult`），所以名字不会和文档
漂移：

```json
{
  "answer": "huan-agent 是一个基于 CloudWeGo Eino 的个人 AI Agent 平台。",
  "provider": "deepseek",
  "model": "deepseek-chat",
  "session_id": "85325ad4-…",
  "stop_reason": "done",
  "tool_failures": 0,
  "usage": {
    "prompt_tokens": 26950, "completion_tokens": 203, "total_tokens": 27153,
    "duration_ms": 2407,
    "cost": { "total": 0, "priced": false }
  },
  "steps": [
    { "step": 1, "tools": [ { "name": "list_dir", "ok": true, "duration_ms": 2 } ] },
    { "step": 2, "tools": [ { "name": "read_file", "ok": true, "duration_ms": 1 } ] },
    { "step": 3, "tools": [] }
  ],
  "failures": []
}
```

- `stop_reason` ∈ `done`（模型自己给了答案）/ `steps` / `tokens` / `deadline`。
- `usage.cost.priced=false` 表示价格表里没有匹配的条目——**不是免费，是没测**。报 0 会被读成免费。
- `steps` 与 `failures` 永远是数组，失败运行也会输出一个合法对象（`error` 字段说明原因），
  所以调用方不必判空。

## 三个真实场景

**git hook：提交前审一遍**

```bash
#!/bin/sh
# .git/hooks/pre-commit
out=$(git diff --cached | huan-agent run - "审一遍这段 diff。只报 must-fix 问题，每条一行；
如果没有任何 must-fix，只输出 OK。" --quiet 2>&1)
case "$out" in
  OK*) exit 0 ;;
  *)   echo "$out"; echo "(huan-agent 认为有 must-fix，可用 git commit --no-verify 跳过)"; exit 1 ;;
esac
```

**CI：挂了自动定位**

```yaml
- name: diagnose failure
  if: failure()
  run: |
    huan-agent run "npm test 失败了。读 package.json 与测试输出，找出根因并给出最小修复方案。" \
      --output json --timeout 5m > diagnosis.json
    jq -r .answer diagnosis.json >> "$GITHUB_STEP_SUMMARY"
```

**cron：每天早报**

```cron
0 9 * * * cd /srv/project && /usr/local/bin/huan-agent run \
  "看一遍昨天的 git log 与 openspec/changes，总结进展与今天的重点" \
  --quiet > /tmp/daily.md 2>/tmp/daily.err
```

## 与其它命令的关系

- **跑出来的会话在控制台可见**：每次运行都有一个 `session_id`，会话与消息都落库，所以能在
  控制台的对话列表里看到它做了什么、每一步调用了什么工具。`--session <id>` 可以挂到既有会话上。
- **用量进统计**：token 与耗时走同一张 usage 表，所以 CI 的消耗会出现在 统计监控 里，而不是
  被悄悄排除在统计之外。
- **链路追踪**：一次运行是一条 trace（本地 SQLite，与对话同一个库），所以事后能查它到底做了什么。
- **审批**：无人值守时没有人可以批准。Phase 21 引入审批闸门后，`tools.approval.mode` 非 `off` 时
  `run` 会**拿不到写/执行工具**（它没有 approver），而不是每次调用都被拒。

## 排障

- **`a prompt is required`**（退出码 2）——忘了给 prompt，或者用了 `-` 但 stdin 是空的。
- **`with two arguments the first must be "-"`**——两个参数时第一个必须是 `-`。
- **退出码 6 且提到 `--session`**——那个会话 id 不存在。`--session` 不会新建会话。
- **退出码 6 且提到 `does not support tool calling`**——该 provider 不支持工具调用，用 `--no-tools`
  或者换一个 provider。
- **退出码 4 但答案看起来不完整**——`stop_reason` 是 `steps` 或 `tokens`：一轮内的预算用完了。
  提高 `--max-steps`，或提高 `chat.turn_max_tokens`。
- **stdout 是空的但退出码是 0**——`--output json` 时答案在对象里，不在流里。
