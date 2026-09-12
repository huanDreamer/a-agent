package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/workspace"
)

// Fixture contents. The tree deliberately contains a version-control directory
// and a package-manager directory with decoys, plus a binary file, so the
// default skips are exercised by every search.
const (
	searchMainGoText = "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	searchUtilGoText = "package sub\n\n// Helper returns a helper string.\nfunc Helper() string {\n\treturn \"helper\"\n}\n"
	searchDeepGoText = "package deep\n\nconst Name = \"deep\"\n"
	searchNotesText  = "alpha\nbeta gamma\ntarget here\ndelta\nTARGET upper\nepsilon\n"
	searchReadmeText = "# Fixture\n\nA sample project.\n"
	searchGitCfgText = "needle\n"
	searchGitGoText  = "package main\n\n// needle decoy\n"
	searchNMGoText   = "package nm\n\n// needle in node_modules\n"
	searchBinaryText = "\x00\x01needle\x00\xff"
)

// searchNewWorkspace builds a workspace over dir.
func searchNewWorkspace(t *testing.T, dir string, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(dir, opts)
	if err != nil {
		t.Fatalf("workspace.New(%q): %v", dir, err)
	}
	return ws
}

// searchWrite creates a file (and its parents) under root.
func searchWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

// searchFixture builds the standard tree and returns a read-only workspace.
func searchFixture(t *testing.T) *workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	searchWrite(t, dir, "main.go", searchMainGoText)
	searchWrite(t, dir, "README.md", searchReadmeText)
	searchWrite(t, dir, "notes.txt", searchNotesText)
	searchWrite(t, dir, "blob.bin", searchBinaryText)
	searchWrite(t, dir, "sub/util.go", searchUtilGoText)
	searchWrite(t, dir, "sub/deep/deep.go", searchDeepGoText)
	searchWrite(t, dir, ".git/config", searchGitCfgText)
	searchWrite(t, dir, ".git/decoy.go", searchGitGoText)
	searchWrite(t, dir, "node_modules/nm.go", searchNMGoText)
	return searchNewWorkspace(t, dir, workspace.Options{ReadOnly: true})
}

func searchCall(t *testing.T, it tool.InvokableTool, args string) (string, error) {
	t.Helper()
	return it.InvokableRun(context.Background(), args)
}

func searchGlobResult(t *testing.T, ws *workspace.Workspace, args string) (GlobOutput, error) {
	t.Helper()
	it, err := NewGlobTool(ws)
	if err != nil {
		t.Fatalf("NewGlobTool: %v", err)
	}
	raw, err := searchCall(t, it, args)
	if err != nil {
		return GlobOutput{}, err
	}
	var out GlobOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal glob output %s: %v", raw, err)
	}
	return out, nil
}

func searchGlobOK(t *testing.T, ws *workspace.Workspace, args string) GlobOutput {
	t.Helper()
	out, err := searchGlobResult(t, ws, args)
	if err != nil {
		t.Fatalf("glob %s: %v", args, err)
	}
	return out
}

func searchGrepResult(t *testing.T, ws *workspace.Workspace, args string) (GrepOutput, error) {
	t.Helper()
	it, err := NewGrepTool(ws)
	if err != nil {
		t.Fatalf("NewGrepTool: %v", err)
	}
	raw, err := searchCall(t, it, args)
	if err != nil {
		return GrepOutput{}, err
	}
	var out GrepOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal grep output %s: %v", raw, err)
	}
	return out, nil
}

func searchGrepOK(t *testing.T, ws *workspace.Workspace, args string) GrepOutput {
	t.Helper()
	out, err := searchGrepResult(t, ws, args)
	if err != nil {
		t.Fatalf("grep %s: %v", args, err)
	}
	return out
}

// searchAssertInside fails if any reported path is absolute or walks upwards:
// a search result must always be a plain workspace-relative path.
func searchAssertInside(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if p == "" {
			t.Errorf("reported an empty path")
			continue
		}
		if filepath.IsAbs(p) {
			t.Errorf("path %q is absolute, want workspace-relative", p)
		}
		if strings.Contains(p, "..") {
			t.Errorf("path %q contains .. and may escape the workspace", p)
		}
	}
}

