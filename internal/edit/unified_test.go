package edit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A diff is a positional description, and this parser deliberately does not trust
// the positions: it locates each hunk by its context lines. These tests are about
// that decision — a diff whose numbers are stale still applies, and a diff whose
// context does not match is refused rather than applied in the wrong place.

// gitDiff is real `git diff` output, including the headers a hand-written diff
// usually omits.
const gitDiff = `diff --git a/calc.go b/calc.go
index 1234567..89abcde 100644
--- a/calc.go
+++ b/calc.go
@@ -1,7 +1,7 @@
 package calc
 
 // Add sums.
-func Add(a, b int) int {
+func Sum(a, b int) int {
 	return a + b
 }
 
`

func TestParseUnifiedFromGit(t *testing.T) {
	ops, warnings, err := ParseUnifiedWithWarnings(gitDiff)
	if err != nil {
		t.Fatalf("ParseUnified: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a well-formed diff should produce no warnings: %v", warnings)
	}
	if len(ops) != 1 || ops[0].Path != "calc.go" {
		t.Fatalf("operations = %+v", ops)
	}
	if len(ops[0].Edits) != 1 {
		t.Fatalf("edits = %+v", ops[0].Edits)
	}
	e := ops[0].Edits[0]
	// The old side is the context and the removed line; the new side is the context
	// and the added line. That is what makes the hunk findable.
	if !strings.Contains(e.Old, "func Add(a, b int) int {") || !strings.Contains(e.Old, "package calc") {
		t.Errorf("old side = %q", e.Old)
	}
	if !strings.Contains(e.New, "func Sum(a, b int) int {") || strings.Contains(e.New, "func Add") {
		t.Errorf("new side = %q", e.New)
	}
}

// TestApplyUnifiedEndToEnd: the parsed diff runs through the same applier.
func TestApplyUnifiedEndToEnd(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"calc.go": "package calc\n\n// Add sums.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\n",
		"use.go":  "package calc\n\nfunc Use() int { return Add(1, 2) }\n",
	})
	diff := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -3,4 +3,4 @@
 // Add sums.
-func Add(a, b int) int {
+func Sum(a, b int) int {
 	return a + b
 }
diff --git a/use.go b/use.go
--- a/use.go
+++ b/use.go
@@ -3,1 +3,1 @@
-func Use() int { return Add(1, 2) }
+func Use() int { return Sum(1, 2) }
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "apply_patch"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, root, "calc.go"); !strings.Contains(got, "func Sum(") {
		t.Errorf("calc.go = %q", got)
	}
	if got := read(t, root, "use.go"); !strings.Contains(got, "Sum(1, 2)") {
		t.Errorf("use.go = %q", got)
	}
}

// TestParseUnifiedIgnoresStaleLineNumbers: the numbers are a hint, the context is
// what locates the hunk. A diff produced before someone added two lines above must
// still apply.
func TestParseUnifiedIgnoresStaleLineNumbers(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		// Two lines more than the diff believes are above the change.
		"a.go": "package a\n\n// extra one\n// extra two\n\nfunc Foo() {}\n",
	})
	// Claims the change is at line 3; it is really at line 6.
	diff := `--- a/a.go
+++ b/a.go
@@ -3,1 +3,1 @@
-func Foo() {}
+func Bar() {}
`
	ops, warnings, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		// The declared count agrees with the body (one line); only the *start* line
		// is stale, and the start line is not something the parser uses at all.
		t.Logf("warnings: %v", warnings)
	}
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("a stale line number must not stop the hunk matching by context: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "apply_patch"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "a.go"); !strings.Contains(got, "func Bar() {}") || !strings.Contains(got, "// extra one") {
		t.Errorf("a.go = %q", got)
	}
}

// TestParseUnifiedWarnsAboutWrongCounts: a wrong count is worth saying out loud,
// but it is not a reason to refuse a diff whose context matches.
func TestParseUnifiedWarnsAboutWrongCounts(t *testing.T) {
	diff := `--- a/a.go
+++ b/a.go
@@ -1,9 +1,9 @@
 package a
-func Foo() {}
+func Bar() {}
`
	_, warnings, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("a wrong count must not be fatal: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("a mismatch between the declared counts and the body should be reported")
	}
	if !strings.Contains(warnings[0], "提示") {
		t.Errorf("the warning should say which one decides: %v", warnings)
	}
}

