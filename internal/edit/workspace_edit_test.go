package edit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/lsp"
)

// A rename arrives as a WorkspaceEdit, in one of two shapes, with positions in
// UTF-16 columns against the original document. Getting any of that wrong means a
// rename that lands in the wrong place — and it lands in the wrong place only in
// files that have more than one reference, which is the case the tool exists for.

func testEdit(line, startCol, endLine, endCol int, text string) lsp.TextEdit {
	return lsp.TextEdit{
		Range: lsp.Range{
			Start: lsp.Position{Line: line, Character: startCol},
			End:   lsp.Position{Line: endLine, Character: endCol},
		},
		NewText: text,
	}
}

// TestFromWorkspaceEditDocumentChanges is the newer shape, and the ordered one.
func TestFromWorkspaceEditDocumentChanges(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Caller() {\n\tFoo()\n\tFoo()\n}\n",
	})
	we := lsp.WorkspaceEdit{DocumentChanges: []lsp.TextDocumentEdit{
		{
			TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{URI: lsp.PathToURI(filepath.Join(root, "a.go"))},
			Edits:        []lsp.TextEdit{testEdit(2, 5, 2, 8, "Bar")},
		},
		{
			TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{URI: lsp.PathToURI(filepath.Join(root, "b.go"))},
			// Bottom-up within the file: the second call site is on a later line.
			Edits: []lsp.TextEdit{testEdit(4, 1, 4, 4, "Bar"), testEdit(3, 1, 3, 4, "Bar")},
		},
	}}

	ops, err := FromWorkspaceEdit(ws, we)
	if err != nil {
		t.Fatalf("FromWorkspaceEdit: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("operations = %+v, want two files", ops)
	}
	if ops[0].Path != "a.go" || ops[1].Path != "b.go" {
		t.Errorf("paths = %s, %s", ops[0].Path, ops[1].Path)
	}
	for _, op := range ops {
		for i, e := range op.Edits {
			if e.Old != "Foo" || e.New != "Bar" {
				t.Errorf("%s edit %d = %q → %q, want Foo → Bar", op.Path, i, e.Old, e.New)
			}
		}
	}

	// And the batch applies end to end through the two-phase applier.
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "rename_symbol"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "rename_symbol"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	a := read(t, root, "a.go")
	if !strings.Contains(a, "func Bar()") {
		t.Errorf("a.go = %q", a)
	}
	b := read(t, root, "b.go")
	if strings.Count(b, "Bar()") != 2 || strings.Contains(b, "Foo()") {
		t.Errorf("b.go = %q, want both call sites renamed and none left", b)
	}
}

// TestFromWorkspaceEditChangesShape is the older shape: a path → edits map with no
// version information.
func TestFromWorkspaceEditChangesShape(t *testing.T) {
	ws, root := newRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	we := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, "a.go")): {testEdit(2, 5, 2, 8, "Bar")},
	}}
	ops, err := FromWorkspaceEdit(ws, we)
	if err != nil {
		t.Fatalf("FromWorkspaceEdit: %v", err)
	}
	if len(ops) != 1 || ops[0].Edits[0].Old != "Foo" {
		t.Errorf("operations = %+v", ops)
	}
}

// TestFromWorkspaceEditRefusesOutsidePathsEntirely is the rule that matters most: a
// rename that reaches outside the workspace is refused *whole*, and the error names
// every path, because applying the inside part would leave the outside referring to
// a name that no longer exists.
func TestFromWorkspaceEditRefusesOutsidePathsEntirely(t *testing.T) {
	ws, root := newRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	before, _ := os.ReadFile(filepath.Join(root, "a.go"))

	we := lsp.WorkspaceEdit{DocumentChanges: []lsp.TextDocumentEdit{
		{
			TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{URI: lsp.PathToURI(filepath.Join(root, "a.go"))},
			Edits:        []lsp.TextEdit{testEdit(2, 5, 2, 8, "Bar")},
		},
		{
			// A file the server knows about but the sandbox does not contain.
			TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{URI: lsp.PathToURI(filepath.Join(filepath.Dir(root), "outside.go"))},
			Edits:        []lsp.TextEdit{testEdit(0, 0, 0, 3, "Bar")},
		},
	}}

	_, err := FromWorkspaceEdit(ws, we)
	if err == nil {
		t.Fatal("a rename reaching outside the workspace must be refused")
	}
	if !strings.Contains(err.Error(), "工作区之外") || !strings.Contains(err.Error(), "outside.go") {
		t.Errorf("the refusal should name the outside file: %v", err)
	}
	if !strings.Contains(err.Error(), "半次重命名") {
		t.Errorf("the refusal should say why it is all-or-nothing: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(before) != string(after) {
		t.Error("the inside file was modified despite the refusal")
	}
}

// TestFromWorkspaceEditRejectsOddShapes: the cases a malformed or hostile server
// would produce.
func TestFromWorkspaceEditRejectsOddShapes(t *testing.T) {
	ws, root := newRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	uri := lsp.PathToURI(filepath.Join(root, "a.go"))

	cases := []struct {
		name string
		we   lsp.WorkspaceEdit
	}{
		{"no edits at all", lsp.WorkspaceEdit{}},
		{"a non-file uri", lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
			"https://example.com/x.go": {testEdit(0, 0, 0, 3, "Bar")},
		}}},
		{"a range past the end of the file", lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
			uri: {testEdit(99, 0, 99, 3, "Bar")},
		}}},
		{"an empty range", lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
			uri: {testEdit(2, 5, 2, 5, "Bar")},
		}}},
		{"a range that ends before it starts", lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
			uri: {testEdit(2, 8, 2, 5, "Bar")},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromWorkspaceEdit(ws, tc.we); err == nil {
				t.Error("this shape must be refused rather than guessed at")
			}
		})
	}
}

