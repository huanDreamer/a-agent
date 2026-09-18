// Package edit applies a batch of text edits to several files, atomically.
//
// The problem it solves is the middle state. Renaming something that is used in
// seven files means seven edits, and an edit tool that applies them one call at a
// time leaves a repository that compiles nowhere the moment the fourth fails —
// with the first three already on disk and no record of what they were. So this
// package is two phases:
//
//  1. **Plan** — read every file, apply every edit in memory, and decide whether
//     the whole batch is possible. It touches no disk. If anything is wrong, the
//     answer is one error naming the file and the edit, and the workspace is
//     byte-for-byte what it was.
//  2. **Apply** — take a checkpoint of every file about to change, then write
//     each one atomically. If a write fails, the files already written are put
//     back from the originals phase 1 kept.
//
// The invariant both phases exist to protect: **either every edit is on disk, or
// none is.**
package edit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/huan/huan-agent/internal/workspace"
)

// Edit is one replacement inside one file.
type Edit struct {
	// Old is the text to find. It must be non-empty: an empty string matches
	// everywhere, including between every pair of characters.
	Old string
	// New replaces it.
	New string
	// ReplaceAll replaces every occurrence. Without it, more than one occurrence
	// is an error rather than a guess.
	ReplaceAll bool
	// Position, when set, applies the edit at a known location instead of
	// searching for the text.
	//
	// A rename needs this. Renaming a symbol that appears three times produces
	// three edits whose old_string is the same word, and treating them as text
	// edits would make the second one "ambiguous" — the applier would refuse a
	// perfectly well-specified change, because *where* the edit goes is exactly
	// what the language server told us and exactly what a text search cannot know.
	//
	// Positions are applied from the bottom of the file upwards, so an edit never
	// invalidates the coordinates of one still to come. That is what makes a list
	// of positional edits a well-defined batch rather than an ordering puzzle.
	Position *Span
}

// Span is a range in a file: 0-based lines, UTF-16 columns, as the language server
// protocol defines them.
type Span struct {
	StartLine int
	StartChar int
	EndLine   int
	EndChar   int
}

// Operation is one file's edits.
type Operation struct {
	// Path is resolved through the workspace sandbox.
	Path string
	// Edits are applied in order, each to the result of the previous one.
	Edits []Edit
}

// Match applies one edit to content, using the same rules as edit_file.
//
// It is the single implementation of "how an edit finds its target", shared so
// that the model does not need a second mental model and so that the two tools'
// failures read the same way.
func Match(content string, e Edit) (result string, count int, err error) {
	if e.Position != nil {
		return matchAt(content, e)
	}
	if e.Old == "" {
		return "", 0, fmt.Errorf("old_string is required and must not be empty; an empty string matches everywhere")
	}
	count = strings.Count(content, e.Old)
	switch {
	case count == 0:
		return "", 0, errNotFound
	case count > 1 && !e.ReplaceAll:
		return "", count, errAmbiguous
	}
	replacements := 1
	if e.ReplaceAll {
		replacements = count
	}
	// n is the known count, never -1: the checks above guarantee a single
	// occurrence unless the caller asked for every one, and a replace-everything
	// fallback is exactly how the wrong occurrence gets clobbered.
	return strings.Replace(content, e.Old, e.New, replacements), count, nil
}