// searchEqual fails unless two slices are equal element by element.
func searchEqual[T comparable](t *testing.T, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// searchLinesAt returns the line numbers reported for one file.
func searchLinesAt(matches []GrepMatch, path string) []int {
	var nums []int
	for _, m := range matches {
		if m.Path == path {
			nums = append(nums, m.LineNumber)
		}
	}
	return nums
}

// searchPaths returns the distinct paths of a match set, in order.
func searchPaths(matches []GrepMatch) []string {
	var paths []string
	seen := map[string]bool{}
	for _, m := range matches {
		if seen[m.Path] {
			continue
		}
		seen[m.Path] = true
		paths = append(paths, m.Path)
	}
	return paths
}

func TestGlobTool_StarMatchesAtAnyDepth(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"*.go"}`)
	searchEqual(t, out.Matches, []string{"main.go", "sub/deep/deep.go", "sub/util.go"})
	if out.Pattern != "*.go" {
		t.Errorf("pattern = %q, want %q", out.Pattern, "*.go")
	}
	if out.Path != "." {
		t.Errorf("path = %q, want %q for the default root", out.Path, ".")
	}
	if out.Truncated {
		t.Error("truncated = true, want false")
	}
	if out.Total != 3 {
		t.Errorf("total = %d, want 3", out.Total)
	}
	searchAssertInside(t, out.Matches...)
}

func TestGlobTool_DoubleStarSpansSegments(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"**/*.go"}`)
	searchEqual(t, out.Matches, []string{"main.go", "sub/deep/deep.go", "sub/util.go"})
	out = searchGlobOK(t, ws, `{"pattern":"sub/**/*.go"}`)
	searchEqual(t, out.Matches, []string{"sub/deep/deep.go", "sub/util.go"})
	out = searchGlobOK(t, ws, `{"pattern":"**/deep/*.go"}`)
	searchEqual(t, out.Matches, []string{"sub/deep/deep.go"})
}

func TestGlobTool_SingleSegmentPattern(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"sub/*.go"}`)
	searchEqual(t, out.Matches, []string{"sub/util.go"})
}

func TestGlobTool_QuestionMarkMatchesOneCharacter(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"?ain.go"}`)
	searchEqual(t, out.Matches, []string{"main.go"})
	out = searchGlobOK(t, ws, `{"pattern":"sub/uti?.go"}`)
	searchEqual(t, out.Matches, []string{"sub/util.go"})
	// Five characters, so nothing matches: "?.go" is not "main.go".
	out = searchGlobOK(t, ws, `{"pattern":"?.go"}`)
	if len(out.Matches) != 0 || out.Total != 0 {
		t.Errorf("matches = %v, total = %d, want none", out.Matches, out.Total)
	}
}

func TestGlobTool_NoMatchIsNotAnError(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"*.rs"}`)
	if len(out.Matches) != 0 {
		t.Errorf("matches = %v, want none", out.Matches)
	}
	if out.Total != 0 || out.Truncated {
		t.Errorf("total = %d, truncated = %v, want 0, false", out.Total, out.Truncated)
	}
}

func TestGlobTool_SkipsNoiseDirectories(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"**/*.go"}`)
	for _, m := range out.Matches {
		if strings.HasPrefix(m, ".git/") || strings.HasPrefix(m, "node_modules/") {
			t.Errorf("matched noise path %q", m)
		}
	}
	// A pattern that names the directory explicitly still searches it.
	out = searchGlobOK(t, ws, `{"pattern":".git/*.go"}`)
	searchEqual(t, out.Matches, []string{".git/decoy.go"})
	out = searchGlobOK(t, ws, `{"pattern":"node_modules/*.go"}`)
	searchEqual(t, out.Matches, []string{"node_modules/nm.go"})
	searchAssertInside(t, out.Matches...)
}

func TestGlobTool_SubtreeRoot(t *testing.T) {
	ws := searchFixture(t)
	out := searchGlobOK(t, ws, `{"pattern":"*.go","path":"sub"}`)
	searchEqual(t, out.Matches, []string{"sub/deep/deep.go", "sub/util.go"})
	if out.Path != "sub" {
		t.Errorf("path = %q, want %q", out.Path, "sub")
	}
	searchAssertInside(t, out.Matches...)
}

