package server

// Tests for the console's workspace surface: the sidebar's folders, the
// directory picker, and the property everything else rests on — that a
// conversation belongs to exactly one workspace and switching one leaves every
// other conversation where it was.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

// workspaceHarness is a running console with a real workspace layer.
type workspaceHarness struct {
	*harness
	manager *workspaces.Manager
	// seedRoot is the directory the seeded workspace points at.
	seedRoot string
	// store is the database both the console and the manager use.
	store store.Store
}

// newWorkspaceHarness starts a console whose chat has a workspace layer over the
// same store the console uses.
func newWorkspaceHarness(t *testing.T, turns [][]*schema.Message) *workspaceHarness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedRoot := t.TempDir()
	mgr, err := workspaces.New(workspaces.Options{
		Store:    st,
		SeedRoot: seedRoot,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("workspaces.New: %v", err)
	}
	// The production paths seed at startup; a harness that skipped it would not
	// be testing the state the server actually runs in.
	if _, err := mgr.EnsureSeed(context.Background()); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}

	reg := tool.NewRegistry()
	runner, err := chat.New(chat.Config{
		Model:  &scriptedModel{turns: turns},
		Tools:  reg,
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	ws, err := workspace.New(seedRoot, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	srv, _ := buildServerWith(t, buildOpts{st: st, chat: ChatDeps{
		Runner:     runner,
		Tools:      reg,
		Workspaces: mgr,
		Workspace:  ws,
	}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return &workspaceHarness{harness: h, manager: mgr, seedRoot: seedRoot, store: st}
}

// createWorkspaceViaAPI registers a fresh directory as a workspace.
func (h *workspaceHarness) createWorkspaceViaAPI(t *testing.T, root, name string) workspaces.Spec {
	t.Helper()
	var out struct {
		Workspace workspaces.Spec `json:"workspace"`
		OK        bool            `json:"ok"`
	}
	h.requestJSON(t, http.MethodPost, "/api/workspaces", map[string]any{
		"root": root, "name": name,
	}, http.StatusOK, &out)
	if !out.OK {
		t.Fatalf("create workspace %s failed", name)
	}
	return out.Workspace
}

// decodeJSON requires a status code and decodes the body into out.
func decodeJSON(t *testing.T, resp *http.Response, want int, out any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, want, bodyOf(resp))
	}
	if out == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// bodyOf reads a response body for a failure message.
func bodyOf(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// requestJSON issues a request with a JSON body and decodes the answer.
func (h *harness) requestJSON(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	var resp *http.Response
	switch method {
	case http.MethodPost:
		resp = h.postJSON(t, path, body)
	case http.MethodPut:
		resp = h.putJSON(t, path, body)
	case http.MethodPatch:
		resp = h.patchJSON(t, path, body)
	case http.MethodDelete:
		resp = h.deleteJSON(t, path)
	case http.MethodGet:
		resp = h.get(t, path)
	default:
		t.Fatalf("unsupported method %s", method)
	}
	decodeJSON(t, resp, want, out)
}

// mkdir creates a directory to point a workspace at.
func mkdirForWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return dir
}

// TestWorkspaceAPI_Lifecycle covers the four things the sidebar does: list,
// create from a directory, rename, delete.
func TestWorkspaceAPI_Lifecycle(t *testing.T) {
	h := newWorkspaceHarness(t, nil)

	var list struct {
		OK         bool              `json:"ok"`
		Workspaces []workspaces.Spec `json:"workspaces"`
		Default    string            `json:"default"`
		Home       string            `json:"home"`
	}
	h.getJSON(t, "/api/workspaces", http.StatusOK, &list)
	if !list.OK {
		t.Fatal("list did not report ok")
	}
	// The seeded workspace is there from the start, and it is the default.
	if len(list.Workspaces) != 1 {
		t.Fatalf("got %d workspaces, want the seeded one: %+v", len(list.Workspaces), list.Workspaces)
	}
	seeded := list.Workspaces[0]
	// The stored root is resolved (a temp directory on macOS is reached through a
	// symlink), so the expectation is the resolved form too.
	if seeded.Root != resolvedSeed(t, h.seedRoot) {
		t.Errorf("seeded root = %q, want %q", seeded.Root, resolvedSeed(t, h.seedRoot))
	}
	if list.Default != seeded.Name {
		t.Errorf("default = %q, want the only workspace", list.Default)
	}
	if list.Home == "" {
		t.Error("home is empty; the picker has no starting point")
	}

	// Create one from a directory that exists. Its name defaults to the
	// directory's base name when the caller does not give one.
	root := mkdirForWorkspace(t, "my-project")
	var created struct {
		Workspace workspaces.Spec `json:"workspace"`
	}
	h.requestJSON(t, http.MethodPost, "/api/workspaces", map[string]any{"root": root},
		http.StatusOK, &created)
	if created.Workspace.Name != "my-project" {
		t.Errorf("name = %q, want the directory's base name", created.Workspace.Name)
	}
	if created.Workspace.Root == "" || created.Workspace.Root == root && !strings.Contains(root, string(filepath.Separator)) {
		t.Errorf("root = %q", created.Workspace.Root)
	}

	// Rename it: the label changes, the directory does not.
	renamedRoot := created.Workspace.Root
	var renamed struct {
		From      string          `json:"from"`
		Workspace workspaces.Spec `json:"workspace"`
	}
	h.requestJSON(t, http.MethodPatch, "/api/workspaces/my-project", map[string]any{"name": "我的项目"},
		http.StatusOK, &renamed)
	if renamed.Workspace.Name != "我的项目" || renamed.Workspace.Root != renamedRoot {
		t.Errorf("rename = %+v, want the same root under a new name", renamed.Workspace)
	}
	if renamed.From != "my-project" {
		t.Errorf("from = %q, want my-project", renamed.From)
	}

	// The old name is gone, and the new one resolves.
	h.getJSON(t, "/api/workspaces/my-project", http.StatusNotFound, nil)
	h.getJSON(t, "/api/workspaces/"+urlEscape("我的项目"), http.StatusOK, nil)

	// Delete it: the files stay, and the answer says where its conversations went.
	var deleted struct {
		MovedTo string `json:"moved_to"`
		Note    string `json:"note"`
	}
	h.requestJSON(t, http.MethodDelete, "/api/workspaces/"+urlEscape("我的项目"), nil, http.StatusOK, &deleted)
	if deleted.MovedTo == "" {
		t.Error("delete did not report where the conversations went")
	}
	if !strings.Contains(deleted.Note, "未") {
		t.Errorf("delete note = %q, want it to say the files are untouched", deleted.Note)
	}
	if _, err := os.Stat(renamedRoot); err != nil {
		t.Errorf("deleting the workspace removed the directory: %v", err)
	}
}

// TestWorkspaceAPI_RejectsBadInput: a picked directory that is not one, and a
// duplicate label, are refused with a reason.
func TestWorkspaceAPI_RejectsBadInput(t *testing.T) {
	h := newWorkspaceHarness(t, nil)

	for _, tc := range []struct {
		root, name, why string
	}{
		{filepath.Join(t.TempDir(), "missing"), "", "missing directory"},
		{"/", "", "filesystem root"},
		{"", "", "empty path"},
	} {
		resp := h.postJSON(t, "/api/workspaces", map[string]any{"root": tc.root, "name": tc.name})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create root=%q: status = %d, want 400 (%s)", tc.root, resp.StatusCode, bodyOf(resp))
		}
		_ = resp.Body.Close()
	}

	// A file is not a directory.
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := h.postJSON(t, "/api/workspaces", map[string]any{"root": file})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create from a file: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// A duplicate label is a conflict, not a bad request: the input is valid.
	seeded, err := h.manager.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	resp = h.postJSON(t, "/api/workspaces", map[string]any{
		"root": mkdirForWorkspace(t, "other"), "name": strings.ToUpper(seeded[0].Name),
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name: status = %d, want 409 (%s)", resp.StatusCode, bodyOf(resp))
	}
	_ = resp.Body.Close()
}

// TestWorkspaceAPI_LastOneCannotBeDeleted: a conversation must belong to a
// workspace, so the last one has to stay.
func TestWorkspaceAPI_LastOneCannotBeDeleted(t *testing.T) {
	h := newWorkspaceHarness(t, nil)
	specs, err := h.manager.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected exactly one workspace, got %d", len(specs))
	}
	resp := h.deleteJSON(t, "/api/workspaces/"+urlEscape(specs[0].Name))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("deleting the last workspace: status = %d, want 409 (%s)", resp.StatusCode, bodyOf(resp))
	}
	_ = resp.Body.Close()
}

// TestWorkspaceAPI_BrowseListsDirectories: the picker is driven by the server's
// filesystem, and offers directories only.
func TestWorkspaceAPI_BrowseListsDirectories(t *testing.T) {
	h := newWorkspaceHarness(t, nil)
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var got struct {
		OK      bool `json:"ok"`
		Path    string
		Parent  string
		Entries []workspaces.DirEntry
	}
	h.getJSON(t, "/api/fs/dirs?path="+urlEscape(root), http.StatusOK, &got)
	if !got.OK {
		t.Fatal("browse did not report ok")
	}
	var names []string
	for _, e := range got.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "alpha,beta" {
		t.Errorf("entries = %v, want [alpha beta] (directories only, sorted)", names)
	}
	if got.Parent == "" {
		t.Error("parent is empty; the picker cannot go up")
	}

	// A bad path answers 200 with ok=false and a reason: the picker shows the
	// reason in place rather than replacing the view with an error.
	var bad struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	h.getJSON(t, "/api/fs/dirs?path="+urlEscape(filepath.Join(t.TempDir(), "nope")), http.StatusOK, &bad)
	if bad.OK || bad.Error == "" {
		t.Errorf("browse of a missing directory = %+v, want ok=false with a reason", bad)
	}
}

// TestSessionAlwaysBelongsToAWorkspace is the invariant the sidebar grouping
// depends on: a new conversation is filed immediately, not on its first message.
func TestSessionAlwaysBelongsToAWorkspace(t *testing.T) {
	h := newWorkspaceHarness(t, nil)
	other := h.createWorkspaceViaAPI(t, mkdirForWorkspace(t, "other"), "other")

	// Without an explicit choice, the conversation starts in the default.
	first := createSession(t, h.harness)
	var detail struct {
		Session struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		} `json:"session"`
		Workspace workspaces.Spec `json:"workspace"`
	}
	h.getJSON(t, "/api/chat/sessions/"+first, http.StatusOK, &detail)
	if detail.Session.Workspace == "" {
		t.Fatal("a new conversation has no workspace")
	}
	if detail.Workspace.Name != detail.Session.Workspace {
		t.Errorf("session says %q, workspace block says %q",
			detail.Session.Workspace, detail.Workspace.Name)
	}

	// A conversation created with an explicit workspace is filed there.
	var created struct {
		Session struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		} `json:"session"`
	}
	h.requestJSON(t, http.MethodPost, "/api/chat/sessions", map[string]any{"workspace": "other"},
		http.StatusOK, &created)
	if created.Session.Workspace != other.Name {
		t.Errorf("explicit workspace = %q, want %q", created.Session.Workspace, other.Name)
	}

	// An unknown workspace is refused rather than silently filed elsewhere.
	resp := h.postJSON(t, "/api/chat/sessions", map[string]any{"workspace": "ghost"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown workspace on create: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The list carries the workspace for every row, which is what the sidebar
	// groups by.
	var list struct {
		Sessions []struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		} `json:"sessions"`
	}
	h.getJSON(t, "/api/chat/sessions", http.StatusOK, &list)
	if len(list.Sessions) < 2 {
		t.Fatalf("expected at least 2 sessions, got %d", len(list.Sessions))
	}
	for _, sess := range list.Sessions {
		if sess.Workspace == "" {
			t.Errorf("session %s has no workspace in the list", sess.ID)
		}
	}

	// And the workspace counts follow.
	specs, err := h.manager.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	total := 0
	for _, spec := range specs {
		total += spec.SessionCount
	}
	if total != len(list.Sessions) {
		t.Errorf("workspaces account for %d conversations, the list has %d", total, len(list.Sessions))
	}
}

