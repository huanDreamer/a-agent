package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	agenttool "github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// This file tests the file tools against a real *workspace.Workspace rooted in a
// temporary directory. The workspace is never faked: the point of these tools is
// what they do to a real filesystem, and the most important test here is the one
// that proves a traversal attempt changes nothing outside the root.

// fileNewWorkspace returns a workspace rooted in a fresh temporary directory.
func fileNewWorkspace(t *testing.T, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// fileMustTool constructs a tool from one of the exported constructors and
// fails the test if the constructor reports an error.
func fileMustTool(t *testing.T, ctor func(*workspace.Workspace) (tool.InvokableTool, error), ws *workspace.Workspace) tool.InvokableTool {
	t.Helper()
	it, err := ctor(ws)
	if err != nil {
		t.Fatalf("construct tool: %v", err)
	}
	return it
}

// fileArgs renders the arguments the way a model would send them, so a test can
// use the same exported input structs the tool decodes.
func fileArgs(t *testing.T, in any) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

func fileRun(t *testing.T, it tool.InvokableTool, args string) (string, error) {
	t.Helper()
	return it.InvokableRun(context.Background(), args)
}

func fileRunOK(t *testing.T, it tool.InvokableTool, args string) string {
	t.Helper()
	out, err := fileRun(t, it, args)
	if err != nil {
		t.Fatalf("invoke %s: %v", args, err)
	}
	return out
}

func fileDecode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode tool output %q: %v", out, err)
	}
	return v
}

// fileSeed writes fixture content, creating parent directories. Tests address
// the filesystem directly, because a bug in write_file must not be able to hide
// behind write_file.
func fileSeed(t *testing.T, ws *workspace.Workspace, rel, content string, perm fs.FileMode) string {
	t.Helper()
	abs := filepath.Join(ws.Root(), filepath.FromSlash(rel))
	fileSeedAt(t, abs, content, perm)
	return abs
}

func fileSeedAt(t *testing.T, abs, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), perm); err != nil {
		t.Fatalf("seed %s: %v", abs, err)
	}
}

func fileDisk(t *testing.T, abs string) string {
	t.Helper()
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	return string(b)
}

func fileModeOf(t *testing.T, abs string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat %s: %v", abs, err)
	}
	return info.Mode().Perm()
}

// fileAbsent asserts the path was never created; Lstat is used so a dangling
// symlink still counts as an existing entry.
func fileAbsent(t *testing.T, abs string) {
	t.Helper()
	if _, err := os.Lstat(abs); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (err=%v), but nothing should have been created there", abs, err)
	}
}

func TestReadFileTool_Numbering(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	src := "alpha\nbeta\ngamma\n"
	fileSeed(t, ws, "notes.txt", src, 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	got := fileDecode[ReadFileOutput](t, fileRunOK(t, it, fileArgs(t, ReadFileInput{Path: "notes.txt"})))

	// The gutter is fixed width and 1-based, with the arrow separating number
	// from text so the model can strip it.
	want := "     1→alpha\n     2→beta\n     3→gamma"
	if got.Content != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}
	if got.Path != "notes.txt" {
		t.Errorf("path = %q, want %q", got.Path, "notes.txt")
	}
	if got.TotalLines != 3 {
		t.Errorf("total_lines = %d, want 3", got.TotalLines)
	}
	if want := int64(len(src)); got.Bytes != want {
		t.Errorf("bytes = %d, want %d", got.Bytes, want)
	}
	if got.Truncated {
		t.Errorf("truncated = true, want false")
	}
	if got.Note != "" {
		t.Errorf("note = %q, want empty", got.Note)
	}
}