func TestGlobTool_TruncatesAtListLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		searchWrite(t, dir, name, "package x\n")
	}
	ws := searchNewWorkspace(t, dir, workspace.Options{
		ReadOnly: true,
		Limits:   workspace.Limits{MaxListEntries: 3},
	})
	out := searchGlobOK(t, ws, `{"pattern":"*.go"}`)
	searchEqual(t, out.Matches, []string{"a.go", "b.go", "c.go"})
	if !out.Truncated {
		t.Error("truncated = false, want true")
	}
	if out.Total != 5 {
		t.Errorf("total = %d, want 5 (every match, not just the returned ones)", out.Total)
	}
}

func TestGlobTool_Errors(t *testing.T) {
	ws := searchFixture(t)
	if _, err := searchGlobResult(t, ws, `{"pattern":"*.go","path":"nope"}`); err == nil {
		t.Error("expected an error for a missing search root")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to mention a missing path", err)
	}
	if _, err := searchGlobResult(t, ws, `{"pattern":"*.go","path":"main.go"}`); err == nil {
		t.Error("expected an error when the search root is a file")
	}
	if _, err := searchGlobResult(t, ws, `{"pattern":""}`); err == nil {
		t.Error("expected an error for an empty pattern")
	}
	if _, err := searchGlobResult(t, ws, `{"pattern":"["}`); err == nil {
		t.Error("expected an error for a malformed pattern")
	}
}

func TestGlobTool_PathTraversalRejected(t *testing.T) {
	ws := searchFixture(t)
	for _, p := range []string{"../", "../..", "/etc"} {
		_, err := searchGlobResult(t, ws, `{"pattern":"*.go","path":"`+p+`"}`)
		if err == nil {
			t.Fatalf("path %q: expected an error", p)
		}
		if !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("path %q: error = %v, want workspace.ErrOutsideWorkspace", p, err)
		}
	}
}

func TestGrepTool_LiteralMatchWithLineNumbers(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"target"}`)
	if out.TotalMatches != 1 || len(out.Matches) != 1 {
		t.Fatalf("matches = %v, want exactly one", out.Matches)
	}
	m := out.Matches[0]
	if m.Path != "notes.txt" {
		t.Errorf("path = %q, want %q", m.Path, "notes.txt")
	}
	if m.LineNumber != 3 {
		t.Errorf("line_number = %d, want 3 (1-based)", m.LineNumber)
	}
	if m.Line != "target here" {
		t.Errorf("line = %q, want %q", m.Line, "target here")
	}
	if m.Context != nil {
		t.Errorf("context = %v, want nil without context_lines", m.Context)
	}
	if out.FilesSearched == 0 {
		t.Error("files_searched = 0, want the text files that were read")
	}
	if out.Truncated {
		t.Error("truncated = true, want false")
	}
	searchAssertInside(t, m.Path)
}

func TestGrepTool_RegexAlternation(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"alpha|epsilon","path":"notes.txt"}`)
	searchEqual(t, searchLinesAt(out.Matches, "notes.txt"), []int{1, 6})
}

func TestGrepTool_IncludeFiltersFiles(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"package","include":"*.go"}`)
	searchEqual(t, searchPaths(out.Matches), []string{"main.go", "sub/deep/deep.go", "sub/util.go"})
	for _, m := range out.Matches {
		if m.LineNumber != 1 {
			t.Errorf("%s: line_number = %d, want 1", m.Path, m.LineNumber)
		}
		if !strings.HasSuffix(m.Path, ".go") {
			t.Errorf("include filter let through %q", m.Path)
		}
	}
	searchAssertInside(t, searchPaths(out.Matches)...)

	out = searchGrepOK(t, ws, `{"pattern":"package","include":"sub/**/*.go"}`)
	searchEqual(t, searchPaths(out.Matches), []string{"sub/deep/deep.go", "sub/util.go"})

	// The include filter keeps matching Go files out of a text search.
	out = searchGrepOK(t, ws, `{"pattern":"target","include":"*.go"}`)
	if len(out.Matches) != 0 {
		t.Errorf("matches = %v, want none", out.Matches)
	}
}

func TestGrepTool_CaseInsensitive(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"target","path":"notes.txt","case_sensitive":false}`)
	searchEqual(t, searchLinesAt(out.Matches, "notes.txt"), []int{3, 5})
	if out.Matches[1].Line != "TARGET upper" {
		t.Errorf("line = %q, want %q", out.Matches[1].Line, "TARGET upper")
	}
}

