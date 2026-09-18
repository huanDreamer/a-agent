# huan-agent admin（Web UI）

huan-agent 的 Web 控制台：对话、统计监控（总览 / 按模型 / 按用户 / 调用记录 / 审计日志 /
链路追踪）与设置（外观 / 模型 / MCP / 技能 / 服务与工具，均为页内 sub-tab）。
Vue 3 + Vite 单页应用，构建产物由 Go 服务端通过 `//go:embed` 嵌入到二进制里。

**登录页只在需要时出现**：`admin.require_login` 默认为 `false`，控制台打开即用，不会看到任何
登录界面。开了校验的部署里，启动探针 `GET /api/me` 才回答 `login_required: true` —— 而且这个
回答是**针对这个访问者**的：`admin.trust_loopback`（默认 `true`）让来自 `127.0.0.1` / `::1`
的请求（也就是服务器本机上的浏览器）不必登录，只有别的机器来的才要密码。判断依据是**连接的
对端地址，不是 X-Forwarded-For**（请求头谁都能写）。控制台在挂载任何面板之前就按这个回答改画
**登录页**（一个密码输入框、没有账号字段 —— 见下）。所以探针必须在开屏时先跑完：否则每个面板
都会各自吃一个 401，页面先是半空再被替换。

## 页面

- **登录**：只对"会被要求登录"的访问者出现 —— 免登录部署、以及开了 `trust_loopback` 时的本机
  访问都不会看到它。单用户、只有一个密码字段；提交给 `POST /api/login`，会话 token 以 httpOnly
  Cookie 形式由浏览器保存，前端不持有它。
  侧栏底部显示登录名（`admin.username`）与 **退出**（`POST /api/logout`，然后整页重载 ——
  会话结束后页面上的会话列表、SSE 流和缓存都不该留下）；本机免登录的访问者没有会话，所以也
  没有这个按钮。会话存在服务端内存里，**服务重启会清空会话**：此时任意请求返回 401，控制台
  回到登录页并说明是会话过期，而不是当成首次访问。
- **对话**：会话列表 + 流式回答 + 工具卡片 + 附件（图片 / 音频）+ 输入框上方的**任务看板**。
- **统计监控**：总览 / 按模型 / 按用户 / 调用记录 / 审计日志 / 链路追踪，六个 sub-tab。
- **设置**：外观 / 模型 / MCP / OpenViking / 技能 / 服务与工具，六个 sub-tab —— 与
  统计监控同一套骨架（页头 + `.subtabs` + 单个滚动面板，当前分区存在
  `state.settings`）。

设置里两条能力链路的要点：

- **MCP** 的服务器存在 SQLite 里，保存后立即连接，工具注册进对话使用的同一个注册表
  （下一轮消息就能调用，不需要重启）；「测试连接」只拨号一次、不落盘也不注册，所以
  一份还没保存的定义不会先进运行时。配置文件的 `mcp.servers` 会被同步进列表，这些行
  只能切换启停，其余字段请改配置文件。
- **技能** 是一份 markdown 文件（YAML frontmatter + 正文）。启用的技能只会把
  **名字与描述** 写进对话的系统提示，正文由模型通过 `skill` 工具按需读取，避免每轮
  都把若干份指令全文塞进上下文。
- 两个分区各有一个 **配置助手**：用默认对话模型把一句自然语言（或粘贴的 README）
  变成一份配置草稿。草稿**只填进表单 / 编辑器，不会写盘** —— 模型最可能出错的地方是
  包名，让操作者过一眼比让模型自己写便宜。