func TestReadFileTool_Windows(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	var src strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&src, "line %d\n", i)
	}
	fileSeed(t, ws, "big.txt", src.String(), 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	cases := []struct {
		name          string
		offset        int
		limit         int
		wantFirst     string
		wantLines     int
		wantTruncated bool
	}{
		{"whole file", 0, 0, "     1→line 1", 10, false},
		{"offset and limit window", 3, 2, "     3→line 3", 2, true},
		{"offset to end of file", 8, 0, "     8→line 8", 3, false},
		{"non-positive offset starts at line 1", -5, 1, "     1→line 1", 1, true},
		{"limit above the cap is capped", 1, 100000, "     1→line 1", 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := fileRunOK(t, it, fileArgs(t, ReadFileInput{Path: "big.txt", Offset: tc.offset, Limit: tc.limit}))
			got := fileDecode[ReadFileOutput](t, out)

			if !strings.HasPrefix(got.Content, tc.wantFirst) {
				t.Errorf("content = %q, want prefix %q", got.Content, tc.wantFirst)
			}
			// Every returned line carries exactly one arrow. The note never
			// contains one, so the count is the line count.
			if n := strings.Count(got.Content, "→"); n != tc.wantLines {
				t.Errorf("returned %d numbered lines, want %d (content = %q)", n, tc.wantLines, got.Content)
			}
			if got.TotalLines != 10 {
				t.Errorf("total_lines = %d, want 10", got.TotalLines)
			}
			if got.Truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v (note = %q)", got.Truncated, tc.wantTruncated, got.Note)
			}
		})
	}
}

func TestReadFileTool_TruncatedByReadLimit(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxReadBytes: 40}})
	src := strings.Repeat("0123456789\n", 8) // 88 bytes, 8 lines
	fileSeed(t, ws, "wide.txt", src, 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	got := fileDecode[ReadFileOutput](t, fileRunOK(t, it, fileArgs(t, ReadFileInput{Path: "wide.txt"})))

	if !got.Truncated {
		t.Fatalf("truncated = false, want true (content = %q)", got.Content)
	}
	if got.Bytes != int64(len(src)) {
		t.Errorf("bytes = %d, want the file size %d", got.Bytes, len(src))
	}
	// Only the three whole lines inside the 40 byte window are returned; the
	// partial fourth line is dropped rather than shown as content.
	if got.TotalLines != 3 {
		t.Errorf("total_lines = %d, want 3", got.TotalLines)
	}
	if n := strings.Count(got.Content, "→"); n != 3 {
		t.Errorf("returned %d numbered lines, want 3 (content = %q)", n, got.Content)
	}
	// A partial file must announce itself, in the content the model actually
	// reads and not only in a sibling field.
	if !strings.Contains(got.Content, "truncated") {
		t.Errorf("content does not say it was truncated: %q", got.Content)
	}
	if !strings.Contains(got.Note, "40") {
		t.Errorf("note = %q, want it to mention the 40 byte read limit", got.Note)
	}
}

func TestReadFileTool_TruncatedMidLine(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxReadBytes: 40}})
	// One line with no newline anywhere: there is nothing to cut back to, so the
	// partial line is returned and has to be reported as incomplete.
	src := strings.Repeat("x", 100)
	fileSeed(t, ws, "long.txt", src, 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	got := fileDecode[ReadFileOutput](t, fileRunOK(t, it, fileArgs(t, ReadFileInput{Path: "long.txt"})))

	if !got.Truncated {
		t.Fatalf("truncated = false, want true (content = %q)", got.Content)
	}
	if !strings.Contains(got.Note, "cut off mid-line") {
		t.Errorf("note = %q, want it to warn that the line is cut off", got.Note)
	}
	if !strings.HasPrefix(got.Content, "     1→xxxx") {
		t.Errorf("content = %q, want a single numbered line holding the prefix", got.Content)
	}
}

