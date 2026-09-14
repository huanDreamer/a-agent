package workspaces

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
)

// testManager builds a Manager over a real store. The seed directory is a real
// temp directory, because a workspace is a directory and faking one would fake
// the thing under test.
func testManager(t *testing.T) (*Manager, store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := t.TempDir()
	m, err := New(Options{Store: st, SeedRoot: seed})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, st, m.seedRoot
}

// mkdir makes a directory to point a workspace at.
func mkdir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return dir
}

// create points a workspace at a fresh directory.
func create(t *testing.T, m *Manager, root, name string) Spec {
	t.Helper()
	spec, err := m.Create(context.Background(), CreateInput{Root: root, Name: name})
	if err != nil {
		t.Fatalf("Create(%s, %s): %v", root, name, err)
	}
	return spec
}

// mustResolve returns the resolved form of a path, the way a sandbox stores it.
func mustResolve(t *testing.T, path string) string {
	t.Helper()
	ws, err := workspace.New(path, workspace.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return ws.Root()
}

// TestResolveDirRefusesAnythingButAnExistingDirectory is the rule that makes
// "pick a directory" meaningful: a typo is reported at the form rather than
// becoming an empty folder the agent then works in.
func TestResolveDirRefusesAnythingButAnExistingDirectory(t *testing.T) {
	dir := mkdir(t, "project")
	if got, err := ResolveDir(dir); err != nil || got != mustResolve(t, dir) {
		t.Errorf("ResolveDir(%s) = %q, %v", dir, got, err)
	}

	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, tc := range []struct{ path, why string }{
		{"", "empty"},
		{"   ", "blank"},
		{filepath.Join(t.TempDir(), "missing"), "does not exist"},
		{file, "is a file"},
		{"/", "filesystem root"},
	} {
		_, err := ResolveDir(tc.path)
		if err == nil {
			t.Errorf("ResolveDir(%q) = nil, want a refusal (%s)", tc.path, tc.why)
			continue
		}
		var invalid *ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("ResolveDir(%q) returned %T, want *ValidationError", tc.path, err)
		}
	}
}

// TestResolveDirResolvesSymlinks: the stored root has to be the path the sandbox
// compares against, or a root reached through a link would reject its children.
func TestResolveDirResolvesSymlinks(t *testing.T) {
	real := mkdir(t, "real")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := ResolveDir(link)
	if err != nil {
		t.Fatalf("ResolveDir(link): %v", err)
	}
	if got != mustResolve(t, real) {
		t.Errorf("ResolveDir(link) = %q, want %q", got, mustResolve(t, real))
	}
}

// TestValidateName covers the label rules: a name is shown in a sidebar and typed
// into Feishu, and it is not a path component — so the only refusals are the ones
// that would break something.
func TestValidateName(t *testing.T) {
	for _, name := range []string{
		"blog", "blog-site", "我的博客", "My Project", "项目 v2", "a.b_c-d",
	} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "   ", " blog", "blog ", "a/b", `a\b`, strings.Repeat("x", 61), "a\x00b"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want a refusal", name)
		}
	}
}

// TestCreateRegistersAnExistingDirectory: nothing is created on disk, and the
// name defaults to the directory's base name.
func TestCreateRegistersAnExistingDirectory(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	root := mkdir(t, "my-project")

	spec, err := m.Create(ctx, CreateInput{Root: root})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if spec.Name != "my-project" {
		t.Errorf("name = %q, want the directory's base name", spec.Name)
	}
	if spec.Root != mustResolve(t, root) {
		t.Errorf("root = %q, want %q", spec.Root, mustResolve(t, root))
	}
	if spec.SessionCount != 0 {
		t.Errorf("session_count = %d, want 0", spec.SessionCount)
	}
	// Creating a workspace never writes into the directory.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("creating a workspace wrote into the directory: %v", entries)
	}

	// A duplicate name is refused, case-insensitively.
	if _, err := m.Create(ctx, CreateInput{Root: mkdir(t, "other"), Name: "MY-PROJECT"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name = %v, want ErrExists", err)
	}
}

// TestCreateRefusesADirectoryThatIsNotThere: the failure has to happen while the
// operator is looking at the form.
func TestCreateRefusesADirectoryThatIsNotThere(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)

	if _, err := m.Create(ctx, CreateInput{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("Create of a missing directory succeeded")
	}
	specs, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(specs) != 0 {
		t.Errorf("a refused create registered something: %+v", specs)
	}
}