- **左侧栏是工作区 + 会话**：每个工作区是一个文件夹，会话挂在它下面。

  - **工作区 = 一个本机目录**，每个会话都属于一个；新建时用 **目录选择器**
    （`DirPicker.vue` + `GET /api/fs/dirs`）浏览服务端文件系统并选中一个已存在的目录 ——
    浏览器拿不到服务端绝对路径，所以只能由服务端列出目录。目录不存在 / 是文件 / 是 `/`
    都会被拒绝并说明原因。
  - 选择器是 **macOS 式分栏（Miller columns）**：每一栏是一个目录的子目录，**点击文件夹后，
    它的内容出现在右侧相邻的新栏**，被点的那个在它自己那一栏里高亮 —— 于是「正要选中的目录」
    永远是「你能看到内容的那个」。再点一次已经在链上的文件夹＝把它右边的栏收起（和 Finder
    一样，并且不会跳到它的父目录）；整条链一起高亮；最右一栏就是当前选中目录的内容。
    路径输入框用于跳到任意绝对路径（`/tmp` 这类符号链接会按服务端解析后的路径显示）。
    `/api/fs/dirs` 每个栏一次请求。
  - 文件夹头可以直接 **新建会话（在该工作区里）、重命名、删除**；重命名只改标签，不动目录；
    删除是两步确认，且**不删除目录里的文件** —— 里面的会话会被移动到另一个工作区，回复里
    写明去了哪里（只剩一个工作区时拒绝删除，因为会话必须有归属）。
  - 目录被删掉的工作区会在列表里标出「目录已不存在」，而不是等到某次工具调用才失败。
  - 会话在工作区之间移动：**对话页顶部**的选择器（title 显示完整目录）。切换失败时选择器
    退回原值 —— 不能让界面显示一个其实没有生效的边界。
  - 工作区**不带策略**：只读 / 允许命令是全局的 `tools.read_only` / `tools.enable_bash`，
    所以这些都是设置页里没有的东西。

## 检查

```bash
cd web
npm run check:ui      # 组件绑定守卫 + 渲染探针（见下）
```

两条检查针对的都是**构建与 lint 看不到**的问题：

- `scripts/check-component-bindings.mjs`：`<script setup>` 里与 prop **同名**的局部绑定会
  在模板中胜出。于是 `v-if="open"` 里的 `open` 变成了一个函数引用（永远为真）——目录选择器
  曾经因此一进页面就弹出来、并且关不掉。同一个陷阱的另一半是模板把 Boolean prop 当函数调用
  （`@click="open(...)"`），只在点击时才炸。两者都在这里静态检查。
- `ssr-probe/`：把组件渲染成 HTML 并断言，例如「`open=false` 时不应该有对话框」「免登录的
  侧栏不该出现『退出』」「首次访问不应自称会话过期」「助手气泡默认把过程折叠成一行、答案在过程之下、老数据退化成
  一个块」。它只覆盖首帧；**点击之后**的行为要靠下面的浏览器验证。屏门的三个分支（免登录 /
  需要登录 / 已登录 / 探针未返回）在这里按 `needsLogin` 的真值表断言 —— 这是整个控制台唯一
  一处「画哪个屏幕」的判断。

更完整的浏览器验证（真实 Chrome + CDP，驱动真实服务端）：

```bash
bash web/.verify/step-run.sh    # 一轮回答的步骤展示（24 项流程断言 + 21 项布局断言）
bash web/.verify/ws-run.sh     # 工作区文件夹与目录选择器（26 项断言）
bash web/.verify/mm-run.sh     # 模型管理与对话（既有）
```

`step-run.sh` 值得单独说明一句：它跑一个脚本化的 OpenAI mock（`step-mock.mjs`，两步：
思考 → 说明 → `list_dir`，再思考 → 回答），并且**把每段文字放慢到 200ms 以上**，因为
「过程在生成时就是展开的」这条只能在一轮还没结束时看；只看结束态的话，「一边生成一边展示」
和「结束后补渲染」长得一模一样。它之后还会跑 `step-layout.mjs`，用浏览器里的几何量断言
折叠后的步骤标题是一行文字高、正文缩进在标题之下、答案始终在过程之下、暗色主题下颜色都
解析得出来（用 canvas 采样，因为色值是 `oklch()`，正则会把 alpha 当成颜色分量）。

`web/.verify/` 是本地验证脚本（已 gitignore），不参与构建。

## 快速开始

```bash
cd web

# 安装依赖（运行时只有 vue / marked / dompurify）
npm install

# 开发模式（默认 http://127.0.0.1:5173，/api 会代理到 :8080）
npm run dev

# 构建：输出到 internal/server/webui/dist（会被 Go 服务端嵌入）
npm run build

# 本地预览构建产物
npm run preview
```

