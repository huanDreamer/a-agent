package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/edit"
	"github.com/huan/huan-agent/internal/workspace"
)

// newPatchRig builds the tool over a temp workspace with the given files.
func newPatchRig(t *testing.T, files map[string]string) (*patchTool, string) {
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
	batch := edit.NewWorkspace(ws, edit.Options{Prefix: ApplyPatchToolName},
		edit.ApplyOptions{Prefix: ApplyPatchToolName})
	inner, err := NewApplyPatchTool(batch, ApplyPatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &patchTool{inner: inner}, ws.Root()
}

// patchTool is the tool plus the two calls a test makes on it, so the helpers
// below do not repeat the eino option list.
type patchTool struct{ inner einotool.InvokableTool }

func (p *patchTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return p.inner.Info(ctx) }

func (p *patchTool) InvokableRun(ctx context.Context, args string, opts ...einotool.Option) (string, error) {
	return p.inner.InvokableRun(ctx, args, opts...)
}

// runPatch invokes the tool with a structured batch.
func runPatch(t *testing.T, tl *patchTool, in ApplyPatchInput) (ApplyPatchOutput, error) {
	t.Helper()
	args, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), string(args))
	if err != nil {
		return ApplyPatchOutput{}, err
	}
	var parsed ApplyPatchOutput
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		t.Fatalf("the tool returned something that is not its output type: %v (%s)", uerr, out)
	}
	return parsed, nil
}

// TestApplyPatchAcrossFiles is the feature, through the tool.
func TestApplyPatchAcrossFiles(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Caller() { Foo() }\n",
	})
	got, err := runPatch(t, tl, ApplyPatchInput{Operations: []PatchOperation{
		{Path: "a.go", Edits: []PatchEdit{{OldString: "func Foo()", NewString: "func Bar()"}}},
		{Path: "b.go", Edits: []PatchEdit{{OldString: "Foo()", NewString: "Bar()"}}},
	}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !got.Applied || len(got.Files) != 2 {
		t.Errorf("output = %+v", got)
	}
	if !strings.Contains(got.Summary, "+") || !strings.Contains(got.Summary, "-") {
		t.Errorf("the summary should carry the line counts: %q", got.Summary)
	}
	for name, want := range map[string]string{"a.go": "func Bar()", "b.go": "Bar()"} {
		b, _ := os.ReadFile(filepath.Join(root, name))
		if !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, want it to contain %q", name, b, want)
		}
	}
}

// TestApplyPatchDryRunTouchesNothing: the plan has to be inspectable before it is
// approved, and a dry run that wrote anything would make that a lie.
func TestApplyPatchDryRunTouchesNothing(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	before, _ := os.ReadFile(filepath.Join(root, "a.go"))

	got, err := runPatch(t, tl, ApplyPatchInput{
		DryRun: true,
		Operations: []PatchOperation{
			{Path: "a.go", Edits: []PatchEdit{{OldString: "func Foo()", NewString: "func Bar()"}}},
		},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !got.DryRun || got.Applied {
		t.Errorf("output = %+v, want a dry run", got)
	}
	if len(got.Preview) != 1 || len(got.Preview[0].Lines) == 0 {
		t.Errorf("a dry run must show the change: %+v", got.Preview)
	}
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(before) != string(after) {
		t.Error("a dry run wrote to the disk")
	}
}

// TestApplyPatchRefusesTheWholeBatch: one bad edit means no file changes, which is
// the entire reason this tool exists rather than three edit_file calls.
func TestApplyPatchRefusesTheWholeBatch(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Caller() { Foo() }\n",
	})
	before := map[string]string{}
	for _, name := range []string{"a.go", "b.go"} {
		b, _ := os.ReadFile(filepath.Join(root, name))
		before[name] = string(b)
	}

	// The first operation is valid, the second names text that is not there.
	_, err := runPatch(t, tl, ApplyPatchInput{Operations: []PatchOperation{
		{Path: "a.go", Edits: []PatchEdit{{OldString: "func Foo()", NewString: "func Bar()"}}},
		{Path: "b.go", Edits: []PatchEdit{{OldString: "not in the file", NewString: "x"}}},
	}})
	if err == nil {
		t.Fatal("a batch with an impossible edit must fail")
	}
	if !strings.Contains(err.Error(), "b.go") || !strings.Contains(err.Error(), "operations[1]") {
		t.Errorf("the error should name the file and the operation: %v", err)
	}
	for name, want := range before {
		b, _ := os.ReadFile(filepath.Join(root, name))
		if string(b) != want {
			t.Errorf("%s changed despite the batch being refused: %q", name, b)
		}
	}
}

