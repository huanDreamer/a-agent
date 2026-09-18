//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/huan/huan-agent/internal/config"
	agenttool "github.com/huan/huan-agent/internal/tool"
)

// The end-to-end form of the edit-feedback claim: after the agent changes a file,
// the result it gets back says whether the change compiles — without it running a
// build.
//
// It goes through the real registration path (so the decorator is applied where
// production applies it), the real workspace sandbox, the real edit_file tool and
// a real gopls. Everything else about this feature is tested in-process; this is
// the one test that can fail because the pieces were never connected.
//
//	go test -tags integration ./cmd/huan-agent/ -run IntegrationEditFeedback -v
func TestIntegrationEditFeedbackReportsTypeError(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed; install it with: go install golang.org/x/tools/gopls@latest")
	}
	resetLanguageServers(t)

	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module feedback\n\ngo 1.21\n",
		"lib.go": `package feedback

// Total returns the sum.
func Total() int {
	return value()
}

func value() int {
	return 41
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	cfg.Tools.Workspace = dir
	cfg.Tools.EnableBash = false // nothing here should need a shell
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.AttachDiagnostics = true

	reg := newRunRegistry(t, cfg)
	tool, ok := reg.Get("edit_file")
	if !ok {
		t.Fatal("edit_file is not registered")
	}

	// Break the file the way a model would: change the producer's return type
	// without changing its caller.
	args := `{"path":"lib.go","old_string":"func value() int {","new_string":"func value() string {"}`
	out, err := invokeTool(t, tool, args)
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}

	if !strings.Contains(out, "诊断") {
		t.Fatalf("the edit result carries no diagnostics; the decorator is not in the path.\nresult:\n%s", out)
	}
	if !strings.Contains(out, "cannot use value()") {
		t.Errorf("the type error the edit introduced is not in the result:\n%s", out)
	}
	if !strings.Contains(out, "lib.go:5:9") {
		t.Errorf("the diagnostic is not rendered as file:line:col:\n%s", out)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("the severity is not named:\n%s", out)
	}

	// And the tool's own result must survive untouched underneath.
	if !strings.HasPrefix(out, "{") {
		t.Errorf("the tool's JSON result was replaced rather than appended to:\n%s", out)
	}
}

// TestIntegrationEditFeedbackSaysCleanAfterFix: the other half. A model that
// fixes the error has to be told so, or it keeps looking.
func TestIntegrationEditFeedbackSaysCleanAfterFix(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed")
	}
	resetLanguageServers(t)

	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "go.mod"), "module clean\n\ngo 1.21\n")
	writeFileT(t, filepath.Join(dir, "lib.go"), `package clean

func Total() int {
	return value()
}

func value() string {
	return "41"
}
`)

	cfg := &config.Config{}
	cfg.Tools.Workspace = dir
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.AttachDiagnostics = true

	reg := newRunRegistry(t, cfg)
	tool, _ := reg.Get("edit_file")

	// The whole function has to change, not just its signature: a body that
	// still returns a string is still a type error, and the first version of this
	// test asserted that a half-fix was clean.
	args := `{"path":"lib.go",` +
		`"old_string":"func value() string {\n\treturn \"41\"\n}",` +
		`"new_string":"func value() int {\n\treturn 41\n}"}`
	out, err := invokeTool(t, tool, args)
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if strings.Contains(out, "error") {
		t.Fatalf("the fix did not take, so this test proves nothing:\n%s", out)
	}
	if !strings.Contains(out, "诊断（该文件）：无") {
		t.Errorf("the fixed file should be reported clean:\n%s", out)
	}
}

// TestIntegrationEditFeedbackIsAbsentWithoutAServer: with the capability off, the
// editing tools must return exactly what they returned before this feature
// existed — down to the byte, so an unrelated deployment pays nothing.
func TestIntegrationEditFeedbackIsAbsentWithoutAServer(t *testing.T) {
	resetLanguageServers(t)

	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "go.mod"), "module off\n\ngo 1.21\n")
	writeFileT(t, filepath.Join(dir, "lib.go"), "package off\n\nvar X = 1\n")

	cfg := &config.Config{}
	cfg.Tools.Workspace = dir
	cfg.Tools.LSP.Enable = false

	reg := newRunRegistry(t, cfg)
	tool, ok := reg.Get("edit_file")
	if !ok {
		t.Fatal("edit_file is not registered")
	}
	out, err := invokeTool(t, tool, `{"path":"lib.go","old_string":"var X = 1","new_string":"var X = 2"}`)
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if strings.Contains(out, "诊断") {
		t.Errorf("with tools.lsp.enable=false there must be no diagnostics section:\n%s", out)
	}
	if _, ok := reg.Get("diagnostics"); ok {
		t.Error("the code-intelligence tools must not be registered when the capability is off")
	}
}

// --- helpers ---

// newRunRegistry builds the registry a CLI turn runs with, over a real store and
// the configured workspace, so the test exercises the same registration path
// production does.
func newRunRegistry(t *testing.T, cfg *config.Config) *agenttool.Registry {
	t.Helper()
	reg := agenttool.NewRegistry()
	st := openTempStore(t)
	// A real logger, not a no-op: when the decorator swallows a language-server
	// failure it logs it, and a test that cannot see that log cannot tell "no
	// diagnostics" from "the server would not start".
	if err := registerBuiltinTools(reg, cfg, st, zaptest.NewLogger(t), nil,
		toolSetOptions{Surface: "cli"}); err != nil {
		t.Fatalf("registerBuiltinTools: %v", err)
	}
	return reg
}

func invokeTool(t *testing.T, tl agenttool.Tool, args string) (string, error) {
	t.Helper()
	return tl.InvokableRun(context.Background(), args)
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
