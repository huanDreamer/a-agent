# Phase 25 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

## 实施记录（2026-09-19）

**已完成并验证**：全部 6 节 —— 存储、索引、路由、工具、命令层接线、控制台。
**未完成 / 未验证**：`save_artifact` 的工具级真实调用（环境无 LLM provider），
以及 `..` 穿越这条攻击路径没有真正打到守卫（原因见第 3 节）。**改动未提交。**

### 第 1 节：存储（`internal/artifact`）

- [x] `Store` / `New` / `Options`：root 为空时报 `ErrDisabled` 而不是替调用方选一个
      目录；root 自身解析符号链接（macOS 上临时目录在 `/var` → `/private/var`，
      不解析会让包含性检查把下面的每个文件都拒掉）
- [x] 落盘布局 `<session>/<时间戳>-<slug>.<扩展名>`，可被 `ls` 直接读懂；会话作为
      路径前缀，让"本会话的产物"是目录操作而不是查询
- [x] `Resolve`：拒 NUL、先 clean、拒绝对路径、拒绝 `..`、在最长已存在前缀上解析
      符号链接后**再查一次**；任何逃逸返回 `ErrOutsideRoot` 而不是夹回root
- [x] 扩展名白名单（11 个）在写入时拒绝，不是存下来以后再操心
- [x] `MIMEByExt` / `MIMEForPath` 导出：写路径与读取路由必须对"这个文件是什么"
      给出同一个答案
- [x] `slugify`：中文标题**故意**返回空串，由随机 token 命名文件，而不是拼音转写
      （多一个依赖，还是一次猜测）
- [x] `writeAtomic`：同目录临时文件 + rename（跨设备 rename 不是原子的），
      多读 1 字节以便区分"正好到上限"和"超了"
- [x] `sessionComponent`：会话 id 只允许字母数字下划线连字符，含路径形状即拒绝
- [x] 测试 19 个用例：会话与类型、中文标题、默认 HTML、kind 定扩展名、无会话落
      root、超限拒绝、不支持类型拒绝、路径形状会话拒绝、逃逸拒绝、符号链接逃逸
      拒绝（**真正打到守卫的那条**）、目录拒绝、删不存在的文件不算错、slug、
      rune 截断、URL 转义、MIME 表

### 第 2 节：索引（`internal/store/artifact.go` + migration v15）

- [x] migration v15 `create_artifacts`：独立表而不是往 `media_assets` 里加行
      （方向相反：人上传 vs 智能体产出；字节位置也不同）
- [x] 列 `id/session_id/title/kind/path/mime/bytes/source/created_at`，
      `session_id` 与 `created_at` 两个索引
- [x] `CreateArtifact` 要求 id 与 path 非空；`GetArtifact` 缺席返回 `ErrNotFound`；
      `DeleteArtifact` 删第二次返回 `ErrNotFound` 而不是静默成功
- [x] `ListArtifacts`：`sessionID` 有值则本会话，空则全库；`created_at DESC, id DESC`
      —— 与 `ListMediaAssets` 正相反，这里是画廊，刚产出的是读者要的
- [x] `Saver`（`internal/artifact/saver.go`）：字节先、行后；插入失败则删掉刚写的
      文件并 warn，绝不留下列表指向 404 的幽灵条目

### 第 3 节：路由与响应头（`internal/server/artifacts.go`）

- [x] 四条路由：`GET /api/artifacts?session=`、`GET/DELETE /api/artifacts/:id`、
      `GET /api/artifacts/files/*path`
- [x] 文件路由注册在开放组、在 handler 里自己判会话：是否需要登录是配置决定
      （`artifacts.public_urls`），路由级中间件表达不了，否则要把同一路径注册两次
- [x] `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups
      allow-modals allow-downloads`（**不含 `allow-same-origin`**）+ `nosniff`：
      模型写的页面在不透明源里运行，拿不到控制台的 cookie / localStorage / API
- [x] `Cache-Control: private, max-age=300`：产物存下即不变，URL 不带版本号
- [x] `SetBodyStream`：32 MiB 的产物不读进内存再发
- [x] 关闭时三个 JSON 端点（list / get / delete）回答
      `{ok:true, enabled:false, message:...}`，措辞由同一个函数
      `artifactsDisabledResponse` 产出，且**不返回 404**（客户端分不清 404 和
      部署坏了）。文件路由是例外：它返回 404 `artifacts are not enabled`——要一个
      不存在的文件本身就是 404，控制台是从列表读到 `enabled` 的
- [x] `withTurnArtifacts`：按轮发布 saver，会话决定归属
- [x] 测试 9 个 HTTP 级用例，含端到端闭环：真实 `Saver` 写入 → 从 URL 读回字节 →
      列表带同一 URL → 删除同时清掉行与文件
- [!] **`..` 穿越用例是单向断言**：Hertz 在匹配前就规范化了 `..`，未编码的穿越请求
      根本进不到 handler（返回 SPA 的 index.html，1003 字节，未泄漏）。因此那条用例
      证明的是"不会泄漏"，不是"守卫拒绝"。真正打到守卫的是符号链接那条（已在真实
      二进制上验证被拒并留下 warn 日志）

### 第 4 节：工具（`internal/tool/artifact.go` + `builtin/artifact.go`）

- [x] `ArtifactSaver` 接口 / `ArtifactInput` / `ArtifactResult` / `WithArtifacts` /
      `ArtifactsFrom`：词汇放在 Tool 接口旁边，因为两个包必须一致而谁也不拥有谁
