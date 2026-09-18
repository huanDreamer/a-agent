# 通用 Agent 系统提示词 任务清单

## 1. `internal/prompt`：提示词的唯一出处

- [x] `base.md`：身份（huan-agent 是通用 Agent，不是问答接口）
- [x] `base.md`：语言与表达（默认中文、跟随用户语言、直接给结果、Markdown、诚实优先）
- [x] `base.md`：做事方式（先查再动手、探索适可而止、一次做完、改动小而准、做完验证、失败说清楚）
- [x] `base.md`：工具使用（工具列表即能力边界、各工具的选择、`edit_file` 唯一匹配、
      非交互命令与 `/dev/null`、`bash_background` 与 `bash_*`、输出截断、破坏性操作、附件、技能）
- [x] `base.md`：边界与安全（工作区限制、密钥不外泄、不绕过拒绝、动手前说一句、收尾报告）
- [x] `surface_web.md`：Markdown 渲染、附件、`ask_user` 的使用纪律、压缩后不要依赖上文
- [x] `surface_cli.md`：终端纯文本、没有 `ask_user`（直接问）、一次回答自成一体
- [x] `surface_feishu.md`：卡片渲染、表格会被拆成多条消息、没有 `ask_user`、消息可能很短
- [x] `prompt.go`：`go:embed` + `Base()` / `For(surface)` / `Effective(override, surface)`
- [x] `prompt.go`：`SurfaceWeb` / `SurfaceCLI` / `SurfaceFeishu` 常量

## 2. 接线：三个界面

- [x] `internal/server/chat.go`：删掉英文常量，`chatPrompt()` 走 `prompt.Effective`
- [x] `cmd/huan-agent/serve.go`：飞书读 `chat.system_prompt`，否则用 Feishu 默认提示词
- [x] `cmd/huan-agent/chat.go`：`--skill` > `--system` > CLI 默认提示词（空会话不再没有 system）
- [x] 覆盖语义只有一处实现（`prompt.Effective`），控制台与飞书不会分叉

## 3. 测试

- [x] `internal/prompt/prompt_test.go`：基础提示词含各章节与工具名、含「无终端」规则
- [x] `internal/prompt/prompt_test.go`：`For` 是「基础 + 界面段落」，各界面段落互不相同
- [x] `internal/prompt/prompt_test.go`：界面名大小写/空白容错，未知界面回落到基础提示词
- [x] `internal/prompt/prompt_test.go`：全中文（英文虚词黑名单 + 汉字占比 ≥ 50%）
- [x] `internal/prompt/prompt_test.go`：体积上限 12 KiB（提示词是每步成本）
- [x] `internal/prompt/prompt_test.go`：`Effective` 替换而非追加、空白等于没填
- [x] `internal/server/traces_test.go`：控制台默认提示词的中文/ask_user/无终端断言，改用
      `prompt.For(prompt.SurfaceWeb)` 作为期望值
- [x] `internal/server/skillapi_test.go`：技能段落仍拼在默认提示词之后
- [x] `cmd/huan-agent/serve_test.go`：`TestBotSystemPromptFollowsTheOperator`（默认、覆盖、
      空白回落、`cfg` 为 nil）

## 4. 文档

- [x] 新增 `docs/prompt.md`：文件位置、各界面拿到什么、覆盖规则、两条硬性约束、改动检查清单
- [x] `docs/admin.md`：`system_prompt` 现在也作用于飞书，且启动时读取（不可运行时改）
- [x] `docs/feishu.md`：新增「What the bot is told」
- [x] `configs/config.example.yaml`：`system_prompt` 注释写清覆盖范围与 CLI 的例外
- [x] `CLAUDE.md`：目录约定补 `internal/prompt/`

## 5. 验证

- [x] `go build ./...`
- [x] `go test ./internal/prompt/... ./cmd/... ./internal/server/...`
- [x] `gofmt -l` 干净
- [ ] `golangci-lint run`（本机未安装，未执行）
