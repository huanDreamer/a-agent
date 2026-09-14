package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

// These tests cover the contract the whole multi-workspace design rests on: the
// registry a turn runs with is derived from that turn's workspace, and a policy
// is applied by the tool being absent rather than by its call failing.

// newBinderTestRig builds a bindings object over a real store, a real manager and
// a temp base directory, with the given base-registry contents.
func newBinderTestRig(t *testing.T, cfg *config.Config, baseTools ...tool.Tool) (*workspaceBindings, *workspaces.Manager, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "binder.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The seed directory is a real one: a workspace is a directory, and the
	// binder resolves tools against whatever it points at.
	defaultRoot := t.TempDir()
	mgr, err := workspaces.New(workspaces.Options{
		Store:    st,
		SeedRoot: defaultRoot,
		ReadOnly: cfg.Tools.ReadOnly,
	})
	if err != nil {
		t.Fatalf("workspaces.New: %v", err)
	}
	if _, err := mgr.EnsureSeed(ctx); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}

	base := tool.NewRegistry()
	for _, bt := range baseTools {
		if err := base.Register(bt); err != nil {
			t.Fatalf("register base tool: %v", err)
		}
	}
	b := &workspaceBindings{
		logger: zap.NewNop(),
		mgr:    mgr,
		set:    newWorkspaceToolSet(cfg, st, zap.NewNop(), toolSetOptions{}),
		base:   base,
	}
	return b, mgr, defaultRoot
}

// scopedCtx is the context a runner hands to a per-turn tool lookup.
func scopedCtx(scope string) context.Context {
	return chat.WithScope(context.Background(), scope)
}

// newScopeWorkspace creates a workspace over a fresh directory and points one
// scope at it.
func newScopeWorkspace(t *testing.T, mgr *workspaces.Manager, scope, name string, in workspaces.CreateInput) workspaces.Spec {
	t.Helper()
	ctx := context.Background()
	in.Name = name
	if in.Root == "" {
		in.Root = t.TempDir()
	}
	spec, err := mgr.Create(ctx, in)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := mgr.Select(ctx, scope, name); err != nil {
		t.Fatalf("select %s for %s: %v", name, scope, err)
	}
	return spec
}

// TestBindingsAreDerivedFromTheTurnScope is the core promise: two scopes, two
// workspaces, and the filesystem tools each turn gets are the ones bound to its
// own root.
func TestBindingsAreDerivedFromTheTurnScope(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	b, mgr, _ := newBinderTestRig(t, cfg)

	alpha := newScopeWorkspace(t, mgr, "web:alpha", "alpha", workspaces.CreateInput{})
	beta := newScopeWorkspace(t, mgr, "web:beta", "beta", workspaces.CreateInput{})

	// The proof is behavioural rather than structural: put a file with a
	// different body in each root and read it back through the tool the turn was
	// given. A tool bound to the wrong root returns the wrong body — or fails.
	bodies := map[string]string{"web:alpha": "alpha-body", "web:beta": "beta-body"}
	roots := map[string]string{"web:alpha": alpha.Root, "web:beta": beta.Root}
	for scope, body := range bodies {
		if err := os.WriteFile(filepath.Join(roots[scope], "probe.txt"), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", scope, err)
		}
	}

	for scope, body := range bodies {
		reg, err := b.forScope(scopedCtx(scope))
		if err != nil {
			t.Fatalf("forScope(%s): %v", scope, err)
		}
		tl, ok := reg.Get("read_file")
		if !ok {
			t.Fatalf("scope %s has no read_file", scope)
		}
		out, err := tl.InvokableRun(scopedCtx(scope), `{"path":"probe.txt"}`)
		if err != nil {
			t.Fatalf("read_file in %s: %v", scope, err)
		}
		if !strings.Contains(out, body) {
			t.Errorf("scope %s read %q, want the body of its own root (%q)", scope, out, body)
		}
		other := bodies[otherScope(scope)]
		if strings.Contains(out, other) {
			t.Errorf("scope %s read the other workspace's file: %q", scope, out)
		}
	}
}

// otherScope returns the scope key that is not the given one.
func otherScope(scope string) string {
	if scope == "web:alpha" {
		return "web:beta"
	}
	return "web:alpha"
}

// TestGlobalReadOnlyWithholdsWriteTools: `tools.read_only` is process-wide, and
// it is applied by not registering the tools at all. A model that can see
// write_file will call it, so "the call was refused" is a worse answer than a
// tool that was never on the menu.
//
// It applies to every workspace equally: a workspace answers *where* the agent
// works, not *what it may do*, so there is deliberately no per-workspace variant
// of this test.
func TestGlobalReadOnlyWithholdsWriteTools(t *testing.T) {
	readWrite := &config.Config{}
	readWrite.Tools.EnableBash = true
	b, mgr, _ := newBinderTestRig(t, readWrite)
	newScopeWorkspace(t, mgr, "web:s1", "project", workspaces.CreateInput{})

	reg, err := b.forScope(scopedCtx("web:s1"))
	if err != nil {
		t.Fatalf("forScope: %v", err)
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", "bash"} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("a read-write deployment is missing %s", name)
		}
	}

	// The same workspace, in a read-only deployment: reads stay, writes and the
	// shell go. `bash` goes because a shell can write files.
	readOnly := &config.Config{}
	readOnly.Tools.ReadOnly = true
	ro, roMgr, _ := newBinderTestRig(t, readOnly)
	newScopeWorkspace(t, roMgr, "web:s1", "project", workspaces.CreateInput{})

	reg, err = ro.forScope(scopedCtx("web:s1"))
	if err != nil {
		t.Fatalf("forScope(read-only): %v", err)
	}
	for _, name := range []string{"write_file", "edit_file", "bash"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("a read-only deployment still offers %s", name)
		}
	}
	for _, name := range []string{"read_file", "list_dir", "glob", "grep"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("read-only deployment is missing the read tool %s", name)
		}
	}
}

