package edit

import (
	"fmt"
	"strconv"
	"strings"
)

// Parsing a unified diff into operations the batch applier can run.
//
// A diff is a *positional* description: the hunk header says which line to start
// at. This parser deliberately does not trust it. The header's line numbers are
// used only to report a mismatch, and the hunk is located by its **context lines**
// — the lines that are supposed to be unchanged around the edit. Two reasons:
//
//   - A diff is often applied to a file that has moved on since it was produced
//     (line numbers drift by a few lines when someone edits above). Locating by
//     context makes those diffs apply; locating by line number makes them corrupt
//     a file that was merely different.
//   - A wrong line number is silent. Applying the hunk three lines off produces a
//     file that compiles nowhere, and the model would have no idea why.
//
// The cost is that a diff whose context does not match is refused rather than
// fuzzily applied. That is the right trade for an agent: a refused patch is a
// message the model can act on, and a misapplied one is a debugging session.

// ParseUnified parses a unified diff (what `git diff` prints) into operations.
//
// It refuses the two cases this stage does not implement — creating and deleting
// files — with the alternative named, because "all or nothing" needs a story about
// the directory entry itself and that story is not written yet.
func ParseUnified(diff string) ([]Operation, error) {
	ops, _, err := ParseUnifiedWithWarnings(diff)
	return ops, err
}

// ParseUnifiedWithWarnings is ParseUnified, plus what it noticed but did not
// refuse: a hunk whose declared line counts disagree with its body.
//
// It is a warning rather than an error on purpose. A diff's numbers go stale the
// moment someone edits above the change, and the context lines are what actually
// decide where the hunk lands — so a diff with wrong numbers and matching context
// is a diff worth applying, and the reader still deserves to know the numbers were
// wrong.
func ParseUnifiedWithWarnings(diff string) ([]Operation, []string, error) {
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	lines := strings.Split(diff, "\n")

	var (
		ops      []Operation
		warnings []string

		// The file the hunks are currently being collected for.
		curPath string
		curOld  string
		curEdit []Edit

		// Declared line numbers of the hunk being read, for the mismatch warning.
		hunkDeclared int
	)
	flush := func() {
		if curPath != "" && len(curEdit) > 0 {
			ops = append(ops, Operation{Path: curPath, Edits: curEdit})
		}
		curPath, curOld, curEdit = "", "", nil
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			// The header names both sides; the --- / +++ pair below is
			// authoritative, so this is only a fallback for a diff that omits them.
			if curPath == "" {
				if p, ok := pathFromDiffGit(line); ok {
					curPath = p
				}
			}

		case strings.HasPrefix(line, "--- "):
			old := diffPath(line[4:])
			if old == "/dev/null" {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 是新建文件的（--- /dev/null），" +
					"本阶段不支持创建文件；用 write_file 创建它")
			}
			curOld = old

		case strings.HasPrefix(line, "+++ "):
			next := diffPath(line[4:])
			if next == "/dev/null" {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 是删除文件的（+++ /dev/null），" +
					"本阶段不支持删除文件；用 bash 里的 rm（会走审批）或者手工处理")
			}
			if curOld != "" && namesDifferent(curOld, next) {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 把 %s 改成了 %s（重命名或路径变更），"+
					"本阶段不支持改路径；先按原路径改内容", curOld, next)
			}
			curPath = next

		case strings.HasPrefix(line, "@@"):
			// A hunk header. `@@ -a,b +c,d @@ optional section heading`
			declared, err := parseHunkHeader(line)
			if err != nil {
				return nil, nil, err
			}
			hunkDeclared = declared
			edit, consumed, warn, err := readHunk(lines, i+1, hunkDeclared)
			if err != nil {
				return nil, nil, err
			}
			if warn != "" {
				warnings = append(warnings, warn)
			}
			i += consumed
			if curPath == "" {
				return nil, nil, fmt.Errorf("apply_patch: diff 里有 @@ 但前面没有 +++ 行，无法确定改哪个文件")
			}
			curEdit = append(curEdit, edit)

		case strings.HasPrefix(line, "\\ No newline"):
			// A marker about the preceding line, already accounted for.

		case line == "" || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "new file mode") ||
			strings.HasPrefix(line, "deleted file mode") || strings.HasPrefix(line, "similarity index") ||
			strings.HasPrefix(line, "rename from") || strings.HasPrefix(line, "rename to"):
			// Diff metadata, or the blank line git prints between file sections.
			if strings.HasPrefix(line, "new file mode") {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 是新建文件的，本阶段不支持创建文件；用 write_file 创建它")
			}
			if strings.HasPrefix(line, "deleted file mode") {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 是删除文件的，本阶段不支持删除文件；用 bash 里的 rm（会走审批）")
			}
			if strings.HasPrefix(line, "rename from") || strings.HasPrefix(line, "rename to") {
				return nil, nil, fmt.Errorf("apply_patch: 这个 diff 是重命名文件的，本阶段不支持改路径")
			}

		default:
			return nil, nil, fmt.Errorf("apply_patch: 看不懂 diff 的这一行：%q", clip(line))
		}
	}
	flush()

	if len(ops) == 0 {
		return nil, nil, fmt.Errorf("apply_patch: 这个 diff 里没有找到任何改动（需要 diff --git / --- +++ / @@ 这三种头）")
	}
	return ops, warnings, nil
}

