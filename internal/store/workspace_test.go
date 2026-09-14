package store

import (
	"context"
	"errors"
	"testing"
)

// TestWorkspaceRoundTrip covers the happy path: a registration survives, points
// at the directory it was given, and reports how many conversations are in it.
func TestWorkspaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "blog", Root: "/tmp/blog"}); err != nil {
		t.Fatalf("UpsertWorkspace: %v", err)
	}
	if err := s.UpsertWorkspace(ctx, Workspace{Name: "api", Root: "/tmp/api"}); err != nil {
		t.Fatalf("UpsertWorkspace(api): %v", err)
	}

	got, err := s.GetWorkspace(ctx, "blog")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Name != "blog" || got.Root != "/tmp/blog" {
		t.Errorf("got %+v, want blog at /tmp/blog", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not filled in")
	}

	list, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 2 || list[0].Name != "api" || list[1].Name != "blog" {
		t.Errorf("list = %+v, want [api blog]", list)
	}
}

// TestWorkspaceSessionCount: the count the sidebar shows comes from the
// bindings, so it cannot drift from where conversations actually are. Feishu
// scopes are excluded: the sidebar counts conversations.
func TestWorkspaceSessionCount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "blog", Root: "/tmp/blog"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, scope := range []string{"web:s1", "web:s2", "feishu:ou_1"} {
		if err := s.SetWorkspaceBinding(ctx, scope, "blog"); err != nil {
			t.Fatalf("bind %s: %v", scope, err)
		}
	}

	got, err := s.GetWorkspace(ctx, "blog")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.SessionCount != 2 {
		t.Errorf("session_count = %d, want 2 (web scopes only)", got.SessionCount)
	}
}

// TestWorkspaceMissingIsNotFound: absent rows must be distinguishable from an
// empty value, because the caller's fallback depends on it.
func TestWorkspaceMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.GetWorkspace(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorkspace = %v, want ErrNotFound", err)
	}
	if err := s.DeleteWorkspace(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteWorkspace = %v, want ErrNotFound", err)
	}
	if err := s.RenameWorkspace(ctx, "nope", "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RenameWorkspace = %v, want ErrNotFound", err)
	}
	if _, err := s.GetWorkspaceBinding(ctx, "web:abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorkspaceBinding = %v, want ErrNotFound", err)
	}
}

// TestWorkspaceNameCaseInsensitiveUnique: two rows answering to one name would
// make which-one-a-scope-means depend on the query, so the schema refuses it.
func TestWorkspaceNameCaseInsensitiveUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "Blog", Root: "/a"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.UpsertWorkspace(ctx, Workspace{Name: "blog", Root: "/b"}); err == nil {
		t.Fatal("a case-variant duplicate was accepted")
	}
}

// TestRenameMovesBindingsAtomically is the property that makes a rename safe: a
// binding left pointing at the old name would strand its conversations — they
// would fall back to another workspace while the sidebar showed them elsewhere.
func TestRenameMovesBindingsAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "blog", Root: "/tmp/blog"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, scope := range []string{"web:s1", "feishu:ou_1"} {
		if err := s.SetWorkspaceBinding(ctx, scope, "blog"); err != nil {
			t.Fatalf("bind %s: %v", scope, err)
		}
	}

	if err := s.RenameWorkspace(ctx, "blog", "我的博客"); err != nil {
		t.Fatalf("RenameWorkspace: %v", err)
	}
	if _, err := s.GetWorkspace(ctx, "blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still resolves: %v", err)
	}
	renamed, err := s.GetWorkspace(ctx, "我的博客")
	if err != nil {
		t.Fatalf("GetWorkspace(renamed): %v", err)
	}
	if renamed.Root != "/tmp/blog" {
		t.Errorf("rename moved the directory: %q", renamed.Root)
	}
	for _, scope := range []string{"web:s1", "feishu:ou_1"} {
		name, err := s.GetWorkspaceBinding(ctx, scope)
		if err != nil || name != "我的博客" {
			t.Errorf("binding %s = %q, %v; want 我的博客", scope, name, err)
		}
	}
	// And the count followed the rename.
	if renamed.SessionCount != 1 {
		t.Errorf("session_count = %d, want 1", renamed.SessionCount)
	}

	// A rename that collides is refused, and nothing changes.
	if err := s.UpsertWorkspace(ctx, Workspace{Name: "api", Root: "/tmp/api"}); err != nil {
		t.Fatalf("upsert api: %v", err)
	}
	if err := s.RenameWorkspace(ctx, "我的博客", "API"); err == nil {
		t.Error("renaming onto an existing name (other case) was accepted")
	}
	if _, err := s.GetWorkspace(ctx, "我的博客"); err != nil {
		t.Errorf("a refused rename still moved the workspace: %v", err)
	}
}