## 构建产物位置

`vite.config.js` 里设置了：

```js
base: './'                                     // 相对路径，服务端可挂在任意前缀下
build.outDir: '../internal/server/webui/dist'  // Go embed 的目标目录
build.assetsDir: 'assets'
build.emptyOutDir: true
```

因此 `npm run build` 会生成：

```
internal/server/webui/dist/index.html
internal/server/webui/dist/assets/index-<hash>.js
internal/server/webui/dist/assets/index-<hash>.css
```

**这个目录必须提交到 git**（Go 服务端 `internal/server/webui` 通过 `//go:embed dist`
嵌入它），而 `web/dist/` 只是本地预览用的临时目录，已被 `.gitignore` 忽略。
改动前端后请重新执行 `npm run build` 并提交 `internal/server/webui/dist/`。

## 与后端的接口

所有请求都带 `credentials: 'same-origin'`，所以开启登录校验的部署会带上会话 Cookie（免登录
的默认部署没有 Cookie 可带）。数据接口返回 401 意味着**手里这个会话已经不被接受**：控制台回到
登录页并说明「登录状态已失效」（服务端重启就会这样，会话在内存里）。唯一不吃这个规则的是
登录请求本身 —— `POST /api/login` 的 401 是「密码不对」，由表单自己处理，不能当成会话过期。

有一种 401 是例外：探针报告**这个访问者不需要登录**，却仍然收到 401（反向代理在要求认证、或
服务端刚换了配置 —— 例如把 `trust_loopback` 关掉后重启）。没有表单可给，于是主区顶部显示一条
说明与「重试」。

| 方法   | 路径                            | 说明                                   |
| ------ | ------------------------------- | -------------------------------------- |
| GET    | `/api/me`                       | 启动探针；`{authenticated, login_required, username?}`，永远 200。`login_required` 说的是**这个访问者**要不要登录（`require_login` 且未被 `trust_loopback` 豁免） |
| POST   | `/api/login`                    | `{password}` → 会话 Cookie；密码错误 / 触发限流返回 401 |
| POST   | `/api/logout`                   | 吊销会话并过期 Cookie                  |
| GET    | `/api/usage/summary`            | 汇总（调用数 / token / 费用 / 延迟）   |
| GET    | `/api/usage/by-model`           | 按模型聚合，`limit`                    |
| GET    | `/api/usage/by-provider`        | 按 provider 聚合，`limit`              |
| GET    | `/api/usage/by-user`            | 按用户聚合，`limit`                    |
| GET    | `/api/usage/by-day`             | 按天聚合，`days`                       |
| GET    | `/api/usage/recent`             | 最近调用明细，`limit`                  |
| GET    | `/api/audit`                    | 工具审计日志，`limit` / `tool` / `user` |
| GET    | `/api/skills`                   | 技能列表（含文件名 / 字节数 / 声明的工具） |
| POST   | `/api/skills/{name}`            | `{enabled:bool}` 开关技能             |
| GET    | `/api/skills/{name}`            | 技能正文（编辑器读它）                 |
| PUT    | `/api/skills/{name}`            | 写技能文件 `{description,tools,body}`（原子写） |
| DELETE | `/api/skills/{name}`            | 删除技能文件并清掉停用记录             |
| POST   | `/api/skills-draft`             | 技能写作助手：`{description,name?}` → 草稿（不落盘） |
| GET    | `/api/mcp/servers`              | MCP 服务器列表（定义 + `runtime` 实时状态） |
| POST   | `/api/mcp/servers`              | 创建 / 编辑 / 启停（`overwrite` 才允许覆盖定义） |
| DELETE | `/api/mcp/servers/{id}`         | 删除（config 来源拒绝）                |
| POST   | `/api/mcp/servers/{id}/test`    | 测试已保存的服务器：连接 → 列工具 → 断开 |
| POST   | `/api/mcp/probe`                | 测试未保存的定义（不写库、不注册）     |
| POST   | `/api/mcp/reload`               | 重连全部已启用服务器                   |
| POST   | `/api/mcp/draft`                | MCP 配置助手：`{description,document?}` → 草稿（不落盘） |
| GET    | `/api/meta`                     | provider / model / 版本 / 开关状态     |
| GET    | `/api/chat/models`              | 可选模型目录、工具名、system prompt    |
| GET    | `/api/chat/sessions`            | 会话列表，`limit`                      |
| POST   | `/api/chat/sessions`            | 新建会话 `{title,provider,model}`      |
| GET    | `/api/chat/sessions/{id}`       | 会话 + 全部消息                        |
| PATCH  | `/api/chat/sessions/{id}`       | 改名 / 换模型                          |
| DELETE | `/api/chat/sessions/{id}`       | 删除会话                               |
| POST   | `/api/chat/sessions/{id}/clear` | 清空消息（保留会话）                   |
| POST   | `/api/chat/sessions/{id}/messages` | **SSE** 流式对话（见下）            |
| POST   | `/api/chat/sessions/{id}/resume` | 接着上一轮的中断处续跑（202 + 与 messages 同形状的 turn；409 已有一轮在跑，400 没有可接续的东西） |
| POST   | `/api/chat/sessions/{id}/questions/{qid}/answer` | 回答 `ask_user` 卡片 `{selected,text}` |
| GET    | `/api/traces/status`            | 追踪开关 + 上报计数器                  |
| GET    | `/api/traces`                   | trace 列表，`limit` / `page` / `session` / `user` / `name` |
| GET    | `/api/traces/{id}`              | trace 详情 + observation 树            |

