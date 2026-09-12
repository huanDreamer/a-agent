package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/workspace"
)

// The file tools below are the agent's hands on the filesystem. Every
// caller-supplied path goes through the workspace sandbox (Resolve or
// ResolveForWrite) before any syscall touches it, and nothing here joins a user
// path itself: confinement is enforced in one place, so a bug in a tool cannot
// become an escape from the workspace.
//
// The visible shapes are chosen for a model, not for a human:
//   - read_file returns line-numbered text so the model can cite and re-read an
//     exact window instead of guessing,
//   - write_file and edit_file write atomically (temp file + rename) so an
//     interrupted call cannot leave a half-written source file behind,
//   - edit_file refuses an ambiguous match, because replacing the wrong
//     occurrence is the single most damaging mistake a coding agent can make.

const (
	// fileDefaultReadLines is the line window used when the model omits limit.
	fileDefaultReadLines = 2000
	// fileMaxReadLines caps limit, so one call cannot flood the context.
	fileMaxReadLines = 5000
	// fileGutterWidth is the fixed width of the line-number gutter; a line is
	// rendered as "   123→content" — right-aligned number, arrow, then the text.
	fileGutterWidth = 6
	// fileGutterSep separates the number from the text. A single non-ASCII rune
	// is used so it cannot be confused with file content, which makes the
	// gutter easy for the model to strip when it quotes a line back.
	fileGutterSep = "→"
	// fileModeNew is the mode used for a file this tool creates. An existing
	// file keeps its own mode instead.
	fileModeNew fs.FileMode = 0o644
	// fileTempPattern names the intermediate file used by the atomic write. It
	// is hidden and distinctive so a listing never mistakes it for user content,
	// and it lives in the destination directory so rename stays on one
	// filesystem (a cross-device rename is not atomic and would fail).
	fileTempPattern = ".huan-agent-write-*"
)

// ReadFileInput is the parameter schema for the "read_file" tool.
type ReadFileInput struct {
	Path   string `json:"path" jsonschema:"description=File to read; a path inside the workspace either relative to its root or absolute. Paths outside the workspace are refused, required"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=1-based line number to start reading from. Omit or 0 to start at line 1"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Maximum number of lines to return. Omit or 0 for the default of 2000; values above 5000 are capped at 5000"`
}

// ReadFileOutput is what the "read_file" tool returns. Content is the numbered
// text; the remaining fields tell the model where it is in a long file and
// whether it has seen everything.
type ReadFileOutput struct {
	// Path is the file's path relative to the workspace root, which is the form
	// every other tool accepts.
	Path string `json:"path"`
	// Content is the requested line window, each line rendered as
	// "   123→text" with its 1-based number.
	Content string `json:"content"`
	// TotalLines counts the lines available in the bytes read; when Truncated
	// is true the file has more lines than this.
	TotalLines int `json:"total_lines"`
	// Truncated reports that the workspace read limit or the line limit cut the
	// output short.
	Truncated bool `json:"truncated"`
	// Bytes is the size of the file on disk, not the size of the window.
	Bytes int64 `json:"bytes"`
	// Note explains what was withheld when Truncated is true.
	Note string `json:"note,omitempty"`
}

// NewReadFileTool returns a Tool that reads a text file inside the workspace and
// returns it with 1-based line numbers, so the model can cite lines and ask for
// a specific window of a long file.
func NewReadFileTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("read_file: workspace is required")
	}
	return utils.InferTool("read_file",
		"Read a text file from the workspace and return it with 1-based line numbers in a \"   123→content\" gutter. "+
			"Use it before edit_file, which needs the exact current text. Optional offset and limit select a window of at most 5000 lines. "+
			"Directories and binary files are refused. Output is capped by the workspace read limit and says so when that happens.",
		func(_ context.Context, in ReadFileInput) (ReadFileOutput, error) {
			return fileRead(ws, in)
		})
}

