# context-management（本阶段增量）

## 目标

让窗口压缩在**一轮 turn 之内**可用，并且**不吞掉不该吞的东西**：系统提示与本轮的目标
必须活过一次压缩。

## 为什么这是增量而不是重写

`internal/context` 的估算、预算、摘要策略与 `Compress` 语义都保持原样；本次只补两件事：
可钉住的消息、以及把 system 提示从"旧消息"里救出来。

## ADDED

### `Pin` 与 `CompressWith`

```go
// Pin 说明压缩时必须逐字保留的消息（最近 N 条尾巴之外的）。
type Pin struct {
	Head     int  // 开头 Head 条（system 提示、工具约定）
	LastUser bool // 最后一条 user 消息（本轮到底要干什么）
}

func (m *Manager) CompressWith(ctx context.Context, msgs []*schema.Message, pin Pin) ([]*schema.Message, string, error)
func (m *Manager) CompressKeeping(ctx context.Context, msgs []*schema.Message, head int, pinLastUser bool) ([]*schema.Message, string, error)
```

- `Compress(ctx, msgs)` 等价于 `CompressWith(ctx, msgs, Pin{})`，行为逐字不变。
- 输出顺序固定为：`msgs[:Head]` → 摘要 system 消息 → 被钉住的最后一条 user 消息 →
  最近 `KeepRecent` 条。
- 钉住的消息不参与摘要输入；被折叠的只是中间那段。
- 没有可折叠的中间段（`KeepRecent >= 可折叠长度`）时原样返回，不产生空窗口。
- `Head` 超过 `len(msgs)` 时按 `len(msgs)` 处理。

## CHANGED

- `sessionMemory.history()` 改用 `CompressKeeping(ctx, msgs, 1, true)`：**修掉一个真实缺陷**
  ——现在 system 提示位于第 0 条，压缩时会被折进摘要，于是压缩一次之后技能/系统提示就
  从窗口里消失了。钉住头部即可。
- runner 的每一步调用模型之前压缩一次：12 步时窗口还能忍，几十步时必须每步都收。

## 边界

- 摘要由 `Summarizer` 产生（默认 LLM 自摘要），压缩本身会消耗一次模型调用 —— 这是
  设计选择：用一次小调用换窗口有界，比撞上下文上限报错便宜。
- 摘要失败时仍返回有界窗口（占位摘要），沿用既有降级策略。