// TestBashDisabledWithholdsBash: command execution is the sharpest tool there is,
// and `tools.enable_bash: false` turns it off without turning off file editing.
func TestBashDisabledWithholdsBash(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = false
	b, mgr, _ := newBinderTestRig(t, cfg)
	newScopeWorkspace(t, mgr, "web:s1", "quiet", workspaces.CreateInput{})

	reg, err := b.forScope(scopedCtx("web:s1"))
	if err != nil {
		t.Fatalf("forScope: %v", err)
	}
	if _, ok := reg.Get("bash"); ok {
		t.Error("enable_bash=false still offers bash")
	}
	if _, ok := reg.Get("write_file"); !ok {
		t.Error("disabling bash also removed write_file")
	}
}

// TestCloneCarriesRuntimeTools: MCP servers and the skill tool register into the
// base registry while the process runs, and a per-turn clone has to carry them —
// otherwise a workspace-bound conversation would silently lose the tools the
// operator added.
func TestCloneCarriesRuntimeTools(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	// Registered before the binder exists, standing in for a configured MCP
	// server.
	b, mgr, _ := newBinderTestRig(t, cfg, fakeNamedTool{name: "mcp_find"})
	// Registered after, standing in for one added in 设置 → MCP.
	if err := b.base.Register(fakeNamedTool{name: "mcp_later"}); err != nil {
		t.Fatalf("register later tool: %v", err)
	}

	newScopeWorkspace(t, mgr, "web:s1", "alpha", workspaces.CreateInput{})
	reg, err := b.forScope(scopedCtx("web:s1"))
	if err != nil {
		t.Fatalf("forScope: %v", err)
	}
	for _, name := range []string{"mcp_find", "mcp_later", "read_file", "write_file", "bash"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("bound registry is missing %q", name)
		}
	}
	// Mutating the bound registry must not touch the base.
	if _, ok := b.base.Get("read_file"); ok {
		t.Error("read_file leaked into the base registry")
	}
	names := reg.Names()
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names is not sorted: %v", names)
	}
}

// TestUnscopedTurnGetsTheDefaultWorkspace: a deployment with no selection, and
// any caller that never stamps a scope, must behave exactly as before.
func TestUnscopedTurnGetsTheDefaultWorkspace(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	b, _, defaultRoot := newBinderTestRig(t, cfg)

	reg, err := b.forScope(context.Background())
	if err != nil {
		t.Fatalf("forScope: %v", err)
	}
	if _, ok := reg.Get("read_file"); !ok {
		t.Fatal("an unscoped turn got no workspace tools")
	}
	tl, _ := reg.Get("read_file")
	if _, err := tl.InvokableRun(context.Background(), `{"path":"nothing-here.txt"}`); err == nil {
		t.Fatal("reading a missing file succeeded")
	} else if !strings.Contains(err.Error(), defaultRoot) {
		t.Errorf("unscoped turn resolves to %v, want the configured default root %s",
			err, defaultRoot)
	}
}

// TestForScopeWithoutAManagerIsInert: the wiring allows a deployment whose chat
// has no workspace layer, and that must yield no tools rather than a panic.
func TestForScopeWithoutAManagerIsInert(t *testing.T) {
	var b *workspaceBindings
	reg, err := b.forScope(context.Background())
	if err != nil {
		t.Fatalf("forScope on a nil binding: %v", err)
	}
	if reg != nil {
		t.Errorf("got a registry from a nil binding: %v", reg.Names())
	}
}

// TestWorkspaceBoundNamesCoverTheBuilders guards the list that decides what a
// clone drops: a builder whose name is missing from it would leak the base
// registry's binding into another workspace.
func TestWorkspaceBoundNamesCoverTheBuilders(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	st := newMediaTestStore(t)
	set := newWorkspaceToolSet(cfg, st, zap.NewNop(), toolSetOptions{})
	ws, err := workspace.New(t.TempDir(), workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	built, err := set.build(ws, "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	known := map[string]bool{}
	for _, name := range workspaceBoundToolNames() {
		known[name] = true
	}
	for _, bt := range built {
		info, ierr := bt.Info(context.Background())
		if ierr != nil {
			t.Fatalf("info: %v", ierr)
		}
		if !known[info.Name] {
			t.Errorf("the builder produces %q, which workspaceBoundToolNames does not list", info.Name)
		}
	}
}

// fakeNamedTool is a minimal tool for registry tests.
type fakeNamedTool struct{ name string }

func (f fakeNamedTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "test tool"}, nil
}

func (f fakeNamedTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "", nil
}