// TestSessionWorkspaceSwitchIsPerSession: two conversations, two directories, no
// interference.
func TestSessionWorkspaceSwitchIsPerSession(t *testing.T) {
	h := newWorkspaceHarness(t, nil)
	alpha := h.createWorkspaceViaAPI(t, mkdirForWorkspace(t, "alpha"), "alpha")
	beta := h.createWorkspaceViaAPI(t, mkdirForWorkspace(t, "beta"), "beta")

	first := createSession(t, h.harness)
	second := createSession(t, h.harness)

	var switched struct {
		From      string          `json:"from"`
		Workspace workspaces.Spec `json:"workspace"`
	}
	h.requestJSON(t, http.MethodPut, "/api/chat/sessions/"+first+"/workspace",
		map[string]any{"name": "alpha"}, http.StatusOK, &switched)
	if switched.Workspace.Name != "alpha" || switched.Workspace.Root != alpha.Root {
		t.Errorf("switch = %+v, want alpha at %s", switched.Workspace, alpha.Root)
	}
	h.requestJSON(t, http.MethodPut, "/api/chat/sessions/"+second+"/workspace",
		map[string]any{"name": "beta"}, http.StatusOK, &switched)
	if switched.Workspace.Root != beta.Root {
		t.Errorf("second session switched to %q, want %q", switched.Workspace.Root, beta.Root)
	}

	// The manager agrees, and each scope kept its own choice.
	for scope, want := range map[string]string{
		workspaces.WebScope(first):  "alpha",
		workspaces.WebScope(second): "beta",
	} {
		spec, err := h.manager.Active(context.Background(), scope)
		if err != nil {
			t.Fatalf("Active(%s): %v", scope, err)
		}
		if spec.Name != want {
			t.Errorf("Active(%s) = %q, want %q", scope, spec.Name, want)
		}
	}

	// An unknown workspace is refused and changes nothing.
	resp := h.putJSON(t, "/api/chat/sessions/"+first+"/workspace", map[string]any{"name": "ghost"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("switching to an unknown workspace: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if spec, _ := h.manager.Active(context.Background(), workspaces.WebScope(first)); spec.Name != "alpha" {
		t.Errorf("a refused switch moved the session to %q", spec.Name)
	}

	// Switching an unknown session is 404 rather than a new binding.
	resp = h.putJSON(t, "/api/chat/sessions/nope/workspace", map[string]any{"name": "alpha"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestSessionListFiltersByWorkspace: the sidebar may fold a folder, and the API
// can answer for one workspace alone.
func TestSessionListFiltersByWorkspace(t *testing.T) {
	h := newWorkspaceHarness(t, nil)
	other := h.createWorkspaceViaAPI(t, mkdirForWorkspace(t, "other"), "other")

	// The conversation that stays in the default is created first: a new
	// conversation follows the most recently used workspace, so creating it after
	// the switch below would put it in `other` too.
	inDefault := createSession(t, h.harness)
	inOther := createSession(t, h.harness)
	h.requestJSON(t, http.MethodPut, "/api/chat/sessions/"+inOther+"/workspace",
		map[string]any{"name": other.Name}, http.StatusOK, nil)
	if inDefault == inOther {
		t.Fatal("both sessions got the same id")
	}

	var list struct {
		Sessions []struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		} `json:"sessions"`
	}
	h.getJSON(t, "/api/chat/sessions?workspace="+urlEscape(other.Name), http.StatusOK, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != inOther {
		t.Errorf("filtered list = %+v, want only the conversation in %q", list.Sessions, other.Name)
	}
}

// TestWorkspaceAPI_Disabled: a server wired without the workspace layer answers
// nothing about it, rather than 404-ing every route in a confusing way.
func TestWorkspaceAPI_Disabled(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{})
	for _, path := range []string{"/api/workspaces", "/api/fs/dirs"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 when the feature is not wired", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// resolvedSeed returns the seed directory in the form the sandbox stores it.
func resolvedSeed(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

// urlEscape escapes a path segment for a URL.
func urlEscape(s string) string {
	return strings.NewReplacer(" ", "%20", "/", "%2F", "?", "%3F", "#", "%23").Replace(s)
}
