package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/lsp"
	"github.com/huan/huan-agent/internal/workspace"
)

// A scripted language-server session. Every answer the tools have to handle —
// a definition, an empty reference list, a file no server covers, a server that
// has not caught up — is a field on this, so the tools can be tested on a machine
// with no language server installed. That is most machines.

type fakeClient struct {
	root       string
	defs       []lsp.Location
	refs       []lsp.Location
	symbols    []lsp.Symbol
	diags      map[string][]lsp.Diagnostic
	settled    bool
	openErr    error
	defErr     error
	refErr     error
	symbolErr  error
	opened     []string
	lastRefInc bool
}

func (f *fakeClient) Open(_ context.Context, path string) error {
	f.opened = append(f.opened, path)
	return f.openErr
}

func (f *fakeClient) Definition(context.Context, string, lsp.LineCol) ([]lsp.Location, error) {
	return f.defs, f.defErr
}

func (f *fakeClient) References(_ context.Context, _ string, _ lsp.LineCol, includeDeclaration bool) ([]lsp.Location, error) {
	f.lastRefInc = includeDeclaration
	return f.refs, f.refErr
}

func (f *fakeClient) WorkspaceSymbols(context.Context, string) ([]lsp.Symbol, error) {
	return f.symbols, f.symbolErr
}

func (f *fakeClient) WaitDiagnostics(_ context.Context, path string, _ time.Duration) ([]lsp.Diagnostic, bool) {
	return f.diags[path], f.settled
}

func (f *fakeClient) Root() string { return f.root }

// fakeSource hands out one scripted client.
//
// coversPath decides which files the source claims to handle, which is what the
// workspace-wide sweep filters on.
type fakeSource struct {
	client     CodeIntelClient
	coversPath func(string) bool
	err        error
}

func (f *fakeSource) ClientFor(context.Context, string) (CodeIntelClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.client, nil
}

func (f *fakeSource) ClientForRoot(context.Context, string) (CodeIntelClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.client, nil
}

func (f *fakeSource) Covers(path string) bool {
	if f.coversPath == nil {
		return false
	}
	return f.coversPath(path)
}

// codeIntelRig is a workspace with two Go files and a file no server covers.
//
// Its paths are the ones a tool will actually use — resolved through the
// workspace — because Workspace.Resolve normalises symlinks (on macOS /var is
// /private/var), and a test that keys its fixtures by the unresolved path would
// silently match nothing.
type codeIntelRig struct {
	opts CodeIntelOptions
	ws   *workspace.Workspace
}

// path returns the resolved absolute path of a fixture, as a tool would see it.
func (r codeIntelRig) path(rel string) string {
	abs, err := r.ws.Resolve(rel)
	if err != nil {
		panic("fixture path " + rel + ": " + err.Error())
	}
	return abs
}

func newCodeIntelRig(t *testing.T, client CodeIntelClient, goOnly bool) (codeIntelRig, CodeIntelOptions) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"internal/a.go": "package internal\n\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n",
		"internal/b.go": "package internal\n\nfunc Baz() { Foo() }\n",
		"notes.md":      "# notes\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := workspace.New(dir, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	covers := func(string) bool { return true }
	if goOnly {
		covers = func(p string) bool { return strings.HasSuffix(p, ".go") }
	}
	opts := CodeIntelOptions{
		Workspace: ws,
		Source:    &fakeSource{client: client, coversPath: covers},
	}
	return codeIntelRig{opts: opts, ws: ws}, opts
}

// TestDiagnosticsRendersUsableLines is the point of the tool: after an edit, the
// model has to be able to act on the answer without decoding anything.
func TestDiagnosticsRendersUsableLines(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.diags = map[string][]lsp.Diagnostic{
		rig.path("internal/a.go"): {
			{Severity: lsp.SeverityError, Message: "undefined: BAZ", Source: "compiler",
				Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 8}}},
			{Severity: lsp.SeverityInformation, Message: "noise", // filtered at warning floor
				Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}}},
		},
	}

	tl, err := NewDiagnosticsTool(opts)
	if err != nil {
		t.Fatalf("NewDiagnosticsTool: %v", err)
	}
	out := invoke(t, tl, `{"path":"internal/a.go"}`)

	var got DiagnosticsOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the tool result is not JSON: %v\n%s", err, out)
	}
	if got.Count != 1 {
		t.Errorf("count = %d, want 1 (info is below the default floor)", got.Count)
	}
	if !strings.Contains(got.Summary, "internal/a.go:3:9  error  undefined: BAZ  (compiler)") {
		t.Errorf("summary is not in file:line:col form:\n%s", got.Summary)
	}
	if !got.Ready {
		t.Error("a settled answer must report ready")
	}
	// The server is asked about the file as it is on disk.
	if len(client.opened) != 1 {
		t.Errorf("the file was not opened before asking: %v", client.opened)
	}
}

