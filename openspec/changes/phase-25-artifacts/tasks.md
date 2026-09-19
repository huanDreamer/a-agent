# Phase 25 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

## 实施记录（2026-09-19）

**已完成并验证**：全部 7 节 —— 存储、索引、路由、工具、命令层接线、控制台，以及
本轮的命名与布局修订（第 7 节：有含义的文件名 + 日期目录）。
**未完成 / 未验证**：`save_artifact` 的工具级真实调用（环境无 LLM provider），
以及 `..` 穿越这条攻击路径没有真正打到守卫（原因见第 3 节）。**改动未提交。**

### 第 1 节：存储（`internal/artifact`）

- [x] `Store` / `New` / `Options`：root 为空时报 `ErrDisabled` 而不是替调用方选一个
      目录；root 自身解析符号链接（macOS 上临时目录在 `/var` → `/private/var`，
      不解析会让包含性检查把下面的每个文件都拒掉）
- [x] 落盘布局 `<session>/<YYYY-MM-DD>/<name>.<ext>`，可被 `ls` 直接读懂；会话作为
      路径前缀，让"本会话的产物"是目录操作而不是查询。
      **本轮修订**：原为 `<session>/<时间戳>-<slug>.<ext>`（时间戳前缀 + 随机 token
      兜底），现改为日期目录 + 有含义的文件名——见第 7 节
- [x] `Resolve`：拒 NUL、先 clean、拒绝对路径、拒绝 `..`、在最长已存在前缀上解析
      符号链接后**再查一次**；任何逃逸返回 `ErrOutsideRoot` 而不是夹回root
- [x] 扩展名白名单（11 个）在写入时拒绝，不是存下来以后再操心
- [x] `MIMEByExt` / `MIMEForPath` 导出：写路径与读取路由必须对"这个文件是什么"
      给出同一个答案
- [x] `slugify`：保留 Unicode 字母与数字，**中文原样保留**，上限按 rune 计
      （`maxStem=48`）。**本轮修订**：原为"中文标题故意返回空串、由随机 token 命名"，
      现改为中文直接进文件名，且不做拼音转写（多一个依赖，还是一次猜测）；
      `randomToken` 已删除，无可用字符时用固定词 `artifact` 兜底
- [x] `writeAtomic`：同目录临时文件 + rename（跨设备 rename 不是原子的），
      多读 1 字节以便区分"正好到上限"和"超了"
- [x] `sessionComponent`：会话 id 只允许字母数字下划线连字符，含路径形状即拒绝
- [x] 测试 23 个用例：会话与类型、中文标题命名文件、无可用标题仍存下、默认 HTML、
      kind 定扩展名、无会话落日期目录、超限拒绝、不支持类型拒绝、路径形状会话拒绝、
      逃逸拒绝、符号链接逃逸拒绝（**真正打到守卫的那条**）、目录拒绝、删不存在的
      文件不算错、slug、artifactStem、同名去重（`-2`/`-3`，且旧内容不被覆盖）、
      两个会话不撞名、rune 截断、URL 转义与中文名往返、MIME 表

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
- [x] 测试 9 个 HTTP 级用例（本轮又加 2 个，共 11 个，见第 7 节），含端到端闭环：
      真实 `Saver` 写入 → 从 URL 读回字节 → 列表带同一 URL → 删除同时清掉行与文件
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
- [x] `web/scripts/check-artifacts.mjs` 挂进 `check:ui`；**本轮新增 12 条断言**：
      `fileName`（取路径末段）、`dayKey`（本地日）、`groupByDay`（新日在前、组内保持
      服务端顺序、无法解析时间的行落尾部桶而非丢弃）
- [x] **本轮**：产物中心按天分组（跨列日头）+ 每行显示存储的文件名；抽屉每行增加
      文件名行。`styles.css` 加 `table.data td.artifact-day-cell` / `.artifact-day-label`
- [x] `npm run check:ui` = **272 PASS / 0 FAIL**（本轮重跑）；`vite build` 产出
      `internal/server/webui/dist/assets/index-DSskJPNz.js`（CSS `index-x_p334aV.css`）
      （上一轮为 260 PASS，JS `index-Cy2EoJR7.js`）

### 第 7 节：命名与布局修订（本轮，2026-09-19）

需求：保存产物时文件名要有含义（不要随机生成），并按日期分目录。

- [x] **文件名来自标题**：`artifactStem(title, name)` 三级回落 —— 标题 → 建议文件名
      的 stem → 固定词 `artifact`。`name` 只取 `path.Base` 再去掉扩展名，所以
      `../../x.html` 这种名字无法改变文件落点