// WriteFileInput is the parameter schema for the "write_file" tool.
type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"description=File to create or overwrite; a path inside the workspace either relative to its root or absolute. Missing parent directories are created, required"`
	Content string `json:"content" jsonschema:"description=Full contents of the file. Any existing content is replaced entirely, required"`
}

// WriteFileOutput is what the "write_file" tool returns.
type WriteFileOutput struct {
	Path string `json:"path"`
	// Bytes is the number of bytes actually written.
	Bytes int64 `json:"bytes"`
	// Created is true when the file did not exist before this call.
	Created bool `json:"created"`
}

// NewWriteFileTool returns a Tool that creates or replaces a file inside the
// workspace. Writes are atomic, so an interrupted call cannot truncate an
// existing source file.
func NewWriteFileTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("write_file: workspace is required")
	}
	return utils.InferTool("write_file",
		"Create or overwrite a file in the workspace with the given content, creating missing parent directories. "+
			"Prefer edit_file for a small change to an existing file, so unrelated content is left untouched. "+
			"The write is atomic and is refused when the workspace is read-only or the content is over its write limit.",
		func(_ context.Context, in WriteFileInput) (WriteFileOutput, error) {
			return fileWrite(ws, in)
		})
}

// EditFileInput is the parameter schema for the "edit_file" tool.
type EditFileInput struct {
	Path string `json:"path" jsonschema:"description=File to edit; a path inside the workspace either relative to its root or absolute, required"`
	// The commas inside these descriptions would be read as tag separators, so
	// the wording deliberately uses semicolons instead.
	OldString  string `json:"old_string" jsonschema:"description=Exact text to replace. It must appear exactly once unless replace_all is true; copy it from read_file including whitespace, required"`
	NewString  string `json:"new_string" jsonschema:"description=Replacement text; an empty string deletes old_string, required"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=Replace every occurrence instead of requiring a unique match"`
}

// EditFileOutput is what the "edit_file" tool returns.
type EditFileOutput struct {
	Path string `json:"path"`
	// Replacements is how many occurrences were replaced (1 unless replace_all).
	Replacements int `json:"replacements"`
	// Bytes is the size of the file after the edit.
	Bytes int64 `json:"bytes"`
}

// NewEditFileTool returns a Tool that replaces an exact string in an existing
// text file. Requiring a unique match is what stops an edit from silently
// corrupting a different occurrence of the same text.
func NewEditFileTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("edit_file: workspace is required")
	}
	return utils.InferTool("edit_file",
		"Replace an exact string in a text file in the workspace. old_string must appear exactly once; "+
			"a zero or multiple match is an error, so re-read the file with read_file and include more surrounding context, "+
			"or set replace_all=true to replace every occurrence. The write is atomic and is refused when the workspace is read-only.",
		func(_ context.Context, in EditFileInput) (EditFileOutput, error) {
			return fileEdit(ws, in)
		})
}

// ListDirInput is the parameter schema for the "list_dir" tool.
type ListDirInput struct {
	Path string `json:"path,omitempty" jsonschema:"description=Directory to list; a path inside the workspace either relative to its root or absolute. Omit or use . for the workspace root"`
}

// ListDirEntry is one directory entry. Type is reported from the directory
// itself, so a symlink is reported as "symlink" rather than as whatever it
// points at: a listing should show what is actually there.
type ListDirEntry struct {
	Name string `json:"name"`
	// Path is the entry's path relative to the workspace root, ready to pass to
	// read_file or edit_file.
	Path string `json:"path"`
	// Type is one of "file", "dir", "symlink", "other".
	Type string `json:"type"`
	Size int64  `json:"size"`
	Mode string `json:"mode,omitempty"`
}

// ListDirOutput is what the "list_dir" tool returns.
type ListDirOutput struct {
	// Path is the listed directory relative to the workspace root; "." is the
	// root itself.
	Path    string         `json:"path"`
	Entries []ListDirEntry `json:"entries"`
	// Total counts every entry in the directory, including any not returned.
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
}

// NewListDirTool returns a Tool that lists a directory inside the workspace,
// directories first and then files, so the model can orient itself before
// reading or editing.
func NewListDirTool(ws *workspace.Workspace) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("list_dir: workspace is required")
	}
	return utils.InferTool("list_dir",
		"List a directory in the workspace: directories first, then files, each group alphabetical. "+
			"Returns each entry's name, workspace-relative path, type (file/dir/symlink/other), size and mode so you can chain further calls. "+
			"Symlinks are reported as symlinks and not followed. Omit path or pass \".\" for the workspace root.",
		func(_ context.Context, in ListDirInput) (ListDirOutput, error) {
			return fileList(ws, in)
		})
}