// matchAt applies a positional edit.
//
// The text at the position is checked against Old before anything is spliced, so a
// stale position (a file that changed under the version the server was editing)
// fails loudly instead of overwriting whatever happens to be there now.
func matchAt(content string, e Edit) (string, int, error) {
	lines := strings.Split(content, "\n")
	span := *e.Position
	if span.StartLine < 0 || span.EndLine >= len(lines) || span.StartLine > span.EndLine {
		return "", 0, fmt.Errorf("位置 %d:%d-%d:%d 超出了文件范围（文件有 %d 行）",
			span.StartLine+1, span.StartChar+1, span.EndLine+1, span.EndChar+1, len(lines))
	}
	start, err := runeOffset(lines[span.StartLine], span.StartChar)
	if err != nil {
		return "", 0, err
	}
	end, err := runeOffset(lines[span.EndLine], span.EndChar)
	if err != nil {
		return "", 0, err
	}
	if span.StartLine == span.EndLine && end < start {
		return "", 0, fmt.Errorf("位置 %d:%d-%d:%d 的结束在开始之前",
			span.StartLine+1, span.StartChar+1, span.EndLine+1, span.EndChar+1)
	}

	var updated string
	if span.StartLine == span.EndLine {
		line := []rune(lines[span.StartLine])
		if start > len(line) || end > len(line) {
			return "", 0, fmt.Errorf("位置 %d:%d-%d:%d 超过了这一行的长度",
				span.StartLine+1, span.StartChar+1, span.EndLine+1, span.EndChar+1)
		}
		found := string(line[start:end])
		if e.Old != "" && found != e.Old {
			return "", 0, fmt.Errorf("位置 %d:%d 上的文本是 %q，不是 %q（文件可能已经变了）",
				span.StartLine+1, span.StartChar+1, found, e.Old)
		}
		replaced := string(line[:start]) + e.New + string(line[end:])
		// Every line before this one is kept: omitting them is not a subtle bug, it
		// is deleting the top of the file — and it shows up as the *next* edit
		// failing with a position past the end, which is a long way from the cause.
		out := make([]string, 0, len(lines))
		out = append(out, lines[:span.StartLine]...)
		out = append(out, replaced)
		out = append(out, lines[span.StartLine+1:]...)
		updated = strings.Join(out, "\n")
		return updated, 1, nil
	}

	// A span across lines: everything inside is replaced.
	head := []rune(lines[span.StartLine])
	tail := []rune(lines[span.EndLine])
	if start > len(head) || end > len(tail) {
		return "", 0, fmt.Errorf("位置 %d:%d-%d:%d 超过了一行的长度",
			span.StartLine+1, span.StartChar+1, span.EndLine+1, span.EndChar+1)
	}
	found := string(head[start:]) + "\n" +
		strings.Join(lines[span.StartLine+1:span.EndLine], "\n") + "\n" + string(tail[:end])
	if e.Old != "" && found != e.Old {
		return "", 0, fmt.Errorf("位置 %d:%d 上的文本与预期不符（文件可能已经变了）",
			span.StartLine+1, span.StartChar+1)
	}
	middle := string(head[:start]) + e.New + string(tail[end:])
	out := make([]string, 0, len(lines))
	out = append(out, lines[:span.StartLine]...)
	out = append(out, middle)
	out = append(out, lines[span.EndLine+1:]...)
	return strings.Join(out, "\n"), 1, nil
}

// Sentinel reasons for a failed match, so a caller can say which one happened
// without parsing a message.
var (
	errNotFound  = errors.New("not found")
	errAmbiguous = errors.New("ambiguous")
)

// FilePlan is one file's computed result.
type FilePlan struct {
	// Path is workspace-relative, which is what a report and a diff should show.
	Path string
	// Abs is the resolved absolute path.
	Abs string
	// Original is what the file holds now, kept for the rollback phase.
	Original []byte
	// Updated is what it will hold.
	Updated []byte
	// Mode is the file's permission bits, preserved across the write.
	Mode os.FileMode
	// Added and Removed are the line counts, for the "+N -M" a report shows.
	Added   int
	Removed int
	// Replacements is how many edits landed.
	Replacements int
}

// Plan is the whole batch.
type Plan struct {
	// Files are in the order the operations named them.
	Files []FilePlan
	// Added and Removed are the totals.
	Added   int
	Removed int
}

// Options bounds what a plan may touch.
type Options struct {
	// MaxBytes is the per-file write limit. 0 uses the workspace's.
	MaxBytes int64
	// MaxFiles bounds one batch, so a mistaken patch cannot open a thousand
	// files.
	MaxFiles int
	// ReadOnly refuses every operation, for a workspace that forbids writes.
	ReadOnly bool
	// Prefix is the tool name used in error messages ("apply_patch"), so the
	// same code reports in the voice of whichever tool called it.
	Prefix string
}

// DefaultMaxFiles bounds one batch.
const DefaultMaxFiles = 200