func TestGrepTool_ContextLines(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"target","path":"notes.txt","case_sensitive":false,"context_lines":1}`)
	if len(out.Matches) != 2 {
		t.Fatalf("matches = %v, want 2", out.Matches)
	}
	want := [][]GrepContextLine{
		{
			{LineNumber: 2, Line: "beta gamma"},
			{LineNumber: 3, Line: "target here", IsMatch: true},
			{LineNumber: 4, Line: "delta"},
		},
		{
			{LineNumber: 4, Line: "delta"},
			{LineNumber: 5, Line: "TARGET upper", IsMatch: true},
			{LineNumber: 6, Line: "epsilon"},
		},
	}
	for i, m := range out.Matches {
		if len(m.Context) != len(want[i]) {
			t.Fatalf("match %d: context = %+v, want %+v", i, m.Context, want[i])
		}
		for j := range want[i] {
			if m.Context[j] != want[i][j] {
				t.Errorf("match %d context[%d] = %+v, want %+v", i, j, m.Context[j], want[i][j])
			}
		}
	}

	// With a wider window the two blocks overlap, and the earlier match shows
	// up as context of the later one — still flagged as a match, not as plain
	// context.
	out = searchGrepOK(t, ws, `{"pattern":"target","path":"notes.txt","case_sensitive":false,"context_lines":2}`)
	if len(out.Matches) != 2 {
		t.Fatalf("matches = %v, want 2", out.Matches)
	}
	searchEqual(t, out.Matches[1].Context, []GrepContextLine{
		{LineNumber: 3, Line: "target here", IsMatch: true},
		{LineNumber: 4, Line: "delta"},
		{LineNumber: 5, Line: "TARGET upper", IsMatch: true},
		{LineNumber: 6, Line: "epsilon"},
	})
}

func TestGrepTool_ContextLinesCappedAndClippedAtFileEdges(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		if i == 10 {
			b.WriteString("line10 target\n")
			continue
		}
		fmt.Fprintf(&b, "line%02d\n", i)
	}
	searchWrite(t, dir, "ctx.txt", b.String())
	ws := searchNewWorkspace(t, dir, workspace.Options{ReadOnly: true})

	// context_lines is capped at 5, so a match on line 10 gets lines 5..15.
	out := searchGrepOK(t, ws, `{"pattern":"target","context_lines":10}`)
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %v, want 1", out.Matches)
	}
	block := out.Matches[0].Context
	if len(block) != 11 {
		t.Fatalf("context has %d lines, want 11 (5 before, match, 5 after)", len(block))
	}
	if block[0].LineNumber != 5 || block[10].LineNumber != 15 {
		t.Errorf("context spans %d..%d, want 5..15", block[0].LineNumber, block[10].LineNumber)
	}
	if !block[5].IsMatch || block[5].LineNumber != 10 {
		t.Errorf("block[5] = %+v, want the match on line 10", block[5])
	}
	for i, line := range block {
		if i != 5 && line.IsMatch {
			t.Errorf("context line %d marked as a match: %+v", i, line)
		}
	}
}

func TestGrepTool_MaxMatchesTruncates(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"a","path":"notes.txt","max_matches":2}`)
	if len(out.Matches) != 2 {
		t.Fatalf("matches = %v, want 2", out.Matches)
	}
	if !out.Truncated {
		t.Error("truncated = false, want true")
	}
	if out.TotalMatches != 2 {
		t.Errorf("total_matches = %d, want 2", out.TotalMatches)
	}
	searchAssertInside(t, searchPaths(out.Matches)...)
}