- [x] **中文保留**：`slugify` 用 `unicode.IsLetter/IsDigit`，`季度报告` →
      `季度报告.html`；其余字符（标点、分隔符、`/`、`\`）收缩为单个 `-`
- [x] **日期目录**：`<session>/<YYYY-MM-DD>/`，**本地日**而非 UTC。原实现用
      `time.Now().UTC()`，而控制台按读者本地日分组，东八区本地 00:00–08:00 两者会
      给出两个不同日期（同一个文件，`ls` 说 19 号、产物中心说 18 号）。改为
      `time.Now().Format(dateLayout)`，并用 `TZ=America/New_York` 实测证明：
      UTC 是 2026-09-19、本地是 2026-09-18 时，文件落在 `2026-09-18/`
- [x] **同名不覆盖**：`uniqueRel` → `<stem>.ext`、`<stem>-2.ext`、`<stem>-3.ext`，
      后缀在扩展名前（type 仍由扩展名决定）。以 `os.Stat` 为准，磁盘上已存在即视为
      占用（即使没有行指向它）。最多 1000 次尝试
- [x] **URL 无需改**：`artifact.URL` 已经逐段 `url.PathEscape`，中文名自动编码为
      `%E5%AD%A3...`，与落盘路径同一来源
- [x] **前置确认（t1）**：写了一个探针实跑 Hertz，确认 `*path` 通配符会把
      `%E5%AD%A3` 解码为 `季度` 再放进 `c.Param("path")`（`c.Path()` 同样已解码，
      `RequestURI()` 保留原始编码）。这决定了"中文名直接进路径"是可行的，而不是
      需要自己维护一套编码
- [x] 新增 `internal/server` 两个用例：`TestArtifactChineseNameRoundTrips`（真实 HTTP
      路由取回中文名产物，9 处断言）、`TestArtifactDateDirectoryIsTheLocalDay`
- [x] 两个用例都反向验证过断言力：把期望文件名改错 → FAIL；把分组顺序去掉
      `reverse()` → 2 条 FAIL
- [x] `go vet ./...` 干净；`go test -count=1 ./...` 全绿（全量跑两次；首次
      `TestJobsAPI_ListReadStopForget` 失败，单跑 3/3 通过且与产物无关，判定既有 flake）
- [x] 真实二进制冒烟：经生产 `Saver` 落盘 → `smoke-1/2026-09-19/季度用量报告.html`，
      URL 带 `%E5%AD%A3`；真实 HTTP GET 返回 200 + sandbox CSP + `nosniff`，字节一致；
      同名三次得 `.html` / `-2.html` / `-3.html` 且三份内容各自保留；未编码与编码的
      中文路径都能取到；列表回传的 URL 与实际可取回的 URL 是同一字符串

### 第 8 节：产物中心一行都不显示（本轮，2026-09-19）

现象：统计监控 → 产物中心只显示数量（「3 个」「68.1 KB」）和一个空列表，
API 明明返回 3 条。

根因：`ArtifactsView.vue` 的模板第 193 行调用 `shortId(item.session_id, 6, 4)`，
但 `import { … } from '../format.js'` 里没有 `shortId`。`<script setup>` 里未绑定的
标识符编译成 `_ctx.shortId`，构建通过、类型检查通过，只在**含会话 id 的行真正渲染
时**抛 `TypeError: ne.shortId is not a function`。空列表不渲染行，所以空状态下看不出
来；一旦有产物就整块渲染失败——连"计数"都是先于行渲染的，于是只剩一个数字。

- [x] 复现（t1）：真实 Chrome + 真实 server，`web/.verify/artifacts-view-repro.mjs`。
      `tbodyRows=0` 而 `count=3`，控制台捕获到 `TypeError: ne.shortId is not a function`。
      编译器层同时确认：`compileTemplate(bindingMetadata=script.bindings)` 把这一行
      编译成 `_ctx.shortId`，`bindings.shortId === null`
- [x] 修 `ArtifactsView.vue`：`shortId` 加进 `../format.js` 的 import。
      编译结果由 `_ctx.shortId` 变为 `$setup.shortId`，`bindings.shortId = 'setup-maybe-ref'`。
      同一文件里其它四个 `format.js` 名字（`formatDayLong` 等）本来就已导入，只有这一个漏了
- [x] 修同类问题 `ModelManager.vue`：`询问窗口与能力` 按钮的 tooltip 用了
      `probeBatchLimit`，而脚本里从未定义它（`_ctx.probeBatchLimit` 渲染成 `undefined`，
      不是抛错，所以更隐蔽）。补上 `const probeBatchLimit = 4`，注释写明它镜像
      `internal/server/modelfacts.go` 的同名常量，是**唯一**事实来源；响应里的
      `remaining` 在常量漂移时仍然正确，这个数只供 tooltip 事先说明
- [x] 补防线（t4）：`scripts/check-component-bindings.mjs` 原来只查"setup 绑定遮蔽
      prop"和"prop 被当函数调用"，两种都看不到"模板引用了不存在的名字"。新增第三条
      规则：用 `compileScript` + `compileTemplate`（喂真实 `bindingMetadata`）复现构建时
      编译，报出所有非 `$` 开头的 `_ctx.<name>`。**反向验证**：把两个 bug 重新注入 →
      同时报出 2 条 UNBOUND 且 exit 1；恢复后 exit 0
- [x] 新增的规则不做白名单：全仓库当前唯一的 `_ctx.` 产出是 `$slots`（Vue 内建，
      按 `$` 前缀排除），所以任何新的 `_ctx.<name>` 都是真问题，不需要例外表
- [x] 补一条 SSR 探针：`renderArtifactsView()` 此前进过 `entry.js` 却**从未被 `run.mjs`
      调用**，所以这一块从来没被渲染过。现在断言它能渲染、过滤控件在、空态文案对。
      注释里写明这是**弱断言**：`useResource` 在 `onMounted` 里取数，SSR 不跑挂载钩子，
      所以它只看得见空态，**抓不到**这个 bug——抓它的是静态检查与真实浏览器验证
- [x] 重建**嵌入用**的 dist：`cd web && npm run build` →
      `internal/server/webui/dist/assets/index-CwyvCS_X.js`（CSS `index-161W_iPv.css`）。
      上一轮记的 `index-DSskJPNz.js` 其实是**旧的**——`check:ui` 里的 `vite build`
      是 `ssr-probe/vite.config.js`，产物进 `ssr-probe/out/`（gitignore），
      **不会**更新 `internal/server/webui/dist/`。发版必须单独跑 `npm run build`
- [x] `npm run check:ui` = **275 PASS / 0 FAIL**（新增 3 条）
- [x] 端到端（t6）：复制一份 `data/` 到 `/tmp/art-verify`，用新二进制在 **18081** 起
      隔离实例（`require_login: false` + `127.0.0.1`，否则启动会被安全守卫拒绝：
      无登录且绑 `0.0.0.0` 等于把命令执行暴露到网络）。真实 Chrome 跑
      `web/.verify/artifacts-view-verify.mjs`：
      3 条产物 → **3 行** + 1 个日分组头（`2026-09-19（周六）3 个`），
      控制台 **0 error**；同时在同一 Chrome 上跑旧 server（8080）仍是
      `rows=0` + `shortId` 报错——同一套断言，改前 FAIL、改后 PASS
- [x] 用户的实例全程未被触碰（pid 94681 保持在线，其 bundle 仍是旧的；
      重启才会生效）

### 验证方式

- [x] `go vet ./...` 干净；`go test -count=1 ./...` 全绿（写本文档时重跑确认）
- [x] 真实二进制冒烟（按 YAML 起 `admin serve`）：`artifacts enabled` 日志、列表
      URL 正确、文件响应带 sandbox CSP 与 `nosniff`、符号链接逃逸被拒并 warn、
      `../../../etc/hosts` 未泄漏
- [x] `npm run check:ui` = **275 PASS / 0 FAIL**（本轮重跑）；嵌入用 dist 由
      `cd web && npm run build` 产出 `internal/server/webui/dist/assets/index-CwyvCS_X.js`
      （上一轮记的 `index-DSskJPNz.js` 是旧的；`check:ui` 内的 vite 构建只更新
      `ssr-probe/out/`，不更新嵌入目录）
- [x] 真实浏览器端到端：隔离实例（18081）+ 真实 Chrome，产物中心 3 条 → 3 行 +
      日分组头，控制台 0 error；同一套断言在旧 bundle 上 rows=0 且报 `shortId`
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
- [ ] 用户的实例未重启，其 bundle 仍是旧的（`index-DSskJPNz.js`）：本轮的修复要等
      重启 `admin serve` 才生效。新二进制已在 `bin/huan-agent`（13:50 构建）
- [ ] `probeBatchLimit` 在前后端各有一份（Go 常量 + Vue 常量）。没有把它塞进 API 响应，
      因为 tooltip 要在点击**之前**就说清一次会问几个，而响应是点击之后才有的；
      真正的事实来源仍是 Go 那份，`remaining` 字段保证漂移时不会骗人
