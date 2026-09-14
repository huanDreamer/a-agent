package viking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/documents"
	"github.com/huan/huan-agent/internal/memory"
)

// fakeServer stands in for an OpenViking server: enough of the API for the
// facade to exercise its whole path (health, memory write, content write,
// search) without one running.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	written  map[string]string
	// healthStatus lets a test make the server look unhealthy.
	healthStatus int
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{written: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		status := f.healthStatus
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			if status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"status":"error","error":{"code":"DOWN","message":"not ready"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"ok","healthy":true,"version":"0.4.19","auth_mode":"dev"}`)
		case r.URL.Path == "/api/v1/content/write":
			var req struct {
				URI     string `json:"uri"`
				Content string `json:"content"`
				Mode    string `json:"mode"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			if req.Mode == "append" {
				if _, ok := f.written[req.URI]; !ok {
					f.mu.Unlock()
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"status":"error","error":{"code":"NOT_FOUND","message":"no such file"}}`)
					return
				}
				f.written[req.URI] += req.Content
			} else {
				f.written[req.URI] = req.Content
			}
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"status":"ok","result":{"uri":%q,"written_bytes":%d}}`, req.URI, len(req.Content))
		case strings.Contains(r.URL.Path, "/messages/batch"):
			_, _ = io.WriteString(w, `{"status":"ok","result":{"added":1}}`)
		case strings.HasSuffix(r.URL.Path, "/commit"):
			_, _ = io.WriteString(w, `{"status":"ok","result":{"status":"accepted","task_id":"t-1"}}`)
		case r.URL.Path == "/api/v1/search/find":
			_, _ = io.WriteString(w, `{"status":"ok","result":{"memories":[{"uri":"viking://user/default/memories/m.md","abstract":"Alice 负责部署","score":0.9}],"total":1}}`)
		default:
			_, _ = io.WriteString(w, `{"status":"ok","result":null}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) writes() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.written))
	for k, v := range f.written {
		out[k] = v
	}
	return out
}

func (f *fakeServer) sawRequest(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r == method+" "+path {
			return true
		}
	}
	return false
}

// enabledConfig returns a config pointed at srv.
func enabledConfig(t *testing.T, srv *fakeServer) config.OpenVikingConfig {
	t.Helper()
	cfg := config.Default().OpenViking
	cfg.Enable = true
	cfg.BaseURL = srv.URL
	cfg.Documents.StatePath = filepath.Join(t.TempDir(), "state.json")
	cfg.TimeoutSeconds = 5
	return cfg
}

func TestNewRefusesDisabledConfig(t *testing.T) {
	_, err := New(config.Default().OpenViking, config.ToolsConfig{}, nil)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("error = %v, want ErrDisabled", err)
	}
	// Enabled but with no address is still not usable: a service must not be
	// built pointing somewhere the operator never chose.
	cfg := config.Default().OpenViking
	cfg.Enable = true
	cfg.BaseURL = "  "
	if cfg.Enabled() {
		t.Fatal("Enabled() = true with a blank base_url, want false")
	}
	if _, err := New(cfg, config.ToolsConfig{}, nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want ErrDisabled", err)
	}
}

func TestStatusReportsConnectionAndCounters(t *testing.T) {
	srv := newFakeServer(t)
	cfg := enabledConfig(t, srv)
	svc, err := New(cfg, config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A memory write, so the counters are not all zero.
	store := svc.WrapStore(mustLocalStore(t))
	if err := store.AddFact(context.Background(), "ns", memory.Fact{Value: "Alice leads deployment"}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	st := svc.Status(context.Background())
	if !st.Enable || !st.Connected {
		t.Fatalf("status = %+v, want connected", st)
	}
	if st.Version != "0.4.19" || st.AuthMode != "dev" {
		t.Errorf("version/auth = %q/%q, want 0.4.19/dev", st.Version, st.AuthMode)
	}
	if got, want := st.Subtree, "viking://user/default/huan-agent"; got != want {
		t.Errorf("subtree = %q, want %q", got, want)
	}
	if !st.Memory.Enable || st.Memory.Stats.Facts == 0 {
		t.Errorf("memory = %+v, want the facts counter to move", st.Memory)
	}
	if !st.MCP.Registered || st.MCP.Name != "openviking" {
		t.Errorf("mcp = %+v, want it registered by default", st.MCP)
	}
	if st.HealthError != "" {
		t.Errorf("HealthError = %q, want empty on a healthy server", st.HealthError)
	}
}

func TestStatusReportsAnUnhealthyServerAsData(t *testing.T) {
	srv := newFakeServer(t)
	srv.healthStatus = http.StatusServiceUnavailable
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := svc.Status(context.Background())
	if st.Connected {
		t.Error("Connected = true, want false")
	}
	if st.HealthError == "" {
		t.Error("HealthError is empty, want the reason the panel shows")
	}
}

func TestWrapStoreIsSharedAndMirrors(t *testing.T) {
	srv := newFakeServer(t)
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	local := mustLocalStore(t)
	first := svc.WrapStore(local)
	second := svc.WrapStore(mustLocalStore(t))
	if first != second {
		t.Error("WrapStore built a second mirror; the batching and counters must be process-wide")
	}
	if svc.Mirror() == nil {
		t.Fatal("Mirror() = nil, want the mirror")
	}

	// A fact reaches both the journal and the extraction path.
	if err := first.AddFact(context.Background(), "ns", memory.Fact{Key: "owner", Value: "Alice"}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	writes := srv.writes()
	journal := ""
	for uri := range writes {
		if strings.HasSuffix(uri, "/memory/journal.md") {
			journal = uri
		}
	}
	if journal == "" {
		t.Fatalf("writes = %v, want the fact journal", writes)
	}
	if !strings.Contains(writes[journal], "owner: Alice") {
		t.Errorf("journal = %q, want the fact in it", writes[journal])
	}
	if !srv.sawRequest("POST", "/api/v1/sessions/huan-agent-ns/messages/batch") {
		t.Errorf("requests = %v, want the extraction submission", srv.requests)
	}
}

func TestWrapStoreWithoutMemoryMirrorReturnsLocal(t *testing.T) {
	srv := newFakeServer(t)
	cfg := enabledConfig(t, srv)
	cfg.Memory.Enable = false
	svc, err := New(cfg, config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	local := mustLocalStore(t)
	if got := svc.WrapStore(local); got != local {
		t.Error("WrapStore wrapped the store though the memory mirror is off")
	}
	if svc.Mirror() != nil {
		t.Error("Mirror() != nil, want nil when the mirror is off")
	}
}

func TestSaveDocumentWritesUnderTheConfiguredRoot(t *testing.T) {
	srv := newFakeServer(t)
	cfg := enabledConfig(t, srv)
	cfg.Documents.RootURI = "viking://user/huan/notes"
	svc, err := New(cfg, config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri, err := svc.SaveDocument(context.Background(), documents.Document{Title: "Design", Content: "body"})
	if err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	if !strings.HasPrefix(uri, "viking://user/huan/notes/documents/") {
		t.Errorf("uri = %q, want it under the configured root", uri)
	}
	if got := len(svc.SyncedDocuments()); got != 1 {
		t.Errorf("SyncedDocuments = %d, want 1", got)
	}
}

func TestSyncWorkspaceRefusesWithoutADirectory(t *testing.T) {
	srv := newFakeServer(t)
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.SyncWorkspace(context.Background(), false); !errors.Is(err, documents.ErrNoWorkspace) {
		t.Fatalf("error = %v, want ErrNoWorkspace (and not a crawl of the working directory)", err)
	}
}

func TestSyncWorkspaceUsesToolsWorkspaceAndRecordsTheReport(t *testing.T) {
	srv := newFakeServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# notes\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{Workspace: dir}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rep, err := svc.SyncWorkspace(context.Background(), false)
	if err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	if rep.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1", rep.Uploaded)
	}
	if got, want := svc.WorkspaceDir(), filepath.Clean(dir); got != want {
		t.Errorf("WorkspaceDir = %q, want %q", got, want)
	}
	st := svc.Status(context.Background())
	if st.Documents.LastSync == nil || st.Documents.LastSync.Uploaded != 1 {
		t.Errorf("status documents = %+v, want the last report recorded", st.Documents)
	}
	if st.Documents.Tracked != 1 {
		t.Errorf("tracked = %d, want 1", st.Documents.Tracked)
	}
}

func TestSearchScopesToTheSubtree(t *testing.T) {
	srv := newFakeServer(t)
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := svc.Search(context.Background(), "部署负责人", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("memories = %+v, want the stub hit", res.Memories)
	}
}

func TestCloseFlushesBufferedMemory(t *testing.T) {
	srv := newFakeServer(t)
	cfg := enabledConfig(t, srv)
	cfg.Memory.FlushEvery = 100 // nothing will flush on its own
	svc, err := New(cfg, config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store := svc.WrapStore(mustLocalStore(t))
	if err := store.Append(context.Background(), "ns", memory.NewEntry(memory.KindUser, "unflushed turn")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	svc.Close()
	if !srv.sawRequest("POST", "/api/v1/sessions/huan-agent-ns/messages/batch") {
		t.Errorf("requests = %v, want the flush on close", srv.requests)
	}
}

func TestConfigAccessors(t *testing.T) {
	srv := newFakeServer(t)
	svc, err := New(enabledConfig(t, srv), config.ToolsConfig{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Client() == nil {
		t.Error("Client() = nil, want the client")
	}
	if svc.Documents() == nil {
		t.Error("Documents() = nil, want the syncer")
	}
	if svc.Config().BaseURL != srv.URL {
		t.Errorf("Config().BaseURL = %q, want %q", svc.Config().BaseURL, srv.URL)
	}
	if svc.DataDir() == "" {
		t.Error("DataDir() empty, want the state file's directory")
	}
}

// mustLocalStore opens a throwaway local memory store.
func mustLocalStore(t *testing.T) memory.Store {
	t.Helper()
	st, err := memory.NewStore(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("memory.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