func TestGrepTool_InvalidRegexIsReported(t *testing.T) {
	ws := searchFixture(t)
	_, err := searchGrepResult(t, ws, `{"pattern":"["}`)
	if err == nil {
		t.Fatal("expected an error for an invalid regular expression")
	}
	if !strings.Contains(err.Error(), "error parsing regexp") {
		t.Errorf("error = %v, want it to quote the regexp compile failure", err)
	}
	if _, err := searchGrepResult(t, ws, `{"pattern":""}`); err == nil {
		t.Error("expected an error for an empty pattern")
	}
}

func TestGrepTool_SkipsBinaryFiles(t *testing.T) {
	ws := searchFixture(t)
	// blob.bin holds "needle" but has a NUL byte, so it is not text.
	out := searchGrepOK(t, ws, `{"pattern":"needle","path":"blob.bin"}`)
	if len(out.Matches) != 0 || out.TotalMatches != 0 {
		t.Errorf("matches = %v, want none from a binary file", out.Matches)
	}
	if out.FilesSearched != 0 {
		t.Errorf("files_searched = %d, want 0", out.FilesSearched)
	}
	// The only remaining "needle" occurrences live in noise directories.
	out = searchGrepOK(t, ws, `{"pattern":"needle"}`)
	if len(out.Matches) != 0 {
		t.Errorf("matches = %v, want none (binary and noise directories are skipped)", out.Matches)
	}
	if out.TotalMatches != 0 {
		t.Errorf("total_matches = %d, want 0", out.TotalMatches)
	}
}

func TestGrepTool_SkipsNoiseDirectoriesButSearchesNamedOnes(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"package"}`)
	for _, m := range out.Matches {
		if strings.HasPrefix(m.Path, ".git/") || strings.HasPrefix(m.Path, "node_modules/") {
			t.Errorf("searched a noise path %q", m.Path)
		}
	}
	// Naming the directory as the search root searches it.
	out = searchGrepOK(t, ws, `{"pattern":"needle","path":".git"}`)
	searchEqual(t, searchLinesAt(out.Matches, ".git/config"), []int{1})
	searchEqual(t, searchLinesAt(out.Matches, ".git/decoy.go"), []int{3})
	if out.FilesSearched != 2 {
		t.Errorf("files_searched = %d, want 2", out.FilesSearched)
	}
	searchAssertInside(t, searchPaths(out.Matches)...)
}

func TestGrepTool_SearchesASingleFile(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"alpha","path":"notes.txt"}`)
	if len(out.Matches) != 1 || out.Matches[0].LineNumber != 1 {
		t.Fatalf("matches = %v, want one match on line 1", out.Matches)
	}
	if out.FilesSearched != 1 {
		t.Errorf("files_searched = %d, want 1", out.FilesSearched)
	}
	if !strings.HasSuffix(out.Matches[0].Path, "notes.txt") {
		t.Errorf("path = %q, want notes.txt", out.Matches[0].Path)
	}
	searchAssertInside(t, out.Matches[0].Path)
}

func TestGrepTool_NoMatchIsNotAnError(t *testing.T) {
	ws := searchFixture(t)
	out := searchGrepOK(t, ws, `{"pattern":"zzz_nope"}`)
	if len(out.Matches) != 0 {
		t.Errorf("matches = %v, want none", out.Matches)
	}
	if out.TotalMatches != 0 || out.Truncated {
		t.Errorf("total_matches = %d, truncated = %v, want 0, false", out.TotalMatches, out.Truncated)
	}
	if out.FilesSearched == 0 {
		t.Error("files_searched = 0, want the text files that were read")
	}
}

