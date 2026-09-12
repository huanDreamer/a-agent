# huan-agent admin（Web UI）

huan-agent 的 Web 控制台：对话、统计监控（总览 / 按模型 / 按用户 / 调用记录 / 审计日志 /
链路追踪）与设置（主题、服务信息、模型目录、工作区与工具、技能开关）。
Vue 3 + Vite 单页应用，构建产物由 Go 服务端通过 `//go:embed` 嵌入到二进制里。

**没有登录页**：`admin.require_login` 默认为 `false`，控制台打开即用。`GET /api/me` 只是
启动探针（关闭登录校验时返回 `200 {authenticated:false}`），不构成任何跳转理由。

## 快速开始

```bash
cd web

# 安装依赖（只有 vue / vite / @vitejs/plugin-vue）
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

所有请求都带 `credentials: 'same-origin'`（默认部署不需要 Cookie）。任何接口返回 401 时，
前端只在主区顶部显示一条说明（该部署把 `admin.require_login` 打开了，而本控制台不提供
登录流程），不会跳转、也没有登录表单。

| 方法   | 路径                            | 说明                                   |
| ------ | ------------------------------- | -------------------------------------- |
| GET    | `/api/me`                       | 启动探针；关闭登录校验时返回 200       |
| GET    | `/api/usage/summary`            | 汇总（调用数 / token / 费用 / 延迟）   |
| GET    | `/api/usage/by-model`           | 按模型聚合，`limit`                    |
| GET    | `/api/usage/by-provider`        | 按 provider 聚合，`limit`              |
| GET    | `/api/usage/by-user`            | 按用户聚合，`limit`                    |
| GET    | `/api/usage/by-day`             | 按天聚合，`days`                       |
| GET    | `/api/usage/recent`             | 最近调用明细，`limit`                  |
| GET    | `/api/audit`                    | 工具审计日志，`limit` / `tool` / `user` |
| GET    | `/api/skills`                   | 技能列表                               |
| POST   | `/api/skills/{name}`            | `{enabled:bool}` 开关技能             |
| GET    | `/api/meta`                     | provider / model / 版本 / 开关状态     |
| GET    | `/api/chat/models`              | 可选模型目录、工具名、system prompt    |
| GET    | `/api/chat/sessions`            | 会话列表，`limit`                      |
| POST   | `/api/chat/sessions`            | 新建会话 `{title,provider,model}`      |
| GET    | `/api/chat/sessions/{id}`       | 会话 + 全部消息                        |
| PATCH  | `/api/chat/sessions/{id}`       | 改名 / 换模型                          |
| DELETE | `/api/chat/sessions/{id}`       | 删除会话                               |
| POST   | `/api/chat/sessions/{id}/clear` | 清空消息（保留会话）                   |
| POST   | `/api/chat/sessions/{id}/messages` | **SSE** 流式对话（见下）            |
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

### 链路追踪

trace 由服务端代理读取，浏览器不会拿到 Langfuse 的 secret key。
`enabled:false`（未配置）与 HTTP 502（Langfuse 不可达）是两种不同的状态，会分别提示：
前者给出需要配置的 `langfuse.enable` / `host` / `public_key` / `secret_key` 与重启说明，
后者提示后端不可达并提供重试。

## 设计约束

- **依赖极简**：运行时只有 `vue`，不引入图表库 / CSS 框架 / router / pinia / axios。
  图表全部是手写内联 SVG（`src/components/DailyTrendChart.vue`），状态用 reactive store
  （`src/state.js`），请求用 `fetch`（`src/api.js`）。
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
- **对话里不展示工具清单**：可用工具只在 设置 → 工作区与工具 里作为只读信息列出。
- **手写渲染**：助手回答的 markdown 子集（``` 代码块、行内 `code`、**加粗**、换行）由
  `src/markdown.js` 解析成 token，再用文本节点渲染（不使用 `v-html`，模型输出无法注入标记）；
  trace 瀑布流由 `src/components/TraceWaterfall.vue` 用普通 DOM + 百分比定位手写，
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
    ├── App.vue                    # 两栏外壳 + 三个顶层面板路由
    ├── api.js                     # fetch 封装 + ApiError + 401 回调
    ├── state.js                   # reactive store（导航、子页、时间范围、刷新令牌、meta）
    ├── useResource.js             # 数据加载原语（loading/error/empty/ready）
    ├── format.js                  # 数字 / 费用 / 时长 / 时间 / JSON 格式化
    ├── metrics.js                 # 指标定义（调用数 / token / 费用）
    ├── sse.js                     # SSE 分帧解析（纯函数，可单独测试）
    ├── markdown.js                # markdown 子集解析（代码块 / 行内 code / 加粗）
    ├── chatStore.js               # 对话状态：会话列表、消息、流式一轮的生命周期
    ├── theme.js                   # 跟随系统 / 亮色 / 暗色（localStorage + <html> 开关）
    ├── ui.js                      # 壳层 UI 状态（移动端抽屉）
    ├── styles.css                 # 全站设计令牌 + 样式（亮/暗两套，无字面色值）
    └── components/
        ├── AppSidebar.vue         # 侧边栏：新建对话 / 会话列表（悬停操作）/ 设置 / 统计监控
        ├── ViewHead.vue           # 主区上下文头部（标题 + 该视图的工具条插槽）
        ├── ViewToolbar.vue        # 用量子页的时间范围 + 刷新（:range="false" 只留刷新）
        ├── DrawerButton.vue       # ≤900px 的抽屉开关（只在窄屏显示）
        ├── Icon.vue               # 内联图标（Lucide 几何 + 属性约定）
        ├── AsyncBlock.vue         # 骨架屏 / 错误重试 / 空状态
        ├── StatCard.vue           # 汇总卡片
        ├── DailyTrendChart.vue    # 手写 SVG 折线+面积图
        ├── BarList.vue            # 横向条形对比
        ├── TotalsTable.vue        # 聚合明细表（含费用列）
        ├── SettingsView.vue       # 设置（外观 / 服务信息 / 模型目录 / 工作区与工具）
        ├── SkillsPanel.vue        # 技能开关（设置的一个区块）
        ├── MonitorView.vue        # 统计监控（六个子页 + 用量工具条）
        ├── DashboardView.vue      # 总览（子页，只渲染内容）
        ├── ByModelView.vue        # 按模型（子页）
        ├── ByUserView.vue         # 按用户（子页）
        ├── RecentView.vue         # 调用记录（子页）
        ├── AuditView.vue          # 审计日志（子页）
        ├── ChatView.vue           # 对话（细头部 + 满高消息区 + 固定输入框）
        ├── ChatMessage.vue        # 消息气泡：思考过程 / 工具卡片 / 用量 / 复制
        ├── ChatComposer.vue       # 一体化输入区（textarea + 模型下拉 + 发送/停止）
        ├── MarkdownText.vue       # markdown 块级渲染（纯文本节点）
        ├── MarkdownInline.vue     # markdown 行内渲染
        ├── JsonBlock.vue          # 可折叠 JSON（null 安全 + 截断）
        ├── TraceWaterfall.vue     # 手写 observation 瀑布流
        └── TraceView.vue          # 链路追踪（子页：状态条 + 列表 + 详情）
```