func TestReadFileTool_RefusesDirectory(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	if err := os.MkdirAll(filepath.Join(ws.Root(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	it := fileMustTool(t, NewReadFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, ReadFileInput{Path: "sub"}))
	if err == nil {
		t.Fatal("expected an error when reading a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") || !strings.Contains(err.Error(), "list_dir") {
		t.Errorf("err = %v, want it to say the path is a directory and point at list_dir", err)
	}
}

func TestReadFileTool_RefusesBinary(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	fileSeed(t, ws, "blob.bin", "MZ\x00\x01\x02binary", 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	out, err := fileRun(t, it, fileArgs(t, ReadFileInput{Path: "blob.bin"}))
	if err == nil {
		t.Fatalf("expected binary content to be refused, got %q", out)
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("err = %v, want it to mention binary content", err)
	}
}

func TestReadFileTool_OffsetPastEOF(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	fileSeed(t, ws, "small.txt", "one\ntwo\n", 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, ReadFileInput{Path: "small.txt", Offset: 99}))
	if err == nil {
		t.Fatal("expected an error for an offset past the end of the file")
	}
	if !strings.Contains(err.Error(), "past the end") || !strings.Contains(err.Error(), "2 lines") {
		t.Errorf("err = %v, want it to say the offset is past the end and how many lines exist", err)
	}
}

func TestReadFileTool_MissingFile(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	it := fileMustTool(t, NewReadFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, ReadFileInput{Path: "nope.txt"}))
	if err == nil {
		t.Fatal("expected an error for a file that does not exist")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "nope.txt") {
		t.Errorf("err = %v, want it to name the missing file", err)
	}
}

func TestReadFileTool_EmptyFile(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	fileSeed(t, ws, "empty.txt", "", 0o644)
	it := fileMustTool(t, NewReadFileTool, ws)

	got := fileDecode[ReadFileOutput](t, fileRunOK(t, it, fileArgs(t, ReadFileInput{Path: "empty.txt"})))
	if got.Content != "" || got.TotalLines != 0 || got.Truncated {
		t.Errorf("empty file read = %+v, want empty content and no truncation", got)
	}
}

func TestWriteFileTool_CreateWithParents(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	it := fileMustTool(t, NewWriteFileTool, ws)

	content := "package main\n\nfunc main() {}\n"
	got := fileDecode[WriteFileOutput](t, fileRunOK(t, it, fileArgs(t, WriteFileInput{
		Path:    "a/b/c/main.go",
		Content: content,
	})))

	if !got.Created {
		t.Errorf("created = false, want true for a new file")
	}
	if got.Path != "a/b/c/main.go" {
		t.Errorf("path = %q, want %q", got.Path, "a/b/c/main.go")
	}
	if want := int64(len(content)); got.Bytes != want {
		t.Errorf("bytes = %d, want %d", got.Bytes, want)
	}

	abs := filepath.Join(ws.Root(), "a", "b", "c", "main.go")
	if fileDisk(t, abs) != content {
		t.Errorf("file on disk = %q, want %q", fileDisk(t, abs), content)
	}
	if got := fileModeOf(t, abs); got != 0o644 {
		t.Errorf("mode = %v, want 0644 for a new file", got)
	}

	// The atomic write must not leave its temporary file behind.
	entries, err := os.ReadDir(filepath.Dir(abs))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "main.go" {
		t.Errorf("directory contains %d entries (%v), want only main.go", len(entries), fileNames(entries))
	}
}

func TestWriteFileTool_OverwritePreservesMode(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	abs := fileSeed(t, ws, "secret.env", "OLD=1\n", 0o600)
	it := fileMustTool(t, NewWriteFileTool, ws)

	content := "NEW=2\n"
	got := fileDecode[WriteFileOutput](t, fileRunOK(t, it, fileArgs(t, WriteFileInput{
		Path:    "secret.env",
		Content: content,
	})))

	if got.Created {
		t.Errorf("created = true, want false when overwriting an existing file")
	}
	if want := int64(len(content)); got.Bytes != want {
		t.Errorf("bytes = %d, want %d", got.Bytes, want)
	}
	if fileDisk(t, abs) != content {
		t.Errorf("file on disk = %q, want %q", fileDisk(t, abs), content)
	}
	if mode := fileModeOf(t, abs); mode != 0o600 {
		t.Errorf("mode = %v, want the existing 0600 to be preserved", mode)
	}
}

func TestWriteFileTool_ReadOnly(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{ReadOnly: true})
	it := fileMustTool(t, NewWriteFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, WriteFileInput{Path: "nope/new.txt", Content: "x"}))
	if err == nil {
		t.Fatal("expected a read-only workspace to refuse a write")
	}
	if !errors.Is(err, workspace.ErrReadOnly) {
		t.Errorf("err = %v, want it to wrap workspace.ErrReadOnly", err)
	}
	fileAbsent(t, filepath.Join(ws.Root(), "nope", "new.txt"))
	// Refusing must happen before parent directories are created.
	fileAbsent(t, filepath.Join(ws.Root(), "nope"))
}