// PlanBatch performs phase one: it reads, validates and computes, and writes
// nothing.
//
// Every failure names the file and the edit index, because "one of your edits did
// not match" is not something a model can act on: it needs to know which, in which
// file, and what to do about it.
func PlanBatch(ctx context.Context, ws *workspace.Workspace, ops []Operation, opts Options) (*Plan, error) {
	if ws == nil {
		return nil, fmt.Errorf("%s: 没有可用的工作区", opts.tool())
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%s: operations 不能为空", opts.tool())
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	if len(ops) > maxFiles {
		return nil, fmt.Errorf("%s: 一次最多改 %d 个文件，这次是 %d 个；拆成几次调用",
			opts.tool(), maxFiles, len(ops))
	}
	if opts.ReadOnly {
		return nil, fmt.Errorf("%s: 这个工作区是只读的", opts.tool())
	}

	seen := make(map[string]int, len(ops))
	plan := &Plan{}

	for i, op := range ops {
		if len(op.Edits) == 0 {
			return nil, fmt.Errorf("%s: operations[%d] (%s) 没有 edits", opts.tool(), i, op.Path)
		}
		abs, err := ws.Resolve(strings.TrimSpace(op.Path))
		if err != nil {
			return nil, fmt.Errorf("%s: operations[%d] (%s): %w", opts.tool(), i, op.Path, err)
		}
		abs = workspace.RealPath(abs)
		rel := ws.Rel(abs)

		// The same path twice would make phase one and phase two disagree: the
		// second operation would plan against content the first one is about to
		// replace. It is checked before the file is read because it is a problem
		// with the request, not with the file.
		if prev, dup := seen[abs]; dup {
			return nil, fmt.Errorf("%s: %s 在一次操作里出现了两次（operations[%d] 与 operations[%d]）；"+
				"把它的 edits 合并到一个 operation 里", opts.tool(), rel, prev, i)
		}
		seen[abs] = i

		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", opts.tool(), rel, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s: %s 是目录，只能改文件", opts.tool(), rel)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: 读取 %s 失败：%w", opts.tool(), rel, err)
		}
		if workspace.IsBinary(data) {
			return nil, fmt.Errorf("%s: %s 看起来是二进制文件，拒绝按文本改它", opts.tool(), rel)
		}

		content := string(data)
		updated := content
		replacements := 0
		for j, e := range op.Edits {
			next, count, err := Match(updated, e)
			if err != nil {
				return nil, editError(opts.tool(), rel, i, j, e, count, err)
			}
			updated = next
			if e.ReplaceAll {
				replacements += count
			} else {
				replacements++
			}
		}
		if updated == content {
			// Not an error: an edit that replaces text with itself is a no-op the
			// model may well have meant. It is worth saying so, but not worth
			// refusing the batch.
			replacements = 0
		}

		maxBytes := opts.MaxBytes
		if maxBytes <= 0 {
			maxBytes = workspace.DefaultMaxWriteBytes
		}
		if int64(len(updated)) > maxBytes {
			return nil, fmt.Errorf("%s: %s 改完之后是 %d 字节，超过单文件上限 %d 字节",
				opts.tool(), rel, len(updated), maxBytes)
		}

		added, removed := lineDelta(content, updated)
		plan.Files = append(plan.Files, FilePlan{
			Path:         rel,
			Abs:          abs,
			Original:     data,
			Updated:      []byte(updated),
			Mode:         info.Mode().Perm(),
			Added:        added,
			Removed:      removed,
			Replacements: replacements,
		})
		plan.Added += added
		plan.Removed += removed
	}
	return plan, nil
}

// editError turns a match failure into the message a model can act on.
//
// Anything that is not one of the two sentinels is passed through with its own
// message: a positional edit that failed says why (a stale position, a range past
// the end), and reporting it as "old_string not found" would send the model to
// re-read a file that is not the problem.
func editError(tool, rel string, opIndex, editIndex int, e Edit, count int, err error) error {
	switch {
	case errors.Is(err, errAmbiguous):
		return fmt.Errorf("%s: operations[%d] (%s) 的第 %d 条 edit 的 old_string 出现了 %d 次，"+
			"所以这次修改是有歧义的；多带一点上下文让它唯一，或者设 replace_all=true 改掉所有出现",
			tool, opIndex, rel, editIndex, count)
	case errors.Is(err, errNotFound):
		return fmt.Errorf("%s: operations[%d] (%s) 的第 %d 条 edit 的 old_string 没有找到；"+
			"用 read_file 重新读一下这个文件，把目标文本连空白一起原样复制过来",
			tool, opIndex, rel, editIndex)
	default:
		return fmt.Errorf("%s: operations[%d] (%s) 的第 %d 条 edit 无法应用：%w",
			tool, opIndex, rel, editIndex, err)
	}
}

// lineDelta counts the lines added and removed, which is what a report shows and
// what a person reads to judge a batch.
func lineDelta(before, after string) (added, removed int) {
	if before == after {
		return 0, 0
	}
	oldLines := strings.Split(before, "\n")
	newLines := strings.Split(after, "\n")

	// A common-prefix/suffix diff rather than a full LCS: it is exact for the
	// insertions and deletions an edit tool produces (which are localized), it is
	// obviously linear, and a wrong count in a report is a cosmetic problem while
	// a quadratic diff on a large file is a hang.
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	return len(newLines) - prefix - suffix, len(oldLines) - prefix - suffix
}