func TestGrepTool_SkipsFilesOverReadLimit(t *testing.T) {
	dir := t.TempDir()
	searchWrite(t, dir, "small.txt", "target small\n")
	searchWrite(t, dir, "big.txt", strings.Repeat("x", 200)+"target big\n")
	ws := searchNewWorkspace(t, dir, workspace.Options{
		ReadOnly: true,
		Limits:   workspace.Limits{MaxReadBytes: 64},
	})

	out := searchGrepOK(t, ws, `{"pattern":"target"}`)
	if len(out.Matches) != 1 || out.Matches[0].Path != "small.txt" {
		t.Fatalf("matches = %v, want only small.txt", out.Matches)
	}
	if out.FilesSkipped != 1 {
		t.Errorf("files_skipped = %d, want 1", out.FilesSkipped)
	}
	if out.FilesSearched != 1 {
		t.Errorf("files_searched = %d, want 1", out.FilesSearched)
	}

	// An explicitly named oversized file is skipped rather than read.
	out = searchGrepOK(t, ws, `{"pattern":"target","path":"big.txt"}`)
	if len(out.Matches) != 0 || out.FilesSkipped != 1 || out.FilesSearched != 0 {
		t.Errorf("matches = %v, files_skipped = %d, files_searched = %d, want none/1/0",
			out.Matches, out.FilesSkipped, out.FilesSearched)
	}
}

func TestGrepTool_ClippedLongLine(t *testing.T) {
	dir := t.TempDir()
	searchWrite(t, dir, "min.js", strings.Repeat("x", 1000)+"needle\n")
	ws := searchNewWorkspace(t, dir, workspace.Options{ReadOnly: true})
	out := searchGrepOK(t, ws, `{"pattern":"needle"}`)
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %v, want 1", out.Matches)
	}
	if got := len([]rune(out.Matches[0].Line)); got != searchMaxLineRunes+3 {
		t.Errorf("line has %d runes, want %d (clipped) + ellipsis", got, searchMaxLineRunes)
	}
	if !strings.HasSuffix(out.Matches[0].Line, "...") {
		t.Errorf("line = %q, want an ellipsis suffix", out.Matches[0].Line)
	}
}

func TestGrepTool_PathTraversalRejected(t *testing.T) {
	ws := searchFixture(t)
	for _, p := range []string{"../", "../..", "/etc/hosts", "sub/../../.."} {
		_, err := searchGrepResult(t, ws, `{"pattern":"target","path":"`+p+`"}`)
		if err == nil {
			t.Fatalf("path %q: expected an error", p)
		}
		if !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("path %q: error = %v, want workspace.ErrOutsideWorkspace", p, err)
		}
	}
}

// TestSearchTools_SchemasReachTheModel checks the generated parameter schemas,
// since a description that silently disappears is invisible at call time. It
// also covers the optional-pointer field, which must be documented as a
// boolean rather than dropped, and the tail of a description: the JSON schema
// generator splits struct tags on commas, so an unescaped comma in a tag
// silently truncates everything after it.
func TestSearchTools_SchemasReachTheModel(t *testing.T) {
	ws := searchFixture(t)

	glob, err := NewGlobTool(ws)
	if err != nil {
		t.Fatalf("NewGlobTool: %v", err)
	}
	globSchema := searchSchemaJSON(t, glob)
	t.Logf("glob schema: %s", globSchema)
	for _, want := range []string{
		"A pattern with no / matches at any depth",
		"** matches any number of segments",
		`"required":["pattern"]`,
	} {
		if !strings.Contains(globSchema, want) {
			t.Errorf("glob schema is missing %q: %s", want, globSchema)
		}
	}

	grep, err := NewGrepTool(ws)
	if err != nil {
		t.Fatalf("NewGrepTool: %v", err)
	}
	grepSchema := searchSchemaJSON(t, grep)
	t.Logf("grep schema: %s", grepSchema)
	for _, want := range []string{
		`"case_sensitive"`,
		`"boolean"`,
		`"max_matches"`,
		`"context_lines"`,
		`"include"`,
		"RE2 regular expression",
		"Capped at 1000",
	} {
		if !strings.Contains(grepSchema, want) {
			t.Errorf("grep schema is missing %s: %s", want, grepSchema)
		}
	}
}

