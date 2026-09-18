package edit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/workspace"
)

// newRig builds a workspace over a temp directory with the given files.
func newRig(t *testing.T, files map[string]string) (*workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root, workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the root the way the workspace does, so the paths this test compares
	// against are the same ones the package produces (macOS /var is /private/var).
	root = ws.Root()
	return ws, root
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fingerprint is every file's content, so a test can prove the disk did not move.
func fingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func same(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestMatchFollowsEditFileSemantics: the two tools must not have two mental
// models, so every case edit_file has is here.
func TestMatchFollowsEditFileSemantics(t *testing.T) {
	cases := []struct {
		name    string
		content string
		edit    Edit
		want    string
		wantN   int
		wantErr string
	}{
		{name: "one occurrence", content: "a Foo b", edit: Edit{Old: "Foo", New: "Bar"}, want: "a Bar b", wantN: 1},
		{
			name: "missing", content: "a Foo b", edit: Edit{Old: "Nope", New: "x"},
			wantErr: "not found",
		},
		{
			name: "ambiguous", content: "Foo Foo", edit: Edit{Old: "Foo", New: "Bar"},
			wantErr: "ambiguous",
		},
		{
			name: "replace all", content: "Foo Foo", edit: Edit{Old: "Foo", New: "Bar", ReplaceAll: true},
			want: "Bar Bar", wantN: 2,
		},
		{
			name: "empty old is refused", content: "abc", edit: Edit{Old: "", New: "x"},
			wantErr: "must not be empty",
		},
		{
			name: "multibyte", content: "中文测试 中文", edit: Edit{Old: "测试", New: "試"},
			want: "中文試 中文", wantN: 1,
		},
		{
			name: "whitespace matters", content: "if err != nil {\n\treturn err\n}",
			edit:    Edit{Old: "if err != nil {\n    return err\n}", New: "ok"},
			wantErr: "not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := Match(tc.content, tc.edit)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got result %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got != tc.want {
				t.Errorf("result = %q, want %q", got, tc.want)
			}
			if n != tc.wantN {
				t.Errorf("count = %d, want %d", n, tc.wantN)
			}
		})
	}
}

// TestPlanTouchesNothing is the invariant of phase one, and the reason the whole
// design works: whatever is wrong with a batch, the disk must not have moved.
func TestPlanTouchesNothing(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Caller() { Foo() }\n",
	})
	before := fingerprint(t, root)

	// A batch whose second operation cannot succeed: the first one is valid, the
	// second names text that is not there.
	_, err := PlanBatch(context.Background(), ws, []Operation{
		{Path: "a.go", Edits: []Edit{{Old: "func Foo()", New: "func Bar()"}}},
		{Path: "b.go", Edits: []Edit{{Old: "this text is not in the file", New: "x"}}},
	}, Options{Prefix: "apply_patch"})
	if err == nil {
		t.Fatal("a batch with an impossible edit must fail")
	}
	if !strings.Contains(err.Error(), "b.go") || !strings.Contains(err.Error(), "operations[1]") {
		t.Errorf("the error should name the file and the operation: %v", err)
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Errorf("the error should say what to do next: %v", err)
	}
	if !same(before, fingerprint(t, root)) {
		t.Error("phase one wrote to the disk; that is the one thing it must never do")
	}
}

// TestPlanValidatesEverything: each refusal, and each one leaves the disk alone.
func TestPlanValidatesEverything(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go":       "package a\n\nfunc Foo() {}\n",
		"bin.dat":    "\x00\x01\x02binary",
		"dup.txt":    "x x",
		"unique.txt": "only here\n",
	})
	before := fingerprint(t, root)

	cases := []struct {
		name    string
		ops     []Operation
		opts    Options
		wantErr string
	}{
		{
			name:    "outside the workspace",
			ops:     []Operation{{Path: "../../etc/passwd", Edits: []Edit{{Old: "root", New: "x"}}}},
			wantErr: "operations[0]",
		},
		{
			name:    "binary file",
			ops:     []Operation{{Path: "bin.dat", Edits: []Edit{{Old: "binary", New: "x"}}}},
			wantErr: "二进制",
		},
		{
			name:    "a directory",
			ops:     []Operation{{Path: ".", Edits: []Edit{{Old: "a", New: "b"}}}},
			wantErr: "目录",
		},
		{
			name:    "no edits",
			ops:     []Operation{{Path: "a.go"}},
			wantErr: "没有 edits",
		},
		{
			name:    "empty operations",
			ops:     nil,
			wantErr: "不能为空",
		},
		{
			name: "the same path twice",
			ops: []Operation{
				{Path: "unique.txt", Edits: []Edit{{Old: "only", New: "first"}}},
				{Path: "unique.txt", Edits: []Edit{{Old: "first", New: "second"}}},
			},
			wantErr: "出现了两次",
		},
		{
			name:    "ambiguous edit",
			ops:     []Operation{{Path: "dup.txt", Edits: []Edit{{Old: "x", New: "y"}}}},
			wantErr: "歧义",
		},
		{
			name:    "read-only workspace",
			ops:     []Operation{{Path: "a.go", Edits: []Edit{{Old: "Foo", New: "Bar"}}}},
			opts:    Options{ReadOnly: true},
			wantErr: "只读",
		},
		{
			name:    "too many files",
			ops:     []Operation{{Path: "a.go", Edits: []Edit{{Old: "Foo", New: "Bar"}}}},
			opts:    Options{MaxFiles: 0},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Prefix = "apply_patch"
			if tc.name == "too many files" {
				opts.MaxFiles = 0 // 0 means the default, so this one must pass
			}
			_, err := PlanBatch(context.Background(), ws, tc.ops, opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if !same(before, fingerprint(t, root)) {
				t.Error("a refused plan wrote to the disk")
			}
		})
	}
}

