package builtin

// This file implements the two code-search tools:
//
//   - glob — "which files are there", matching file paths against a pattern.
//   - grep — "where is this used", matching file contents against a regexp.
//
// Both start from a path resolved by workspace.Resolve and report every path
// through workspace.Rel, so a result can never point outside the sandbox. The
// walk uses filepath.WalkDir, which does not follow symlinks, and every
// symlinked entry is skipped outright: resolving the starting directory alone
// would still let a link planted inside the root lead the walk out of it.
// Not following symlinks is also what a coder expects from ripgrep.
//
// Every unexported name here is prefixed "search" because sibling files in this
// package are written in parallel and must not collide.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/workspace"
)

const (
	// searchSniffBytes is how much of a file is read to decide whether it is
	// binary. It matches the sniff window workspace.IsBinary uses, and because
	// the prefix is reused by the line reader the file is still read only once.
	searchSniffBytes = 8192
	// searchMaxLineRunes caps a single displayed line so one minified file
	// cannot flood the model's context.
	searchMaxLineRunes = 400
	// searchDefaultMaxMatches is grep's match budget when max_matches is absent.
	searchDefaultMaxMatches = 100
	// searchMaxMatchesCap is the largest max_matches a caller may ask for.
	searchMaxMatchesCap = 1000
	// searchMaxContextLines is the largest context window a caller may ask for.
	searchMaxContextLines = 5
)

// searchNoiseDirs are directory names skipped by default: they are large,
// generated, or version-control metadata, and a search that descends into them
// buries the answer. A pattern (or a search root) that names one explicitly
// still searches it, because "look in .git" is a legitimate request.
var searchNoiseDirs = []string{
	".git",
	".hg",
	".svn",
	"node_modules",
	"vendor",
	"__pycache__",
	".venv",
	".idea",
	".vscode",
	".next",
	".cache",
	"dist",
}

// searchBoolDefault reports whether a pointer-to-bool behaves as the caller's
// default when the field was left out of the JSON arguments.
func searchBoolDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// searchClamp applies a default to a non-positive value and then bounds it.
func searchClamp(v, def, min, max int) int {
	if v <= 0 {
		v = def
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}

// searchResolveRoot turns the caller's path argument into a sandbox-checked
// absolute path. An empty argument means the workspace root.
func searchResolveRoot(ws *workspace.Workspace, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	abs, err := ws.Resolve(p)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", p, err)
	}
	return abs, nil
}

// searchNamesSegment reports whether a slash-separated pattern or path contains
// a segment exactly equal to name (that is, mentions it literally).
func searchNamesSegment(pattern, name string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(pattern), "/") {
		if seg == name {
			return true
		}
	}
	return false
}

// searchNoiseSkip returns the directory names a walk must not descend into.
// fromRoot is the workspace-relative directory the search starts at; mentions
// are the patterns the caller supplied. A noise directory that is the search
// root, or that a pattern names literally, is not skipped.
func searchNoiseSkip(fromRoot string, mentions ...string) map[string]bool {
	skip := make(map[string]bool, len(searchNoiseDirs))
	for _, dir := range searchNoiseDirs {
		if searchNamesSegment(fromRoot, dir) {
			continue
		}
		named := false
		for _, m := range mentions {
			if searchNamesSegment(m, dir) {
				named = true
				break
			}
		}
		if !named {
			skip[dir] = true
		}
	}
	return skip
}

// searchGlobPattern is a compiled path pattern. Matching is done segment by
// segment with path.Match, which gives the familiar glob syntax, plus "**" for
// "any number of segments including none". A pattern without any "/" is
// implicitly prefixed with "**/" so that "*.go" means "*.go at any depth",
// which is what a model (and a coder) almost always means.
type searchGlobPattern struct {
	segments []string
	raw      string
}

