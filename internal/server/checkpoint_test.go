package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/checkpoint"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspaces"
)

// TestCheckpointRoutesAreAbsentWhenDisabled: a console that offers a rollback
// button for a deployment with no checkpoints is offering a button that always
// fails.
func TestCheckpointRoutesAreAbsentWhenDisabled(t *testing.T) {
	h := newHarness(t, nil)
	h.login(t)

	resp := h.get(t, "/api/chat/sessions/whatever/turns/1/checkpoint")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when checkpoints are not configured", resp.StatusCode)
	}
}

// TestCheckpointEndpoints covers the two routes over real HTTP.
//
// The session is in a freshly seeded workspace, so the path resolution the
// endpoints depend on is the real one rather than a stub.
func TestCheckpointEndpoints(t *testing.T) {
	dir := t.TempDir()
	h, sessionID := newCheckpointHarness(t, dir)
	h.login(t)

	// There is no checkpoint yet: the file has not been changed through a tool.
	resp := h.get(t, "/api/chat/sessions/"+sessionID+"/turns/1/checkpoint")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 before anything was captured", resp.StatusCode)
	}

	// A turn number that is not a number is a client error, not a 404.
	bad := h.get(t, "/api/chat/sessions/"+sessionID+"/turns/nope/checkpoint")
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-numeric turn", bad.StatusCode)
	}

	// An unknown session is a 404.
	missing := h.get(t, "/api/chat/sessions/no-such-session/turns/1/checkpoint")
	defer func() { _ = missing.Body.Close() }()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown session", missing.StatusCode)
	}
}

// TestCheckpointRoundTripOverHTTP is the whole console feature: a turn changes a
// file, the checkpoint records it, and the rollback puts it back.
func TestCheckpointRoundTripOverHTTP(t *testing.T) {
	dir := t.TempDir()
	h, sessionID := newCheckpointHarness(t, dir)
	h.login(t)

	// The workspace the session resolved to is the one the checkpointer must use;
	// ask the server for it the same way the endpoints do.
	sess, err := h.store.GetChatSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	ws := h.srv.checkpointWorkspaceFor(context.Background(), sess)
	if ws == nil {
		t.Fatal("the session has no workspace, so checkpoints cannot resolve paths")
	}
	cp := h.srv.checkpointFor(ws)
	if cp == nil {
		t.Fatal("no checkpointer for the session's workspace")
	}

	// A file in that workspace, captured and then changed the way a turn would.
	path := filepath.Join(ws.sandbox.Root(), "notes.txt")
	if err := os.WriteFile(path, []byte("before the turn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cp.Capture(ctx, sessionID, 1, "notes.txt"); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(path, []byte("the turn's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cp.RecordAfter(ctx, sessionID, 1, "notes.txt"); err != nil {
		t.Fatalf("RecordAfter: %v", err)
	}

	// The endpoint reports what the checkpoint holds.
	resp := h.get(t, "/api/chat/sessions/"+sessionID+"/turns/1/checkpoint")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	var got struct {
		Files   []checkpoint.FileEntry `json:"files"`
		Summary string                 `json:"summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "notes.txt" {
		t.Errorf("files = %+v", got.Files)
	}
	if got.Summary == "" {
		t.Error("the response carries no summary, so a UI has nothing to show")
	}

	// And the rollback restores it without a force.
	rb := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turns/1/rollback", map[string]any{})
	defer func() { _ = rb.Body.Close() }()
	if rb.StatusCode != http.StatusOK {
		t.Fatalf("rollback status = %d", rb.StatusCode)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "before the turn\n" {
		t.Errorf("file content = %q, want the pre-image", body)
	}
}

// TestRollbackConflictIsAConflict: the answer a UI branches on to offer the force.
func TestRollbackConflictIsAConflict(t *testing.T) {
	dir := t.TempDir()
	h, sessionID := newCheckpointHarness(t, dir)
	h.login(t)

	sess, _ := h.store.GetChatSession(context.Background(), sessionID)
	ws := h.srv.checkpointWorkspaceFor(context.Background(), sess)
	cp := h.srv.checkpointFor(ws)

	path := filepath.Join(ws.sandbox.Root(), "notes.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cp.Capture(ctx, sessionID, 1, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	// The turn changed it, and then something else did — neither matches.
	if err := os.WriteFile(path, []byte("the turn's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cp.RecordAfter(ctx, sessionID, 1, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("someone else's newer work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turns/1/rollback", map[string]any{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "someone else's newer work\n" {
		t.Errorf("the newer work was overwritten: %q", body)
	}

	// With force it goes back.
	forced := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turns/1/rollback", map[string]any{"force": true})
	defer func() { _ = forced.Body.Close() }()
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("forced status = %d", forced.StatusCode)
	}
	body, _ = os.ReadFile(path)
	if string(body) != "before\n" {
		t.Errorf("forced rollback left %q", body)
	}
}

// TestTurnInstallsACheckpointTurn: the console's turn bookkeeping has to open a
// turn, or captures made during it would have nowhere to be filed.
func TestTurnInstallsACheckpointTurn(t *testing.T) {
	dir := t.TempDir()
	h, sessionID := newCheckpointHarness(t, dir)
	h.login(t)

	sess, err := h.store.GetChatSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, end := h.srv.beginCheckpointTurn(context.Background(), sess)
	defer end()

	if _, ok := checkpoint.TurnFrom(ctx); !ok {
		t.Fatal("no checkpoint turn was installed on the context; captures during the turn would be filed nowhere")
	}
	// And the directory exists, so the turn was really opened.
	turns, err := os.ReadDir(filepath.Join(h.srv.checkpointSettings.Dir, sessionID))
	if err != nil {
		t.Fatalf("the checkpoint directory for the session was not created: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("turn directories = %d, want 1", len(turns))
	}
}

// newCheckpointHarness builds a console with checkpoints on, in a temp directory,
// plus a session whose workspace is that directory.
func newCheckpointHarness(t *testing.T, dir string) (*harness, string) {
	// dir is the workspace the session resolves to; the checkpoint directory is a
	// separate temp directory, as it is in production (beside the database).
	t.Helper()

	// A turn that changes nothing is enough: this test drives the checkpoint layer
	// directly for the capture, and needs the turn machinery only to open and close.
	runner, err := chat.New(chat.Config{
		Model:    &scriptedModel{turns: [][]*schema.Message{{{Role: schema.Assistant, Content: "ok"}}}},
		Tools:    tool.NewRegistry(),
		MaxSteps: 2,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	// A real workspace layer, because the checkpoints resolve paths through the
	// session's workspace and a nil one would make every endpoint answer "no
	// workspace" — which is a real state, just not the one under test.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := workspaces.New(workspaces.Options{Store: st, SeedRoot: dir, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("workspaces.New: %v", err)
	}
	if _, err := mgr.EnsureSeed(context.Background()); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{
		st:   st,
		chat: ChatDeps{Runner: runner, Tools: tool.NewRegistry(), Workspaces: mgr},
		checkpoints: CheckpointSettings{
			Enable:     true,
			Dir:        filepath.Join(t.TempDir(), "cps"),
			KeepTurns:  5,
			MaxTotalMB: 64,
		},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, createSession(t, h)
}