// TestTwoWorkspacesAreIsolated is the property the whole feature rests on: a tool
// bound to one directory cannot reach another's files, even by absolute path.
func TestTwoWorkspacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	alpha := create(t, m, mkdir(t, "alpha"), "alpha")
	beta := create(t, m, mkdir(t, "beta"), "beta")

	wsA, err := m.Open(ctx, alpha.Name)
	if err != nil {
		t.Fatalf("Open(alpha): %v", err)
	}
	wsB, err := m.Open(ctx, beta.Name)
	if err != nil {
		t.Fatalf("Open(beta): %v", err)
	}
	if wsA.Root() == wsB.Root() {
		t.Fatalf("both workspaces resolved to %q", wsA.Root())
	}

	if got, err := wsA.Resolve("src/main.go"); err != nil {
		t.Fatalf("Resolve in alpha: %v", err)
	} else if want := filepath.Join(alpha.Root, "src", "main.go"); got != want {
		t.Errorf("alpha resolved to %q, want %q", got, want)
	}
	for _, p := range []string{filepath.Join(beta.Root, "secret.txt"), "../beta/secret.txt"} {
		if _, err := wsA.Resolve(p); !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("alpha.Resolve(%q) = %v, want ErrOutsideWorkspace", p, err)
		}
	}
	if _, err := wsB.Resolve(filepath.Join(alpha.Root, "secret.txt")); !errors.Is(err, workspace.ErrOutsideWorkspace) {
		t.Error("beta reached into alpha")
	}
}

// TestSelectionIsPerScope: two conversations choose different workspaces and
// neither sees the other's choice.
func TestSelectionIsPerScope(t *testing.T) {
	ctx := context.Background()
	m, _, seedRoot := testManager(t)
	if _, err := m.EnsureSeed(ctx); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	create(t, m, mkdir(t, "alpha"), "alpha")
	create(t, m, mkdir(t, "beta"), "beta")

	if _, err := m.Select(ctx, "web:s1", "alpha"); err != nil {
		t.Fatalf("Select(web:s1): %v", err)
	}
	if _, err := m.Select(ctx, "feishu:ou_1", "beta"); err != nil {
		t.Fatalf("Select(feishu:ou_1): %v", err)
	}

	for scope, want := range map[string]string{"web:s1": "alpha", "feishu:ou_1": "beta"} {
		got, err := m.Active(ctx, scope)
		if err != nil {
			t.Fatalf("Active(%s): %v", scope, err)
		}
		if got.Name != want {
			t.Errorf("Active(%s) = %q, want %q", scope, got.Name, want)
		}
	}
	// A scope that chose nothing still resolves to a real workspace.
	if got, err := m.Active(ctx, "web:untouched"); err != nil || got.Root == "" {
		t.Errorf("Active(web:untouched) = %+v, %v", got, err)
	}
	// The seeded workspace is still registered and reachable by name.
	if spec, err := m.Get(ctx, DefaultNameFor(seedRoot)); err != nil {
		t.Errorf("the seeded workspace is gone: %v", err)
	} else if spec.Root != mustResolve(t, seedRoot) {
		t.Errorf("seed root = %q, want %q", spec.Root, mustResolve(t, seedRoot))
	}

	// Resolve is the hot path: it must return the sandbox for the scope's own
	// workspace, not a shared one.
	wsA, specA, err := m.Resolve(ctx, "web:s1")
	if err != nil {
		t.Fatalf("Resolve(web:s1): %v", err)
	}
	wsB, _, err := m.Resolve(ctx, "feishu:ou_1")
	if err != nil {
		t.Fatalf("Resolve(feishu:ou_1): %v", err)
	}
	if specA.Name != "alpha" {
		t.Errorf("Resolve(web:s1) spec = %q, want alpha", specA.Name)
	}
	if wsA.Root() == wsB.Root() {
		t.Fatalf("both scopes resolved to %q", wsA.Root())
	}
}

// TestSelectRefusalsAreActionable: an unknown name is an answer, not a silent
// fallback to some other directory.
func TestSelectRefusalsAreActionable(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	create(t, m, mkdir(t, "alpha"), "alpha")

	if _, err := m.Select(ctx, "web:s1", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Select(ghost) = %v, want ErrNotFound", err)
	}
	if _, err := m.Select(ctx, "", "alpha"); err == nil {
		t.Error("Select with no scope succeeded")
	}

	// Case-insensitive selection resolves to the stored name.
	if spec, err := m.Select(ctx, "web:s1", "ALPHA"); err != nil || spec.Name != "alpha" {
		t.Errorf("Select(ALPHA) = %q, %v; want alpha", spec.Name, err)
	}
}