// searchCompileGlob validates and splits a pattern. An empty pattern list (the
// zero value) matches everything, which is how grep's optional include filter
// is represented.
func searchCompileGlob(pattern string) (searchGlobPattern, error) {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	if strings.TrimSpace(pattern) == "" {
		return searchGlobPattern{}, fmt.Errorf("pattern is required")
	}
	// A leading "/" is read as "from the workspace root" rather than as an
	// absolute host path; the search never leaves the workspace either way.
	p = strings.TrimPrefix(p, "/")

	segments := make([]string, 0, 4)
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg != "**" {
			// path.Match reports ErrBadPattern for a malformed segment.
			if _, err := path.Match(seg, "x"); err != nil {
				return searchGlobPattern{}, fmt.Errorf("invalid pattern %q: %w", pattern, err)
			}
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 {
		return searchGlobPattern{}, fmt.Errorf("pattern %q has no path segments", pattern)
	}
	if len(segments) == 1 {
		segments = append([]string{"**"}, segments...)
	}
	return searchGlobPattern{segments: segments, raw: pattern}, nil
}

// match reports whether a workspace-relative slash-separated path matches.
func (g searchGlobPattern) match(rel string) bool {
	if len(g.segments) == 0 {
		return true
	}
	return searchMatchSegments(g.segments, strings.Split(rel, "/"))
}

// searchMatchSegments matches pattern segments against path segments. "**"
// consumes zero or more segments.
func searchMatchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if searchMatchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return searchMatchSegments(pat[1:], segs[1:])
}

// GlobInput is the parameter schema for the "glob" tool.
type GlobInput struct {
	Pattern string `json:"pattern" jsonschema:"description=Path pattern matched against workspace-relative file paths. * matches inside one path segment. ? matches exactly one character inside a segment. ** matches any number of segments. A pattern with no / matches at any depth. Example: **/*_test.go,required"`
	Path    string `json:"path,omitempty" jsonschema:"description=Directory to search relative to the workspace root. Defaults to the whole workspace."`
}

// GlobOutput is what the glob tool returns. Matches are workspace-relative
// paths sorted alphabetically; Total counts every file that matched even when
// Matches was cut short by the workspace list limit.
type GlobOutput struct {
	Pattern   string   `json:"pattern"`
	Path      string   `json:"path"`
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated"`
	Total     int      `json:"total"`
}

// NewGlobTool returns a Tool that finds files by path pattern. It is the
// "which files are there" tool; use grep to search file contents.
func NewGlobTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("glob: workspace is required")
	}
	return utils.InferTool("glob",
		"Find files by path pattern — the \"which files are there\" tool. Use glob when you know (part of) a file name or its directory; use grep when you need to search file contents. "+
			"Pattern syntax: * matches any run of characters inside one path segment, ? matches exactly one character inside a segment, and ** matches any number of path segments including none. "+
			"A pattern containing no / such as `*.go` matches at any depth. Examples: `**/*_test.go`, `internal/**/*.go`, `configs/?ime.yaml`. "+
			"Results are workspace-relative file paths sorted alphabetically and capped at the workspace list limit: `truncated` says the list was cut and `total` says how many files matched altogether. "+
			"Noise directories (.git, node_modules, vendor, __pycache__ and similar) are skipped unless the pattern names them explicitly such as `.git/*`. Directories are never returned, only files. "+
			"Set `path` to search a subtree instead of the whole workspace.",
		func(ctx context.Context, in GlobInput) (GlobOutput, error) {
			return searchRunGlob(ctx, ws, in)
		})
}

// searchRunGlob walks the resolved search root and collects matching files.
func searchRunGlob(ctx context.Context, ws *workspace.Workspace, in GlobInput) (GlobOutput, error) {
	pat, err := searchCompileGlob(in.Pattern)
	if err != nil {
		return GlobOutput{}, fmt.Errorf("glob: %w", err)
	}
	root, err := searchResolveRoot(ws, in.Path)
	if err != nil {
		return GlobOutput{}, fmt.Errorf("glob: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return GlobOutput{}, fmt.Errorf("glob: path %q does not exist in the workspace", ws.Rel(root))
		}
		return GlobOutput{}, fmt.Errorf("glob: stat %q: %w", ws.Rel(root), err)
	}
	if !info.IsDir() {
		return GlobOutput{}, fmt.Errorf("glob: path %q is not a directory", ws.Rel(root))
	}

	limit := ws.Limits().MaxListEntries
	skip := searchNoiseSkip(ws.Rel(root), pat.raw)
	matches := make([]string, 0, 16)
	total := 0

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == root {
				return err
			}
			// An unreadable directory should not abort a whole search.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// WalkDir would not descend into a symlinked directory anyway;
			// skipping symlinked files keeps reported paths inside the root.
			return nil
		}
		rel := ws.Rel(p)
		if !pat.match(rel) {
			return nil
		}
		total++
		if len(matches) < limit {
			matches = append(matches, rel)
		}
		return nil
	})
	if walkErr != nil {
		return GlobOutput{}, fmt.Errorf("glob: %w", walkErr)
	}
	sort.Strings(matches)
	return GlobOutput{
		Pattern:   in.Pattern,
		Path:      ws.Rel(root),
		Matches:   matches,
		Truncated: total > len(matches),
		Total:     total,
	}, nil
}