// fileRead implements read_file.
func fileRead(ws *workspace.Workspace, in ReadFileInput) (ReadFileOutput, error) {
	abs, err := ws.Resolve(in.Path)
	if err != nil {
		return ReadFileOutput{}, fmt.Errorf("read_file: %w", err)
	}
	rel := ws.Rel(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return ReadFileOutput{}, fmt.Errorf("read_file: stat %s: %w", rel, err)
	}
	if info.IsDir() {
		return ReadFileOutput{}, fmt.Errorf("read_file: %s is a directory; use list_dir to see what is inside it", rel)
	}

	maxBytes := ws.Limits().MaxReadBytes
	chunk, err := fileReadPrefix(abs, maxBytes)
	if err != nil {
		return ReadFileOutput{}, fmt.Errorf("read_file: %s: %w", rel, err)
	}
	// A NUL byte in the sniff window means this is not text. Returning it would
	// burn context on mojibake, so say what happened instead.
	if workspace.IsBinary(chunk.data) {
		return ReadFileOutput{}, fmt.Errorf("read_file: %s looks like a binary file; refusing to return it as text", rel)
	}

	lines := fileSplitLines(string(chunk.data))

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = fileDefaultReadLines
	}
	if limit > fileMaxReadLines {
		limit = fileMaxReadLines
	}

	// An empty file has no lines to point at, so any offset is vacuously fine
	// and the call returns empty content instead of an error. A non-empty file
	// with an offset past its last line is a real mistake worth reporting.
	if len(lines) > 0 && offset > len(lines) {
		if chunk.truncated {
			return ReadFileOutput{}, fmt.Errorf(
				"read_file: offset %d is past the %d lines read from %s; the file is %d bytes and only its first %d bytes were read",
				offset, len(lines), rel, info.Size(), maxBytes)
		}
		return ReadFileOutput{}, fmt.Errorf("read_file: offset %d is past the end of %s, which has %d lines", offset, rel, len(lines))
	}

	// start is clamped even though the check above rejects a real past-the-end
	// offset, because an empty file has zero lines for offset=3 to slice into.
	start := offset - 1
	if start > len(lines) {
		start = len(lines)
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	window := lines[start:end]

	var b strings.Builder
	for i, line := range window {
		if i > 0 {
			b.WriteByte('\n')
		}
		// The number and the text are separated by a rune that file content
		// essentially never contains, so the model can split them reliably.
		fmt.Fprintf(&b, "%*d%s%s", fileGutterWidth, offset+i, fileGutterSep, line)
	}

	out := ReadFileOutput{
		Path:       rel,
		Content:    b.String(),
		TotalLines: len(lines),
		Bytes:      info.Size(),
	}

	var notes []string
	if chunk.truncated {
		// Saying which of the two happened matters: a model that is told the
		// lines above are complete will trust the last one, so a cut-off line
		// has to be called out as cut off.
		if chunk.partial {
			notes = append(notes, fmt.Sprintf("the file is %d bytes and only its first %d bytes were read; the last line above is cut off mid-line",
				info.Size(), maxBytes))
		} else {
			notes = append(notes, fmt.Sprintf("the file is %d bytes and only its first %d bytes were read, so the lines above are complete",
				info.Size(), maxBytes))
		}
	}
	if end < len(lines) {
		notes = append(notes, fmt.Sprintf("showing lines %d-%d of %d; call read_file again with offset=%d to continue",
			offset, end, len(lines), end+1))
	}
	if len(notes) > 0 {
		out.Truncated = true
		out.Note = strings.Join(notes, "; ")
		// Repeating the note inside the content matters: the model reads
		// Content far more attentively than it reads a sibling field, and a
		// silent partial file is how an agent "reads" the whole thing and gets
		// the rest wrong.
		out.Content += "\n\n[read_file truncated: " + out.Note + "]"
	}
	return out, nil
}

// fileChunk is the head of a file read under the workspace read limit.
type fileChunk struct {
	// data holds whole lines only, unless partial is set.
	data []byte
	// truncated reports that the file is longer than the limit.
	truncated bool
	// partial reports that data ends mid-line, which happens when even a single
	// line does not fit in the limit.
	partial bool
}