// TestRenameKeepsTheDirectoryAndMovesTheConversations: renaming is a label
// change, and nothing about where the agent works may change with it.
func TestRenameKeepsTheDirectoryAndMovesTheConversations(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	spec := create(t, m, mkdir(t, "blog"), "blog")
	if _, err := m.Select(ctx, "web:s1", "blog"); err != nil {
		t.Fatalf("Select: %v", err)
	}

	renamed, err := m.Rename(ctx, "blog", "我的博客")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Root != spec.Root {
		t.Errorf("rename moved the directory: %q -> %q", spec.Root, renamed.Root)
	}
	if _, err := m.Get(ctx, "blog"); !errors.Is(err, ErrNotFound) {
		t.Error("the old name still resolves")
	}
	// The conversation followed the rename rather than being stranded.
	active, err := m.Active(ctx, "web:s1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active.Name != "我的博客" {
		t.Errorf("Active = %q, want the renamed workspace", active.Name)
	}

	create(t, m, mkdir(t, "api"), "api")
	if _, err := m.Rename(ctx, "我的博客", "api"); !errors.Is(err, ErrExists) {
		t.Errorf("rename onto an existing name = %v, want ErrExists", err)
	}
	if _, err := m.Rename(ctx, "ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename of an unknown workspace = %v, want ErrNotFound", err)
	}
}

// TestDeleteMovesConversationsAndKeepsFiles: the two things a person needs to be
// able to believe about deleting a workspace.
func TestDeleteMovesConversationsAndKeepsFiles(t *testing.T) {
	ctx := context.Background()
	m, st, _ := testManager(t)
	if _, err := m.EnsureSeed(ctx); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	create(t, m, mkdir(t, "keep"), "keep")
	doomed := create(t, m, mkdir(t, "doomed"), "doomed")

	file := filepath.Join(doomed.Root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "s1", Title: "s1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := m.Select(ctx, "web:s1", "doomed"); err != nil {
		t.Fatalf("Select: %v", err)
	}

	res, err := m.Delete(ctx, "doomed")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.MovedTo == "" {
		t.Error("delete did not report where the conversations went")
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("deleting the workspace removed a file: %v", err)
	}
	if _, err := m.Get(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	// The conversation still belongs to a workspace — every conversation must.
	active, err := m.Active(ctx, "web:s1")
	if err != nil {
		t.Fatalf("Active after delete: %v", err)
	}
	if active.Name != res.MovedTo || active.Root == "" {
		t.Errorf("conversation landed in %+v, delete reported %q", active, res.MovedTo)
	}

	// The last workspace cannot be deleted: there would be nowhere to put its
	// conversations.
	remaining, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, spec := range remaining[1:] {
		if _, err := m.Delete(ctx, spec.Name); err != nil {
			t.Fatalf("Delete(%s): %v", spec.Name, err)
		}
	}
	last := remaining[0].Name
	if _, err := m.Delete(ctx, last); !errors.Is(err, ErrLastWorkspace) {
		t.Errorf("deleting the last workspace = %v, want ErrLastWorkspace", err)
	}
	if _, err := m.Get(ctx, last); err != nil {
		t.Errorf("the last workspace was removed anyway: %v", err)
	}
}

// TestEnsureSeedRegistersAndBackfills is the invariant for existing data: a
// database with conversations and no workspaces ends up with both.
func TestEnsureSeedRegistersAndBackfills(t *testing.T) {
	ctx := context.Background()
	m, st, seedRoot := testManager(t)

	// Conversations that predate workspaces.
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateChatSession(ctx, store.ChatSession{ID: id, Title: id}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	seed, err := m.EnsureSeed(ctx)
	if err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	// The root is stored resolved: a temp directory on macOS is reached through
	// a symlink, and the sandbox compares against the resolved form.
	if seed.Root != mustResolve(t, seedRoot) {
		t.Errorf("seed root = %q, want %q", seed.Root, mustResolve(t, seedRoot))
	}
	if seed.Name != DefaultNameFor(seedRoot) {
		t.Errorf("seed name = %q, want the directory's base name", seed.Name)
	}

	// Both conversations now belong to it.
	sessions, err := st.ListChatSessions(ctx, store.ChatSessionFilter{})
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	for _, sess := range sessions {
		if sess.Workspace != seed.Name {
			t.Errorf("session %s is in %q, want %q", sess.ID, sess.Workspace, seed.Name)
		}
	}
	if seed.SessionCount != 2 {
		t.Errorf("seed session_count = %d, want 2", seed.SessionCount)
	}

	// Idempotent: a second call adds nothing.
	again, err := m.EnsureSeed(ctx)
	if err != nil {
		t.Fatalf("EnsureSeed again: %v", err)
	}
	if again.Name != seed.Name {
		t.Errorf("second seed changed the workspace: %q -> %q", seed.Name, again.Name)
	}
	specs, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(specs) != 1 {
		t.Errorf("second seed added a workspace: %+v", specs)
	}
}

// TestEnsureSeedCreatesTheConfiguredDirectory: `tools.workspace` was always
// created on demand, and a deployment whose root does not exist yet has to keep
// working the way it did.
func TestEnsureSeedCreatesTheConfiguredDirectory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	missing := filepath.Join(t.TempDir(), "not-yet")
	m, err := New(Options{Store: st, SeedRoot: missing})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec, err := m.EnsureSeed(ctx)
	if err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	if info, serr := os.Stat(spec.Root); serr != nil || !info.IsDir() {
		t.Errorf("the configured directory was not created: %v", serr)
	}
}

// TestDefaultPrefersTheLastConversation: a new conversation should start where
// the last one was, not somewhere arbitrary.
func TestDefaultPrefersTheLastConversation(t *testing.T) {
	ctx := context.Background()
	m, st, _ := testManager(t)
	if _, err := m.EnsureSeed(ctx); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	alpha := create(t, m, mkdir(t, "alpha"), "alpha")

	// With no conversations yet, the default is a real workspace.
	if got, err := m.Default(ctx); err != nil || got.Root == "" {
		t.Fatalf("Default with no conversations = %+v, %v", got, err)
	}

	// One conversation in alpha makes alpha the default.
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "s1", Title: "s1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := m.Select(ctx, "web:s1", alpha.Name); err != nil {
		t.Fatalf("Select: %v", err)
	}
	got, err := m.Default(ctx)
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("Default = %q, want the last conversation's workspace (alpha)", got.Name)
	}
}

