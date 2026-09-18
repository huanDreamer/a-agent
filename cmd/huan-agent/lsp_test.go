package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	agenttool "github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// resetLanguageServers drops the process-wide manager between tests.
//
// The manager is a singleton in production, which is correct — one language
// server indexes a module and every workspace shares it — but it means a test
// process, which runs many configurations, has to be able to start over. The
// reset lives here rather than in lsp.go so the production file has no test-only
// surface.
func resetLanguageServers(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		closeLanguageServers()
		lspOnce = sync.Once{}
		lspManager = nil
	})
}

func codeIntelTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.New(dir, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// TestCodeIntelligenceOffRegistersNothing: with the capability off, the tool
// list must be exactly what it was before the feature existed.
func TestCodeIntelligenceOffRegistersNothing(t *testing.T) {
	resetLanguageServers(t)
	cfg := &config.Config{}
	cfg.Tools.LSP.Enable = false

	tools, diagnoser := codeIntelligenceFor(cfg, codeIntelTestWorkspace(t), zap.NewNop())
	if len(tools) != 0 {
		t.Errorf("tools = %d, want none with tools.lsp.enable=false", len(tools))
	}
	if diagnoser != nil {
		t.Error("the edit-feedback decorator must not be installed when the capability is off")
	}
}

// TestCodeIntelligenceAbsentWhenServerIsNotInstalled is the degradation rule
// stated in the spec: four tools whose every call fails with "not installed" are
// worse than no tools at all.
func TestCodeIntelligenceAbsentWhenServerIsNotInstalled(t *testing.T) {
	resetLanguageServers(t)
	cfg := &config.Config{}
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.Servers = []config.LSPServerConfig{{
		Name:      "missing",
		Command:   "huan-agent-no-such-language-server",
		Languages: []string{"go"},
	}}

	tools, diagnoser := codeIntelligenceFor(cfg, codeIntelTestWorkspace(t), zap.NewNop())
	if len(tools) != 0 {
		t.Errorf("tools = %d, want none: the binary is not on PATH", len(tools))
	}
	if diagnoser != nil {
		t.Error("diagnostics must not be attached when no server can run")
	}
}

// TestCodeIntelligenceRegisteredWhenServerIsAvailable: the four tools appear, all
// read-only, and the diagnoser is offered when diagnostics are turned on.
//
// The server command is "go" rather than gopls: this test is about the
// registration decision, which is a LookPath check, and it must pass on a machine
// without gopls installed.
func TestCodeIntelligenceRegisteredWhenServerIsAvailable(t *testing.T) {
	resetLanguageServers(t)
	cfg := &config.Config{}
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.AttachDiagnostics = true
	cfg.Tools.LSP.Servers = []config.LSPServerConfig{{
		Name:        "stub",
		Command:     "go",
		Args:        []string{"version"},
		Languages:   []string{"go"},
		RootMarkers: []string{"go.mod"},
	}}

	tools, diagnoser := codeIntelligenceFor(cfg, codeIntelTestWorkspace(t), zap.NewNop())
	if len(tools) != 4 {
		t.Fatalf("tools = %d, want the four code-intelligence tools", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		info, err := tl.Info(t.Context())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		names[info.Name] = true
		if got := string(agenttool.CapabilityOf(tl)); got != "read" {
			t.Errorf("%s is %q, want read: it observes and never writes", info.Name, got)
		}
		if strings.TrimSpace(info.Desc) == "" {
			t.Errorf("%s has no description; a model will not call a tool it cannot understand", info.Name)
		}
	}
	for _, want := range []string{
		"diagnostics", "goto_definition", "find_references", "workspace_symbols",
	} {
		if !names[want] {
			t.Errorf("%s was not registered (got %v)", want, names)
		}
	}
	if diagnoser == nil {
		t.Fatal("attach_diagnostics is on, so the decorator must be offered")
	}
	// It must be the workspace-bound adapter, not a bare manager: the manager
	// does not know the boundary a project search may not climb above.
	if _, ok := diagnoser.(workspaceLSP); !ok {
		t.Errorf("diagnoser = %T, want workspaceLSP", diagnoser)
	}
}

// TestCodeIntelligenceWithoutDiagnosticsStillRegistersTools: the two halves are
// independent. Turning off the feedback must not remove the queries.
func TestCodeIntelligenceWithoutDiagnosticsStillRegistersTools(t *testing.T) {
	resetLanguageServers(t)
	cfg := &config.Config{}
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.AttachDiagnostics = false
	cfg.Tools.LSP.Servers = []config.LSPServerConfig{{
		Name: "stub", Command: "go", Languages: []string{"go"},
	}}

	tools, diagnoser := codeIntelligenceFor(cfg, codeIntelTestWorkspace(t), zap.NewNop())
	if len(tools) != 4 {
		t.Errorf("tools = %d, want 4", len(tools))
	}
	if diagnoser != nil {
		t.Error("attach_diagnostics=false must not install the decorator")
	}
}

// TestFeedbackOptionsCarryTheWorkspaceBoundary: the decorator resolves paths
// inside the sandbox and may not let a project search climb out of it.
func TestFeedbackOptionsCarryTheWorkspaceBoundary(t *testing.T) {
	cfg := &config.Config{}
	ws := codeIntelTestWorkspace(t)
	opts := feedbackOptions(cfg, ws)

	if opts.Workspace != ws {
		t.Error("the decorator must resolve paths through the workspace")
	}
	if opts.Ceiling != ws.Root() {
		t.Errorf("ceiling = %q, want the workspace root %q", opts.Ceiling, ws.Root())
	}
	if opts.Max <= 0 || opts.Wait <= 0 {
		t.Errorf("the decorator needs a bounded cap and wait: %+v", opts)
	}
}

// TestWorkspaceLSPReturnsNilClientRatherThanTypedNil guards the classic Go trap:
// a nil *lsp.Client stored in an interface is not nil, and the caller's "no
// server covers this file" branch would never be taken.
func TestWorkspaceLSPReturnsNilClientRatherThanTypedNil(t *testing.T) {
	resetLanguageServers(t)
	cfg := &config.Config{}
	cfg.Tools.LSP.Enable = true
	cfg.Tools.LSP.Servers = []config.LSPServerConfig{{
		Name: "gopls-like", Command: "go", Languages: []string{"go"},
	}}
	mgr := languageServers(cfg, zap.NewNop())

	ws := codeIntelTestWorkspace(t)
	dir := ws.Root()
	// A file type no server covers.
	path := filepath.Join(dir, "notes.md")

	src := workspaceLSP{mgr: mgr, ws: ws}
	client, err := src.ClientFor(t.Context(), path)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if client != nil {
		t.Errorf("client = %#v, want an untyped nil so the caller can detect it", client)
	}
	if src.Covers(path) {
		t.Error("a .md file is not covered by a Go-only server")
	}
}
