# Phase 3 — Memory & Context 任务清单

- [x] `internal/memory/`：JSONL 长期存储 + 短期 Buffer + 关键词索引
- [x] `internal/context/`：token 估算 + Budget + 自动压缩/摘要
- [x] `internal/config`：`memory.*`、`context.*`
- [x] `cmd/chat.go` + `cmd/memory.go`：REPL 接入 + `/remember` `/recall`
- [x] `configs/config.example.yaml` 示例
- [x] 单元测试：memory / context 覆盖率 ≥ 70%
- [x] `go build` / `go vet` / `golangci-lint` 通过
- [x] 归档本 phase proposal