func TestWriteFileTool_SizeLimit(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxWriteBytes: 16}})
	it := fileMustTool(t, NewWriteFileTool, ws)

	atLimit := strings.Repeat("x", 16)
	got := fileDecode[WriteFileOutput](t, fileRunOK(t, it, fileArgs(t, WriteFileInput{
		Path:    "at-limit.txt",
		Content: atLimit,
	})))
	if got.Bytes != 16 {
		t.Errorf("bytes = %d, want 16 for a write exactly at the limit", got.Bytes)
	}
	if fileDisk(t, filepath.Join(ws.Root(), "at-limit.txt")) != atLimit {
		t.Error("the at-limit file was not written correctly")
	}

	_, err := fileRun(t, it, fileArgs(t, WriteFileInput{Path: "over.txt", Content: strings.Repeat("y", 17)}))
	if err == nil {
		t.Fatal("expected a write over the limit to be refused")
	}
	if !strings.Contains(err.Error(), "write limit") {
		t.Errorf("err = %v, want it to mention the write limit", err)
	}
	fileAbsent(t, filepath.Join(ws.Root(), "over.txt"))
}

func TestEditFileTool_UniqueReplacement(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	abs := fileSeed(t, ws, "cfg.txt", "name = \"alpha\"\nnames = \"alpha\"\n", 0o644)
	it := fileMustTool(t, NewEditFileTool, ws)

	got := fileDecode[EditFileOutput](t, fileRunOK(t, it, fileArgs(t, EditFileInput{
		Path:      "cfg.txt",
		OldString: `name = "alpha"`,
		NewString: `name = "beta"`,
	})))

	if got.Replacements != 1 {
		t.Errorf("replacements = %d, want 1", got.Replacements)
	}
	// The near-identical `names = ...` line must be untouched: a substring
	// match is not allowed to spill into a longer identifier.
	want := "name = \"beta\"\nnames = \"alpha\"\n"
	if onDisk := fileDisk(t, abs); onDisk != want {
		t.Errorf("file on disk = %q, want %q", onDisk, want)
	}
	if got.Bytes != int64(len(want)) {
		t.Errorf("bytes = %d, want %d", got.Bytes, len(want))
	}
}

func TestEditFileTool_ReplaceAll(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	abs := fileSeed(t, ws, "t.txt", "foo = 1\nbar = 2\nfoo = 3\n", 0o644)
	it := fileMustTool(t, NewEditFileTool, ws)

	got := fileDecode[EditFileOutput](t, fileRunOK(t, it, fileArgs(t, EditFileInput{
		Path:       "t.txt",
		OldString:  "foo = ",
		NewString:  "baz = ",
		ReplaceAll: true,
	})))

	if got.Replacements != 2 {
		t.Errorf("replacements = %d, want 2", got.Replacements)
	}
	want := "baz = 1\nbar = 2\nbaz = 3\n"
	if onDisk := fileDisk(t, abs); onDisk != want {
		t.Errorf("file on disk = %q, want %q", onDisk, want)
	}
}