// TestApplyPatchRefusesReadOnly.
func TestApplyPatchRefusesReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(root, workspace.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	batch := edit.NewWorkspace(ws, edit.Options{Prefix: ApplyPatchToolName},
		edit.ApplyOptions{Prefix: ApplyPatchToolName})
	inner, err := NewApplyPatchTool(batch, ApplyPatchOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	tl := &patchTool{inner: inner}
	_, err = runPatch(t, tl, ApplyPatchInput{Operations: []PatchOperation{
		{Path: "a.go", Edits: []PatchEdit{{OldString: "package a", NewString: "package b"}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "只读") {
		t.Errorf("a read-only workspace must refuse: %v", err)
	}
}

// TestApplyPatchNeedsSomethingToDo.
func TestApplyPatchNeedsSomethingToDo(t *testing.T) {
	tl, _ := newPatchRig(t, map[string]string{"a.go": "package a\n"})
	_, err := runPatch(t, tl, ApplyPatchInput{})
	if err == nil || !strings.Contains(err.Error(), "operations 或 patch") {
		t.Errorf("an empty request must be refused with the two forms named: %v", err)
	}
}

// TestApplyPatchSchemaHasNoCommaDamage: the JSON-schema tag parser splits on
// commas, so a description containing one loses the rest of the sentence and the
// model sees a truncated parameter. This is the guard for the nested fields too.
func TestApplyPatchSchemaHasNoCommaDamage(t *testing.T) {
	tl, _ := newPatchRig(t, map[string]string{"a.go": "package a\n"})
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc := info.Desc
	for _, want := range []string{"all or nothing", "edit_file", "rename_symbol", "dry_run"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the tool description should mention %q (the division of labour is what keeps the model from using grep for a rename): %s", want, desc)
		}
	}
	if !strings.Contains(desc, "cannot create or delete files") {
		t.Error("the description should state what this stage does not do")
	}
	// The parameters exist and are documented.
	if info.ParamsOneOf == nil {
		t.Fatal("no parameters")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(js)
	params := string(raw)
	for _, want := range []string{"operations", "patch", "dry_run", "old_string", "new_string", "replace_all"} {
		if !strings.Contains(params, want) {
			t.Errorf("the schema is missing %q", want)
		}
	}
}

// TestApplyPatchAcceptsAUnifiedDiff: the second input form, which exists for the
// case where the model already has a diff. It is parsed into the same operations
// the structured form produces and goes through the same applier, so the atomicity
// guarantee does not depend on which form was used.
func TestApplyPatchAcceptsAUnifiedDiff(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{
		"calc.go": "package calc\n\n// Add sums.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
	})
	diff := `--- a/calc.go
+++ b/calc.go
@@ -3,4 +3,4 @@
 // Add sums.
-func Add(a, b int) int {
+func Sum(a, b int) int {
 	return a + b
 }
`
	got, err := runPatch(t, tl, ApplyPatchInput{Patch: diff})
	if err != nil {
		t.Fatalf("apply diff: %v", err)
	}
	if !got.Applied || len(got.Files) != 1 || got.Files[0].Path != "calc.go" {
		t.Errorf("output = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "calc.go")); !strings.Contains(string(b), "func Sum(") {
		t.Errorf("calc.go = %q", b)
	}
}

// TestApplyPatchDiffDryRun: a diff can be inspected before it is applied, which is
// what makes pasting one safe.
func TestApplyPatchDiffDryRun(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	diff := "--- a/a.go\n+++ b/a.go\n@@ -3,1 +3,1 @@\n-func Foo() {}\n+func Bar() {}\n"
	got, err := runPatch(t, tl, ApplyPatchInput{Patch: diff, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !got.DryRun || got.Applied {
		t.Errorf("output = %+v", got)
	}
	if len(got.Preview) != 1 {
		t.Errorf("a dry run should show the change: %+v", got.Preview)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.go")); !strings.Contains(string(b), "Foo") {
		t.Error("a dry run wrote to the disk")
	}
}

// TestApplyPatchRefusesADiffThatCreatesFiles: the refusal has to reach the model
// with the alternative named, and nothing may be written.
func TestApplyPatchRefusesADiffThatCreatesFiles(t *testing.T) {
	tl, root := newPatchRig(t, map[string]string{"a.go": "package a\n"})
	diff := "diff --git a/b.go b/b.go\nnew file mode 100644\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1,1 @@\n+package b\n"
	_, err := runPatch(t, tl, ApplyPatchInput{Patch: diff})
	if err == nil {
		t.Fatal("creating a file must be refused")
	}
	if !strings.Contains(err.Error(), "write_file") {
		t.Errorf("the refusal should name the alternative: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "b.go")); statErr == nil {
		t.Error("a refused patch created a file")
	}
}
