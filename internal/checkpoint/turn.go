package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/tool"
)

// The turn context and the tool guard.
//
// A capture has to happen *before* the write it protects and be attributed to the
// turn it belongs to, and neither of those is something a tool can work out for
// itself. So the turn's owner puts a Turn on the context and the guard — one
// decorator, applied where the tools are registered — reads it.
//
// A guard with no Turn on its context does nothing at all. That is the right
// default: a surface that has not set one up is a surface where nobody has asked
// for checkpoints, and capturing into a made-up turn would leave directories
// nothing can restore.

// Turn identifies the turn a capture belongs to.
type Turn struct {
	// Session is the conversation. It names the directory.
	Session string
	// Index is the turn's position within the session. It only has to increase
	// for the session: the retention policy keeps the highest numbers, so a value
	// that goes backwards would prune away the checkpoint a person is about to
	// need.
	Index int
}

type turnKey struct{}

// WithTurn puts a turn on a context.
func WithTurn(ctx context.Context, t Turn) context.Context {
	if t.Session == "" || t.Index <= 0 {
		return ctx
	}
	return context.WithValue(ctx, turnKey{}, t)
}

// TurnFrom reads the turn from a context, or reports that there is none.
func TurnFrom(ctx context.Context) (Turn, bool) {
	t, ok := ctx.Value(turnKey{}).(Turn)
	return t, ok && t.Session != "" && t.Index > 0
}

// NextTurn returns an index no checkpoint of this session has used.
//
// It is derived from what exists on disk rather than kept in memory, so it
// survives a restart: a process that forgot its counter and started again at 1
// would capture a pre-image into a directory that already holds one, and the
// restore would then be of the wrong turn.
func (c *Checkpointer) NextTurn(session string) (int, error) {
	if c == nil {
		return 0, ErrDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	turns, err := c.listTurnDirs()
	if err != nil {
		return 0, err
	}
	want := sanitizeSegment(session)
	highest := 0
	for _, t := range turns {
		if t.session == want && t.turn > highest {
			highest = t.turn
		}
	}
	return highest + 1, nil
}

// Guard wraps a file-mutating tool so its pre-image is recorded before it runs.
//
// It runs before the tool, not after, which is the whole point: after the write,
// the file's previous content is gone.
func (c *Checkpointer) Guard(t tool.Tool, pathField string) tool.Tool {
	inv, ok := t.(einotool.InvokableTool)
	if !ok || c == nil || !c.Enabled() {
		return t
	}
	if pathField == "" {
		pathField = "path"
	}
	return &guard{Tool: t, invoke: inv, c: c, pathField: pathField}
}

// Capability reports the wrapped tool's capability, so a guard is transparent to
// capability-based filtering — the trap that makes a gated write look like a read.
func (g *guard) Capability() tool.Capability { return tool.CapabilityOf(g.Tool) }

// Concurrency forwards the wrapped tool's declaration, for the same reason.
func (g *guard) Concurrency() tool.Concurrency { return tool.ConcurrencyOf(g.Tool) }

type guard struct {
	tool.Tool
	invoke    einotool.InvokableTool
	c         *Checkpointer
	pathField string
}

// InvokableRun captures the pre-image, then runs the tool.
//
// A capture failure does not stop the write: refusing an edit because a backup
// could not be taken would turn a degraded safety net into a broken tool. It is
// logged instead, and the turn's report shows the file as uncovered.
func (g *guard) InvokableRun(ctx context.Context, args string, opts ...einotool.Option) (string, error) {
	turn, ok := TurnFrom(ctx)
	if !ok {
		return g.invoke.InvokableRun(ctx, args, opts...)
	}
	path, ok := argString(args, g.pathField)
	if !ok {
		return g.invoke.InvokableRun(ctx, args, opts...)
	}
	if err := g.c.Capture(ctx, turn.Session, turn.Index, path); err != nil {
		if g.c.logger != nil {
			g.c.logger.Warn("checkpoint: 前像捕获失败，本次写入仍会继续",
				"session", turn.Session, "turn", turn.Index, "path", path, "error", err.Error())
		}
	}
	out, err := g.invoke.InvokableRun(ctx, args, opts...)
	if err == nil {
		// Only on success: a failed write left the file as it was, and recording
		// an after-image for it would make a later restore treat untouched content
		// as the turn's own edit.
		if rerr := g.c.RecordAfter(ctx, turn.Session, turn.Index, path); rerr != nil && g.c.logger != nil {
			g.c.logger.Warn("checkpoint: 记录写入结果失败（回退时该文件会被视为冲突）",
				"session", turn.Session, "turn", turn.Index, "path", path, "error", rerr.Error())
		}
	}
	return out, err
}

// argString reads one string argument out of a tool call's JSON.
func argString(args, field string) (string, bool) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", false
	}
	raw, ok := in[field]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// GuardNames are the tools whose writes the guard covers, with the argument that
// names the file.
//
// It is a table rather than a rule about capabilities because the question is
// "which argument is the file", and only the tool knows that. A tool that writes
// files and is not listed here is not covered — which is why the manifest reports
// what it holds rather than claiming to describe the turn.
var GuardNames = []struct {
	Tool  string
	Field string
}{
	{"write_file", "path"},
	{"edit_file", "path"},
	{"apply_patch", "path"},
}

// Describe renders a manifest for a person, listing what a rollback would do.
func (m Manifest) Describe() string {
	if len(m.Files) == 0 {
		return "这一轮没有通过文件工具改动任何文件。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "这一轮改动了 %d 个文件", len(m.Files))
	captured, skipped := 0, 0
	for _, f := range m.Files {
		if f.Skipped != "" {
			skipped++
			continue
		}
		captured++
	}
	fmt.Fprintf(&b, "（可回退 %d 个", captured)
	if skipped > 0 {
		fmt.Fprintf(&b, "，%d 个不在覆盖范围内", skipped)
	}
	b.WriteString("）：")
	for _, f := range m.Files {
		marker := ""
		if !f.Existed {
			marker = "（本轮新建）"
		}
		if f.Skipped != "" {
			marker = "（未纳入：" + f.Skipped + "）"
		}
		fmt.Fprintf(&b, "\n  %s%s", f.Path, marker)
	}
	if m.GitStatus != "" || m.GitStatusEnd != "" {
		b.WriteString("\n（检查点只覆盖通过文件工具的改动；bash 造成的改动不在其中，用 git status 看全貌。）")
	}
	return b.String()
}