用量类接口都支持 `since` / `until`（RFC3339）、`user`、`provider`、`model` 查询参数。
时间范围选择器（24小时 / 7天 / 30天 / 全部）与「刷新」是 **统计监控** 的工具条，只挂在
总览 / 按模型 / 按用户 / 调用记录 四个用量子页上（它们把范围换算成 `since`，趋势图换算成
`days`）；审计日志的接口没有时间过滤，只给「刷新」；链路追踪用自己的过滤条件。

如果 API 不在站点根路径下，可以在 `index.html` 里于打包脚本之前设置
`window.__ADMIN_API_BASE__ = '/some/prefix'`。

### 流式对话（SSE）

`POST /api/chat/sessions/{id}/messages` 返回 `text/event-stream`。`EventSource` 只能发 GET，
所以这里用 `fetch` + `response.body.getReader()` 自己解析 SSE 分帧（`src/sse.js`）：
按空行切帧、忽略 `: ping` 心跳、跨 chunk 缓冲半个帧、`stream_end` 结束读取。
`done` 事件的 `text` 是最终答案（权威值，会覆盖已流式输出的内容）；
停止按钮用 `AbortController` 中断请求，服务端仍会保存已经产出的内容，
因此中断后重新加载依然能看到这一轮。

**提问卡片（`ask_user` 事件）**：模型调用 `ask_user` 工具时会发出该事件，本轮**挂起**等待回答。事件有两种形态，用字段区分：带 `ask` 的是新问题（`ask_status: "pending"`，卡片出现，选项/自定义输入可用），
带 `ask_id` 的是结局（`ask_status` 为 `answered` / `timeout` / `cancelled`，`answered` 时另有 `ask_answer`）。
提交走上面那条独立的 POST —— 流式响应已经被这一轮的事件流占用，提交只能另开一个请求，问题 id 是两者的连接点。
超时或本轮被中断（停止 / 刷新 / 关标签页）时模型收到「用户没有回答」并自行收口，卡片会显示对应说明；
已经结束的问题再提交会得到 404，卡片据此改为「已取消」而不是反复失败。刷新后卡片由该轮 assistant 消息里
持久化的工具调用（参数=问题、结果=答案）复原（`src/ask.js` 的 `askFromTool`）。

