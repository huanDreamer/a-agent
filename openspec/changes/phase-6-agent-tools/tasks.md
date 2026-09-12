# Phase 6 — 任务清单

## 1. 工作区沙箱（`internal/workspace`）

- [x] `Resolve`：单一解析入口，拒绝 `..`、越界绝对路径、NUL、空路径
- [x] **符号链接逃逸防护**：对已存在的最深层前缀求符号链接后重新校验
- [x] 尚不存在的写入目标通过父目录校验
- [x] 根目录自身若为符号链接，先解析一次，避免子路径被误拒
- [x] `ResolveForWrite`：只读与写入大小校验
- [x] 读/写/列举上限（默认 512KiB / 4MiB / 500）
- [x] `IsBinary`（NUL 嗅探窗口）
- [x] `Rel` 输出工作区相对路径（斜杠分隔，输出稳定）
- [x] 覆盖率 88.9%，含逃逸测试同时断言“无副作用”

## 2. 文件工具

- [x] `read_file`：1-based 行号（`   12→内容`）、offset/limit、目录拒绝、二进制拒绝、截断同时体现在字段与内容尾部
- [x] `write_file`：自动建父目录、**原子写**（临时文件 + rename）、保留原文件权限
- [x] `edit_file`：**要求唯一匹配**；0 匹配/多匹配/空目标各自给出可操作的错误；拒绝时文件字节不变
- [x] `list_dir`：目录优先排序、类型/大小/相对路径、截断标记
- [x] 覆盖率：files.go 87.6%

## 3. 检索工具

- [x] `glob`：`*` / `?` / `**`（自行实现，未加依赖）、无分隔符模式匹配任意深度
- [x] 噪声目录（.git / node_modules 等 12 个）默认跳过，显式指定时仍可搜
- [x] `grep`：RE2、`include` 过滤、大小写、上下文行、匹配数上限、超长行裁剪
- [x] 二进制与超大文件跳过并计数
- [x] 覆盖率：search.go 多数函数 100%

## 4. bash 工具

- [x] `/bin/sh -c`，cwd 通过沙箱解析并锁定
- [x] 超时（默认 120s，可配置，单次调用只能缩短）
- [x] stdout / stderr 分别捕获与截断，附丢弃字节数说明
- [x] **进程组终止**：超时杀整组，后台子进程无法存活
- [x] **修复 os/exec 陷阱**：`sh -c 'sleep 5 &'` 不会触发 `cmd.Cancel`（shell 已退出），改用 watchdog + 无条件 reap
- [x] 拒绝清单（rm -rf /、mkfs、dd of=/dev/、fork bomb、shutdown…）
- [x] 所有拒绝发生在 Start 之前，测试断言未执行
- [x] 覆盖率：bash.go 92.9%

## 5. 能力与过滤

- [x] `Capability`（read/write/exec）+ `WithCapability`
- [x] 未声明能力的工具按 `read` 处理（保守默认）
- [x] `FilterByCapabilities` —— **修复自身缺陷**：原实现遍历全部已注册工具，可能复活被源 allow-list 排除的工具；改为从源实际许可集合出发
- [x] 只读工作区**不注册**写/执行工具（让模型看不到，而非调用时才拒）

## 6. 配置与接线

- [x] `tools.workspace` / `read_only` / `enable_bash` / `bash_timeout_seconds` / `max_read_kb` / `max_write_mb` / `max_list_entries` / `deny_patterns`
- [x] 默认拒绝清单内置；配置可覆盖
- [x] `registerBuiltinTools(reg, cfg)` 接入 CLI / Web / 飞书
- [x] 工作区默认取进程工作目录（开箱即用）

## 7. 顺带修掉的 3 个真实缺陷

- [x] **工具耗时恒为 0ms**：赋值写在 defer 中，晚于事件发送；UI「耗时」与审计日志都在记录无意义的 0
- [x] **描述被静默截断**：eino 按逗号切分 jsonschema tag，`IANA timezone name, e.g. Asia/Shanghai. Empty = server local time` 只到 `IANA timezone name`，丢掉可操作的那半句；另有 grep 的 pattern 描述里混入了函数名。加了基于反射的守卫测试
- [x] **拒绝清单过宽**：`\b(shutdown|reboot)\b` 会拦下 `echo reboot-status` 和提到 reboot 的 commit message；改为锚定命令位置

## 8. Spec / 文档

- [x] openspec proposal（本目录）
- [x] `specs/workspace-sandbox/spec.md`
- [x] `specs/agent-tools/spec.md`
- [x] `docs/tools.md`（含**诚实的边界说明**：限定的是工作目录，不是命令能触达的范围）
- [x] 进度页更新

## 9. 质量门禁

- [x] `go build ./...`
- [x] `go vet ./...`
- [x] `go test -race ./...`（20 包全过，无 data race）
- [x] 新增包覆盖率 ≥ 70%（workspace 88.9% / builtin 88.8% / tool 89.0%）
- [x] `gofmt` 干净
- [x] 真实端到端：读取 → glob → grep → 改写 → 写入 → 执行校验 → 回答，且**磁盘文件确实被修改**
