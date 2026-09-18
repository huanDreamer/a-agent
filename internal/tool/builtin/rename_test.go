package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/edit"
	"github.com/huan/huan-agent/internal/lsp"
	"github.com/huan/huan-agent/internal/workspace"
)

// fakeRenamer answers with a scripted WorkspaceEdit, so the tool's own behaviour
// (refusals, dry run, the report) is what these tests exercise.
type fakeRenamer struct {
	we  lsp.WorkspaceEdit
	err error
	// gotName records the new name the tool asked for.
	gotName string
}

func (f *fakeRenamer) Rename(_ context.Context, _ string, _ lsp.LineCol, newName string) (lsp.WorkspaceEdit, error) {
	f.gotName = newName
	return f.we, f.err
}

// renameRig builds the tool over a temp workspace.
func renameRig(t *testing.T, files map[string]string, renamer *fakeRenamer) (*patchTool, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root, workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	batch := edit.NewWorkspace(ws, edit.Options{Prefix: RenameSymbolToolName},
		edit.ApplyOptions{Prefix: RenameSymbolToolName})
	inner, err := NewRenameSymbolTool(renamer, batch)
	if err != nil {
		t.Fatal(err)
	}
	return &patchTool{inner: inner}, ws.Root()
}

func runRename(t *testing.T, tl *patchTool, in RenameSymbolInput) (RenameSymbolOutput, error) {
	t.Helper()
	args, _ := json.Marshal(in)
	out, err := tl.InvokableRun(context.Background(), string(args))
	if err != nil {
		return RenameSymbolOutput{}, err
	}
	var parsed RenameSymbolOutput
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		t.Fatalf("bad output: %v (%s)", uerr, out)
	}
	return parsed, nil
}

// renameEdit is a one-file WorkspaceEdit replacing a symbol at a position.
func renameEdit(root, file string, line, start, end int, newName string) lsp.WorkspaceEdit {
	return lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, file)): {{
			Range: lsp.Range{
				Start: lsp.Position{Line: line, Character: start},
				End:   lsp.Position{Line: line, Character: end},
			},
			NewText: newName,
		}},
	}}
}

// TestRenameSymbolAcrossFiles is the feature: the tool changes every reference the
// language server named, in one atomic batch.
func TestRenameSymbolAcrossFiles(t *testing.T) {
	renamer := &fakeRenamer{}
	tl, root := renameRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc C() {\n\tFoo()\n}\n",
	}, renamer)
	// The server answers about both files, which is what a real one does.
	renamer.we = lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		lsp.PathToURI(filepath.Join(root, "a.go")): {{
			Range:   lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 8}},
			NewText: "Bar",
		}},
		lsp.PathToURI(filepath.Join(root, "b.go")): {{
			Range:   lsp.Range{Start: lsp.Position{Line: 3, Character: 1}, End: lsp.Position{Line: 3, Character: 4}},
			NewText: "Bar",
		}},
	}}

	got, err := runRename(t, tl, RenameSymbolInput{Path: "a.go", Line: 3, Column: 6, NewName: "Bar"})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !got.Applied || len(got.Files) != 2 || got.Occurrences != 2 {
		t.Errorf("output = %+v", got)
	}
	if renamer.gotName != "Bar" {
		t.Errorf("the server was asked for %q", renamer.gotName)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "b.go")); !strings.Contains(string(b), "Bar()") {
		t.Errorf("b.go = %q", b)
	}
	if !strings.Contains(got.Summary, "2 处引用") {
		t.Errorf("the summary should count the references: %q", got.Summary)
	}
}

// TestRenameSymbolDryRun: the plan is inspectable, and nothing is written.
func TestRenameSymbolDryRun(t *testing.T) {
	renamer := &fakeRenamer{}
	tl, root := renameRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"}, renamer)
	renamer.we = renameEdit(root, "a.go", 2, 5, 8, "Bar")

	got, err := runRename(t, tl, RenameSymbolInput{Path: "a.go", Line: 3, Column: 6, NewName: "Bar", DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !got.DryRun || got.Applied {
		t.Errorf("output = %+v", got)
	}
	if len(got.Preview) != 1 || len(got.Preview[0].Lines) == 0 {
		t.Errorf("a dry run must show the change: %+v", got.Preview)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.go")); !strings.Contains(string(b), "Foo") {
		t.Error("a dry run wrote to the disk")
	}
}

// TestRenameSymbolValidatesItsInput: a 0 or negative position is a mistake the
// message has to explain, because "line 0" is what a model produces when it forgets
// the protocol is 1-based.
func TestRenameSymbolValidatesItsInput(t *testing.T) {
	renamer := &fakeRenamer{}
	tl, _ := renameRig(t, map[string]string{"a.go": "package a\n"}, renamer)

	cases := []struct {
		name    string
		in      RenameSymbolInput
		wantErr string
	}{
		{"no path", RenameSymbolInput{Line: 1, Column: 1, NewName: "B"}, "path"},
		{"no name", RenameSymbolInput{Path: "a.go", Line: 1, Column: 1}, "new_name"},
		{"zero line", RenameSymbolInput{Path: "a.go", Line: 0, Column: 1, NewName: "B"}, "1-based"},
		{"zero column", RenameSymbolInput{Path: "a.go", Line: 1, Column: 0, NewName: "B"}, "1-based"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRename(t, tl, tc.in)
			if err == nil {
				t.Fatal("this input must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestRenameSymbolReportsAServerRefusal: "this position cannot be renamed" has to
// reach the model as a reason, not as an empty result.
func TestRenameSymbolReportsAServerRefusal(t *testing.T) {
	renamer := &fakeRenamer{err: os.ErrPermission}
	tl, _ := renameRig(t, map[string]string{"a.go": "package a\n"}, renamer)

	_, err := runRename(t, tl, RenameSymbolInput{Path: "a.go", Line: 1, Column: 1, NewName: "B"})
	if err == nil {
		t.Fatal("a server refusal must be reported")
	}
	if !strings.Contains(err.Error(), "rename_symbol") {
		t.Errorf("the error should be attributable to this tool: %v", err)
	}
}

// TestRenameSymbolDescriptionStatesTheDivisionOfLabour: the model picks the tool,
// so the description is part of the interface.
func TestRenameSymbolDescriptionStatesTheDivisionOfLabour(t *testing.T) {
	renamer := &fakeRenamer{}
	tl, _ := renameRig(t, map[string]string{"a.go": "package a\n"}, renamer)
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"language server", "apply_patch", "dry_run", "all or nothing", "1-based"} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("the description should mention %q: %s", want, info.Desc)
		}
	}
}