// TestParseUnifiedRefusesAMismatchedContext: the other half of locating by context.
func TestParseUnifiedRefusesAMismatchedContext(t *testing.T) {
	ws, _ := newRig(t, map[string]string{"a.go": "package a\n\nfunc SomethingElse() {}\n"})
	diff := `--- a/a.go
+++ b/a.go
@@ -1,3 +1,3 @@
 package a
 
-func Foo() {}
+func Bar() {}
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The parser succeeds — the diff is well-formed — and the *plan* refuses,
	// because the context block is not in this file. That division is deliberate:
	// the plan is where the files are known.
	if _, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"}); err == nil {
		t.Fatal("a hunk whose context does not match must be refused")
	} else if !strings.Contains(err.Error(), "read_file") {
		t.Errorf("the refusal should say what to do: %v", err)
	}
}

// TestParseUnifiedRefusesCreationAndDeletion: this stage cannot create or delete,
// and the message names the alternative.
func TestParseUnifiedRefusesCreationAndDeletion(t *testing.T) {
	cases := []struct {
		name    string
		diff    string
		wantErr string
	}{
		{
			name: "new file",
			diff: `diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package a
+`,
			wantErr: "write_file",
		},
		{
			name: "deleted file",
			diff: `diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package a
-`,
			wantErr: "删除",
		},
		{
			name: "renamed file",
			diff: `diff --git a/old.go b/new.go
rename from old.go
rename to new.go
`,
			wantErr: "重命名",
		},
		{
			name: "path changed without a rename header",
			diff: `--- a/old.go
+++ b/new.go
@@ -1,1 +1,1 @@
-package a
+package b
`,
			wantErr: "路径",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUnified(tc.diff)
			if err == nil {
				t.Fatal("this diff must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestParseUnifiedMultipleHunks: several hunks in one file become several edits, in
// the order they appear.
func TestParseUnifiedMultipleHunks(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc One() {}\n\nfunc Two() {}\n\nfunc Three() {}\n",
	})
	diff := `--- a/a.go
+++ b/a.go
@@ -3,1 +3,1 @@
-func One() {}
+func OneX() {}
@@ -7,1 +7,1 @@
-func Three() {}
+func ThreeX() {}
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ops[0].Edits) != 2 {
		t.Fatalf("edits = %+v", ops[0].Edits)
	}
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "apply_patch"}); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, "a.go")
	if !strings.Contains(got, "OneX") || !strings.Contains(got, "ThreeX") || !strings.Contains(got, "func Two() {}") {
		t.Errorf("a.go = %q", got)
	}
}

// TestParseUnifiedNoNewlineMarker: `\ No newline at end of file` is a marker about
// the preceding line, not content.
func TestParseUnifiedNoNewlineMarker(t *testing.T) {
	ws, root := newRig(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}"})
	diff := `--- a/a.go
+++ b/a.go
@@ -3,1 +3,1 @@
-func Foo() {}
+func Bar() {}
\ No newline at end of file
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range ops[0].Edits {
		if strings.Contains(e.Old, `\ No newline`) || strings.Contains(e.New, `\ No newline`) {
			t.Errorf("the marker leaked into the text: %+v", e)
		}
	}
	plan, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"})
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := Apply(context.Background(), ws, plan, ApplyOptions{Prefix: "apply_patch"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "a.go"); got != "package a\n\nfunc Bar() {}" {
		t.Errorf("a.go = %q", got)
	}
}

// TestParseUnifiedRefusesATraversalPath: the sandbox is what finally decides, but a
// path that tries to leave must be refused by the plan rather than silently
// skipped.
func TestParseUnifiedRefusesATraversalPath(t *testing.T) {
	ws, _ := newRig(t, map[string]string{"a.go": "package a\n"})
	diff := `--- a/../../etc/passwd
+++ b/../../etc/passwd
@@ -1,1 +1,1 @@
-root:x:0:0
+root:x:1:1
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		// Refusing at parse time is also acceptable; what must not happen is
		// applying it.
		return
	}
	if _, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"}); err == nil {
		t.Fatal("a path outside the workspace must be refused")
	}
}

// TestParseUnifiedRejectsNonsense: a model that pastes something that is not a diff
// gets told so, rather than a silent no-op.
func TestParseUnifiedRejectsNonsense(t *testing.T) {
	cases := map[string]string{
		"prose":                 "我改了 calc.go 里的函数名，请应用。",
		"an empty diff":         "",
		"a lone hunk":           "@@ -1,1 +1,1 @@\n-a\n+b\n",
		"a bad hunk":            "--- a/a.go\n+++ b/a.go\n@@ nonsense @@\n-a\n+b\n",
		"a stray line":          "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n?a\n+b\n",
		"adds only, no context": "--- a/a.go\n+++ b/a.go\n@@ -0,0 +1,1 @@\n+package a\n",
	}
	for name, diff := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseUnified(diff); err == nil {
				t.Error("this input must be refused rather than treated as no changes")
			}
		})
	}
}

// TestApplyUnifiedIsAllOrNothing: the diff form inherits the batch guarantee, which
// is the reason it is parsed into the same operations rather than applied directly.
func TestApplyUnifiedIsAllOrNothing(t *testing.T) {
	ws, root := newRig(t, map[string]string{
		"a.go": "package a\n\nfunc One() {}\n",
		"b.go": "package a\n\nfunc Two() {}\n",
	})
	before := fingerprint(t, root)

	// The second file's hunk does not match anything in it.
	diff := `--- a/a.go
+++ b/a.go
@@ -3,1 +3,1 @@
-func One() {}
+func OneX() {}
--- a/b.go
+++ b/b.go
@@ -3,1 +3,1 @@
-func Missing() {}
+func MissingX() {}
`
	ops, _, err := ParseUnifiedWithWarnings(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := PlanBatch(context.Background(), ws, ops, Options{Prefix: "apply_patch"}); err == nil {
		t.Fatal("the batch must be refused as a whole")
	}
	if !same(before, fingerprint(t, root)) {
		t.Error("a refused diff changed the disk")
	}
	_ = os.Getpid
	_ = filepath.Join
}
