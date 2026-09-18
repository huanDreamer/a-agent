package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// TestServerSpecCovers: the match decides whether a file gets a language server
// at all. A miss means the code-intelligence tools are absent for that file, and
// the model goes back to grep without knowing why.
func TestServerSpecCovers(t *testing.T) {
	goSrv := ServerSpec{Languages: []string{"go"}}
	dotted := ServerSpec{Languages: []string{".go", ".tmpl"}}
	ts := ServerSpec{Languages: []string{"typescript", "typescriptreact"}}

	cases := []struct {
		name string
		spec ServerSpec
		path string
		want bool
	}{
		{"language id spelling", goSrv, "/a/b.go", true},
		{"extension spelling", dotted, "/a/b.go", true},
		{"second extension", dotted, "/a/b.tmpl", true},
		{"other language", goSrv, "/a/b.py", false},
		{"no extension", goSrv, "/a/Makefile", false},
		{"tsx maps to typescriptreact", ts, "/a/b.tsx", true},
		{"case insensitive extension", goSrv, "/a/B.GO", true},
		{"empty languages covers nothing", ServerSpec{}, "/a/b.go", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.Covers(tt.path); got != tt.want {
				t.Errorf("%+v.Covers(%q) = %v, want %v", tt.spec.Languages, tt.path, got, tt.want)
			}
		})
	}
}