// TestDiagnosticsSaysSoWhenThereIsNothing: an empty answer that renders as empty
// text is indistinguishable from a tool that did not run.
func TestDiagnosticsSaysSoWhenThereIsNothing(t *testing.T) {
	client := &fakeClient{settled: true, diags: map[string][]lsp.Diagnostic{}}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewDiagnosticsTool(opts)
	var got DiagnosticsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/a.go"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Summary, "无诊断") {
		t.Errorf("summary = %q, want it to say the file is clean", got.Summary)
	}
}

// TestDiagnosticsReportsNotReady: a server that has not caught up must not be
// presented as "clean", which is the most dangerous wrong answer this tool can
// give.
func TestDiagnosticsReportsNotReady(t *testing.T) {
	client := &fakeClient{settled: false, diags: map[string][]lsp.Diagnostic{}}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewDiagnosticsTool(opts)
	var got DiagnosticsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/a.go"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Ready {
		t.Error("ready must be false when the server has not caught up")
	}
	if !strings.Contains(got.Summary, "尚未返回诊断") && !strings.Contains(got.Summary, "不完整") {
		t.Errorf("summary must not read as clean:\n%s", got.Summary)
	}
}

// TestDiagnosticsWithoutAServer: the tool can exist while the server for that
// file type does not (a .md file with only gopls configured).
func TestDiagnosticsWithoutAServer(t *testing.T) {
	client := &fakeClient{settled: true}
	_, opts := newCodeIntelRig(t, client, false)
	// "No server covers this file" is what the manager reports by returning a nil
	// client rather than an error.
	opts.Source = &fakeSource{client: nil}

	tl, _ := NewDiagnosticsTool(opts)
	var got DiagnosticsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"notes.md"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Summary, "没有可用的语言服务器") {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
}

// TestDiagnosticsSeverityFloor: the floor is what keeps a medium Go file's
// hundreds of hints from burying the one error that matters.
func TestDiagnosticsSeverityFloor(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.diags = map[string][]lsp.Diagnostic{rig.path("internal/a.go"): {
		{Severity: lsp.SeverityError, Message: "an error"},
		{Severity: lsp.SeverityWarning, Message: "a warning"},
		{Severity: lsp.SeverityInformation, Message: "an info"},
		{Severity: lsp.SeverityHint, Message: "a hint"},
	}}

	tl, _ := NewDiagnosticsTool(opts)
	for _, tc := range []struct {
		severity string
		want     int
	}{
		{"", 2}, {"error", 1}, {"warning", 2}, {"info", 3}, {"hint", 4},
	} {
		args := `{"path":"internal/a.go"}`
		if tc.severity != "" {
			args = `{"path":"internal/a.go","severity":"` + tc.severity + `"}`
		}
		var got DiagnosticsOutput
		if err := json.Unmarshal([]byte(invoke(t, tl, args)), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Count != tc.want {
			t.Errorf("severity %q gave %d diagnostics, want %d", tc.severity, got.Count, tc.want)
		}
	}

	if _, err := tl.InvokableRun(context.Background(), `{"path":"internal/a.go","severity":"loud"}`); err == nil {
		t.Error("an unknown severity must be refused with a message the model can act on")
	}
}

// TestDiagnosticsWholeWorkspaceIsBoundedAndHonest: the sweep has to say how many
// files it actually checked, because "no diagnostics" from a sweep of 3 files is
// not the same claim as from a sweep of 3000.
func TestDiagnosticsWholeWorkspaceIsBoundedAndHonest(t *testing.T) {
	client := &fakeClient{settled: true, diags: map[string][]lsp.Diagnostic{}}
	rig, opts := newCodeIntelRig(t, client, true)
	client.diags[rig.path("internal/b.go")] = []lsp.Diagnostic{{
		Severity: lsp.SeverityError, Message: "broken", Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 0}},
	}}

	tl, _ := NewDiagnosticsTool(opts)
	var got DiagnosticsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// notes.md is not covered, so only the two .go files are opened.
	if got.Files != 2 {
		t.Errorf("files = %d, want 2 (the sweep must honour Covers)", got.Files)
	}
	if got.Count != 1 || !strings.Contains(got.Summary, "internal/b.go:2:1") {
		t.Errorf("summary = %q (items %+v)", got.Summary, got.Items)
	}
}