**计划与重试（`plan` / `step_retry` 事件）**：`{"type":"plan","step":n,"plan":{…}|null}` 每次计划变更
发一次，客户端把它归一化后放进 `chat.plan`，看板据此重画（计划不属于某一轮，所以事件落在 store
上而不是这个 turn 上；刷新后由 `GET /api/chat/sessions/{id}` 的 `plan` 字段复原）。
`{"type":"step_retry","step":n,"attempt":2,"max_attempts":3,"delay_ms":1600,"error":"unexpected EOF"}`
表示**这一步**的模型调用失败、服务端马上重跑它：客户端必须把该 step 已经收到的 `text` / `reasoning`
清空（本地 reducer 里 `turn.text` 也要回退，`turn.reasoning` 只回退被丢弃的那一段——前面几步的思考
是真的发生过的事），并显示一行「第 2/3 次尝试，1.6s 后重试（unexpected EOF）」。不清空的话，重试后
的答案会接在失败的那半截后面，看起来像模型自己重复了一遍。

### 链路追踪

trace 由服务端代理读取，浏览器不会拿到 Langfuse 的 secret key。
`enabled:false`（未配置）与 HTTP 502（Langfuse 不可达）是两种不同的状态，会分别提示：
前者给出需要配置的 `langfuse.enable` / `host` / `public_key` / `secret_key` 与重启说明，
后者提示后端不可达并提供重试。

## 设计约束

- **依赖极简**：运行时只有 `vue` + `marked`（GFM 解析）+ `dompurify`（净化），不引入图表库 /
  CSS 框架 / router / pinia / axios，也没有语法高亮库（后续单独做）。图表全部是手写内联 SVG
  （`src/components/DailyTrendChart.vue`），状态用 reactive store（`src/state.js`），请求用
  `fetch`（`src/api.js`）。
- **主题与设计令牌**：抄自 DeepSeek harness 的 shadcn 风格中性色板，全部用 `oklch`
  定义在 `src/styles.css` 的 `:root` 里（`--background` / `--foreground` / `--card` /
  `--muted` / `--muted-foreground` / `--border` / `--ring` / `--destructive` …），
  `--radius: .625rem`。**样式表里没有任何字面色值**：语义色（`--success` / `--warning` /
  `--info` / `--accent-purple` 及图表色 `--chart-1..3`）也是令牌，亮/暗各一套。
- **亮色为默认**：`src/theme.js` 按「localStorage 里的显式选择 → `prefers-color-scheme`
  → 亮色」解析，并把 `class="dark"` 与 `data-theme` 写到 `<html>`；`styles.css` 同时保留
  `@media (prefers-color-scheme: dark)` 分支（首屏 / 无 JS 时不闪错主题），显式选择优先。
  主题切换在 **设置 → 外观**（跟随系统 / 亮色 / 暗色，当前项带对勾），选择持久化。
- **图标**：`src/components/Icon.vue` 手写内联 SVG，几何与属性约定（`viewBox="0 0 24 24"`、
  `fill="none"`、`stroke="currentColor"`、`stroke-width="2"`、圆角端点/连接）跟随
  Lucide —— 也就是 harness 使用的那套图标；不引入任何图标库。
- **中文界面**，响应式支持到约 380px 宽度。
- **布局：对话优先、满屏高**：整壳高 `100svh`（回退 `100vh`），只有内部面板滚动。
  左侧 264px 侧边栏只有三块：顶部「新建对话」按钮、占满剩余高度并可滚动的会话列表、底部
  两个入口「设置」与「统计监控」。没有品牌、没有 provider/model 行、没有账号行、没有退出。
  900px 以下侧边栏变成抽屉，由主区上下文头部的按钮打开。
- **会话行操作只在悬停 / 键盘聚焦时出现**：`.session-actions` 常驻 DOM 但
  `visibility: hidden`（不是只把 opacity 调 0——那样点击仍会落在看不见的按钮上，且会参与
  命中测试），行 `:hover` 或 `:focus-within` 时显现；操作是行的绝对定位浮层，出现时不改变
  列表高度。键盘用户 Tab 进某一行时 `:focus-within` 已生效，因此下一次 Tab 就能落到按钮上。
  ≤900px 没有 hover，媒体查询里直接把它们显示出来。