func TestEditFileTool_Refusals(t *testing.T) {
	content := "dup dup\n"

	cases := []struct {
		name    string
		in      EditFileInput
		wantSub []string
	}{
		{
			name:    "zero matches",
			in:      EditFileInput{Path: "f.txt", OldString: "nowhere", NewString: "x"},
			wantSub: []string{"not found", "read_file"},
		},
		{
			name:    "ambiguous match without replace_all",
			in:      EditFileInput{Path: "f.txt", OldString: "dup", NewString: "x"},
			wantSub: []string{"2 times", "replace_all"},
		},
		{
			name:    "empty old_string",
			in:      EditFileInput{Path: "f.txt", OldString: "", NewString: "x"},
			wantSub: []string{"old_string", "empty"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := fileNewWorkspace(t, workspace.Options{})
			abs := fileSeed(t, ws, "f.txt", content, 0o644)
			it := fileMustTool(t, NewEditFileTool, ws)

			_, err := fileRun(t, it, fileArgs(t, tc.in))
			if err == nil {
				t.Fatal("expected the edit to be refused")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err = %v, want it to contain %q", err, sub)
				}
			}
			// A refused edit must leave the file byte-for-byte unchanged.
			if onDisk := fileDisk(t, abs); onDisk != content {
				t.Errorf("file on disk = %q, want it unchanged (%q)", onDisk, content)
			}
		})
	}
}

func TestEditFileTool_RefusesBinary(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	fileSeed(t, ws, "blob.bin", "MZ\x00\x01\x02binary", 0o644)
	it := fileMustTool(t, NewEditFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, EditFileInput{Path: "blob.bin", OldString: "binary", NewString: "text"}))
	if err == nil {
		t.Fatal("expected a binary file to be refused")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("err = %v, want it to mention binary content", err)
	}
}

func TestEditFileTool_ReadOnly(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{ReadOnly: true})
	abs := fileSeed(t, ws, "f.txt", "before\n", 0o644)
	it := fileMustTool(t, NewEditFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, EditFileInput{Path: "f.txt", OldString: "before", NewString: "after"}))
	if err == nil {
		t.Fatal("expected a read-only workspace to refuse an edit")
	}
	if !errors.Is(err, workspace.ErrReadOnly) {
		t.Errorf("err = %v, want it to wrap workspace.ErrReadOnly", err)
	}
	if onDisk := fileDisk(t, abs); onDisk != "before\n" {
		t.Errorf("file on disk = %q, want it unchanged", onDisk)
	}
}

func TestEditFileTool_SizeLimit(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxWriteBytes: 8}})
	abs := fileSeed(t, ws, "f.txt", "hello\n", 0o644)
	it := fileMustTool(t, NewEditFileTool, ws)

	_, err := fileRun(t, it, fileArgs(t, EditFileInput{
		Path:      "f.txt",
		OldString: "hello",
		NewString: "hello world and then some",
	}))
	if err == nil {
		t.Fatal("expected an edit that grows the file over the write limit to be refused")
	}
	if !strings.Contains(err.Error(), "write limit") {
		t.Errorf("err = %v, want it to mention the write limit", err)
	}
	if onDisk := fileDisk(t, abs); onDisk != "hello\n" {
		t.Errorf("file on disk = %q, want it unchanged", onDisk)
	}
}