// TestRootFor: a language server indexes the project its root names, so picking
// the wrong root turns "no references found" into a wrong answer rather than a
// missing one.
func TestRootFor(t *testing.T) {
	// root/module/pkg/file.go, with a go.mod at root/module.
	ceiling := t.TempDir()
	module := filepath.Join(ceiling, "module")
	pkg := filepath.Join(module, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(pkg, "a.go")
	if err := os.WriteFile(file, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := RootFor(file, []string{"go.mod"}, ceiling)
	want, _ := filepath.EvalSymlinks(module)
	gotReal, _ := filepath.EvalSymlinks(got)
	if gotReal != want {
		t.Errorf("RootFor = %q, want the directory holding go.mod (%q)", got, module)
	}

	// No marker anywhere: the ceiling is used, so the server still starts.
	got = RootFor(file, []string{"nothing.here"}, ceiling)
	gotReal, _ = filepath.EvalSymlinks(got)
	wantCeiling, _ := filepath.EvalSymlinks(ceiling)
	if gotReal != wantCeiling {
		t.Errorf("RootFor with no marker = %q, want the ceiling %q", got, ceiling)
	}

	// The search must not climb out of the workspace, or a marker in a parent
	// directory would start a server over a tree the agent is not confined to.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(outside, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(nested, "b.go")
	if err := os.WriteFile(inner, []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = RootFor(inner, []string{"go.mod"}, nested)
	gotReal, _ = filepath.EvalSymlinks(got)
	wantNested, _ := filepath.EvalSymlinks(nested)
	if gotReal != wantNested {
		t.Errorf("RootFor climbed past the workspace ceiling: %q (want %q)", got, nested)
	}
}

// TestManagerDegradesWhenServerIsMissing is the property that keeps a missing
// language server from taking the agent down with it: no server, no error thrown
// at the agent, and no retry storm.
func TestManagerDegradesWhenServerIsMissing(t *testing.T) {
	var logged []string
	logger := &recordingLogger{warn: func(f string, a ...any) {
		logged = append(logged, f)
	}}
	m := NewManager(ManagerOptions{
		Servers: []ServerSpec{{
			Name:        "definitely-missing",
			Command:     "huan-agent-no-such-language-server",
			Languages:   []string{"go"},
			RootMarkers: []string{"go.mod"},
		}},
		Logger: logger,
	})
	defer func() { _ = m.Close() }()

	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := m.ClientFor(context.Background(), file, dir)
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if !IsServerMissing(err) {
		t.Errorf("the error should be recognisable as 'not installed', got: %v", err)
	}
	// The hint has to say how to fix it, not only that something is missing.
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("the error should carry an install hint, got: %v", err)
	}
	if len(logged) != 1 {
		t.Fatalf("warnings = %d, want exactly one (a warning per call is a log flood)", len(logged))
	}

	// A second call must not try to start it again.
	start := time.Now()
	if _, err := m.ClientFor(context.Background(), file, dir); err == nil {
		t.Fatal("expected the remembered failure")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the second call took %v; a remembered failure must not re-attempt a spawn", elapsed)
	}
	if len(logged) != 1 {
		t.Errorf("the failure was re-logged: %d warnings", len(logged))
	}
}

// TestManagerNoServerCoversFileIsNotAnError: (nil, nil) is the contract the tool
// registration depends on. Turning it into an error would make every non-Go file
// look broken.
func TestManagerNoServerCoversFileIsNotAnError(t *testing.T) {
	m := NewManager(ManagerOptions{Servers: DefaultServers()})
	defer func() { _ = m.Close() }()

	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(file, []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := m.ClientFor(context.Background(), file, dir)
	if err != nil {
		t.Fatalf("a file with no server must not be an error: %v", err)
	}
	if client != nil {
		t.Errorf("client = %+v, want nil", client)
	}
	if m.Covers(file) {
		t.Error("Covers must be false for a file no server handles")
	}
	if !m.Covers(filepath.Join(dir, "a.go")) {
		t.Error("Covers must be true for a .go file with gopls configured")
	}
}

// TestManagerHasEnabledServer: the workspace-wide tools are gated on this, and a
// disabled list must not put two tools on the menu that can only fail.
func TestManagerHasEnabledServer(t *testing.T) {
	off := false
	disabled := NewManager(ManagerOptions{Servers: []ServerSpec{
		{Name: "gopls", Command: "gopls", Languages: []string{"go"}, Enabled: &off},
	}})
	defer func() { _ = disabled.Close() }()
	if disabled.HasEnabledServer() {
		t.Error("a disabled server must not count as available")
	}

	blank := NewManager(ManagerOptions{Servers: []ServerSpec{{Name: "x", Command: "  ", Languages: []string{"go"}}}})
	defer func() { _ = blank.Close() }()
	if blank.HasEnabledServer() {
		t.Error("a server with no command must not count as available")
	}

	// No servers configured at all falls back to the built-in table.
	def := NewManager(ManagerOptions{})
	defer func() { _ = def.Close() }()
	if !def.HasEnabledServer() {
		t.Error("the built-in table should provide gopls")
	}
}

// TestDefaultServersMatchTheDocumentedExample: the built-in table is data the
// docs describe, so the two drifting apart means the docs are wrong.
func TestDefaultServersMatchTheDocumentedExample(t *testing.T) {
	servers := DefaultServers()
	if len(servers) == 0 {
		t.Fatal("no default servers")
	}
	gopls := servers[0]
	if gopls.Command != "gopls" {
		t.Errorf("first default server command = %q, want gopls", gopls.Command)
	}
	if !gopls.Covers("/x/y.go") {
		t.Error("the default server must cover Go files")
	}
	found := false
	for _, m := range gopls.RootMarkers {
		if m == "go.mod" {
			found = true
		}
	}
	if !found {
		t.Errorf("root markers = %v, want go.mod among them", gopls.RootMarkers)
	}
	if !gopls.IsEnabled() {
		t.Error("a default server must be enabled when nothing says otherwise")
	}
}

// --- edit feedback ---

// fakeDiagnoser scripts the three answers the decorator has to handle. None of
// them needs a language server, which is the point of the interface.
type fakeDiagnoser struct {
	items []Diagnostic
	// state is the outcome. The zero value is NoServer, which is what a
	// zero-value fake should mean: nothing was asked.
	state Diagnosed
	err   error
	calls int
}

func (f *fakeDiagnoser) FileDiagnostics(_ context.Context, _, _ string, _ time.Duration) ([]Diagnostic, Diagnosed, error) {
	f.calls++
	if f.err != nil {
		return nil, NoServer, f.err
	}
	return f.items, f.state, nil
}

// fakeTool is an invokable tool that returns a fixed result.
type fakeTool struct {
	name   string
	result string
	err    error
}

func (f fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "fake"}, nil
}

func (f fakeTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return f.result, f.err
}

// TestFeedbackAttachesDiagnostics is the behaviour the phase exists for: the
// model finds out about the type error it just introduced without running a
// build.
func TestFeedbackAttachesDiagnostics(t *testing.T) {
	d := &fakeDiagnoser{
		state: Ready,
		items: []Diagnostic{
			{Severity: SeverityError, Message: "undefined: BAZ", Source: "compiler",
				Range: Range{Start: Position{Line: 41, Character: 8}}},
			{Severity: SeverityWarning, Message: "unused parameter: ctx",
				Range: Range{Start: Position{Line: 87, Character: 1}}},
			{Severity: SeverityHint, Message: "consider a comment", // filtered out
				Range: Range{Start: Position{Line: 1, Character: 0}}},
		},
	}
	inner := fakeTool{name: "edit_file", result: `{"path":"internal/a.go","replacements":1}`}
	decorated := NewFeedback(inner, d, FeedbackOptions{})

	got, err := decorated.(einotool.InvokableTool).InvokableRun(context.Background(),
		`{"path":"internal/a.go","old_string":"x","new_string":"y"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	// The original result is untouched, so a caller that parses it still can.
	if !strings.HasPrefix(got, inner.result) {
		t.Errorf("the tool's own result was modified:\n%s", got)
	}
	if !strings.Contains(got, "诊断（该文件，2 条 error/warning）：") {
		t.Errorf("missing the diagnostics header:\n%s", got)
	}
	if !strings.Contains(got, "internal/a.go:42:9  error  undefined: BAZ  (compiler)") {
		t.Errorf("the error is not rendered in file:line:col form:\n%s", got)
	}
	if !strings.Contains(got, "internal/a.go:88:2  warning  unused parameter: ctx") {
		t.Errorf("the warning is missing or misplaced:\n%s", got)
	}
	if strings.Contains(got, "consider a comment") {
		t.Errorf("hints must be filtered out by default:\n%s", got)
	}
	// Errors come first.
	if strings.Index(got, "undefined: BAZ") > strings.Index(got, "unused parameter") {
		t.Error("diagnostics are not sorted with errors first")
	}
}

// TestInstallHintNamesTheCommand: the whole value of the hint is that a missing
// tool comes with the line that installs it.
func TestInstallHintNamesTheCommand(t *testing.T) {
	if got := installHint("gopls"); !strings.Contains(got, "go install") {
		t.Errorf("gopls hint = %q", got)
	}
	if got := installHint("something-else"); !strings.Contains(got, "something-else") {
		t.Errorf("a generic hint should still name the command: %q", got)
	}
}

// TestFeedbackSaysCleanWhenClean: an absent section reads as "the check did not
// run", which makes the model either ignore it or re-run a check.
func TestFeedbackSaysCleanWhenClean(t *testing.T) {
	d := &fakeDiagnoser{state: Ready, items: []Diagnostic{}}
	inner := fakeTool{name: "write_file", result: `{"path":"a.go","bytes":3}`}
	got, _ := NewFeedback(inner, d, FeedbackOptions{}).(einotool.InvokableTool).
		InvokableRun(context.Background(), `{"path":"a.go"}`)

	if !strings.Contains(got, "诊断（该文件）：无") {
		t.Errorf("a clean file should say so:\n%s", got)
	}
}

// TestFeedbackNotReady: a server that has not answered yet must not stall the
// edit, and must not pretend the file is clean.
func TestFeedbackNotReady(t *testing.T) {
	d := &fakeDiagnoser{state: NotReady}
	inner := fakeTool{name: "write_file", result: `{"path":"a.go"}`}
	got, err := NewFeedback(inner, d, FeedbackOptions{Wait: 1500 * time.Millisecond}).(einotool.InvokableTool).InvokableRun(context.Background(), `{"path":"a.go"}`)
	if err != nil {
		t.Fatalf("a slow server must not fail the edit: %v", err)
	}
	if !strings.Contains(got, "尚未就绪") || !strings.Contains(got, "1.5s") {
		t.Errorf("expected a not-ready note naming the wait:\n%s", got)
	}
}

// TestFeedbackNoServerChangesNothing: with no language server the decorator must
// be invisible, down to the byte. Anything else is overhead paid by every
// deployment that does not use the feature.
func TestFeedbackNoServerChangesNothing(t *testing.T) {
	d := &fakeDiagnoser{state: NoServer}
	inner := fakeTool{name: "write_file", result: `{"path":"a.go"}`}
	got, err := NewFeedback(inner, d, FeedbackOptions{}).(einotool.InvokableTool).
		InvokableRun(context.Background(), `{"path":"a.go"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if got != inner.result {
		t.Errorf("result changed with no server: %q", got)
	}
}

// TestFeedbackErrorPathIsUntouched: a failed edit keeps its failure. Mixed
// signals about why an edit failed are worse than the failure.
func TestFeedbackErrorPathIsUntouched(t *testing.T) {
	d := &fakeDiagnoser{state: Ready, items: []Diagnostic{{Severity: SeverityError, Message: "boom"}}}
	inner := fakeTool{name: "edit_file", result: "partial", err: context.DeadlineExceeded}
	got, err := NewFeedback(inner, d, FeedbackOptions{}).(einotool.InvokableTool).
		InvokableRun(context.Background(), `{"path":"a.go"}`)
	if err == nil {
		t.Fatal("the error was swallowed")
	}
	if got != "partial" {
		t.Errorf("result = %q, want it passed through", got)
	}
}

// TestFeedbackServerErrorItDoesNotBreakTheEdit: a wedged language server must not
// turn a successful write into a failed one.
func TestFeedbackServerErrorDoesNotBreakTheEdit(t *testing.T) {
	d := &fakeDiagnoser{err: errServerMissing}
	inner := fakeTool{name: "write_file", result: `{"path":"a.go"}`}
	got, err := NewFeedback(inner, d, FeedbackOptions{}).(einotool.InvokableTool).
		InvokableRun(context.Background(), `{"path":"a.go"}`)
	if err != nil {
		t.Fatalf("a missing server must not fail a successful write: %v", err)
	}
	if got != inner.result {
		t.Errorf("result = %q, want it unchanged", got)
	}
}

// TestFeedbackTruncatesLoudly: the cap is fine, a silent cap is not.
func TestFeedbackTruncatesLoudly(t *testing.T) {
	var items []Diagnostic
	for i := 0; i < 30; i++ {
		items = append(items, Diagnostic{Severity: SeverityError, Message: "e",
			Range: Range{Start: Position{Line: i, Character: 0}}})
	}
	d := &fakeDiagnoser{state: Ready, items: items}
	inner := fakeTool{name: "write_file", result: `{"path":"internal/a.go"}`}
	ws := newTestWorkspace(t)
	got, _ := NewFeedback(inner, d, FeedbackOptions{Max: 5, Workspace: ws}).(einotool.InvokableTool).
		InvokableRun(context.Background(), `{"path":"internal/a.go"}`)

	if !strings.Contains(got, "共 30，显示前 5") {
		t.Errorf("truncation must be stated:\n%s", got)
	}
	if n := strings.Count(got, "\n  internal/a.go"); n != 5 {
		t.Errorf("rendered %d items, want 5", n)
	}
}

// TestFeedbackResolvesPathsThroughTheWorkspace: the decorator must not be a way
// to point the language server at a file outside the sandbox.
func TestFeedbackResolvesPathsThroughTheWorkspace(t *testing.T) {
	ws := newTestWorkspace(t)
	d := &fakeDiagnoser{state: Ready, items: []Diagnostic{}}
	inner := fakeTool{name: "write_file", result: `{}`}
	decorated := NewFeedback(inner, d, FeedbackOptions{Workspace: ws})

	// A path that escapes the workspace is not resolved, so no lookup happens.
	_, _ = decorated.(einotool.InvokableTool).InvokableRun(context.Background(), `{"path":"../../../etc/passwd"}`)
	if d.calls != 0 {
		t.Errorf("the diagnoser was called for a path outside the workspace (%d calls)", d.calls)
	}

	// A legitimately relative path is resolved and looked up.
	_, _ = decorated.(einotool.InvokableTool).InvokableRun(context.Background(), `{"path":"internal/a.go"}`)
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1", d.calls)
	}
}

// newTestWorkspace builds a real sandbox over a temp directory, so path
// resolution behaves exactly as it does in production.
func newTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "a.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// recordingLogger captures warn calls for the degradation test.
type recordingLogger struct {
	mu    sync.Mutex
	warn  func(string, ...any)
	debug func(string, ...any)
}

func (l *recordingLogger) Warnf(format string, args ...any) {
	if l.warn != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.warn(format, args...)
	}
}

func (l *recordingLogger) Debugf(format string, args ...any) {
	if l.debug != nil {
		l.debug(format, args...)
	}
}

// TestFeedbackForwardsConcurrency: a decorator that embeds the Tool interface
// promotes only that interface's methods, so a declaration it does not forward is
// a declaration that disappears. That is not hypothetical — the same omission in
// the approval gate made a write tool look read-only — so every decorator in this
// repository has a test for it.
func TestFeedbackForwardsConcurrency(t *testing.T) {
	// namedTool here is lsp's own: a tool.Tool that declares nothing, which is
	// exactly the case a decorator must forward faithfully.
	inner := tool.WithConcurrency(plainTool{name: "edit_file"}, tool.ParallelSafe)
	decorated := NewFeedback(inner, &fakeDiagnoser{state: Ready}, FeedbackOptions{})
	if got := tool.ConcurrencyOf(decorated); got != tool.ParallelSafe {
		t.Errorf("the feedback decorator lost the declaration: %q", got)
	}
	if got := tool.CapabilityOf(decorated); got != tool.CapRead {
		t.Errorf("the feedback decorator lost the capability: %q", got)
	}
}

// plainTool is a tool.Tool that declares no capability and no concurrency, so the
// decorator tests can see what is forwarded and what is invented.
type plainTool struct{ name string }

func (p plainTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: p.name, Desc: "plain"}, nil
}

func (p plainTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "ok", nil
}