- **输入框与模型选择器合为一体**：一个圆角容器里，上面是自动增高的 textarea（静止约 4 行，
  96px 起，长到 260px 后内部滚动），下面一行左侧是 `provider / model` 下拉 chip、右侧是
  圆形 `--primary` 发送按钮（流式期间换成「停止」）。容器 `:focus-within` 时出现焦点环。
  输入框回车发送、Shift+Enter 换行，中文输入法组字期间回车不发送。
- **任务看板贴在输入框正上方**（`TaskBoard.vue` + `src/plan.js`）：模型执行长任务时会用
  `plan_create` / `plan_update` 建立并推进一份清单，看板把它实时画出来——目标（单行省略）+
  进度 `n/m` + 细进度条 + 三组：**执行中**（`in_progress`）、**待执行**（`pending`，`failed`
  也在这一组且标红并写出原因，`skipped` 划掉）、**已完成**（`done`）。没有计划（`plan` 为
  `null`，含"计划里没有任务"）时整个组件不渲染。
  它的位置是刻意的：计划属于"这一轮还没做完的事"，不属于消息历史——放进消息流会被新消息
  推走。高度有上限（约 180px，条目多了组列表内部滚动），因为它是常驻的。分组顺序、进度、
  一行摘要都是 `plan.js` 的纯函数（`normalizePlan` 容忍脏数据：未知 status 当待执行、缺 id
  按序号补 `t1/t2`、没标题的整条丢掉），所以它们能在没有浏览器时被断言。
  折叠状态（`chat.planCollapsed`）在 store 里，切标签页不会丢。
- **「继续执行」出现在两个地方，条件是同一个**（计划还有未完成项、并且没有轮次在跑）：
  看板头部右侧，以及**失败**的助手气泡上（与「重试」并列——重试是重发原话，继续执行是
  接着上一轮的中断处跑，已经做完的步骤不会重做）。点击走 `POST .../resume`，与发消息同一
  流程：先出现用户气泡「继续执行」，被拒绝（409 / 400）就把气泡撤掉并写出原因，绝不留下
  一条其实没有发出去的消息。
- **对话里不展示工具清单**：可用工具只在 设置 → 服务与工具 里作为只读信息列出，
  并且那是模型**真正**能调用的集合（内置 + MCP 服务器提供的 + `skill`）。
- **助手回答的 Markdown**：`src/markdown.js` 的 `renderMarkdown()` 用 `marked`（GFM）解析、
  用 `DOMPurify` 净化，**同一个函数**里完成「解析 + 净化」并返回安全的 HTML 字符串；只有
  `src/components/MarkdownText.vue` 通过 `v-html` 渲染它，全仓库没有第二条通往 `v-html` 的
  路径（这是文件开头写明的不变式）。净化配置显式禁用 `script` / `style` / `iframe` /
  `object` / `embed` / `form` / `input` / `button` / `link` / `meta` / `base` / `img`（模型的
  `<img src="http://…">` 是追踪信标）与所有 `on*` 事件属性，URL 只允许 `http` / `https` /
  `mailto` 及无 scheme 的相对写法，因此 `javascript:` / `data:` / `vbscript:` 会被剥掉；
  每个 `<a>` 由 `afterSanitizeAttributes` 钩子强制加上 `target="_blank"`、
  `rel="noopener noreferrer"` 与 `md-link` class。代码块的「语言标签 + 复制」、表格的滚动
  容器、任务列表的复选框都是**净化之后**在 DOM 上生成的（`createElement` / `textContent`），
  所以 `button` / `input` 可以一直留在禁用列表里。用户自己的消息仍是纯文本（`{{ }}` 插值，
  不走 markdown）；思考过程同样按 markdown 渲染，只是字号更小。
  **流式**：代码围栏未闭合时 `marked` 本来就按代码块渲染，不会漏出反引号；只在仍处于流式
  时补齐「表格分隔行」的列数（`:?-+:?`），避免表格在几个 delta 里退回成管道符段落。结果按
  输入串记忆化（`Map`，LRU 上限 80）。实测 5.6 KB 消息：冷渲染中位 0.9ms、p95 1.2ms，
  命中缓存 0.013ms（约 70 倍）。
