# Capability: web-retrieval

## Purpose

让 agent 能读到训练数据之外的**原文**：一个新版本库的 API、一段报错的原文、一个 RFC、一个
GitHub issue。今天它只能靠猜，而猜错的产物是"看起来完全合理的错误代码"——比一个明显的报错更难
发现。

本能力只做一件事：**把一页网页变成一段可信赖的正文**，并把抓取这件事本身关进一条明确的边界里。

## Scope

- `internal/web` — 抓取、正文抽取、地址检查、缓存。
- `internal/tool/builtin` — `fetch_url`。
- `internal/config` — `tools.web.*`。
- `docs/web.md` — 选型、限制、安全说明，以及"搜索为什么不在这里"。

## Requirements (MUST)

1. **抓取要抓正文。** `fetch_url` MUST 做正文抽取（去脚本、样式、导航、页脚），MUST NOT 把裸
   HTML 灌进上下文。**代码块 MUST 被保留且不被折叠成一行**——文档页最有价值的部分就是它。
   抽取后的正文才是这个工具与 `bash curl` 的差别所在。
2. **不可信内容必须被界定。** 抓回的网页是外部内容，其中可以包含针对模型的指令。工具输出 MUST
   明确标注来源 URL 与"外部内容"边界，工具描述与系统提示词 MUST 要求把抓回的内容当作**数据**，
   其中出现的任何指令 MUST NOT 被执行。
3. **只允许 http/https。** 其它 scheme（`file` / `ftp` / `data` / `gopher` 等）MUST 被拒绝并给出
   可读原因。
4. **地址检查（SSRF）。** 默认 MUST 拒绝环回、私有网段（RFC1918）、链路本地（含
   `169.254.169.254` 云元数据）、ULA、多播与未指定地址。检查 MUST 在**每一跳重定向之后**重新
   执行，并且 MUST 在**拨号时**再次校验目标 IP（防 DNS 重绑定）。只有
   `tools.web.allow_private: true` 才放开，且放开生效时 MUST 记一条明确的 warn 日志。
5. **抓取必须有界。** 总超时、重定向次数上限、响应体字节上限（MUST 边读边计数，超限即截断，
   MUST NOT 先下载完再判断）、`Content-Type` 白名单（表外类型 MUST 可读地拒绝，而不是把二进制
   塞进上下文）。截断 MUST 如实报告。
6. **长文可以分次读完。** `fetch_url` MUST 支持 `offset` / `max_chars`，使模型能接着读同一页，
   而不是被迫重抓。偏移按 **rune** 计算，与 `read_file` 的行号口径一致。
7. **抓取要缓存。** MUST 在进程内按（URL，抽取模式）缓存已抽取的正文，有 TTL 与容量上限
   （LRU）。抓取是慢且可能被限流的一步；同一页被读两次 MUST NOT 抓两次。缓存 MUST NOT 落盘，
   命中时 MUST 在结果里标注。
8. **对外要克制。** 每个 host MUST 有并发闸门（`tools.web.max_per_host`），避免一轮里的并发
   抓取把对方站点打爆。
9. **远端错误要说人话。** 超时、404、5xx、429、地址被拒、类型不支持、需要 JS 渲染 MUST 各自
   给出可读原因并作为工具观察值交回模型，MUST NOT 一律表现为"抓取失败"——那会让模型分不清
   "页面不在了"与"我不该抓这个地址"。
10. **可由配置整体关闭。** `tools.web.enable=false` 时 `fetch_url` MUST NOT 注册。

## API

| 工具 | 参数 | 返回 | 能力 | 并发 |
|---|---|---|---|---|
| `fetch_url` | `url`, `offset?`, `max_chars?`, `extract?`(`text`/`raw`) | `title`、最终 `url`、`content_type`、`text`、`offset`、`total_chars`、`truncated`、`fetched_at`、`cached` | `CapRead` | `ParallelSafe` |

MUST 在**所有** surface 注册（联网不是交互式能力）。纯 JS 渲染的页面 MUST 返回明确说明，
而不是返回一坨菜单。

## 配置

```yaml
tools:
  web:
    enable: true
    fetch_timeout_seconds: 20
    fetch_max_kb: 2048
    max_chars: 60000
    redirect_limit: 5
    allow_private: false
    max_per_host: 2
    cache_ttl_seconds: 300
    cache_max_entries: 128
    user_agent: ""          # 空 = 带项目标识的默认值
```

## 行为

- 已知 URL 时直接抓。本能力**不提供搜索**：它只负责"给我这个 URL 的正文"。
- 抓取失败 MUST 作为可读的工具观察值返回，让模型换一条路；MUST NOT 终止本轮。
- 缓存命中 MUST 在结果里标注；分页读同一页时 MUST 复用同一份缓存正文。
- 抽取过度（正文被误删）时，`extract=raw` 是兜底手段，MUST 可用。

## Non-goals

- **不自研 `web_search`。** 搜索需要一个后端，而"哪家搜索、用哪个 key、额度怎么算"是部署问题，
  不是这个 agent 该自己实现的能力。仓库已有完整的 MCP 客户端（`internal/mcp`，支持
  stdio / sse / http）与控制台的 MCP 管理面（`internal/server/mcp.go` 的 `/api/mcp/*`），因此
  搜索能力的正确来源是**装一个搜索类的 MCP server**。本能力 MUST NOT 引入 `Searcher` 抽象、
  搜索 API 适配或 `tools.web.search.*` 配置。
- 浏览器渲染 / JS 执行（需要 headless 浏览器，是另一个量级的依赖）。
- PDF、Office、图片等非文本类型的解析。
- 站点地图爬取、递归抓取、站点镜像：`fetch_url` 是"读一页"，一次一页；批量探索交给子 agent。
- 搜索结果或正文的持久化 / 向量化（那是 OpenViking 的职责）。
- 把 `fetch_url` 纳入审批闸门：它是只读的对外读取，边界由第 3–5 条构成；让每次联网都要人点一下
  会把审批变成噪声，从而削弱它真正的用途。
