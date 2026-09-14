package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/documents"
	"github.com/huan/huan-agent/internal/viking"
)

// stubConsole is a scriptable OpenVikingConsole, so the console's routes can be
// tested without an OpenViking server.
type stubConsole struct {
	status    viking.Status
	report    documents.Report
	syncErr   error
	saveURI   string
	saveErr   error
	saved     []documents.Document
	documents []documents.Entry
	flushed   int
	lastFull  bool
}

func (s *stubConsole) Status(context.Context) viking.Status { return s.status }

func (s *stubConsole) SyncWorkspace(_ context.Context, full bool) (documents.Report, error) {
	s.lastFull = full
	return s.report, s.syncErr
}

func (s *stubConsole) SaveDocument(_ context.Context, doc documents.Document) (string, error) {
	s.saved = append(s.saved, doc)
	return s.saveURI, s.saveErr
}

func (s *stubConsole) SyncedDocuments() []documents.Entry { return s.documents }

func (s *stubConsole) Flush(context.Context) { s.flushed++ }

// newOpenVikingHarness starts a server whose console talks to stub.
func newOpenVikingHarness(t *testing.T, stub OpenVikingConsole) *harness {
	t.Helper()
	srv, st := buildServerWith(t, buildOpts{openviking: stub})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h
}

func TestOpenVikingAPI_StatusReportsDisabledWithoutError(t *testing.T) {
	// No console configured: the panel must be able to say "not configured"
	// instead of showing a failure the operator cannot clear.
	h := newOpenVikingHarness(t, nil)

	var body struct {
		Enable  bool   `json:"enable"`
		Message string `json:"message"`
	}
	h.getJSON(t, "/api/openviking/status", http.StatusOK, &body)
	if body.Enable {
		t.Error("enable = true, want false when the integration is not configured")
	}
	if body.Message == "" {
		t.Error("message is empty, want a hint at which switch to flip")
	}
}

func TestOpenVikingAPI_StatusReturnsTheIntegrationState(t *testing.T) {
	stub := &stubConsole{status: viking.Status{
		Enable:    true,
		BaseURL:   "http://127.0.0.1:1933",
		Account:   "default",
		User:      "default",
		Subtree:   "viking://user/default/huan-agent",
		Connected: true,
		Version:   "0.4.19",
		AuthMode:  "dev",
		Memory:    viking.MemoryStatus{Enable: true, Commit: true},
		Documents: viking.DocumentsStatus{
			Enable: true, RootURI: "viking://user/default/huan-agent", Tracked: 2, WorkspaceDir: "/tmp/ws",
		},
		MCP: viking.MCPStatus{Registered: true, Name: "openviking"},
	}}
	h := newOpenVikingHarness(t, stub)

	var body viking.Status
	h.getJSON(t, "/api/openviking/status", http.StatusOK, &body)
	if !body.Enable || !body.Connected || body.Version != "0.4.19" {
		t.Errorf("status = %+v, want the stub's state", body)
	}
	if !body.MCP.Registered || body.MCP.Name != "openviking" {
		t.Errorf("mcp = %+v, want it registered", body.MCP)
	}
}