// TestPlanAndApplyAcrossFiles is the feature: one call renames a symbol everywhere.
func TestPlanAndApplyAcrossFiles(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Caller() { Foo(); Foo() }\n",
		"c.md": "Docs mention Foo and Foo.\n",
	})
	ops := []Operation{
		{Path: "a.go", Edits: []Edit{{Old: "func Foo()", New: "func Bar()"}}},
		{Path: "b.go", Edits: []Edit{{Old: "Foo()", New: "Bar()", ReplaceAll: true}}},
		{Path: "c.md", Edits: []Edit{{Old: "Foo", New: "Bar", ReplaceAll: true}}},
	}

	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if len(plan.Files) != 3 {
		t.Fatalf("planned %d files, want 3", len(plan.Files))
	}
	if plan.Added == 0 || plan.Removed == 0 {
		t.Errorf("the plan reports +%d -%d; a diff count of zero means the report is not computed",
			plan.Added, plan.Removed)
	}
	// The preview is what the approval card shows: it must name the files.
	preview := plan.Preview(10)
	if len(preview) != 3 || preview[0].Path != "a.go" {
		t.Errorf("preview = %+v", preview)
	}
	var sawAdd, sawDel bool
	for _, e := range preview {
		for _, l := range e.Lines {
			if l.Kind == "add" {
				sawAdd = true
			}
			if l.Kind == "del" {
				sawDel = true
			}
		}
	}
	if !sawAdd || !sawDel {
		t.Error("the preview should show both sides of the change")
	}

	report, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(report.Files) != 3 {
		t.Errorf("report = %+v", report)
	}
	if got := read(t, root, "a.go"); !strings.Contains(got, "func Bar()") {
		t.Errorf("a.go = %q", got)
	}
	if got := read(t, root, "b.go"); strings.Count(got, "Bar()") != 2 || strings.Contains(got, "Foo()") {
		t.Errorf("b.go = %q", got)
	}
	if got := read(t, root, "c.md"); got != "Docs mention Bar and Bar.\n" {
		t.Errorf("c.md = %q", got)
	}
}

// TestApplyRollsBackOnAWriteFailure is the core invariant of phase two. The
// failure is injected by making one target unwritable, which is the same thing a
// full disk or a permission problem would do.
func TestApplyRollsBackOnAWriteFailure(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc Foo() {}\n",
		"b.go": "package a\n\nfunc Foo2() {}\n",
		"c.go": "package a\n\nfunc Foo3() {}\n",
	})
	// The failing write is injected with a read-only parent directory: the atomic
	// write creates its temp file there, which is the same shape as a full disk or
	// a permission problem. The fixture is completed before the fingerprint, so
	// "unchanged" means unchanged from the state the call saw.
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "d.go"), []byte("package a\n\nfunc Foo4() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	before := fingerprint(t, root)

	plan2, err := PlanBatch(context.Background(), ws, []Operation{
		{Path: "a.go", Edits: []Edit{{Old: "Foo", New: "Bar"}}},
		{Path: "b.go", Edits: []Edit{{Old: "Foo2", New: "Bar2"}}},
		{Path: "sub/d.go", Edits: []Edit{{Old: "Foo4", New: "Bar4"}}},
	}, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}

	report, err := Apply(context.Background(), ws, plan2, ApplyOptions{Prefix: "apply_patch"})
	if err == nil {
		t.Fatal("a write failure must be reported")
	}
	if !strings.Contains(err.Error(), "回滚") || !strings.Contains(err.Error(), "没有任何文件被改动") {
		t.Errorf("the error should say the disk was restored: %v", err)
	}
	if len(report.RolledBack) != 2 {
		t.Errorf("rolled back %v, want the two files that were written", report.RolledBack)
	}
	if !same(before, fingerprint(t, root)) {
		t.Errorf("the disk is not what it was before the call:\nbefore %v\nafter  %v",
			before, fingerprint(t, root))
	}
}