func TestListDirTool_OrderTypesAndRootDefault(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	for _, dir := range []string{"src", "docs"} {
		if err := os.MkdirAll(filepath.Join(ws.Root(), dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fileSeed(t, ws, "zeta.txt", "z\n", 0o644)
	fileSeed(t, ws, "alpha.txt", "a\n", 0o644)
	if err := os.Symlink("alpha.txt", filepath.Join(ws.Root(), "link.txt")); err != nil {
		t.Fatal(err)
	}
	it := fileMustTool(t, NewListDirTool, ws)

	wantNames := []string{"docs", "src", "alpha.txt", "zeta.txt", "link.txt"}
	wantTypes := []string{"dir", "dir", "file", "file", "symlink"}

	// An omitted path and "." both mean the workspace root.
	for _, in := range []ListDirInput{{}, {Path: "."}} {
		got := fileDecode[ListDirOutput](t, fileRunOK(t, it, fileArgs(t, in)))

		if got.Path != "." {
			t.Errorf("path = %q, want %q for the workspace root", got.Path, ".")
		}
		if got.Total != len(wantNames) {
			t.Errorf("total = %d, want %d", got.Total, len(wantNames))
		}
		if got.Truncated {
			t.Errorf("truncated = true, want false")
		}
		if len(got.Entries) != len(wantNames) {
			t.Fatalf("entries = %d, want %d", len(got.Entries), len(wantNames))
		}
		for i, want := range wantNames {
			entry := got.Entries[i]
			if entry.Name != want {
				t.Errorf("entry %d = %q, want %q (order must be directories first, then files, then symlinks)", i, entry.Name, want)
			}
			// The relative path is what lets the model chain a read or edit.
			if entry.Path != want {
				t.Errorf("entry %q path = %q, want %q", entry.Name, entry.Path, want)
			}
			if entry.Type != wantTypes[i] {
				t.Errorf("entry %q type = %q, want %q", entry.Name, entry.Type, wantTypes[i])
			}
			if entry.Mode == "" {
				t.Errorf("entry %q has no mode", entry.Name)
			}
		}
	}
}

func TestListDirTool_SubdirectoryAndTruncation(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxListEntries: 2}})
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		fileSeed(t, ws, name, name+"\n", 0o644)
	}
	it := fileMustTool(t, NewListDirTool, ws)

	got := fileDecode[ListDirOutput](t, fileRunOK(t, it, fileArgs(t, ListDirInput{Path: "."})))

	if !got.Truncated {
		t.Errorf("truncated = false, want true with 4 entries and a limit of 2")
	}
	if got.Total != 4 {
		t.Errorf("total = %d, want the full 4 entries", got.Total)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(got.Entries))
	}
	// Truncation keeps the first entries in sorted order, so a listing is
	// deterministic rather than dependent on readdir order.
	if got.Entries[0].Name != "a.txt" || got.Entries[1].Name != "b.txt" {
		t.Errorf("entries = %q, want [a.txt b.txt]", []string{got.Entries[0].Name, got.Entries[1].Name})
	}
	if !strings.Contains(got.Note, "4") {
		t.Errorf("note = %q, want it to say how many entries were withheld", got.Note)
	}
}

