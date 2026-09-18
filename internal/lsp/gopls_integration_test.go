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

// The integration tests run a real language server. They are behind a build tag
// because gopls is a large external binary that most machines do not have, and a
// test that has to be skipped is worse than no test — the protocol and lifecycle
// behaviour is covered by the fake server in client_test.go.
//
// Run them with:
//
//	go test -tags integration ./internal/lsp/ -run Integration -v -timeout 300s
//
// What they are for is the one thing a fake cannot check: that this client
// actually speaks to a real server. Everything else is tested in-process.

// goModuleFixture writes a small Go module with a type error and returns its
// root and the file holding the error.
func goModuleFixture(t *testing.T, broken bool) (root, file string) {
	t.Helper()
	root = t.TempDir()

	files := map[string]string{
		"go.mod": "module fixture\n\ngo 1.21\n",
		"lib.go": `package fixture

// Answer returns a number.
func Answer() int {
	return helper()
}

func helper() int {
	return 41
}
`,
	}
	if broken {
		// helper returns a string, so Answer's return type does not match. This
		// is the error the whole phase is about noticing without a build.
		files["lib.go"] = `package fixture

// Answer returns a number.
func Answer() int {
	return helper()
}

func helper() string {
	return "41"
}
`
	}
	files["use.go"] = `package fixture

// Use calls Answer from another file, so a reference query has two hits.
func Use() int {
	return Answer()
}
`

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root, filepath.Join(root, "lib.go")
}

// newGoplsManager builds a manager over the real gopls, skipping the test when
// it is not installed.
func newGoplsManager(t *testing.T, root string) *Manager {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed; install it with: go install golang.org/x/tools/gopls@latest")
	}
	m := NewManager(ManagerOptions{
		Servers: []ServerSpec{{
			Name:        "gopls",
			Command:     "gopls",
			Args:        []string{"-mode=stdio"},
			Languages:   []string{"go"},
			RootMarkers: []string{"go.mod"},
		}},
		// A whole-module index on a cold cache is slow, and these tests run one
		// server per fixture.
		HandshakeTimeout: 60 * time.Second,
		RequestTimeout:   60 * time.Second,
		Version:          "test",
	})
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestIntegrationDiagnosticsFindsARealTypeError is the claim the phase rests on:
// after a change, the agent can find out it introduced a type error without
// running a build.
func TestIntegrationDiagnosticsFindsARealTypeError(t *testing.T) {
	root, file := goModuleFixture(t, true)
	m := newGoplsManager(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	items, state, err := m.FileDiagnostics(ctx, file, root, 60*time.Second)
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	// The state is a three-way answer, not a bool: "no server covers this file" and
	// "the server has not reported yet" are different facts, and a test that
	// collapsed them would pass against the wrong one.
	if state != Ready {
		t.Fatalf("gopls did not publish diagnostics for a file with a type error (state %v)", state)
	}

	var found bool
	for _, d := range items {
		if d.Severity == SeverityError && strings.Contains(d.Message, "cannot use helper()") {
			found = true
			if d.Range.Start.Line == 0 {
				t.Errorf("the diagnostic is at line 0; a real error should carry a position: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("the type error was not reported; got: %+v", items)
	}
}

// TestIntegrationDiagnosticsClearsAfterTheFix: a fixed error has to stop being
// reported. An empty publish is a real report, and treating it as "nothing
// arrived" leaves the model chasing a problem it already solved.
func TestIntegrationDiagnosticsClearsAfterTheFix(t *testing.T) {
	root, file := goModuleFixture(t, true)
	m := newGoplsManager(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	items, _, err := m.FileDiagnostics(ctx, file, root, 60*time.Second)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected the fixture to start broken")
	}

	// Fix it the way the agent would: rewrite the file.
	fixed := `package fixture

// Answer returns a number.
func Answer() int {
	return helper()
}

func helper() int {
	return 41
}
`
	if err := os.WriteFile(file, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}

	items, state, err := m.FileDiagnostics(ctx, file, root, 60*time.Second)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if state != Ready {
		t.Fatalf("gopls did not republish after the fix (state %v)", state)
	}
	for _, d := range items {
		if d.Severity == SeverityError {
			t.Errorf("a fixed file still reports an error: %+v", d)
		}
	}
}

// TestIntegrationDefinitionAndReferences: the two answers grep cannot give.
func TestIntegrationDefinitionAndReferences(t *testing.T) {
	root, file := goModuleFixture(t, false)
	m := newGoplsManager(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client, err := m.ClientFor(ctx, file, root)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if client == nil {
		t.Fatal("no client for a .go file with gopls configured")
	}

	// lib.go line 5 is `return helper()`; column 9 is inside the identifier.
	defs, err := client.Definition(ctx, file, LineCol{Line: 5, Column: 9})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("no definition found for helper()")
	}
	path, uerr := URIToPath(defs[0].URI)
	if uerr != nil {
		t.Fatalf("URIToPath(%q): %v", defs[0].URI, uerr)
	}
	if filepath.Base(path) != "lib.go" {
		t.Errorf("definition is in %s, want lib.go", path)
	}
	// lib.go is: package(1) blank(2) comment(3) func Answer(4) return(5) }(6)
	// blank(7) func helper(8). gopls answers with 0-based line 7, and the
	// conversion has to give the 1-based line a reader of read_file would count.
	if got := defs[0].Range.Start.Line + 1; got != 8 {
		t.Errorf("definition line = %d, want 8 (the func helper declaration)", got)
	}

	// References to Answer should include the definition and the call in use.go.
	refs, err := client.References(ctx, file, LineCol{Line: 4, Column: 7}, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) < 2 {
		t.Fatalf("references = %d, want at least the definition and the call site (%+v)", len(refs), refs)
	}
	var sawUse bool
	for _, r := range refs {
		if p, err := URIToPath(r.URI); err == nil && filepath.Base(p) == "use.go" {
			sawUse = true
		}
	}
	if !sawUse {
		t.Errorf("the call site in use.go was not among the references: %+v", refs)
	}
}

// TestIntegrationWorkspaceSymbols: the "which code handles X" question.
func TestIntegrationWorkspaceSymbols(t *testing.T) {
	root, file := goModuleFixture(t, false)
	m := newGoplsManager(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client, err := m.ClientFor(ctx, file, root)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	syms, err := client.WorkspaceSymbols(ctx, "Answer")
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("no symbol named Answer was found")
	}
	if syms[0].Name != "Answer" {
		t.Errorf("first symbol = %q, want Answer", syms[0].Name)
	}
	if syms[0].Kind == 0 {
		t.Errorf("the symbol has no kind: %+v", syms[0])
	}
}

// TestIntegrationServerIsReapedAndLeavesNothingRunning: a language server is a
// child process, and one left behind holds locks on the module cache.
func TestIntegrationServerIsReapedAndLeavesNothingRunning(t *testing.T) {
	root, file := goModuleFixture(t, false)
	m := newGoplsManager(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, _, err := m.FileDiagnostics(ctx, file, root, 60*time.Second); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	m.mu.Lock()
	n := len(m.instances)
	var pid int
	for _, inst := range m.instances {
		if inst.cmd != nil && inst.cmd.Process != nil {
			pid = inst.cmd.Process.Pid
		}
	}
	m.mu.Unlock()
	if n == 0 {
		t.Fatal("no language server is running after a query")
	}
	if !processAlive(pid) {
		t.Fatalf("recorded pid %d is not alive", pid)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The process group must actually be gone, not merely forgotten.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("language server pid %d is still alive after Close", pid)
}