// Preview renders a bounded diff of the plan, for dry_run output and for the
// approval card.
//
// It is computed from the plan rather than from the disk, so the preview a person
// approves is the change that will happen, not a description of it.
func (p *Plan) Preview(maxLinesPerFile int) []PreviewEntry {
	if p == nil {
		return nil
	}
	if maxLinesPerFile <= 0 {
		maxLinesPerFile = 20
	}
	out := make([]PreviewEntry, 0, len(p.Files))
	for _, f := range p.Files {
		entry := PreviewEntry{Path: f.Path, Added: f.Added, Removed: f.Removed}
		oldLines := strings.Split(string(f.Original), "\n")
		newLines := strings.Split(string(f.Updated), "\n")
		prefix := 0
		for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
			oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
			suffix++
		}
		for _, l := range oldLines[prefix : len(oldLines)-suffix] {
			if len(entry.Lines) >= maxLinesPerFile {
				entry.Truncated = true
				break
			}
			entry.Lines = append(entry.Lines, PreviewLine{Kind: "del", Text: l})
		}
		for _, l := range newLines[prefix : len(newLines)-suffix] {
			if len(entry.Lines) >= maxLinesPerFile {
				entry.Truncated = true
				break
			}
			entry.Lines = append(entry.Lines, PreviewLine{Kind: "add", Text: l})
		}
		out = append(out, entry)
	}
	return out
}

// PreviewEntry is one file's preview.
type PreviewEntry struct {
	Path      string        `json:"path"`
	Added     int           `json:"added"`
	Removed   int           `json:"removed"`
	Lines     []PreviewLine `json:"lines,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// PreviewLine is one line of a preview.
type PreviewLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Capturer takes a checkpoint before a file is changed.
//
// It is an interface so this package does not depend on the checkpoint package: a
// batch edit needs "record the pre-image first", not the whole machinery.
type Capturer interface {
	Capture(ctx context.Context, session string, turn int, path string) error
}

// Apply performs phase two: checkpoints, then writes, then rolls back on failure.
func Apply(ctx context.Context, ws *workspace.Workspace, plan *Plan, opts ApplyOptions) (Report, error) {
	report := Report{}
	if plan == nil || len(plan.Files) == 0 {
		return report, fmt.Errorf("%s: 没有要应用的改动", opts.tool())
	}

	// Checkpoints first, for every file, before anything is touched. The two-phase
	// apply guarantees consistency *within* this call; checkpoints are what make it
	// undoable *after* it — and a checkpoint taken after the first write would
	// record the changed file.
	if opts.Capturer != nil {
		session, turn := opts.Session, opts.Turn
		if opts.TurnFrom != nil {
			if s, t, ok := opts.TurnFrom(ctx); ok {
				session, turn = s, t
			}
		}
		for _, f := range plan.Files {
			if err := opts.Capturer.Capture(ctx, session, turn, f.Path); err != nil && opts.Logger != nil {
				opts.Logger.Warn("edit: 取检查点失败，这次调用仍然继续（这一轮的回退会缺这个文件）",
					"path", f.Path, "error", err.Error())
			}
		}
	}

	written := make([]FilePlan, 0, len(plan.Files))
	for _, f := range plan.Files {
		n, err := workspace.WriteFileAtomic(f.Abs, f.Updated, f.Mode)
		if err != nil {
			// Phase two failed partway. Put back what it already wrote, so the
			// workspace is what it was before the call rather than a mixture of
			// old and new that compiles nowhere.
			rolledBack, rollbackErr := rollback(written)
			report.RolledBack = rolledBack
			report.Files = append(report.Files, FileResult{
				Path: f.Path, Added: f.Added, Removed: f.Removed, Error: err.Error(),
			})
			if rollbackErr != nil {
				// A failed rollback is the one outcome worse than a partial write,
				// and the report has to say so loudly: the workspace is now in a
				// state nobody chose.
				return report, fmt.Errorf("%s: 写入 %s 失败（%w），而且回滚也没有完成（%v）；"+
					"磁盘现在处于混合状态，请用 git status 检查并手工恢复",
					opts.tool(), f.Path, err, rollbackErr)
			}
			return report, fmt.Errorf("%s: 写入 %s 失败（%w）；已经写入的 %d 个文件已回滚，"+
				"磁盘与你调用前一致，没有任何文件被改动",
				opts.tool(), f.Path, err, len(rolledBack))
		}
		written = append(written, f)
		report.Files = append(report.Files, FileResult{
			Path: f.Path, Added: f.Added, Removed: f.Removed, Bytes: n,
			Replacements: f.Replacements,
		})
	}
	report.Added = plan.Added
	report.Removed = plan.Removed
	// The write counter is what the workspace's own accounting reports; the atomic
	// write bypasses the tool layer, so it is updated here.
	if ws != nil {
		ws.RecordWrites(int64(len(written)))
	}
	return report, nil
}

// rollback puts the files already written back, and reports which ones it managed.
func rollback(written []FilePlan) ([]string, error) {
	var done []string
	var firstErr error
	for _, f := range written {
		if _, err := workspace.WriteFileAtomic(f.Abs, f.Original, f.Mode); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", f.Path, err)
			}
			continue
		}
		done = append(done, f.Path)
	}
	return done, firstErr
}

// ApplyOptions configures phase two.
type ApplyOptions struct {
	// Capturer records pre-images; nil skips it.
	Capturer Capturer
	// TurnFrom reads the checkpoint turn off the context. It is a function rather
	// than two fields because the turn belongs to the *call*, not to the tool: a
	// console turn opens its checkpoint on the context, and a tool built once at
	// startup cannot know it.
	//
	// When it returns false, Session and Turn are used instead.
	TurnFrom func(ctx context.Context) (session string, turn int, ok bool)
	// Session and Turn identify the checkpoint turn.
	Session string
	Turn    int
	// Logger reports a checkpoint failure without failing the batch.
	Logger Logger
	// Prefix is the tool name used in error messages.
	Prefix string
}

// Logger is the slice of logging this package needs.
type Logger interface {
	Warn(msg string, keysAndValues ...any)
}

// Report is what a batch did.
type Report struct {
	Files      []FileResult `json:"files"`
	Added      int          `json:"added"`
	Removed    int          `json:"removed"`
	RolledBack []string     `json:"rolled_back,omitempty"`
}

// FileResult is one file's outcome.
type FileResult struct {
	Path         string `json:"path"`
	Added        int    `json:"added"`
	Removed      int    `json:"removed"`
	Bytes        int64  `json:"bytes,omitempty"`
	Replacements int    `json:"replacements,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Summary renders the batch for a person: what changed, and nothing else.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "改了 %d 个文件（+%d -%d）：", len(r.Files), r.Added, r.Removed)
	for _, f := range r.Files {
		fmt.Fprintf(&b, "\n  %s  +%d -%d", f.Path, f.Added, f.Removed)
	}
	if len(r.RolledBack) > 0 {
		fmt.Fprintf(&b, "\n已回滚：%s", strings.Join(r.RolledBack, "、"))
	}
	return b.String()
}

