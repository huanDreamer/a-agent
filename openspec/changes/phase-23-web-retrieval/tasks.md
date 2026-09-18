# Phase 23 — 任务清单

约定：`[x]` 完成，`[ ]` 未完成。每一节对应一次可独立验证的提交。

范围：**只有 `fetch_url`**。搜索不自研（走 MCP），因此本清单没有搜索后端、`Searcher` 抽象或
搜索配置段。


## 实施记录（2026-09-18）

**全部完成并验证**。`internal/web`（SSRF 边界 / 正文抽取 / 抓取与分页 / 同站链接）、
`fetch_url` 工具、`tools.web.*` 配置、控制台与 `run` 的接线、文档。
**`web_search` 按用户指示不再自研**：搜索走既有 MCP 客户端，规格与文档都已写明。

### 交付

- [x] `internal/web/ssrf.go`：`Guard.CheckURL`（协议白名单 + 解析后逐地址判定）与
      `Guard.CheckIP`（**建连时再判一次**，堵 DNS rebind）。拒绝理由具名化：云元数据地址
      （169.254.169.254，会交出实例凭据）排在通用链路本地之前，因为读日志的人要看到
      自己差一点够到什么
- [x] `internal/web/extract.go`：**只用标准库**的 HTML → 文本抽取（`x/net` 不在 go.mod，而模块
      缓存在沙箱里只读，所以手写一个有界的抽取器，并说明它不是浏览器）。剥 script/style/nav/
      footer/注释，`<pre>`/`<code>` **原样保留并加围栏**
- [x] `internal/web/links.go`：同站链接抽取（相对路径解析、去重、上限 40）——"读完这一页再读
      下一节"因此不用猜 URL
- [x] `internal/web/fetch.go`：`Fetcher`（超时 / `MaxBytes` **边读边计数** / 重定向上限与
      **逐跳重检** / Content-Type 白名单 / gzip+deflate / 进程内 TTL+LRU 缓存 /
      `offset`+`max_chars` 按 **rune** 分页）
- [x] `internal/tool/builtin/web.go`：`fetch_url`，结果**明确框定为不可信内容**
      （`<<<UNTRUSTED-WEB-CONTENT>>>`），声明 `CapRead` + `ParallelSafe`
- [x] `tools.web.*` 配置（enable / allow_private / timeout_seconds / max_kb / max_chars /
      cache_ttl_seconds / user_agent）+ `*Or()` + `SetDefaults`
- [x] 接线：`cmd/huan-agent/web.go`（进程级 fetcher；`allow_private=true` 时启动打 warn）；
      控制台与 `run` 都注册
- [x] `configs/config.example.yaml` 的 `tools.web` 段 + `docs/tools.md` 新增「Reading the web」一节
      （含"**搜索不在其中**：web_search 走 MCP，不是这里"）

### 测试（22 个，全部通过）

10 类地址逐一拒绝 + 3 个公网地址放行；协议与无主机名拒绝；按名字解析到环回被拒；
**rebind 被拒**；`allow_private` 确实生效；抽取（标题实体解码、导航/脚本/注释/页脚被剥、
**代码块原样**、同站链接、注释里藏的指令被丢掉）；分页（窗口正确 + 第二段命中缓存 + 提示 offset）；
Content-Type 拒绝；HTTP 错误；超大响应截断且如实说明；重定向跟随 + 目标被拒；重定向链上限；
纯文本/JSON 直通；纯 JS 页面给出说明；context 取消。

端到端（真模型 + 真网络）：`fetch_url("https://go.dev/blog/context")` → 模型总结出正确的作者、
日期与 `Context` 接口的四个方法，说明正文抽取保住了实质内容。

### 实施中由测试抓到的真实问题

**`tidy` 把代码块的缩进吃掉了。** 空白折叠对每一行都做 `strings.Fields` 合并，于是 `<pre>` 里的
缩进在最后一步被抹平——而代码块的缩进就是代码本身。测试逐字比对
`"if err != nil {\n    return err\n}"` 才暴露它（前两轮只查关键词，所以没抓到）。修法是让 `tidy`
感知围栏：围栏内只去行尾空白。

它的形态值得记下来：**抽取的每一步都对，最后一步的"润色"把数据改坏了。**
