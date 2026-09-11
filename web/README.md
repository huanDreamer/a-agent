# huan-agent admin（Web UI）

huan-agent 的 Web 控制台：对话、用量统计、调用记录、审计日志、技能开关与链路追踪。
Vue 3 + Vite 单页应用，构建产物由 Go 服务端通过 `//go:embed` 嵌入到二进制里。

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

所有请求都带 `credentials: 'same-origin'`，登录态由 `POST /api/login` 下发的 Cookie 维持。
任何接口返回 401 时，前端会立即回到登录页。

| 方法   | 路径                            | 说明                                   |
| ------ | ------------------------------- | -------------------------------------- |
| POST   | `/api/login`                    | `{password}` → `{ok, username}`        |
| POST   | `/api/logout`                   | 退出登录                               |
| GET    | `/api/me`                       | 当前会话状态                           |
| GET    | `/api/health`                   | 版本与运行时长                         |
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

用量类接口都支持 `since` / `until`（RFC3339）、`user`、`provider`、`model` 查询参数；
页面顶部的时间范围选择器（24小时 / 7天 / 30天 / 全部）会把这些查询统一换算成 `since`。

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
- **暗色主题**：与 `docs/status.html` 保持一致的色板（背景 `#0b0f17`、卡片 `#131a26`、
  边框 `#243044`、蓝色 `#58a6ff`、绿色 `#3fb950` 等），卡片圆角 14px。
- **中文界面**，响应式支持到约 380px 宽度；对话框在 900px 以下把会话列表收成抽屉。
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
    ├── App.vue                    # 登录态切换 + 标签页路由
    ├── api.js                     # fetch 封装 + ApiError + 401 回调
    ├── state.js                   # reactive store（登录、时间范围、刷新令牌）
    ├── useResource.js             # 数据加载原语（loading/error/empty/ready）
    ├── format.js                  # 数字 / 费用 / 时长 / 时间 / JSON 格式化
    ├── metrics.js                 # 指标定义（调用数 / token / 费用）
    ├── sse.js                     # SSE 分帧解析（纯函数，可单独测试）
    ├── markdown.js                # markdown 子集解析（代码块 / 行内 code / 加粗）
    ├── chatStore.js               # 对话状态：会话列表、消息、流式一轮的生命周期
    ├── styles.css                 # 全站样式（暗色主题）
    └── components/
        ├── AppHeader.vue          # 顶栏：品牌、meta、时间范围、刷新、退出、标签页
        ├── AsyncBlock.vue         # 骨架屏 / 错误重试 / 空状态
        ├── StatCard.vue           # 汇总卡片
        ├── DailyTrendChart.vue    # 手写 SVG 折线+面积图
        ├── BarList.vue            # 横向条形对比
        ├── TotalsTable.vue        # 聚合明细表（含费用列）
        ├── LoginView.vue          # 登录
        ├── DashboardView.vue      # 总览
        ├── ByModelView.vue        # 按模型
        ├── ByUserView.vue         # 按用户
        ├── RecentView.vue         # 调用记录
        ├── AuditView.vue          # 审计日志
        ├── SkillsView.vue         # 技能开关
        ├── ChatView.vue           # 对话（会话侧栏 + 流式消息区）
        ├── ChatSidebar.vue        # 会话列表 + 改名 / 清空 / 删除（二次确认）
        ├── ChatMessage.vue        # 消息气泡：思考过程 / 工具卡片 / 用量 / 复制
        ├── ChatComposer.vue       # 输入框（Enter 发送、Shift+Enter 换行、停止）
        ├── MarkdownText.vue       # markdown 块级渲染（纯文本节点）
        ├── MarkdownInline.vue     # markdown 行内渲染
        ├── JsonBlock.vue          # 可折叠 JSON（null 安全 + 截断）
        ├── TraceWaterfall.vue     # 手写 observation 瀑布流
        └── TraceView.vue          # 链路追踪（状态条 + 列表 + 详情）
```