func TestOpenVikingAPI_SyncReturnsReport(t *testing.T) {
	stub := &stubConsole{report: documents.Report{
		Scanned: 3, Uploaded: 2, Unchanged: 1, RootURI: "viking://user/default/huan-agent",
	}}
	h := newOpenVikingHarness(t, stub)

	resp := h.postJSON(t, "/api/openviking/sync", map[string]any{"full": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK     bool             `json:"ok"`
		Report documents.Report `json:"report"`
	}
	decodeBody(t, resp, &body)
	if !body.OK || body.Report.Uploaded != 2 {
		t.Errorf("body = %+v, want ok with 2 uploads", body)
	}
	if stub.lastFull {
		t.Error("full = true, want the incremental default")
	}
}

func TestOpenVikingAPI_SyncFullFlagIsPassed(t *testing.T) {
	stub := &stubConsole{}
	h := newOpenVikingHarness(t, stub)

	resp := h.postJSON(t, "/api/openviking/sync", map[string]any{"full": true})
	requireStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	if !stub.lastFull {
		t.Error("full = false, want the flag to reach the syncer")
	}
}

func TestOpenVikingAPI_SyncWithoutWorkspaceIsAResultNotAnError(t *testing.T) {
	stub := &stubConsole{syncErr: documents.ErrNoWorkspace}
	h := newOpenVikingHarness(t, stub)

	resp := h.postJSON(t, "/api/openviking/sync", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeBody(t, resp, &body)
	if body.OK || body.Error == "" {
		t.Errorf("body = %+v, want ok=false with the reason", body)
	}
}

func TestOpenVikingAPI_SyncWhileRunningIsAConflict(t *testing.T) {
	stub := &stubConsole{syncErr: documents.ErrSyncing}
	h := newOpenVikingHarness(t, stub)

	// A sync already in flight is a state, not a server error, so it is a 409
	// the console can phrase as "already running".
	resp := h.postJSON(t, "/api/openviking/sync", nil)
	requireStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
}

func TestOpenVikingAPI_SaveValidatesAndStores(t *testing.T) {
	stub := &stubConsole{saveURI: "viking://user/default/huan-agent/documents/2026/09/x.md"}
	h := newOpenVikingHarness(t, stub)

	// Missing fields are a client error, so the form can point at the field.
	resp := h.postJSON(t, "/api/openviking/save", map[string]any{"title": "only a title"})
	requireStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	resp = h.postJSON(t, "/api/openviking/save", map[string]any{
		"title": "设计笔记", "content": "正文", "tags": []string{"design"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK  bool   `json:"ok"`
		URI string `json:"uri"`
	}
	decodeBody(t, resp, &body)
	if !body.OK || body.URI == "" {
		t.Fatalf("body = %+v, want the stored uri", body)
	}
	if len(stub.saved) != 1 {
		t.Fatalf("saved = %d, want 1", len(stub.saved))
	}
	if got := stub.saved[0].Source; got != "console" {
		t.Errorf("source = %q, want console", got)
	}
}

func TestOpenVikingAPI_SaveFailureIsReportedInBody(t *testing.T) {
	stub := &stubConsole{saveErr: errors.New("openviking: POST /api/v1/content/write: HTTP 500: BOOM")}
	h := newOpenVikingHarness(t, stub)

	resp := h.postJSON(t, "/api/openviking/save", map[string]any{"title": "t", "content": "c"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeBody(t, resp, &body)
	if body.OK || body.Error == "" {
		t.Errorf("body = %+v, want ok=false with the reason", body)
	}
}

func TestOpenVikingAPI_DocumentsListsWhatWasStored(t *testing.T) {
	when := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	stub := &stubConsole{documents: []documents.Entry{
		{Path: "docs/a.md", URI: "viking://user/default/huan-agent/workspace/docs/a.md", Size: 12, SyncedAt: when, Kind: "workspace"},
	}}
	h := newOpenVikingHarness(t, stub)

	var body struct {
		OK        bool              `json:"ok"`
		Documents []documents.Entry `json:"documents"`
	}
	h.getJSON(t, "/api/openviking/documents", http.StatusOK, &body)
	if len(body.Documents) != 1 || body.Documents[0].Path != "docs/a.md" {
		t.Errorf("documents = %+v, want the stub's entry", body.Documents)
	}
}

func TestOpenVikingAPI_DocumentsIsAnEmptyListNotNull(t *testing.T) {
	// The UI renders this array directly; null would need a special case there.
	h := newOpenVikingHarness(t, &stubConsole{})

	var body struct {
		Documents []documents.Entry `json:"documents"`
	}
	h.getJSON(t, "/api/openviking/documents", http.StatusOK, &body)
	if body.Documents == nil {
		t.Error("documents = null, want an empty array")
	}
}

func TestOpenVikingAPI_FlushCallsThrough(t *testing.T) {
	stub := &stubConsole{}
	h := newOpenVikingHarness(t, stub)

	resp := h.postJSON(t, "/api/openviking/flush", nil)
	requireStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	if stub.flushed != 1 {
		t.Errorf("flushed = %d, want 1", stub.flushed)
	}
}

func TestOpenVikingAPI_DisabledRoutesStillAnswer(t *testing.T) {
	// Every route must stay renderable with the integration off, which is the
	// state a fresh install is in.
	h := newOpenVikingHarness(t, nil)

	for _, path := range []string{"/api/openviking/status", "/api/openviking/documents"} {
		var body struct {
			Enable bool `json:"enable"`
		}
		h.getJSON(t, path, http.StatusOK, &body)
		if body.Enable {
			t.Errorf("%s: enable = true, want false", path)
		}
	}
	for _, path := range []string{"/api/openviking/sync", "/api/openviking/flush"} {
		resp := h.postJSON(t, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		var body struct {
			Enable bool `json:"enable"`
		}
		decodeBody(t, resp, &body)
		if body.Enable {
			t.Errorf("%s: enable = true, want false", path)
		}
	}
}

func TestOpenVikingAPI_RequiresASession(t *testing.T) {
	srv, _ := buildServerWith(t, buildOpts{openviking: &stubConsole{}})
	startHarness(t, srv)
	// A client with no cookie jar: the route is behind the same session check as
	// the rest of the console.
	unauth := &harness{base: "http://" + srv.Addr(), client: &http.Client{}}
	resp := unauth.get(t, "/api/openviking/status")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
