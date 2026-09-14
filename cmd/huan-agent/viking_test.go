package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/viking"
)

// newTestVikingService builds a real service pointed at a stub OpenViking
// server, so the wiring under test (config → tool registration → HTTP write) is
// the production path rather than a mock of it.
func newTestVikingService(t *testing.T, cfg config.OpenVikingConfig) *viking.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = io.WriteString(w, `{"status":"ok","healthy":true,"version":"0.4.19","auth_mode":"dev"}`)
		case r.URL.Path == "/api/v1/content/write":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(body, &req)
			_, _ = io.WriteString(w, `{"status":"ok","result":{"uri":"`+req.URI+`"}}`)
		default:
			_, _ = io.WriteString(w, `{"status":"ok","result":null}`)
		}
	}))
	t.Cleanup(srv.Close)

	cfg.Enable = true
	cfg.BaseURL = srv.URL
	cfg.Documents.StatePath = filepath.Join(t.TempDir(), "state.json")
	svc, err := viking.New(cfg, config.ToolsConfig{Workspace: t.TempDir()}, zap.NewNop())
	if err != nil {
		t.Fatalf("viking.New: %v", err)
	}
	return svc
}

func TestRegisterDocumentToolIsAbsentWithoutTheIntegration(t *testing.T) {
	cfg := config.Default()
	reg := tool.NewRegistry()

	// No service at all (the integration is off): the model must not be able to
	// see a tool that cannot work.
	if err := registerDocumentTool(reg, cfg, nil); err != nil {
		t.Fatalf("registerDocumentTool(nil service): %v", err)
	}
	if _, ok := reg.Get("save_document"); ok {
		t.Error("save_document registered without OpenViking, want it absent")
	}

	// A service with document storage switched off is the same case.
	svc := newTestVikingService(t, cfg.OpenViking)
	cfg.OpenViking.Documents.Enable = false
	if err := registerDocumentTool(reg, cfg, svc); err != nil {
		t.Fatalf("registerDocumentTool(disabled documents): %v", err)
	}
	if _, ok := reg.Get("save_document"); ok {
		t.Error("save_document registered with openviking.documents.enable = false, want it absent")
	}
}

func TestRegisterDocumentToolWritesThroughWhenEnabled(t *testing.T) {
	cfg := config.Default()
	svc := newTestVikingService(t, cfg.OpenViking)
	reg := tool.NewRegistry()

	if err := registerDocumentTool(reg, cfg, svc); err != nil {
		t.Fatalf("registerDocumentTool: %v", err)
	}
	specs, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range specs {
		if s.Name == "save_document" {
			found = true
			if !strings.Contains(s.Description, "document store") || !strings.Contains(s.Description, "searchable") {
				t.Errorf("description = %q, want it to name the destination and why it matters", s.Description)
			}
		}
	}
	if !found {
		t.Fatalf("save_document not registered; specs = %+v", specs)
	}

	tool, ok := reg.Get("save_document")
	if !ok {
		t.Fatal("save_document missing from the registry")
	}
	out, err := tool.InvokableRun(context.Background(), `{"title":"Deploy notes","content":"# Notes"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "viking://user/default/huan-agent/documents/") {
		t.Errorf("output = %q, want the stored URI", out)
	}
}

func TestVikingConsoleAdapterKeepsNilNil(t *testing.T) {
	// A nil *viking.Service must not become a non-nil interface: that would
	// satisfy the console's interface and panic on the first request.
	if got := vikingConsole(nil); got != nil {
		t.Errorf("vikingConsole(nil) = %v, want nil", got)
	}
	svc := newTestVikingService(t, config.Default().OpenViking)
	if got := vikingConsole(svc); got == nil {
		t.Error("vikingConsole(service) = nil, want the adapter")
	}
}

func TestNewVikingServiceReturnsNilWhenDisabled(t *testing.T) {
	cfg := config.Default() // openviking.enable is false by default
	svc, err := newVikingService(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("newVikingService: %v", err)
	}
	if svc != nil {
		t.Error("newVikingService returned a service for a disabled config, want nil")
	}
}

func TestBuildMemoryStoreSkipsMirrorWithoutService(t *testing.T) {
	cfg := config.Default()
	cfg.Memory.Dir = filepath.Join(t.TempDir(), "memory")
	store, closeFn, err := buildMemoryStore(cfg, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("buildMemoryStore: %v", err)
	}
	if store == nil {
		t.Fatal("store = nil, want the local store")
	}
	closeFn()

	// Memory off means no store at all, and the closer must still be callable.
	cfg.Memory.Enable = false
	store, closeFn, err = buildMemoryStore(cfg, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("buildMemoryStore (disabled): %v", err)
	}
	if store != nil {
		t.Error("store != nil with memory disabled, want nil")
	}
	closeFn()
}
