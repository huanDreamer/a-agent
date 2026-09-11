# huan-agent admin（Web UI）

huan-agent 的 Web 控制台：用量统计、调用记录、审计日志与技能开关。
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

用量类接口都支持 `since` / `until`（RFC3339）、`user`、`provider`、`model` 查询参数；
页面顶部的时间范围选择器（24小时 / 7天 / 30天 / 全部）会把这些查询统一换算成 `since`。

如果 API 不在站点根路径下，可以在 `index.html` 里于打包脚本之前设置
`window.__ADMIN_API_BASE__ = '/some/prefix'`。

## 设计约束

- **依赖极简**：运行时只有 `vue`，不引入图表库 / CSS 框架 / router / pinia / axios。
  图表全部是手写内联 SVG（`src/components/DailyTrendChart.vue`），状态用 reactive store
  （`src/state.js`），请求用 `fetch`（`src/api.js`）。
- **暗色主题**：与 `docs/status.html` 保持一致的色板（背景 `#0b0f17`、卡片 `#131a26`、
  边框 `#243044`、蓝色 `#58a6ff`、绿色 `#3fb950` 等），卡片圆角 14px。
- **中文界面**，响应式支持到约 380px 宽度。
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
    ├── format.js                  # 数字 / 费用 / 时长 / 时间格式化
    ├── metrics.js                 # 指标定义（调用数 / token / 费用）
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
        └── SkillsView.vue         # 技能开关
```