- [x] `save_artifact` 工具**不带宿主参数**——产物归属是本轮的属性，不是工具的属性
- [x] `Content` 走 `io.Reader` 而不是 `string`：上限在读取时生效，不是整块进内存之后
- [x] 没有 saver 时用模型能照做的中文回答，而不是晦涩失败
- [x] `normalizeArtifactKind`：`doc/markdown/md/text`→document、`page`→html、
      `img/picture/photo/chart/screenshot`→image；认不出的报出真实选项
- [x] 注册为 `CapRead` + `Serial`：不因只读工作区而禁用（它写的是部署自己的存储、
      连路径都选不了），串行是为防同秒同名覆盖（否则两行指一个文件）
- [x] 测试 6 个：内容透传、无 saver、必填校验、无 URL 时如实说明、kind 归一化、
      工具名

### 第 5 节：命令层接线（`cmd/huan-agent/artifacts.go`）

- [x] `newCommandArtifactStore` / `withCommandArtifacts` / `registerArtifactTool` /
      `artifactRoot`；CLI、一次性 run、飞书共用 `internal/artifact.Saver`，
      没有第二套命名冲突/孤儿文件/URL 规则
- [x] 命令侧**不发布 URL**：这些界面没有 HTTP，报一个无人应答的地址比不报更糟
- [x] 接线点：`chat.go:249/381/584`、`run.go:423`、`admin.go:540`、
      `server/turns.go:433`（本会话产物归属）

### 第 6 节：控制台（`web/`）

- [x] 对话顶栏「产物」按钮 + 数量，点击打开抽屉；本会话无产物时隐藏
- [x] `artifactsStore.js`：一个 loader 一个答案，按住会话切换时的竞态（慢响应不得
      落到新会话标题下）；**没有定时器**——产物一存在就是完成态，没有可轮询的东西；
      改为会话切换、流里出现 `save_artifact`（250ms 防抖）、手动刷新时重载
- [x] 统计监控 → 产物中心：全部会话的产物，行上带归属会话、类型过滤、会话过滤；
      无会话的产物（一次性 run）明说，不留空白单元格
- [x] 每行 打开 / 下载 / 删除，删除先确认
- [x] `web/scripts/check-artifacts.mjs` 挂进 `check:ui`
- [x] `npm run check:ui` = 260 PASS / 0 FAIL；`vite build` 产出
      `internal/server/webui/dist/assets/index-Cy2EoJR7.js`

### 验证方式

- [x] `go vet ./internal/artifact/... ./internal/server/... ./internal/tool/...
      ./internal/store/...` 干净；`go test` 同范围全绿（写本文档时重跑确认）
- [x] 真实二进制冒烟（按 YAML 起 `admin serve`）：`artifacts enabled` 日志、列表
      URL 正确、文件响应带 sandbox CSP 与 `nosniff`、符号链接逃逸被拒并 warn、
      `../../../etc/hosts` 未泄漏
- [x] `npm run check:ui`（上一轮跑）= 260 PASS / 0 FAIL；`vite build` 产出
      `internal/server/webui/dist/assets/index-Cy2EoJR7.js`。**本轮未重跑前端校验**
      （本轮只改了 markdown），故这一条按上一轮结果引用
- [!] `save_artifact` 的**工具级真实调用未跑到**：环境没有配置 LLM provider
      （`huan-agent run` 报 `no provider configured`）。缺口用"直接调用真实 `Saver`
      + HTTP 端点验证读取"补上，但"模型实际传参 → 工具落盘"这一段未经端到端验证

### 校对时发现的既有小瑕疵（未改代码）

- [!] `supportedExts()`（11 项，用于错误提示）与 `mimeByExt`（13 项，实际接受）
  不一致：`.htm` 与 `.jpeg` 能被存储和正确服务，却不在"不支持的扩展名"提示里。
  存不下来的是别的扩展名，功能没有漏洞，只是提示少两个别名。
  `kindByExt` 里两者都在，分类不受影响。留待定夺：要么补进 `supportedExts()`，
  要么从 `mimeByExt` 去掉这两个别名。

### 校对文档时改正的两处（文档写错过，不是代码有毛病）

- [x] 「关闭时每个端点都不返回 404」→ 改为「三个 JSON 端点不返回 404，文件路由
      返回 404」。代码本来就是对的（`handleArtifactFile` 用 404），是 spec 的措辞
      把文件路由一起圈进去了
- [x] proposal 的扩展名白名单漏了 `.htm` → 补上，与 `mimeByExt` 的 13 项一致

## 未做（有意）

- [ ] `openspec/specs/project.md` 的 capability 表未登记本能力：该表只到 phase 7
      （8–24 均未登记），补它会把本次改动扩大到 25 个能力，留待统一整理
- [ ] 改动尚未 commit（`internal/artifact/`、`internal/server/artifacts*.go`、
      `internal/store/artifact.go`、`internal/tool/artifact.go`、
      `internal/tool/builtin/artifact*.go`、`cmd/huan-agent/artifacts.go`、
      `web/src/artifacts*.js`、两个 Vue 组件、`internal/server/webui/dist/` 等）
- [ ] 没有新建 `docs/artifacts.md`：产物已经有 `docs/admin.md`（控制台与运维）和 `docs/tools.md`（`save_artifact`）两处文档，再加一页就是第三处重复