// fileReadPrefix reads at most maxBytes of the file plus one byte, so the caller
// can tell "exactly at the limit" from "over the limit" without a second stat.
// When the file is over the limit the trailing partial line is dropped, because
// returning half a line invites the model to treat a cut-off token as real
// content; if there is no newline to cut at, the prefix is returned whole and
// flagged partial so the caller can say so.
func fileReadPrefix(abs string, maxBytes int64) (fileChunk, error) {
	f, err := os.Open(abs)
	if err != nil {
		return fileChunk{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return fileChunk{}, fmt.Errorf("read: %w", err)
	}
	if int64(len(data)) <= maxBytes {
		return fileChunk{data: data}, nil
	}
	data = data[:maxBytes]
	i := bytes.LastIndexByte(data, '\n')
	if i < 0 {
		return fileChunk{data: data, truncated: true, partial: true}, nil
	}
	return fileChunk{data: data[:i], truncated: true}, nil
}

// fileSplitLines splits text into lines without inventing a phantom last line
// for a trailing newline, and without leaving the CR of CRLF endings in the
// text the model sees.
func fileSplitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// fileWrite implements write_file.
func fileWrite(ws *workspace.Workspace, in WriteFileInput) (WriteFileOutput, error) {
	// ResolveForWrite applies the read-only and size limits before any syscall,
	// so a refused write creates neither the file nor its parent directories.
	abs, err := ws.ResolveForWrite(in.Path, int64(len(in.Content)))
	if err != nil {
		return WriteFileOutput{}, fmt.Errorf("write_file: %w", err)
	}
	abs = fileRealPath(abs)
	rel := ws.Rel(abs)

	perm := fileModeNew
	created := true
	switch info, serr := os.Stat(abs); {
	case serr == nil:
		if info.IsDir() {
			return WriteFileOutput{}, fmt.Errorf("write_file: %s is a directory", rel)
		}
		// Overwriting keeps the file's own mode, so write_file does not
		// silently chmod a file the user deliberately made private.
		perm = info.Mode().Perm()
		created = false
	case errors.Is(serr, fs.ErrNotExist):
		// New file: the default mode applies.
	default:
		return WriteFileOutput{}, fmt.Errorf("write_file: stat %s: %w", rel, serr)
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return WriteFileOutput{}, fmt.Errorf("write_file: create parent directories for %s: %w", rel, err)
	}
	n, err := fileWriteAtomic(abs, []byte(in.Content), perm)
	if err != nil {
		return WriteFileOutput{}, fmt.Errorf("write_file: %s: %w", rel, err)
	}
	return WriteFileOutput{Path: rel, Bytes: n, Created: created}, nil
}

// fileEdit implements edit_file.
func fileEdit(ws *workspace.Workspace, in EditFileInput) (EditFileOutput, error) {
	// An empty target matches at every position, including between every pair of
	// characters, so there is no sane way to honour it.
	if in.OldString == "" {
		return EditFileOutput{}, fmt.Errorf("edit_file: old_string is required and must not be empty; an empty string matches everywhere")
	}
	abs, err := ws.Resolve(in.Path)
	if err != nil {
		return EditFileOutput{}, fmt.Errorf("edit_file: %w", err)
	}
	abs = fileRealPath(abs)
	rel := ws.Rel(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return EditFileOutput{}, fmt.Errorf("edit_file: stat %s: %w", rel, err)
	}
	if info.IsDir() {
		return EditFileOutput{}, fmt.Errorf("edit_file: %s is a directory; only files can be edited", rel)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return EditFileOutput{}, fmt.Errorf("edit_file: read %s: %w", rel, err)
	}
	if workspace.IsBinary(data) {
		return EditFileOutput{}, fmt.Errorf("edit_file: %s looks like a binary file; refusing to edit it as text", rel)
	}
	content := string(data)

	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return EditFileOutput{}, fmt.Errorf(
			"edit_file: old_string was not found in %s; re-read the file with read_file and copy the target text exactly, whitespace included", rel)
	case count > 1 && !in.ReplaceAll:
		return EditFileOutput{}, fmt.Errorf(
			"edit_file: old_string appears %d times in %s, so this edit is ambiguous; include more surrounding context to make it unique or set replace_all=true to replace every occurrence",
			count, rel)
	}

	replacements := 1
	if in.ReplaceAll {
		replacements = count
	}
	// n is the known count, never -1: the checks above guarantee a single
	// occurrence unless the caller explicitly asked for every one, and a
	// replace-everything fallback here is exactly how the wrong occurrence gets
	// clobbered.
	updated := strings.Replace(content, in.OldString, in.NewString, replacements)

	// The whole file is rewritten, so the new size is what the write limit has
	// to be checked against.
	if _, err := ws.ResolveForWrite(in.Path, int64(len(updated))); err != nil {
		return EditFileOutput{}, fmt.Errorf("edit_file: %w", err)
	}
	n, err := fileWriteAtomic(abs, []byte(updated), info.Mode().Perm())
	if err != nil {
		return EditFileOutput{}, fmt.Errorf("edit_file: %s: %w", rel, err)
	}
	return EditFileOutput{Path: rel, Replacements: replacements, Bytes: n}, nil
}