// GrepInput is the parameter schema for the "grep" tool.
type GrepInput struct {
	Pattern       string `json:"pattern" jsonschema:"description=RE2 regular expression to search for. Example: TODO|FIXME,required"`
	Path          string `json:"path,omitempty" jsonschema:"description=File or directory to search relative to the workspace root. Defaults to the whole workspace."`
	Include       string `json:"include,omitempty" jsonschema:"description=Glob restricting which file names are searched (for example *.go). Ignored when path names a single file. Defaults to all text files."`
	MaxMatches    int    `json:"max_matches,omitempty" jsonschema:"description=Maximum number of matches to return. Default 100. Capped at 1000."`
	CaseSensitive *bool  `json:"case_sensitive,omitempty" jsonschema:"description=Match case sensitively. Default true."`
	ContextLines  int    `json:"context_lines,omitempty" jsonschema:"description=Lines of context to return around each match. Default 0. Capped at 5."`
}

// GrepMatch is one matching line. Context, present only when context_lines > 0,
// is the block of surrounding lines; the matching line itself is in the block
// with is_match true. Blocks of nearby matches may overlap, and any line in a
// block that satisfies the pattern is flagged with is_match true even when it is
// context for another match.
type GrepMatch struct {
	Path       string            `json:"path"`
	LineNumber int               `json:"line_number"`
	Line       string            `json:"line"`
	Context    []GrepContextLine `json:"context,omitempty"`
}

// GrepContextLine is one line of a context block.
type GrepContextLine struct {
	LineNumber int    `json:"line_number"`
	Line       string `json:"line"`
	IsMatch    bool   `json:"is_match"`
}

// GrepOutput is what the grep tool returns. Truncated means the match limit was
// reached, so more matches may exist. FilesSkipped counts files that were not
// searched because they exceed the workspace read limit.
type GrepOutput struct {
	Pattern       string      `json:"pattern"`
	Matches       []GrepMatch `json:"matches"`
	FilesSearched int         `json:"files_searched"`
	FilesSkipped  int         `json:"files_skipped"`
	Truncated     bool        `json:"truncated"`
	TotalMatches  int         `json:"total_matches"`
}

// NewGrepTool returns a Tool that searches file contents with an RE2 regular
// expression. It is the "where is this used" tool; use glob to find files by
// name or path.
func NewGrepTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("grep: workspace is required")
	}
	return utils.InferTool("grep",
		"Search file contents for a regular expression (RE2 syntax) — the \"where is this used\" tool. Use grep to find where a symbol, string or error message appears; use glob to find files by name or path. "+
			"Returns workspace-relative paths with 1-based line numbers and the matching line, optionally with context lines marked by `is_match`. Every displayed line is clipped to 400 characters so one minified line cannot flood the context. "+
			"Binary files, files larger than the workspace read limit, and noise directories (.git, node_modules, vendor and similar) are skipped; `files_skipped` counts the oversized ones. "+
			"Use `include` (for example `*.go`) to restrict the search to certain file names and `path` to search a single file or a subtree. "+
			"The search stops once `max_matches` matches are found and then sets `truncated`. An invalid regexp returns the compile error so the pattern can be fixed.",
		func(ctx context.Context, in GrepInput) (GrepOutput, error) {
			return searchRunGrep(ctx, ws, in)
		})
}