// TestActiveFallsBackWhenBindingDangles: a workspace deleted underneath the
// database must not break the conversation that had chosen it.
func TestActiveFallsBackWhenBindingDangles(t *testing.T) {
	ctx := context.Background()
	m, st, _ := testManager(t)
	if _, err := m.EnsureSeed(ctx); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}
	if err := st.SetWorkspaceBinding(ctx, "web:s1", "ghost"); err != nil {
		t.Fatalf("SetWorkspaceBinding: %v", err)
	}
	got, err := m.Active(ctx, "web:s1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got.Name != DefaultNameFor(m.seedRoot) {
		t.Errorf("Active = %q, want the seeded workspace", got.Name)
	}
}

// TestBrowseListsDirectoriesOnly: the picker must offer directories, and must not
// offer files as if they could be a workspace.
func TestBrowseListsDirectoriesOnly(t *testing.T) {
	m, _, _ := testManager(t)
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	listing, err := m.Browse(root)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if listing.Path != mustResolve(t, root) {
		t.Errorf("path = %q, want %q", listing.Path, mustResolve(t, root))
	}
	if listing.Parent != filepath.Dir(mustResolve(t, root)) {
		t.Errorf("parent = %q", listing.Parent)
	}
	var names []string
	for _, e := range listing.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "alpha,beta" {
		t.Errorf("entries = %v, want [alpha beta] (no files, sorted)", names)
	}
	if listing.Home == "" {
		t.Error("home is empty; the picker has no way back")
	}
}

// TestBrowseFollowsSymlinkedDirectories: a symlinked project directory is common
// enough that not following one would hide it from the picker.
func TestBrowseFollowsSymlinkedDirectories(t *testing.T) {
	m, _, _ := testManager(t)
	root := t.TempDir()
	target := mkdir(t, "real-project")
	if err := os.Symlink(target, filepath.Join(root, "project-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	listing, err := m.Browse(root)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(listing.Entries) != 1 || !listing.Entries[0].Link {
		t.Fatalf("entries = %+v, want the symlink marked as one", listing.Entries)
	}
}

// TestBrowseRefusals: a bad path is reported with a reason, and the filesystem
// root is listed but flagged as unchooosable.
func TestBrowseRefusals(t *testing.T) {
	m, _, _ := testManager(t)

	if _, err := m.Browse(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Browse of a missing directory succeeded")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := m.Browse(file); err == nil {
		t.Error("Browse of a file succeeded")
	}

	root, err := m.Browse("/")
	if err != nil {
		t.Fatalf("Browse(/): %v", err)
	}
	if !root.RootTooHigh || root.Parent != "" {
		t.Errorf("the filesystem root should be flagged and parentless: %+v", root)
	}
}

// TestNewValidatesOptions: a manager without a store or a seed root is a wiring
// bug and must fail at construction rather than at the first tool call.
func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{SeedRoot: t.TempDir()}); !errors.Is(err, ErrNoStore) {
		t.Errorf("New without a store = %v, want ErrNoStore", err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := New(Options{Store: st}); err == nil {
		t.Error("New without a seed root succeeded")
	}
}