// tool returns the name to report under.
func (o Options) tool() string {
	if strings.TrimSpace(o.Prefix) != "" {
		return o.Prefix
	}
	return "edit"
}

func (o ApplyOptions) tool() string {
	if strings.TrimSpace(o.Prefix) != "" {
		return o.Prefix
	}
	return "edit"
}

// absoluteWithin reports whether abs is inside root. It is a belt-and-braces
// check for the rename path, where paths arrive from a language server rather
// than from the sandbox.
func absoluteWithin(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Workspace binds a sandbox and the batch's bounds to the two phases.
//
// A tool is handed one of these rather than the workspace plus three option sets:
// what a tool needs to know is "plan this batch and apply it inside the limits this
// deployment set", and bundling them is what keeps the limits from being passed
// inconsistently at one of the two call sites.
type Workspace struct {
	ws    *workspace.Workspace
	plan  Options
	apply ApplyOptions
}

// NewWorkspace binds them.
func NewWorkspace(ws *workspace.Workspace, plan Options, apply ApplyOptions) *Workspace {
	if plan.Prefix == "" {
		plan.Prefix = "apply_patch"
	}
	if apply.Prefix == "" {
		apply.Prefix = plan.Prefix
	}
	return &Workspace{ws: ws, plan: plan, apply: apply}
}

// Plan performs phase one.
func (w *Workspace) Plan(ctx context.Context, ops []Operation) (*Plan, error) {
	return PlanBatch(ctx, w.ws, ops, w.plan)
}

// Apply performs phase two.
func (w *Workspace) Apply(ctx context.Context, plan *Plan) (Report, error) {
	return Apply(ctx, w.ws, plan, w.apply)
}

// Sandbox exposes the underlying workspace, for a caller that needs it (the
// rename path resolves language-server paths through it).
func (w *Workspace) Sandbox() *workspace.Workspace { return w.ws }