// TestMoveWorkspaceBindings: deleting a workspace must not strand its
// conversations, and the count of what moved is reported to the operator.
func TestMoveWorkspaceBindings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, name := range []string{"from", "to"} {
		if err := s.UpsertWorkspace(ctx, Workspace{Name: name, Root: "/tmp/" + name}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}
	for _, scope := range []string{"web:s1", "web:s2"} {
		if err := s.SetWorkspaceBinding(ctx, scope, "from"); err != nil {
			t.Fatalf("bind: %v", err)
		}
	}
	if err := s.SetWorkspaceBinding(ctx, "web:s3", "to"); err != nil {
		t.Fatalf("bind s3: %v", err)
	}

	n, err := s.MoveWorkspaceBindings(ctx, "from", "to")
	if err != nil {
		t.Fatalf("MoveWorkspaceBindings: %v", err)
	}
	if n != 3 {
		t.Errorf("destination now holds %d, want 3", n)
	}
	for _, scope := range []string{"web:s1", "web:s2", "web:s3"} {
		name, err := s.GetWorkspaceBinding(ctx, scope)
		if err != nil || name != "to" {
			t.Errorf("binding %s = %q, %v; want to", scope, name, err)
		}
	}
}

// TestDeleteWorkspaceRemovesBindings: even when a caller deletes without moving
// first, no binding is left pointing at a workspace that no longer exists.
func TestDeleteWorkspaceRemovesBindings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "ws", Root: "/tmp/ws"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, scope := range []string{"web:s1", "feishu:ou_1"} {
		if err := s.SetWorkspaceBinding(ctx, scope, "ws"); err != nil {
			t.Fatalf("bind: %v", err)
		}
	}
	if err := s.UpsertWorkspace(ctx, Workspace{Name: "keep", Root: "/tmp/keep"}); err != nil {
		t.Fatalf("upsert keep: %v", err)
	}
	if err := s.SetWorkspaceBinding(ctx, "web:other", "keep"); err != nil {
		t.Fatalf("bind other: %v", err)
	}

	if err := s.DeleteWorkspace(ctx, "ws"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	for _, scope := range []string{"web:s1", "feishu:ou_1"} {
		if _, err := s.GetWorkspaceBinding(ctx, scope); !errors.Is(err, ErrNotFound) {
			t.Errorf("binding for %s survived the delete: %v", scope, err)
		}
	}
	if name, err := s.GetWorkspaceBinding(ctx, "web:other"); err != nil || name != "keep" {
		t.Errorf("unrelated binding was removed: %q, %v", name, err)
	}
}