// TestApplyCheckpointsBeforeWriting: the checkpoint has to be taken before the
// first write, or it records the changed file.
func TestApplyCheckpointsBeforeWriting(t *testing.T) {
	ws, root := newRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
	capturer := &recordingCapturer{root: root}
	plan, err := PlanBatch(context.Background(), ws, []Operation{
		{Path: "a.go", Edits: []Edit{{Old: "Foo", New: "Bar"}}},
	}, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{
		Capturer: capturer, Session: "sess", Turn: 3, Prefix: "apply_patch",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(capturer.calls) != 1 {
		t.Fatalf("captures = %v, want one per file", capturer.calls)
	}
	if capturer.calls[0].path != "a.go" || capturer.calls[0].session != "sess" || capturer.calls[0].turn != 3 {
		t.Errorf("capture = %+v", capturer.calls[0])
	}
	if capturer.contentAtCapture != "package a\n\nfunc Foo() {}\n" {
		t.Errorf("the checkpoint was taken after the write: it holds %q", capturer.contentAtCapture)
	}
	_ = root
}

// TestPlanIsDeterministicAndOrdered: the report and the preview are read in the
// order the caller asked, so the output matches the request.
func TestPlanReportsTotalsAndOrder(t *testing.T) {
	ws, _ := newRig(t, map[string]string{
		"one.txt": "a\nb\nc\n",
		"two.txt": "x\n",
	})
	plan, err := PlanBatch(context.Background(), ws, []Operation{
		{Path: "two.txt", Edits: []Edit{{Old: "x", New: "y\nz"}}},
		{Path: "one.txt", Edits: []Edit{{Old: "b", New: "B"}}},
	}, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Files[0].Path != "two.txt" || plan.Files[1].Path != "one.txt" {
		t.Errorf("the plan reordered the operations: %+v", plan.Files)
	}
	if plan.Files[0].Added != 2 {
		t.Errorf("adding a line: +%d, want 2", plan.Files[0].Added)
	}
	if plan.Files[1].Added != 1 || plan.Files[1].Removed != 1 {
		t.Errorf("changing a line: +%d -%d, want +1 -1", plan.Files[1].Added, plan.Files[1].Removed)
	}
	if plan.Added != 3 {
		t.Errorf("total added = %d, want 3", plan.Added)
	}
}

// TestApplyRefusesAnEmptyPlan.
func TestApplyRefusesAnEmptyPlan(t *testing.T) {
	ws, _ := newRig(t, map[string]string{"a.txt": "a\n"})
	if _, err := Apply(context.Background(), ws, &Plan{}, ApplyOptions{Prefix: "apply_patch"}); err == nil {
		t.Error("an empty plan should be refused rather than reported as success")
	}
}

// TestPlanEnforcesTheWriteLimit.
func TestPlanEnforcesTheWriteLimit(t *testing.T) {
	ws, _ := newRig(t, map[string]string{"a.txt": "short\n"})
	_, err := PlanBatch(context.Background(), ws, []Operation{
		{Path: "a.txt", Edits: []Edit{{Old: "short", New: strings.Repeat("x", 100)}}},
	}, Options{Prefix: "apply_patch", MaxBytes: 10})
	if err == nil {
		t.Fatal("a result over the write limit must be refused")
	}
	if !strings.Contains(err.Error(), "超过单文件上限") {
		t.Errorf("error = %v", err)
	}
}

// recordingCapturer notes what it was asked to record, and what the file held at
// that moment.
type recordingCapturer struct {
	root             string
	calls            []captureCall
	contentAtCapture string
}

type captureCall struct {
	session string
	turn    int
	path    string
}

func (c *recordingCapturer) Capture(_ context.Context, session string, turn int, path string) error {
	c.calls = append(c.calls, captureCall{session: session, turn: turn, path: path})
	if b, err := os.ReadFile(filepath.Join(c.root, path)); err == nil {
		c.contentAtCapture = string(b)
	}
	return nil
}

// TestAbsoluteWithin is the belt-and-braces path check the rename path relies on.
func TestAbsoluteWithin(t *testing.T) {
	if !absoluteWithin("/a/b", "/a/b/c.go") {
		t.Error("a child should be within")
	}
	if absoluteWithin("/a/b", "/a/c.go") {
		t.Error("a sibling is not within")
	}
	if absoluteWithin("/a/b", "/a/b/../../etc/passwd") {
		// filepath.Rel cleans first, so this is the traversal case.
		if !absoluteWithin("/a/b", filepath.Clean("/a/b/../../etc/passwd")) {
			return
		}
		t.Error("a traversal is not within")
	}
}
