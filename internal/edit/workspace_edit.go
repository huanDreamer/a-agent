package edit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/huan/huan-agent/internal/lsp"
	"github.com/huan/huan-agent/internal/workspace"
)

// Turning a language server's rename into a batch this package can apply.
//
// The whole point of routing a rename through this package is that the rename
// inherits the two-phase invariant: a rename that touches nine files either lands
// in all of them or in none. The conversion is therefore a *planning* step, and it
// is where the out-of-bounds check has to happen — before anything is written.

// FromWorkspaceEdit converts a rename's answer into operations against a
// workspace.
//
// **A path outside the sandbox fails the whole conversion**, and the error lists
// every offending path rather than the first one: a rename half-applied inside the
// workspace is worse than a refused one, because the parts outside were left
// referring to a name that no longer exists here.
func FromWorkspaceEdit(ws *workspace.Workspace, we lsp.WorkspaceEdit) ([]Operation, error) {
	if ws == nil {
		return nil, fmt.Errorf("rename: 没有可用的工作区")
	}
	files := we.Flatten()
	if len(files) == 0 {
		return nil, fmt.Errorf("rename: 语言服务器没有给出任何改动")
	}

	root := ws.Root()
	var outside []string
	ops := make([]Operation, 0, len(files))

	for _, f := range files {
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("rename: 语言服务器给出的文件路径无法解析")
		}
		abs := filepath.Clean(f.Path)
		// The language server names absolute paths, which have not been through
		// the sandbox. Resolving the *relative* form is what puts them through it.
		if !withinRoot(root, abs) {
			rel, rerr := filepath.Rel(root, abs)
			if rerr != nil {
				rel = abs
			}
			outside = append(outside, rel)
			continue
		}
		rel := ws.Rel(abs)

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("rename: 读取 %s 失败：%w", rel, err)
		}
		if workspace.IsBinary(data) {
			return nil, fmt.Errorf("rename: %s 看起来是二进制文件，拒绝按文本改它", rel)
		}
		content := string(data)

		edits, err := editsForFile(rel, content, f.Edits)
		if err != nil {
			return nil, err
		}
		if len(edits) == 0 {
			continue
		}
		ops = append(ops, Operation{Path: rel, Edits: edits})
	}

	if len(outside) > 0 {
		sort.Strings(outside)
		return nil, fmt.Errorf("rename: 这次重命名会改到工作区之外的 %d 个文件（%s），"+
			"所以整个操作被拒绝——半次重命名比失败更糟：工作区外的部分会继续引用一个已经不存在的名字",
			len(outside), strings.Join(outside, "、"))
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("rename: 没有落在工作区内的改动")
	}
	return ops, nil
}

// spanText extracts the text a 0-based, UTF-16-columned range covers. It is used
// to check what is actually at a span, so a stale position fails loudly rather
// than replacing whatever has since moved there.
func spanText(lines []string, startLine, startChar, endLine, endChar int) (string, error) {
	if startLine < 0 || endLine >= len(lines) || startLine > endLine {
		return "", fmt.Errorf("改动区间 %d:%d-%d:%d 超出了文件范围", startLine+1, startChar+1, endLine+1, endChar+1)
	}
	if startLine == endLine {
		line := lines[startLine]
		s, err := runeOffset(line, startChar)
		if err != nil {
			return "", err
		}
		e, err := runeOffset(line, endChar)
		if err != nil {
			return "", err
		}
		runes := []rune(line)
		if s > len(runes) {
			s = len(runes)
		}
		if e > len(runes) {
			e = len(runes)
		}
		if e < s {
			return "", fmt.Errorf("改动区间的结束位置在开始位置之前")
		}
		return string(runes[s:e]), nil
	}
	var b strings.Builder
	first := []rune(lines[startLine])
	s, err := runeOffset(lines[startLine], startChar)
	if err != nil {
		return "", err
	}
	if s > len(first) {
		s = len(first)
	}
	b.WriteString(string(first[s:]))
	for i := startLine + 1; i < endLine; i++ {
		b.WriteString("\n")
		b.WriteString(lines[i])
	}
	last := []rune(lines[endLine])
	e, err := runeOffset(lines[endLine], endChar)
	if err != nil {
		return "", err
	}
	if e > len(last) {
		e = len(last)
	}
	b.WriteString("\n")
	b.WriteString(string(last[:e]))
	return b.String(), nil
}