// TestListScopesAndUnboundSessions: the two queries the startup backfill and the
// delete path are built on.
func TestListScopesAndUnboundSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "ws", Root: "/tmp/ws"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Three conversations, one of which already has a workspace.
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := s.CreateChatSession(ctx, ChatSession{ID: id, Title: id}); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	if err := s.SetWorkspaceBinding(ctx, "web:s1", "ws"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	unbound, err := s.ListUnboundWebSessions(ctx)
	if err != nil {
		t.Fatalf("ListUnboundWebSessions: %v", err)
	}
	if len(unbound) != 2 {
		t.Errorf("unbound = %v, want s2 and s3", unbound)
	}

	scopes, err := s.ListScopesInWorkspace(ctx, "ws")
	if err != nil {
		t.Fatalf("ListScopesInWorkspace: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "web:s1" {
		t.Errorf("scopes = %v, want [web:s1]", scopes)
	}
}

// TestListChatSessionsCarriesWorkspace: the sidebar groups by this field, so a
// list that dropped it would leave every conversation unfiled.
func TestListChatSessionsCarriesWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertWorkspace(ctx, Workspace{Name: "blog", Root: "/tmp/blog"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if err := s.CreateChatSession(ctx, ChatSession{ID: id, Title: id}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := s.SetWorkspaceBinding(ctx, "web:s1", "blog"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	list, err := s.ListChatSessions(ctx, ChatSessionFilter{})
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	byID := map[string]string{}
	for _, sess := range list {
		byID[sess.ID] = sess.Workspace
	}
	if byID["s1"] != "blog" {
		t.Errorf("s1 workspace = %q, want blog", byID["s1"])
	}
	if byID["s2"] != "" {
		t.Errorf("s2 workspace = %q, want empty (it has no binding)", byID["s2"])
	}

	// The filter narrows to one workspace.
	filtered, err := s.ListChatSessions(ctx, ChatSessionFilter{Workspace: "blog"})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "s1" {
		t.Errorf("filtered = %+v, want just s1", filtered)
	}

	// And Get carries it too, so an opened conversation knows its workspace
	// without a second request.
	one, err := s.GetChatSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	if one.Workspace != "blog" {
		t.Errorf("GetChatSession workspace = %q, want blog", one.Workspace)
	}
}

// TestInvocationRecordsWorkspace: the audit's answer to "which project" comes
// from this column, and a row written without one must still read back cleanly.
func TestInvocationRecordsWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordInvocation(ctx, InvocationEvent{
		SessionID: "s1", ToolName: "write_file", Arguments: `{"path":"a.go"}`,
		Result: "ok", Workspace: "blog",
	}); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}
	if err := s.RecordInvocation(ctx, InvocationEvent{
		SessionID: "s1", ToolName: "time", Result: "ok",
	}); err != nil {
		t.Fatalf("RecordInvocation(no workspace): %v", err)
	}

	rows, err := s.QueryInvocations(ctx, InvocationFilter{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Newest first: the row without a workspace is the later insert.
	if rows[0].Workspace != "" {
		t.Errorf("unexpected workspace on the second row: %q", rows[0].Workspace)
	}
	if rows[1].Workspace != "blog" {
		t.Errorf("workspace not persisted: %q", rows[1].Workspace)
	}
}

// TestMediaAssetRecordsWorkspace: the attachment's workspace is what makes a
// relative path resolvable after a switch, so it must survive every query path.
func TestMediaAssetRecordsWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	asset := MediaAsset{
		ID: "a1", SessionID: "s1", Kind: MediaKindImage,
		Path: "uploads/a1.png", Workspace: "blog", MIME: "image/png", Bytes: 3,
	}
	if err := s.CreateMediaAsset(ctx, asset); err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}

	got, err := s.GetMediaAsset(ctx, "a1")
	if err != nil {
		t.Fatalf("GetMediaAsset: %v", err)
	}
	if got.Workspace != "blog" {
		t.Errorf("GetMediaAsset workspace = %q, want blog", got.Workspace)
	}
	list, err := s.ListMediaAssets(ctx, "s1")
	if err != nil {
		t.Fatalf("ListMediaAssets: %v", err)
	}
	if len(list) != 1 || list[0].Workspace != "blog" {
		t.Errorf("ListMediaAssets lost the workspace: %+v", list)
	}
}
