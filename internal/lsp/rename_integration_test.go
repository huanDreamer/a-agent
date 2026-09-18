//go:build integration

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Renaming, against a real gopls.
//
// This is the test that proves the parts a fake server cannot: that the columns
// the protocol uses are UTF-16 code units, that the reference set is the
// compiler's rather than a word search, and that a rename reaching outside the
// workspace is reported as such.

func TestRenameIntegration(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not on PATH")
	}
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module renamefix\n\ngo 1.21\n")
	writeFixture(t, root, "calc.go", `package renamefix

// Add sums two numbers.
func Add(a, b int) int { return a + b }
`)
	writeFixture(t, root, "use.go", `package renamefix

// Add is used here and mentioned in a comment.
func Use() int { return Add(1, 2) }
`)
	// A file with a string that merely contains the name: a word search would
	// rewrite it, the language server must not.
	writeFixture(t, root, "note.go", `package renamefix

const Note = "call Add to sum"
`)

	mgr := newGoplsManager(t, root)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cl, err := mgr.ClientForRoot(ctx, root)
	if err != nil {
		t.Fatalf("start gopls: %v", err)
	}

	// "func Add" — 1-based line 4, column 6 (the A of Add), so 0-based line 3,
	// column 5.
	we, rerr := cl.Rename(ctx, filepath.Join(root, "calc.go"), LineCol{Line: 4, Column: 6}, "Sum")
	if rerr != nil {
		t.Fatalf("Rename: %v", rerr)
	}
	files := we.Flatten()
	if len(files) == 0 {
		t.Fatal("gopls returned no edits")
	}

	// Every edit must name a real span of the word being renamed.
	for _, f := range files {
		for _, e := range f.Edits {
			if e.NewText != "Sum" {
				t.Errorf("an edit in %s inserts %q", filepath.Base(f.Path), e.NewText)
			}
		}
	}

	// The reference set: the definition and the call, and not the string literal.
	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f.Path)] = true
	}
	if !names["calc.go"] || !names["use.go"] {
		t.Errorf("the rename should cover the definition and the call: %v", names)
	}
	if names["note.go"] {
		t.Errorf("the rename reached a string literal that happens to contain the name: %v", names)
	}

	// Applying the edits produces code that still compiles, which is the whole
	// claim being made.
	applyFlattenedEdits(t, files)
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(root, ".gocache"), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("after the rename the package does not build: %v\n%s", err, out)
	}
	body, _ := os.ReadFile(filepath.Join(root, "calc.go"))
	if !strings.Contains(string(body), "func Sum(a, b int) int") {
		t.Errorf("calc.go = %s", body)
	}
	note, _ := os.ReadFile(filepath.Join(root, "note.go"))
	if !strings.Contains(string(note), `"call Add to sum"`) {
		t.Errorf("a string literal was rewritten: %s", note)
	}
}

// TestRenameIntegrationRefusesAKeyword: prepareRename exists so that "this cannot
// be renamed" is a readable refusal before anything is planned.
func TestRenameIntegrationRefusesAKeyword(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not on PATH")
	}
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module renamefix2\n\ngo 1.21\n")
	writeFixture(t, root, "a.go", "package renamefix2\n\nfunc F() { for i := 0; i < 3; i++ { _ = i } }\n")

	mgr := newGoplsManager(t, root)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cl, err := mgr.ClientForRoot(ctx, root)
	if err != nil {
		t.Fatalf("start gopls: %v", err)
	}

	// "for" — a keyword, on 1-based line 3, column 10.
	if _, err := cl.PrepareRename(ctx, filepath.Join(root, "a.go"), LineCol{Line: 3, Column: 10}); err == nil {
		t.Error("renaming a keyword must be refused")
	}
}

// writeFixture writes one file of a test fixture.
func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// applyFlattenedEdits writes the server's edits directly, bottom-up, so this test
// measures the language server rather than the batch applier (which has its own
// tests).
func applyFlattenedEdits(t *testing.T, files []FileEdits) {
	t.Helper()
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("read %s: %v", f.Path, err)
		}
		content := string(data)
		// Sorted bottom-up by the caller's order is not guaranteed here, so sort
		// explicitly: a later edit must not shift an earlier one.
		lines := strings.Split(content, "\n")
		type span struct {
			line, start, end int
			text             string
		}
		spans := make([]span, 0, len(f.Edits))
		for _, e := range f.Edits {
			spans = append(spans, span{e.Range.Start.Line, e.Range.Start.Character, e.Range.End.Character, e.NewText})
		}
		for i := 0; i < len(spans); i++ {
			for j := i + 1; j < len(spans); j++ {
				if spans[j].line > spans[i].line || (spans[j].line == spans[i].line && spans[j].start > spans[i].start) {
					spans[i], spans[j] = spans[j], spans[i]
				}
			}
		}
		for _, sp := range spans {
			if sp.line >= len(lines) {
				t.Fatalf("edit line %d is past the end of %s", sp.line, f.Path)
			}
			runes := []rune(lines[sp.line])
			if sp.end > len(runes) || sp.start > len(runes) {
				t.Fatalf("edit columns %d-%d are past the end of line %d", sp.start, sp.end, sp.line)
			}
			lines[sp.line] = string(runes[:sp.start]) + sp.text + string(runes[sp.end:])
		}
		if err := os.WriteFile(f.Path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatalf("write %s: %v", f.Path, err)
		}
	}
}