// editsForFile converts one file's text edits into edit operations.
//
// Every edit is positional, and they are ordered from the bottom of the file
// upwards. Both matter:
//
//   - *Positional*, because a rename's edits all name the same word. As text edits
//     the second one would be "ambiguous" and the batch would refuse a change the
//     language server specified exactly.
//   - *Bottom-up*, because applying an edit near the end of a file cannot move
//     anything above it, so the coordinates still to be used stay valid. Applying
//     them top-down would shift every later span by the length difference, which is
//     the bug that only shows up in files with more than one reference.
func editsForFile(rel, content string, textEdits []lsp.TextEdit) ([]Edit, error) {
	if len(textEdits) == 0 {
		return nil, nil
	}
	lines := strings.Split(content, "\n")

	spans := make([]lsp.Range, 0, len(textEdits))
	texts := make([]string, 0, len(textEdits))
	for _, te := range textEdits {
		spans = append(spans, te.Range)
		texts = append(texts, te.NewText)
	}
	// Bottom-up, and right-to-left within a line.
	order := make([]int, len(spans))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := spans[order[a]], spans[order[b]]
		if ra.Start.Line != rb.Start.Line {
			return ra.Start.Line > rb.Start.Line
		}
		return ra.Start.Character > rb.Start.Character
	})

	edits := make([]Edit, 0, len(order))
	for _, i := range order {
		r := spans[i]
		if r.Start.Line < 0 || r.End.Line >= len(lines) || r.Start.Line > r.End.Line {
			return nil, fmt.Errorf("rename: %s: 语言服务器给出的区间 %d:%d-%d:%d 超出了文件范围（文件有 %d 行）",
				rel, r.Start.Line+1, r.Start.Character+1, r.End.Line+1, r.End.Character+1, len(lines))
		}
		old, err := spanText(lines, r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
		if err != nil {
			return nil, fmt.Errorf("rename: %s: %w", rel, err)
		}
		if old == "" {
			return nil, fmt.Errorf("rename: %s: 语言服务器给出的改动区间是空的（%d:%d-%d:%d），无法定位要替换的文本",
				rel, r.Start.Line+1, r.Start.Character+1, r.End.Line+1, r.End.Character+1)
		}
		edits = append(edits, Edit{
			Old: old,
			New: texts[i],
			Position: &Span{
				StartLine: r.Start.Line, StartChar: r.Start.Character,
				EndLine: r.End.Line, EndChar: r.End.Character,
			},
		})
	}
	return edits, nil
}

// runeOffset converts a UTF-16 column to a rune index within a line.
func runeOffset(line string, column int) (int, error) {
	if column < 0 {
		return 0, fmt.Errorf("列号 %d 是负数", column)
	}
	runes := []rune(line)
	units := 0
	for i, r := range runes {
		if units >= column {
			return i, nil
		}
		units += utf16Len(r)
	}
	if units < column {
		// A column past the end of the line: the server counted a character that
		// is not there, which happens with trailing whitespace the editor shows
		// and the file does not have.
		return len(runes), nil
	}
	return len(runes), nil
}

// utf16Len is how many UTF-16 code units a rune occupies.
func utf16Len(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// withinRoot reports whether abs is inside root.
func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// PlanWorkspaceEdit converts a rename and plans it in one step.
//
// A batch adapter exposes it so a tool never has to hold the sandbox itself: the
// conversion needs it (paths from a language server have not been through it) and
// the planning needs it, and passing both around separately is how one of them ends
// up being the wrong workspace.
func (w *Workspace) PlanWorkspaceEdit(ctx context.Context, we lsp.WorkspaceEdit) (*Plan, error) {
	ops, err := FromWorkspaceEdit(w.ws, we)
	if err != nil {
		return nil, err
	}
	return PlanBatch(ctx, w.ws, ops, w.plan)
}

// The two interfaces the editing tools declare are both satisfied by this adapter,
// which is the point: a patch and a rename share one atomicity implementation, so
// there is only one place where "either every edit lands or none does" is written.
var _ interface {
	Plan(context.Context, []Operation) (*Plan, error)
	Apply(context.Context, *Plan) (Report, error)
	PlanWorkspaceEdit(context.Context, lsp.WorkspaceEdit) (*Plan, error)
} = (*Workspace)(nil)