// TestGotoDefinition: a definition answer shows the place and the line, so the
// model can judge relevance without opening the file.
func TestGotoDefinition(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.defs = []lsp.Location{{
		URI:   lsp.PathToURI(rig.path("internal/a.go")),
		Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}},
	}}

	tl, err := NewGotoDefinitionTool(opts)
	if err != nil {
		t.Fatalf("NewGotoDefinitionTool: %v", err)
	}
	var got LocationsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/b.go","line":3,"column":14}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 1 {
		t.Fatalf("count = %d, items %+v", got.Count, got.Items)
	}
	if got.Items[0].Path != "internal/a.go" || got.Items[0].Line != 3 {
		t.Errorf("item = %+v, want internal/a.go:3", got.Items[0])
	}
	if got.Items[0].Snippet != "func Foo() {}" {
		t.Errorf("snippet = %q, want the definition's line", got.Items[0].Snippet)
	}
	if !strings.Contains(got.Summary, "internal/a.go:3:6") {
		t.Errorf("summary = %q", got.Summary)
	}
}

// TestGotoDefinitionEmpty: no definition is a normal answer, and it has to come
// with a hint about why — usually a column that is not on the symbol.
func TestGotoDefinitionEmpty(t *testing.T) {
	client := &fakeClient{settled: true}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewGotoDefinitionTool(opts)
	var got LocationsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/b.go","line":3,"column":1}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
	if !strings.Contains(got.Summary, "column") {
		t.Errorf("an empty answer should hint at the likely cause, got %q", got.Summary)
	}
}