// readHunk reads one hunk's body and builds the edit for it.
//
// The edit's old_string is the hunk's context **and** removed lines, and its
// new_string is the context and added lines. That is what makes the hunk located
// by its context: the applier has to find that exact block in the file, so a hunk
// whose surroundings changed is refused rather than applied in the wrong place.
func readHunk(lines []string, start, declared int) (Edit, int, string, error) {
	var (
		before   []string
		after    []string
		reads    int // context lines read
		removes  int
		adds     int
		consumed int
	)
	for idx := start; idx < len(lines); idx++ {
		line := lines[idx]
		consumed = idx - start + 1

		if line == "" {
			// A blank line ends the hunk: within a hunk an empty line is written as
			// a single space, so a truly empty line is the separator git prints
			// between file sections.
			consumed--
			break
		}
		if strings.HasPrefix(line, "\\ No newline") {
			// The file does not end with a newline. That affects the final byte, not
			// the text being matched, so it is noted and skipped.
			continue
		}
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			consumed--
			break
		}

		switch line[0] {
		case ' ':
			before = append(before, line[1:])
			after = append(after, line[1:])
			reads++
		case '-':
			before = append(before, line[1:])
			removes++
		case '+':
			after = append(after, line[1:])
			adds++
		default:
			return Edit{}, 0, "", fmt.Errorf("apply_patch: hunk 里有一行不以空格、- 或 + 开头：%q", clip(line))
		}
	}

	if len(before) == 0 {
		// Nothing to find means nothing to match: a hunk that only adds lines with
		// no context gives the applier no anchor at all.
		return Edit{}, 0, "", fmt.Errorf("apply_patch: 有一处改动只有新增行、没有任何上下文行，" +
			"无法确定它该落在哪里；请让 diff 带上前后各一行上下文（git diff -U1 或默认的 -U3）")
	}

	warning := ""
	if declared > 0 && declared != reads+removes {
		warning = fmt.Sprintf("diff 里 @@ 声明旧文件有 %d 行，实际上下文加删除是 %d 行；"+
			"以上下文行定位（行数只是提示）", declared, reads+removes)
	}
	if adds+reads == 0 {
		return Edit{}, 0, "", fmt.Errorf("apply_patch: hunk 里既没有新增也没有删除")
	}
	return Edit{
		Old: strings.Join(before, "\n"),
		New: strings.Join(after, "\n"),
	}, consumed, warning, nil
}

// parseHunkHeader reads `@@ -old,count +new,count @@` and returns the declared
// number of lines the old side covers.
func parseHunkHeader(line string) (int, error) {
	rest := strings.TrimPrefix(line, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, fmt.Errorf("apply_patch: 这个 hunk 头不完整：%q", clip(line))
	}
	spec := strings.TrimSpace(rest[:end])
	// spec is "-12,3 +12,4" (the second field may be omitted when it is 1).
	parts := strings.Fields(spec)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "-") {
		return 0, fmt.Errorf("apply_patch: 看不懂这个 hunk 头：%q", clip(line))
	}
	oldSpec := strings.TrimPrefix(parts[0], "-")
	count := 1
	if comma := strings.Index(oldSpec, ","); comma >= 0 {
		n, err := strconv.Atoi(strings.TrimSpace(oldSpec[comma+1:]))
		if err != nil {
			return 0, fmt.Errorf("apply_patch: hunk 头里的行数不是数字：%q", clip(line))
		}
		count = n
	} else if _, err := strconv.Atoi(strings.TrimSpace(oldSpec)); err != nil {
		return 0, fmt.Errorf("apply_patch: hunk 头里的起始行不是数字：%q", clip(line))
	}
	return count, nil
}

// diffPath strips the a/ or b/ prefix and the trailing timestamp a diff may carry.
func diffPath(raw string) string {
	field := strings.TrimSpace(raw)
	// `--- a/x.go\t2024-01-01 00:00:00` and `+++ b/x.go` are both common.
	if tab := strings.IndexAny(field, "\t"); tab >= 0 {
		field = field[:tab]
	}
	if sp := strings.LastIndex(field, " "); sp >= 0 {
		// A timestamp separated by a space (the POSIX form).
		if strings.Contains(field[sp+1:], ":") {
			field = field[:sp]
		}
	}
	field = strings.TrimSpace(field)
	if field == "/dev/null" {
		return field
	}
	for _, prefix := range []string{"a/", "b/", "./"} {
		if strings.HasPrefix(field, prefix) {
			return strings.TrimPrefix(field, prefix)
		}
	}
	return field
}

// pathFromDiffGit reads the b/ path out of a `diff --git a/x b/y` header.
func pathFromDiffGit(line string) (string, bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return "", false
	}
	return diffPath(fields[1]), true
}

// namesDifferent reports whether two diff paths name different files, ignoring the
// prefix difference a diff header may have.
func namesDifferent(a, b string) bool {
	return strings.TrimPrefix(a, "a/") != strings.TrimPrefix(b, "b/")
}

// clip bounds a line before it goes into an error message.
func clip(line string) string {
	const max = 120
	if len(line) <= max {
		return line
	}
	return line[:max] + "…"
}