func TestListDirTool_Errors(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	fileSeed(t, ws, "a.txt", "a\n", 0o644)
	it := fileMustTool(t, NewListDirTool, ws)

	t.Run("missing directory", func(t *testing.T) {
		_, err := fileRun(t, it, fileArgs(t, ListDirInput{Path: "missing"}))
		if err == nil {
			t.Fatal("expected an error for a directory that does not exist")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want it to wrap fs.ErrNotExist", err)
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		_, err := fileRun(t, it, fileArgs(t, ListDirInput{Path: "a.txt"}))
		if err == nil {
			t.Fatal("expected an error when listing a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("err = %v, want it to say the path is not a directory", err)
		}
	})
}

// TestFileTools_PathTraversalRefused is the boundary test: every tool must
// refuse a path that leaves the workspace, and must do so without touching the
// target. A tool that returns a nice error after reading or writing anyway is
// still an escape, so both halves are asserted.
func TestFileTools_PathTraversalRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(root, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	const secret = "TOP-SECRET\n"
	secretAbs := filepath.Join(base, "secret.txt")
	fileSeedAt(t, secretAbs, secret, 0o600)
	// A symlink inside the workspace pointing out of it: the literal path is
	// confined, so only the sandbox's symlink check can catch this.
	linkAbs := filepath.Join(ws.Root(), "escape-link")
	if err := os.Symlink(secretAbs, linkAbs); err != nil {
		t.Fatal(err)
	}

	readIt := fileMustTool(t, NewReadFileTool, ws)
	writeIt := fileMustTool(t, NewWriteFileTool, ws)
	editIt := fileMustTool(t, NewEditFileTool, ws)
	listIt := fileMustTool(t, NewListDirTool, ws)

	escapedAbs := filepath.Join(base, "escaped.txt")
	absOutside := filepath.Join(base, "abs-outside.txt")

	cases := []struct {
		name string
		it   tool.InvokableTool
		args string
	}{
		{"read via ..", readIt, fileArgs(t, ReadFileInput{Path: "../secret.txt"})},
		{"read absolute outside", readIt, fileArgs(t, ReadFileInput{Path: absOutside})},
		{"read through an escaping symlink", readIt, fileArgs(t, ReadFileInput{Path: "escape-link"})},
		{"write via ..", writeIt, fileArgs(t, WriteFileInput{Path: "../escaped.txt", Content: "pwned"})},
		{"write absolute outside", writeIt, fileArgs(t, WriteFileInput{Path: absOutside, Content: "pwned"})},
		{"write through an escaping symlink", writeIt, fileArgs(t, WriteFileInput{Path: "escape-link", Content: "pwned"})},
		{"edit via ..", editIt, fileArgs(t, EditFileInput{Path: "../secret.txt", OldString: "TOP-SECRET", NewString: "pwned"})},
		{"edit through an escaping symlink", editIt, fileArgs(t, EditFileInput{Path: "escape-link", OldString: "TOP-SECRET", NewString: "pwned"})},
		{"list via ..", listIt, fileArgs(t, ListDirInput{Path: "../"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := fileRun(t, tc.it, tc.args)
			if err == nil {
				t.Fatalf("expected a refusal, got %q", out)
			}
			if !errors.Is(err, workspace.ErrOutsideWorkspace) {
				t.Fatalf("err = %v, want it to wrap workspace.ErrOutsideWorkspace", err)
			}
			if strings.Contains(out, "TOP-SECRET") {
				t.Errorf("output leaked the secret: %q", out)
			}
		})
	}

	// The refusals must have had no side effect anywhere.
	fileAbsent(t, escapedAbs)
	fileAbsent(t, absOutside)
	if onDisk := fileDisk(t, secretAbs); onDisk != secret {
		t.Errorf("file outside the workspace = %q, want it untouched (%q)", onDisk, secret)
	}
	// The symlink itself must still be a symlink, not a regular file a rename
	// left behind.
	if info, err := os.Lstat(linkAbs); err != nil {
		t.Fatalf("lstat %s: %v", linkAbs, err)
	} else if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink (mode %v)", linkAbs, info.Mode())
	}
	// And nothing was created inside the workspace either.
	entries, err := os.ReadDir(ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "escape-link" {
		t.Errorf("workspace contains %v, want only the pre-existing symlink", fileNames(entries))
	}
}

func TestFileTools_NamesAndDescriptions(t *testing.T) {
	ws := fileNewWorkspace(t, workspace.Options{})
	cases := []struct {
		name      string
		invokable tool.InvokableTool
		wantDesc  string
	}{
		{"read_file", fileMustTool(t, NewReadFileTool, ws), "line numbers"},
		{"write_file", fileMustTool(t, NewWriteFileTool, ws), "atomic"},
		{"edit_file", fileMustTool(t, NewEditFileTool, ws), "exactly once"},
		{"list_dir", fileMustTool(t, NewListDirTool, ws), "directories first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The description is what the model reads to choose a tool, and the
			// registry derives the parameter schema from the input struct.
			spec, err := agenttool.SpecOf(context.Background(), tc.invokable)
			if err != nil {
				t.Fatalf("SpecOf: %v", err)
			}
			if spec.Name != tc.name {
				t.Errorf("name = %q, want %q", spec.Name, tc.name)
			}
			if !strings.Contains(spec.Description, tc.wantDesc) {
				t.Errorf("description = %q, want it to mention %q", spec.Description, tc.wantDesc)
			}
			if !strings.Contains(spec.ParametersJSONSchema, `"path"`) {
				t.Errorf("schema = %s, want a path property", spec.ParametersJSONSchema)
			}
		})
	}
}

func fileNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