// TestFindReferencesIsTheToolForChangingASignature: this is the answer grep
// cannot give, and the declaration flag has to reach the server.
func TestFindReferences(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.refs = []lsp.Location{
		{URI: lsp.PathToURI(rig.path("internal/a.go")), Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}},
		{URI: lsp.PathToURI(rig.path("internal/b.go")), Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 12}}},
	}

	tl, err := NewFindReferencesTool(opts)
	if err != nil {
		t.Fatalf("NewFindReferencesTool: %v", err)
	}
	var got LocationsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl,
		`{"path":"internal/a.go","line":3,"column":6,"include_declaration":true}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !client.lastRefInc {
		t.Error("include_declaration did not reach the server")
	}
	if got.Count != 2 {
		t.Fatalf("count = %d, want 2", got.Count)
	}
	// Sorted by file then line, so the answer is stable.
	if got.Items[0].Path != "internal/a.go" || got.Items[1].Path != "internal/b.go" {
		t.Errorf("items are not sorted: %+v", got.Items)
	}
	if !strings.Contains(got.Summary, "2 处引用") {
		t.Errorf("summary = %q", got.Summary)
	}
}

// TestFindReferencesEmptyHintsAtTheBoundary: no references is either a dead
// symbol or a symbol used outside this workspace, and the difference matters
// before deleting something.
func TestFindReferencesEmpty(t *testing.T) {
	client := &fakeClient{settled: true, refs: []lsp.Location{}}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewFindReferencesTool(opts)
	var got LocationsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/a.go","line":3,"column":6}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Summary, "其他模块") {
		t.Errorf("an empty reference list should mention the workspace boundary, got %q", got.Summary)
	}
}

// TestFindReferencesTruncatesLoudly: a hot function has hundreds of call sites.
func TestFindReferencesTruncatesLoudly(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	// All twelve are inside the file: a location past the end of a file cannot be
	// converted to a 1-based position and is dropped, which is right for a
	// reference (a wrong location is worse than none) but is not what this test
	// is about.
	for i := 0; i < 12; i++ {
		client.refs = append(client.refs, lsp.Location{
			URI:   lsp.PathToURI(rig.path("internal/a.go")),
			Range: lsp.Range{Start: lsp.Position{Line: i % 5, Character: i}},
		})
	}
	opts.MaxResults = 5

	tl, _ := NewFindReferencesTool(opts)
	var got LocationsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"path":"internal/a.go","line":3,"column":6}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 12 {
		t.Errorf("count = %d, want the true total 12", got.Count)
	}
	if len(got.Items) != 5 {
		t.Errorf("items = %d, want the cap 5", len(got.Items))
	}
	if !strings.Contains(got.Summary, "只显示前 5 处") {
		t.Errorf("the cap must be stated: %q", got.Summary)
	}
}

// TestWorkspaceSymbols: this is the tool for "which code handles X" — a name
// search that returns real definitions instead of every similar-looking string.
func TestWorkspaceSymbols(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.root = rig.ws.Root()
	client.symbols = []lsp.Symbol{
		{Name: "Authorize", Kind: 12, ContainerName: "auth",
			Location: lsp.Location{URI: lsp.PathToURI(rig.path("internal/a.go")),
				Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 0}}}},
		{Name: "AuthConfig", Kind: 23,
			Location: lsp.Location{URI: lsp.PathToURI(rig.path("internal/b.go")),
				Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}}}},
	}

	tl, err := NewWorkspaceSymbolsTool(opts)
	if err != nil {
		t.Fatalf("NewWorkspaceSymbolsTool: %v", err)
	}
	var got SymbolsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"query":"Auth"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 2 {
		t.Fatalf("count = %d, items %+v", got.Count, got.Items)
	}
	// Sorted by file then line, which is what makes the answer stable.
	if got.Items[0].Name != "Authorize" || got.Items[0].Kind != "function" {
		t.Errorf("first item = %+v", got.Items[0])
	}
	if got.Items[1].Name != "AuthConfig" || got.Items[1].Kind != "struct" {
		t.Errorf("second item = %+v (the kind number must be rendered as a name)", got.Items[1])
	}
	if !strings.Contains(got.Summary, "internal/a.go:3  function  Authorize  (auth)") {
		t.Errorf("summary = %q", got.Summary)
	}
}

// TestWorkspaceSymbolsDropsResultsOutsideTheWorkspace: a symbol search can
// return results from a dependency's source; reporting a path the agent cannot
// open would send it somewhere it is not allowed to go.
func TestWorkspaceSymbolsDropsResultsOutsideTheWorkspace(t *testing.T) {
	client := &fakeClient{settled: true}
	rig, opts := newCodeIntelRig(t, client, true)
	client.root = rig.ws.Root()
	outside := t.TempDir()
	client.symbols = []lsp.Symbol{
		{Name: "Ours", Kind: 12, Location: lsp.Location{
			URI: lsp.PathToURI(rig.path("internal/a.go"))}},
		{Name: "Theirs", Kind: 12, Location: lsp.Location{
			URI: lsp.PathToURI(filepath.Join(outside, "dep.go"))}},
	}

	tl, _ := NewWorkspaceSymbolsTool(opts)
	var got SymbolsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"query":"s"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 1 || got.Items[0].Name != "Ours" {
		t.Errorf("items = %+v, want only the in-workspace symbol", got.Items)
	}
}

// TestWorkspaceSymbolsEmptySuggestsANextStep: "not found" alone leaves the model
// with nothing to do.
func TestWorkspaceSymbolsEmpty(t *testing.T) {
	client := &fakeClient{settled: true, symbols: nil}
	rig, opts := newCodeIntelRig(t, client, true)
	client.root = rig.ws.Root()

	tl, _ := NewWorkspaceSymbolsTool(opts)
	var got SymbolsOutput
	if err := json.Unmarshal([]byte(invoke(t, tl, `{"query":"Nothing"}`)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Summary, "Nothing") || !strings.Contains(got.Summary, "grep") {
		t.Errorf("summary = %q, want the query echoed and a fallback suggested", got.Summary)
	}
}

// TestPositionValidationReachesTheModel: a bad line or column must be refused
// with a message that says what to fix, not clamped into an answer about a
// different line.
func TestPositionValidationReachesTheModel(t *testing.T) {
	client := &fakeClient{settled: true}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewGotoDefinitionTool(opts)
	// internal/a.go has 5 lines; line 99 is past the end.
	_, err := tl.InvokableRun(context.Background(), `{"path":"internal/a.go","line":99,"column":1}`)
	if err == nil {
		t.Fatal("a position past the end of the file must be refused")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want it to say the position is out of range", err)
	}
}

// TestPathsAreConfined: the tools must not become a way to read a file outside
// the workspace through the language server.
func TestPathsAreConfined(t *testing.T) {
	client := &fakeClient{settled: true}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewDiagnosticsTool(opts)
	if _, err := tl.InvokableRun(context.Background(), `{"path":"../../../etc/passwd"}`); err == nil {
		t.Error("a path outside the workspace must be refused")
	}
	if len(client.opened) != 0 {
		t.Errorf("the server was asked about a path outside the workspace: %v", client.opened)
	}
}

// TestServerErrorsArePassedThrough: when the language server fails, the model
// should see why rather than "no results".
func TestServerErrorsArePassedThrough(t *testing.T) {
	client := &fakeClient{settled: true, refErr: errors.New("server exploded")}
	_, opts := newCodeIntelRig(t, client, true)

	tl, _ := NewFindReferencesTool(opts)
	_, err := tl.InvokableRun(context.Background(), `{"path":"internal/a.go","line":3,"column":6}`)
	if err == nil {
		t.Fatal("expected the server error to surface")
	}
	if !strings.Contains(err.Error(), "server exploded") {
		t.Errorf("error = %v, want the server's own message", err)
	}
}