// fileList implements list_dir.
func fileList(ws *workspace.Workspace, in ListDirInput) (ListDirOutput, error) {
	p := strings.TrimSpace(in.Path)
	if p == "" {
		// Resolve rejects an empty path by design, and "the workspace" is what
		// an omitted path means.
		p = "."
	}
	abs, err := ws.Resolve(p)
	if err != nil {
		return ListDirOutput{}, fmt.Errorf("list_dir: %w", err)
	}
	rel := ws.Rel(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return ListDirOutput{}, fmt.Errorf("list_dir: stat %s: %w", rel, err)
	}
	if !info.IsDir() {
		return ListDirOutput{}, fmt.Errorf("list_dir: %s is not a directory; use read_file to read a file", rel)
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return ListDirOutput{}, fmt.Errorf("list_dir: read %s: %w", rel, err)
	}

	entries := make([]ListDirEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		entry := ListDirEntry{
			Name: de.Name(),
			Path: ws.Rel(filepath.Join(abs, de.Name())),
			Type: fileEntryType(de),
		}
		// Info is the lstat of the entry, so a symlink is described as a
		// symlink and never followed.
		if fi, ierr := de.Info(); ierr == nil {
			entry.Size = fi.Size()
			entry.Mode = fi.Mode().String()
		}
		entries = append(entries, entry)
	}
	total := len(entries)

	// Sort everything first, then truncate, so the entries a truncated listing
	// does return are the same ones a full listing would have started with.
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := fileEntryRank(entries[i].Type), fileEntryRank(entries[j].Type)
		if ri != rj {
			return ri < rj
		}
		return entries[i].Name < entries[j].Name
	})

	out := ListDirOutput{Path: rel, Entries: entries, Total: total}
	if max := ws.Limits().MaxListEntries; max > 0 && total > max {
		out.Entries = entries[:max]
		out.Truncated = true
		out.Note = fmt.Sprintf("showing the first %d of %d entries; list a subdirectory or raise the limit to see the rest", max, total)
	}
	return out, nil
}

// fileEntryType classifies a directory entry from the directory itself, without
// following symlinks.
func fileEntryType(de fs.DirEntry) string {
	switch {
	case de.Type()&fs.ModeSymlink != 0:
		return "symlink"
	case de.IsDir():
		return "dir"
	case de.Type().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// fileEntryRank orders a listing: directories first, then files, then the less
// common types, each group sorted by name by the caller.
func fileEntryRank(entryType string) int {
	switch entryType {
	case "dir":
		return 0
	case "file":
		return 1
	case "symlink":
		return 2
	default:
		return 3
	}
}

// fileRealPath returns the path a mutation should actually target. Workspace
// Resolve confines the path but returns it literally, so writing to a symlink
// that lives inside the workspace would replace the link instead of the file it
// points at. Resolve has already proven that following the link stays inside the
// workspace, so evaluating it here cannot widen access.
func fileRealPath(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// Not existing yet, or a dangling link: the literal path is the target.
	return abs
}

// fileWriteAtomic replaces the file at abs with data, writing a temp file in the
// same directory first and then renaming it over the target. Rename is atomic
// within a filesystem, so a reader never observes a half-written file and an
// interrupted call leaves the original untouched.
func fileWriteAtomic(abs string, data []byte, perm fs.FileMode) (int64, error) {
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, fileTempPattern)
	if err != nil {
		return 0, fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup. After a successful rename there is nothing left at
	// tmpName, so this only removes a file from a failed attempt.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// The mode is applied to the temp file before the rename, which is how the
	// overwritten file keeps its permissions.
	if err := tmp.Chmod(perm); err != nil {
		return 0, fmt.Errorf("set mode on temp file: %w", err)
	}
	n, err := tmp.Write(data)
	if err != nil {
		return 0, fmt.Errorf("write temp file: %w", err)
	}
	// Sync before rename so a crash cannot publish a name whose contents were
	// never flushed.
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return 0, fmt.Errorf("rename temp file over target: %w", err)
	}
	return int64(n), nil
}