- **提问卡片（`AskUserCard.vue`）不经过 MarkdownText**：问题按纯文本插值（`white-space: pre-wrap`
  保留换行）。这既是「唯一 `v-html` 出口」的收紧，也让卡片能被 SSR 探针渲染。步骤块里的
  思考 / 过程文字走 MarkdownText，因此探针构建 `ssr-probe/` 时把 `MarkdownText.vue` 换成
  `ssr-probe/markdowntext-stub.js`（纯文本渲染）—— 否则 `markdown.js` 会因为没有 DOM 在
  import 阶段就抛错（见下条）。
  卡片的两种来源（实时事件、刷新后的工具调用）在 `src/ask.js` 里归一成同一个模型，组件只认这一个输入。
- **`MarkdownText.vue` 在探针里被替换**：`src/markdown.js` 的 `renderMarkdown()` 会在净化之后
  用 `document.createElement` 给代码块和表格加装饰，所以它无法在 Node 里渲染。探针要断言
  的正是「一轮回答的结构」（`ChatMessage.vue` 的所有者），于是 `ssr-probe/vite.config.js`
  把该组件别名到 `ssr-probe/markdowntext-stub.js`：同样的 props，纯文本渲染。全站唯一的
  `v-html` 出口、净化规则和浏览器构建都不受影响。
- **一轮回答按步骤渲染**（`ChatMessage.vue` + `src/steps.js`）：助手气泡 = 上面折叠起来的
  「执行过程」+ 下面独立的答案。
  - 折叠时只有一行：`执行过程 · N 次工具调用 · M 条消息`（外加失败标记与工具合计耗时）。
    「M 条消息」就是这一轮的 ReAct 迭代数 —— 每次迭代对应一条 assistant 消息；两个计数与
    下面能展开的内容同口径（`ask_user` 不算工具调用，它是卡片）。
  - 展开后是每个 ReAct 步骤一块：标题是摘要行（跑了哪些工具、耗时），里面是该步的思考与
    它说的话，工具卡片再各有一层折叠（长任务里卡片才是体积的大头）。
  - 折叠规则在 `steps.js` 的 `processShouldBeOpen()`：跑着时展开、结束后折叠，读者点过之后
    由读者说了算（`pinned`，只记这一次挂载）。所以「执行结束就把步骤和思考收起来、只看结果」
    是默认行为，而不是需要点一下的状态。`step_end` 把该步文字标记为过程（不进入答案）。
  - 步骤自身的状态（`open` / `touched`）挂在步骤对象上，所以 `steps.js` 的 `visibleSteps()`
    返回的是调用方自己的对象、只派生「这一步显示哪些工具」，绝不复制 —— 复制会让点击落在
    一次性的拷贝上（这个 bug 在浏览器验证里被发现过）。
  老数据（migration 13 之前没有 `steps` 列）退化成「整轮一个块」：那份「哪句话促成了哪个
  调用」的配对信息当时没有写下来，退化是如实陈述而不是渲染器的缺陷。
- trace 瀑布流由 `src/components/TraceWaterfall.vue` 用普通 DOM + 百分比定位手写，
  缺失时间戳 / 时长为 0 / 父节点成环都退化为满宽或根节点，不会出现 NaN 宽度。
- **不留白面板**：每个数据视图都有骨架屏、错误重试条和「暂无数据」空状态
  （`src/components/AsyncBlock.vue`）。
- 费用为 `cost.priced === false` 时显示 `—`（表示没有匹配到价格表，费用未知而非 0）。

## 目录结构

