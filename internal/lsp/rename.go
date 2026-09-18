package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Renaming, through the language server rather than through a search.
//
// The difference is not convenience, it is correctness: a string search for a
// symbol's name hits comments, string literals, a different package's function
// with the same name, and misses nothing only by luck. The language server knows
// which identifier the user is pointing at and which references that binding
// resolves to, which is the same information the compiler has.

// WorkspaceEdit is a rename's answer: the changes to make, in one of the two
// shapes the protocol defines.
//
// Both shapes are real and both are produced by servers in the wild, so a client
// that understands only one silently loses half of a rename: `changes` is a plain
// path → edits map, and `documentChanges` is the newer, ordered form that can also
// carry a document version.
type WorkspaceEdit struct {
	// Changes maps a path to that file's edits.
	Changes map[string][]TextEdit `json:"changes,omitempty"`
	// DocumentChanges is the newer form: one entry per file, in order.
	DocumentChanges []TextDocumentEdit `json:"documentChanges,omitempty"`
}

// TextDocumentEdit is one file's edits inside documentChanges.
type TextDocumentEdit struct {
	TextDocument OptionalVersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                              `json:"edits"`
}

// OptionalVersionedTextDocumentIdentifier names a file, optionally with the
// version the server believed it was editing.
type OptionalVersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version *int   `json:"version,omitempty"`
}

// TextEdit is one replacement inside a file.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// FileEdits is one file's edits, resolved to a path.
type FileEdits struct {
	// Path is the absolute path the server named.
	Path string
	// Edits are in the order the server sent them.
	Edits []TextEdit
}

// Flatten turns either shape into a per-file list, in a deterministic order.
//
// DocumentChanges wins when both are present: a server that sends both is sending
// the ordered form as the authoritative one, and applying the other as well would
// double every edit.
func (w WorkspaceEdit) Flatten() []FileEdits {
	var out []FileEdits
	if len(w.DocumentChanges) > 0 {
		for _, d := range w.DocumentChanges {
			path := pathFromURI(d.TextDocument.URI)
			if path == "" {
				continue
			}
			out = append(out, FileEdits{Path: path, Edits: d.Edits})
		}
		return out
	}
	paths := make([]string, 0, len(w.Changes))
	for raw := range w.Changes {
		paths = append(paths, raw)
	}
	// Map iteration is random, and a rename that reported its files in a different
	// order on every run would make the report and the checkpoint order
	// unreproducible.
	sort.Strings(paths)
	for _, raw := range paths {
		out = append(out, FileEdits{Path: pathFromURI(raw), Edits: w.Changes[raw]})
	}
	return out
}

// PrepareRename asks whether the position can be renamed at all.
//
// It exists because the alternative is a rename that fails at the end with a
// server-specific message: "this is a keyword", "this position is inside a
// string", "this file is not part of the project". Asking first turns that into a
// readable refusal before anything is planned.
func (c *Client) PrepareRename(ctx context.Context, path string, at LineCol) (Range, error) {
	_, pos, err := c.Prepare(ctx, path, at)
	if err != nil {
		return Range{}, err
	}
	raw, err := c.call(ctx, "textDocument/prepareRename", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
		Position:     pos,
	})
	if err != nil {
		return Range{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return Range{}, fmt.Errorf("lsp: 这个位置上没有可以重命名的符号")
	}

	// The result is either a range, or an object with a range and a placeholder
	// (older and newer shapes of the same answer).
	var asRange Range
	if err := json.Unmarshal(raw, &asRange); err == nil && asRange.Start.Line > 0 {
		return asRange, nil
	}
	var withPlaceholder struct {
		Range       Range  `json:"range"`
		Placeholder string `json:"placeholder"`
	}
	if err := json.Unmarshal(raw, &withPlaceholder); err == nil && withPlaceholder.Range.Start.Line > 0 {
		return withPlaceholder.Range, nil
	}
	return Range{}, fmt.Errorf("lsp: 语言服务器返回了无法理解的 prepareRename 结果：%s", clipBytes(raw))
}

// Rename returns the edits that rename the symbol at a 1-based position.
func (c *Client) Rename(ctx context.Context, path string, at LineCol, newName string) (WorkspaceEdit, error) {
	if strings.TrimSpace(newName) == "" {
		return WorkspaceEdit{}, fmt.Errorf("lsp: 新名字不能为空")
	}
	_, pos, err := c.Prepare(ctx, path, at)
	if err != nil {
		return WorkspaceEdit{}, err
	}
	// **The file's siblings must be opened before asking.** A language server only
	// knows about documents it has been told about, so a rename asked for
	// immediately after the target file was opened answers with the references it
	// has seen — which is the target file and nothing else. A rename that silently
	// misses the other files is the worst outcome this feature can produce: it
	// leaves a repository that does not build, and it looks like it worked.
	//
	// A real session proved this: the first rename on a two-file module changed the
	// definition and left the call site in the other file, and the model had to
	// notice and patch it by hand.
	c.openSiblings(ctx, path)
	raw, err := c.call(ctx, "textDocument/rename", RenameParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
			Position:     pos,
		},
		NewName: newName,
	})
	if err != nil {
		return WorkspaceEdit{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		// A server that answers null is saying "no edits", which for a rename means
		// it could not find anything to rename — not that the symbol has no
		// references.
		return WorkspaceEdit{}, fmt.Errorf("lsp: 语言服务器没有给出任何改动（这个位置可能不可重命名）")
	}
	var we WorkspaceEdit
	if err := json.Unmarshal(raw, &we); err != nil {
		return WorkspaceEdit{}, fmt.Errorf("lsp: 解析重命名结果失败：%w", err)
	}
	if len(we.Flatten()) == 0 {
		return WorkspaceEdit{}, fmt.Errorf("lsp: 重命名结果里没有任何文件")
	}
	return we, nil
}

// RenameParams is the request body of textDocument/rename.
type RenameParams struct {
	TextDocumentPositionParams
	NewName string `json:"newName"`
}

// pathFromURI converts a file:// URI to a path, tolerating the encodings servers
// use and the Windows drive-letter form.
func pathFromURI(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "file:") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	p := u.Path
	if p == "" {
		p = strings.TrimPrefix(uri, "file://")
	}
	if unescaped, err := url.PathUnescape(p); err == nil {
		p = unescaped
	}
	// The protocol says a leading slash before a drive letter is not part of the
	// path on Windows.
	if len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// clipBytes bounds a server's answer before it goes into an error message.
func clipBytes(raw []byte) string {
	const max = 200
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}

// openSiblings tells the server about the other source files beside path.
//
// It is deliberately bounded to one directory: a package's files are what a
// reference set needs for the rename to be complete in the common case, and
// walking a whole repository would mean opening thousands of documents for one
// request. The bound is stated rather than hidden — a rename across a large
// monorepo may still need the files opened by other tools first.
func (c *Client) openSiblings(ctx context.Context, path string) {
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	const maxSiblings = 64
	opened := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue
		}
		if opened >= maxSiblings {
			return
		}
		sibling := filepath.Join(dir, e.Name())
		if sibling == path {
			continue
		}
		// A failure here is not fatal: the rename proceeds with whatever the server
		// knows, and a file that could not be read could not be renamed anyway.
		_ = c.Open(ctx, sibling)
		opened++
	}
}