// TestSearchToolsDoNotFollowSymlinksOutOfTheWorkspace is the confinement test:
// resolving the search root is not enough on its own, because a link planted
// inside the workspace could otherwise lead the walk outside it.
func TestSearchToolsDoNotFollowSymlinksOutOfTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	searchWrite(t, outside, "secret.go", "package secret\n\n// needle outside\n")
	searchWrite(t, outside, "outside.txt", "needle outside\n")

	dir := t.TempDir()
	searchWrite(t, dir, "inside.go", "package inside\n")
	searchWrite(t, dir, "inside.txt", "needle inside\n")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(dir, "secret-link.go")); err != nil {
		t.Fatal(err)
	}
	ws := searchNewWorkspace(t, dir, workspace.Options{ReadOnly: true})

	out := searchGlobOK(t, ws, `{"pattern":"*.go"}`)
	searchEqual(t, out.Matches, []string{"inside.go"})

	grepped := searchGrepOK(t, ws, `{"pattern":"needle"}`)
	searchEqual(t, searchPaths(grepped.Matches), []string{"inside.txt"})
	searchAssertInside(t, searchPaths(grepped.Matches)...)

	// Aiming a search at a link that leaves the root is refused, not followed.
	for _, args := range []string{
		`{"pattern":"*.go","path":"escape"}`,
		`{"pattern":"needle","path":"escape"}`,
		`{"pattern":"needle","path":"secret-link.go"}`,
	} {
		if _, err := searchGlobResult(t, ws, args); !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("glob %s: error = %v, want workspace.ErrOutsideWorkspace", args, err)
		}
	}
	for _, args := range []string{
		`{"pattern":"needle","path":"escape"}`,
		`{"pattern":"needle","path":"secret-link.go"}`,
	} {
		if _, err := searchGrepResult(t, ws, args); !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("grep %s: error = %v, want workspace.ErrOutsideWorkspace", args, err)
		}
	}
}

func TestSearchToolsRequireWorkspace(t *testing.T) {
	if _, err := NewGlobTool(nil); err == nil {
		t.Error("NewGlobTool(nil) should fail")
	}
	if _, err := NewGrepTool(nil); err == nil {
		t.Error("NewGrepTool(nil) should fail")
	}
}

func TestSearchToolsCanceledContext(t *testing.T) {
	ws := searchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	glob, err := NewGlobTool(ws)
	if err != nil {
		t.Fatalf("NewGlobTool: %v", err)
	}
	if _, err := glob.InvokableRun(ctx, `{"pattern":"*.go"}`); !errors.Is(err, context.Canceled) {
		t.Errorf("glob error = %v, want context.Canceled", err)
	}

	grep, err := NewGrepTool(ws)
	if err != nil {
		t.Fatalf("NewGrepTool: %v", err)
	}
	if _, err := grep.InvokableRun(ctx, `{"pattern":"package"}`); !errors.Is(err, context.Canceled) {
		t.Errorf("grep error = %v, want context.Canceled", err)
	}
}

func TestGrepTool_InvalidIncludePattern(t *testing.T) {
	ws := searchFixture(t)
	if _, err := searchGrepResult(t, ws, `{"pattern":"package","include":"["}`); err == nil {
		t.Error("expected an error for a malformed include glob")
	}
}

// TestGrepTool_LongFileKeepsLineNumbers covers the reader's buffer compaction:
// with more lines than the compaction threshold the reported numbers must still
// be the original 1-based ones.
func TestGrepTool_LongFileKeepsLineNumbers(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 600; i++ {
		if i == 550 {
			b.WriteString("marker here\n")
			continue
		}
		fmt.Fprintf(&b, "line%03d\n", i)
	}
	searchWrite(t, dir, "long.txt", b.String())
	ws := searchNewWorkspace(t, dir, workspace.Options{ReadOnly: true})

	out := searchGrepOK(t, ws, `{"pattern":"marker","context_lines":2}`)
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %v, want exactly one", out.Matches)
	}
	if out.Matches[0].LineNumber != 550 {
		t.Errorf("line_number = %d, want 550", out.Matches[0].LineNumber)
	}
	searchEqual(t, out.Matches[0].Context, []GrepContextLine{
		{LineNumber: 548, Line: "line548"},
		{LineNumber: 549, Line: "line549"},
		{LineNumber: 550, Line: "marker here", IsMatch: true},
		{LineNumber: 551, Line: "line551"},
		{LineNumber: 552, Line: "line552"},
	})
}

func searchSchemaJSON(t *testing.T, it tool.InvokableTool) string {
	t.Helper()
	info, err := it.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("ParamsOneOf is nil")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	b, err := json.Marshal(js)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}