```
web/
├── index.html                     # 挂载点，标题 "huan-agent admin"
├── vite.config.js                 # base/outDir/assetsDir 配置
├── package.json
└── src/
    ├── main.js                    # createApp
    ├── App.vue                    # 屏门（探针 → 登录页 / 两栏外壳）+ 三个顶层面板路由
    ├── api.js                     # fetch 封装 + ApiError + 401 回调（登录请求自带处理）
    ├── state.js                   # reactive store（导航、子页、时间范围、刷新令牌、meta、会话）
    ├── useResource.js             # 数据加载原语（loading/error/empty/ready）
    ├── format.js                  # 数字 / 费用 / 时长 / 时间 / JSON 格式化
    ├── metrics.js                 # 指标定义（调用数 / token / 费用）
    ├── sse.js                     # SSE 分帧解析（纯函数，可单独测试）
    ├── markdown.js                # 唯一的 markdown → 安全 HTML 入口（marked + DOMPurify）
    ├── icons.js                   # 图标几何数据（Icon.vue 与代码块复制按钮共用）
    ├── chatStore.js               # 对话状态：会话列表、消息、流式一轮的生命周期
    ├── ask.js                     # ask_user 卡片模型（实时事件与历史工具调用归一）
    ├── plan.js                    # 任务看板模型（计划归一化 / 分组 / 进度 / 摘要，纯函数）
    ├── steps.js                   # 一轮回答的步骤模型（持久化 JSON / 实时事件归一 + 摘要与折叠规则）
    ├── theme.js                   # 跟随系统 / 亮色 / 暗色（localStorage + <html> 开关）
    ├── ui.js                      # 壳层 UI 状态（移动端抽屉）
    ├── styles.css                 # 全站设计令牌 + 样式（亮/暗两套，无字面色值）
    └── components/
        ├── AppSidebar.vue         # 侧边栏：工作区文件夹（新建/重命名/删除）+ 会话列表 + 设置/统计监控 + 账号/退出
        ├── LoginView.vue          # 登录页（仅 require_login: true 的部署；单用户，只有密码字段）
        ├── ViewHead.vue           # 主区上下文头部（标题 + 该视图的工具条插槽）
        ├── ViewToolbar.vue        # 用量子页的时间范围 + 刷新（:range="false" 只留刷新）
        ├── DrawerButton.vue       # ≤900px 的抽屉开关（只在窄屏显示）
        ├── Icon.vue               # 内联图标（Lucide 几何 + 属性约定）
        ├── AsyncBlock.vue         # 骨架屏 / 错误重试 / 空状态
        ├── StatCard.vue           # 汇总卡片
        ├── DailyTrendChart.vue    # 手写 SVG 折线+面积图
        ├── BarList.vue            # 横向条形对比
        ├── TotalsTable.vue        # 聚合明细表（含费用列）
        ├── SettingsView.vue       # 设置（外观 / 模型 / MCP / OpenViking / 技能 / 服务）
        ├── DirPicker.vue          # 目录选择器（浏览服务端文件系统，供新建工作区用）
        ├── SkillsPanel.vue        # 技能开关（设置的一个区块）
        ├── MonitorView.vue        # 统计监控（六个子页 + 用量工具条）
        ├── DashboardView.vue      # 总览（子页，只渲染内容）
        ├── ByModelView.vue        # 按模型（子页）
        ├── ByUserView.vue         # 按用户（子页）
        ├── RecentView.vue         # 调用记录（子页）
        ├── AuditView.vue          # 审计日志（子页）
        ├── ChatView.vue           # 对话（细头部 + 满高消息区 + 任务看板 + 固定输入框）
        ├── ChatMessage.vue        # 消息气泡：步骤块（思考 + 工具卡片）/ 答案 / 用量 / 复制
        ├── ChatComposer.vue       # 一体化输入区（textarea + 模型下拉 + 发送/停止）
        ├── TaskBoard.vue          # 任务看板（输入框上方：目标 + 进度 + 执行中/待执行/已完成 + 继续执行）
        ├── AskUserCard.vue        # 模型的提问卡片（选项 / 多选 / 自定义输入 / 提交 / 已答 / 超时）
        ├── MarkdownText.vue       # markdown 渲染（v-html 唯一出口 + 代码块复制委托）
        ├── JsonBlock.vue          # 可折叠 JSON（null 安全 + 截断）
        ├── TraceWaterfall.vue     # 手写 observation 瀑布流
        └── TraceView.vue          # 链路追踪（子页：状态条 + 列表 + 详情）
```