// searchRunGrep searches a directory tree or a single file.
func searchRunGrep(ctx context.Context, ws *workspace.Workspace, in GrepInput) (GrepOutput, error) {
	if in.Pattern == "" {
		return GrepOutput{}, fmt.Errorf("grep: pattern is required")
	}
	expr := in.Pattern
	if !searchBoolDefault(in.CaseSensitive, true) {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		// regexp's own message names the offending construct, so the model can
		// correct the pattern on the next call.
		return GrepOutput{}, fmt.Errorf("grep: invalid regular expression %q: %w", in.Pattern, err)
	}

	maxMatches := searchClamp(in.MaxMatches, searchDefaultMaxMatches, 1, searchMaxMatchesCap)
	ctxLines := searchClamp(in.ContextLines, 0, 0, searchMaxContextLines)

	root, err := searchResolveRoot(ws, in.Path)
	if err != nil {
		return GrepOutput{}, fmt.Errorf("grep: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return GrepOutput{}, fmt.Errorf("grep: path %q does not exist in the workspace", ws.Rel(root))
		}
		return GrepOutput{}, fmt.Errorf("grep: stat %q: %w", ws.Rel(root), err)
	}

	out := GrepOutput{Pattern: in.Pattern, Matches: []GrepMatch{}}
	if !info.IsDir() {
		// An explicitly named file is searched as-is; a fault here is reported
		// rather than swallowed, because the caller asked for that one file.
		scan, serr := searchScanFile(ctx, ws, root, re, ctxLines, maxMatches)
		if serr != nil {
			return GrepOutput{}, fmt.Errorf("grep: %w", serr)
		}
		switch {
		case scan.skipped:
			out.FilesSkipped = 1
		case scan.searched:
			out.FilesSearched = 1
			out.Matches = scan.matches
			out.Truncated = scan.truncated
		}
		out.TotalMatches = len(out.Matches)
		return out, nil
	}

	skip := searchNoiseSkip(ws.Rel(root), in.Include)
	// The zero value of searchGlobPattern matches every file, which is what an
	// absent include filter means.
	var include searchGlobPattern
	if strings.TrimSpace(in.Include) != "" {
		include, err = searchCompileGlob(in.Include)
		if err != nil {
			return GrepOutput{}, fmt.Errorf("grep: %w", err)
		}
	}

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel := ws.Rel(p)
		if !include.match(rel) {
			return nil
		}
		scan, serr := searchScanFile(ctx, ws, p, re, ctxLines, maxMatches-len(out.Matches))
		if serr != nil {
			// One unreadable file must not fail the whole search.
			return nil
		}
		if scan.skipped {
			out.FilesSkipped++
			return nil
		}
		if !scan.searched {
			return nil
		}
		out.FilesSearched++
		out.Matches = append(out.Matches, scan.matches...)
		if scan.truncated {
			out.Truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return GrepOutput{}, fmt.Errorf("grep: %w", walkErr)
	}
	out.TotalMatches = len(out.Matches)
	return out, nil
}

// searchScanResult is the outcome of scanning one file.
type searchScanResult struct {
	matches   []GrepMatch
	searched  bool // the file was read as text
	skipped   bool // the file was too large to read
	truncated bool // budget was exhausted inside this file
}

// searchScanFile reads one file and reports its matches. The file is opened
// once: a prefix is read first to sniff for binary content, and the line reader
// then continues from that prefix, so nothing is read twice.
func searchScanFile(ctx context.Context, ws *workspace.Workspace, abs string, re *regexp.Regexp, ctxLines, budget int) (searchScanResult, error) {
	display := ws.Rel(abs)
	f, err := os.Open(abs)
	if err != nil {
		return searchScanResult{}, fmt.Errorf("open %q: %w", display, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return searchScanResult{}, fmt.Errorf("stat %q: %w", display, err)
	}
	if info.Size() > ws.Limits().MaxReadBytes {
		// Never pull a huge file into memory just to reject it.
		return searchScanResult{skipped: true}, nil
	}

	head := make([]byte, searchSniffBytes)
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return searchScanResult{}, fmt.Errorf("read %q: %w", display, rerr)
	}
	prefix := head[:n]
	if workspace.IsBinary(prefix) {
		return searchScanResult{}, nil
	}

	reader := searchNewLineReader(io.MultiReader(bytes.NewReader(prefix), f))
	res := searchScanResult{searched: true}
	var before []searchLine

	for {
		if cerr := ctx.Err(); cerr != nil {
			return searchScanResult{}, cerr
		}
		ln, ok, rerr := reader.readLine()
		if rerr != nil {
			return searchScanResult{}, fmt.Errorf("read %q: %w", display, rerr)
		}
		if !ok {
			break
		}
		if re.MatchString(ln.text) {
			m := GrepMatch{
				Path:       display,
				LineNumber: ln.num,
				Line:       searchClipLine(ln.text),
			}
			if ctxLines > 0 {
				block := make([]GrepContextLine, 0, len(before)+1+ctxLines)
				for _, b := range before {
					// A nearby line may itself match; it is context for this
					// match but must still be flagged as a match.
					block = append(block, GrepContextLine{
						LineNumber: b.num,
						Line:       searchClipLine(b.text),
						IsMatch:    b.matched,
					})
				}
				block = append(block, GrepContextLine{LineNumber: ln.num, Line: searchClipLine(ln.text), IsMatch: true})
				// Peek, never consume: the main loop must still examine these
				// lines as matches of their own.
				for i := 0; i < ctxLines; i++ {
					next, ok, rerr := reader.peek(i)
					if rerr != nil {
						return searchScanResult{}, fmt.Errorf("read %q: %w", display, rerr)
					}
					if !ok {
						break
					}
					block = append(block, GrepContextLine{
						LineNumber: next.num,
						Line:       searchClipLine(next.text),
						IsMatch:    re.MatchString(next.text),
					})
				}
				m.Context = block
			}
			res.matches = append(res.matches, m)
			if len(res.matches) >= budget {
				res.truncated = true
				return res, nil
			}
		}
		ln.matched = re.MatchString(ln.text)
		before = searchPushRing(before, ln, ctxLines)
	}
	return res, nil
}