// TestFromWorkspaceEditHandlesUTF16Columns: positions are UTF-16 code units, so an
// emoji or a CJK identifier before the symbol shifts the columns. Reading them as
// runes or bytes lands the edit in the wrong place — and only in files that contain
// such a character before the symbol.
func TestFromWorkspaceEditHandlesUTF16Columns(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\n// 中文注释 emoji 🎯 之后\nfunc Foo() {}\n",
	})
	// The line "func Foo() {}" is line index 3; "Foo" starts at column 5.
	we := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, "a.go")): {testEdit(3, 5, 3, 8, "Bar")},
	}}
	ops, err := FromWorkspaceEdit(ws, we)
	if err != nil {
		t.Fatalf("FromWorkspaceEdit: %v", err)
	}
	if ops[0].Edits[0].Old != "Foo" {
		t.Errorf("the edit targeted %q; a UTF-16 column was misread", ops[0].Edits[0].Old)
	}

	// And a symbol on the CJK line itself: "注释" starts at column 3 of runes but
	// the emoji counts as two UTF-16 units, so the server's column for something
	// after it is larger than the rune index.
	// The line is "// 中文注释 emoji 🎯 之后": in UTF-16 units, the emoji occupies
	// two, so 之后 starts at column 17 even though it is the 13th rune.
	we2 := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, "a.go")): {testEdit(2, 17, 2, 19, "XY")},
	}}
	ops2, err := FromWorkspaceEdit(ws, we2)
	if err != nil {
		t.Fatalf("second edit: %v", err)
	}
	if got := ops2[0].Edits[0].Old; got != "之后" {
		t.Errorf("edit after a surrogate pair targeted %q, want 之后", got)
	}
}

// TestFromWorkspaceEditMultiLineSpan: a server may insert a newline, and a span that
// crosses lines has to be reconstructed exactly or the old_string will not match.
func TestFromWorkspaceEditMultiLineSpan(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {\n\treturn\n}\n",
	})
	we := lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, "a.go")): {testEdit(2, 5, 4, 1, "Bar")},
	}}
	ops, err := FromWorkspaceEdit(ws, we)
	if err != nil {
		t.Fatalf("FromWorkspaceEdit: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("operations = %+v", ops)
	}
	if got := ops[0].Edits[0].Old; got != "Foo() {\n\treturn\n}" {
		t.Errorf("multi-line span = %q", got)
	}
	// It is a real edit: the plan finds it.
	if _, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "rename_symbol"}); err != nil {
		t.Errorf("the reconstructed span does not match the file: %v", err)
	}
}

// TestPositionalEditKeepsTheWholeFile: a positional edit replaces a span, and the
// lines around it are the file. Omitting the lines before the span deletes the top
// of the file — which is what a missing prefix append does, and which surfaces as
// the *next* edit failing with a position past the end, a long way from the cause.
func TestPositionalEditKeepsTheWholeFile(t *testing.T) {
	content := "line one\nline two\nline three\nline four\n"
	// Replace "three" on line 2 (0-based), columns 5..10.
	got, n, err := Match(content, Edit{
		Old:      "three",
		New:      "THREE",
		Position: &Span{StartLine: 2, StartChar: 5, EndLine: 2, EndChar: 10},
	})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d", n)
	}
	want := "line one\nline two\nline THREE\nline four\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

// TestPositionalEditRefusesAStalePosition: the text at the position is checked, so
// a file that changed under the version the server was editing fails loudly instead
// of overwriting whatever is there now.
func TestPositionalEditRefusesAStalePosition(t *testing.T) {
	content := "alpha\nbeta\n"
	_, _, err := Match(content, Edit{
		Old:      "gamma",
		New:      "delta",
		Position: &Span{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 5},
	})
	if err == nil {
		t.Fatal("a stale position must be refused")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "gamma") {
		t.Errorf("the refusal should show what is there and what was expected: %v", err)
	}
}

// TestPositionalEditsApplyBottomUpAsABatch: the coordinates only stay valid because
// the edits are applied from the end of the file backwards.
func TestPositionalEditsApplyBottomUpAsABatch(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.txt": "Foo\nFoo\nFoo\n",
	})
	// Three spans of the same word — what a rename produces. As text edits the
	// second would be ambiguous; as positional edits each is exact.
	ops := []Operation{{Path: "a.txt", Edits: []Edit{
		{Old: "Foo", New: "Bar", Position: &Span{StartLine: 2, StartChar: 0, EndLine: 2, EndChar: 3}},
		{Old: "Foo", New: "Bar", Position: &Span{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 3}},
		{Old: "Foo", New: "Bar", Position: &Span{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3}},
	}}}
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "rename_symbol"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "rename_symbol"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, root, "a.txt"); got != "Bar\nBar\nBar\n" {
		t.Errorf("result = %q, want every occurrence renamed", got)
	}
}