// searchLine is one line of a file with its 1-based number. matched records
// whether the line satisfied the pattern, so a line that matches can be flagged
// when it shows up in another match's context block.
type searchLine struct {
	num     int
	text    string
	matched bool
}

// searchLineCompactAt is how many consumed lines the reader may accumulate
// before the buffer is trimmed.
const searchLineCompactAt = 256

// searchLineReader streams lines with a bounded look-ahead. peek fills the
// buffer without moving the cursor, so a line pulled in for trailing context is
// still consumed — and therefore still matched — by the main loop afterwards.
type searchLineReader struct {
	r    *bufio.Reader
	buf  []searchLine
	pos  int
	read int
}

func searchNewLineReader(r io.Reader) *searchLineReader {
	return &searchLineReader{r: bufio.NewReader(r)}
}

// readLine consumes and returns the next line, or ok=false at end of input.
// ReadString is used rather than bufio.Scanner so that a very long line is not
// an error.
func (l *searchLineReader) readLine() (searchLine, bool, error) {
	if l.pos >= len(l.buf) {
		if _, ok, err := l.fill(); err != nil || !ok {
			return searchLine{}, false, err
		}
	}
	ln := l.buf[l.pos]
	l.pos++
	l.compact()
	return ln, true, nil
}

// peek returns the line ahead positions past the cursor (0 is the next line)
// without consuming it, or ok=false at end of input.
func (l *searchLineReader) peek(ahead int) (searchLine, bool, error) {
	for l.pos+ahead >= len(l.buf) {
		if _, ok, err := l.fill(); err != nil || !ok {
			return searchLine{}, false, err
		}
	}
	return l.buf[l.pos+ahead], true, nil
}

// fill reads one more line from the underlying reader into the buffer.
func (l *searchLineReader) fill() (searchLine, bool, error) {
	s, err := l.r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return searchLine{}, false, err
	}
	if s == "" {
		return searchLine{}, false, nil
	}
	l.read++
	ln := searchLine{num: l.read, text: searchTrimLineEnding(s)}
	l.buf = append(l.buf, ln)
	return ln, true, nil
}

// compact drops consumed lines so the buffer stays bounded in a long file.
// Lines ahead of the cursor (context look-ahead) are always kept.
func (l *searchLineReader) compact() {
	if l.pos < searchLineCompactAt {
		return
	}
	l.buf = append(l.buf[:0], l.buf[l.pos:]...)
	l.pos = 0
}

// searchPushRing appends a line to a ring holding at most size lines.
func searchPushRing(ring []searchLine, ln searchLine, size int) []searchLine {
	if size <= 0 {
		return nil
	}
	ring = append(ring, ln)
	if len(ring) > size {
		ring = ring[len(ring)-size:]
	}
	return ring
}

// searchTrimLineEnding drops the line terminator so the model sees the line as
// written, without a trailing "\r" on CRLF files.
func searchTrimLineEnding(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// searchClipLine caps a displayed line, counting runes so a multi-byte
// character is never cut in half.
func searchClipLine(s string) string {
	if utf8.RuneCountInString(s) <= searchMaxLineRunes {
		return s
	}
	return string([]rune(s)[:searchMaxLineRunes]) + "..."
}
