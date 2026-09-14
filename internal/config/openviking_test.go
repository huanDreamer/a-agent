package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenVikingDisabledByDefault(t *testing.T) {
	cfg := Default()
	if cfg.OpenViking.Enabled() {
		t.Fatal("OpenViking.Enabled() = true by default, want false (a machine without the server must be unaffected)")
	}
	if got, want := cfg.OpenViking.BaseURL, DefaultOpenVikingBaseURL; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
	// The sub-features default on *within* the section, so enabling the
	// integration is one flag rather than five.
	if !cfg.OpenViking.Memory.Enable || !cfg.OpenViking.Documents.Enable || !cfg.OpenViking.MCP.Register {
		t.Errorf("sub-features off by default: %+v", cfg.OpenViking)
	}
}

func TestOpenVikingScopeDefaults(t *testing.T) {
	ov := OpenVikingConfig{}
	if got, want := ov.Subtree(), "viking://user/default/huan-agent"; got != want {
		t.Errorf("Subtree() = %q, want %q", got, want)
	}
	ov.User = "bob"
	if got, want := ov.Subtree(), "viking://user/bob/huan-agent"; got != want {
		t.Errorf("Subtree() with user = %q, want %q", got, want)
	}
	if got := ov.DocumentsRoot(); got != ov.Subtree() {
		t.Errorf("DocumentsRoot() = %q, want the subtree %q", got, ov.Subtree())
	}
	if got := ov.RecallScope(); got != ov.Subtree() {
		t.Errorf("RecallScope() = %q, want the subtree %q", got, ov.Subtree())
	}
	ov.Documents.RootURI = "viking://resources/notes/"
	if got, want := ov.DocumentsRoot(), "viking://resources/notes"; got != want {
		t.Errorf("DocumentsRoot() = %q, want %q (trailing slash trimmed)", got, want)
	}
	ov.Memory.RecallTarget = "viking://user/bob/other"
	if got, want := ov.RecallScope(), "viking://user/bob/other"; got != want {
		t.Errorf("RecallScope() = %q, want %q", got, want)
	}
}

func TestOpenVikingDocumentAccessors(t *testing.T) {
	var docs OpenVikingDocumentsConfig
	if got := len(docs.IncludesOr()); got == 0 {
		t.Error("IncludesOr() empty, want the built-in allow-list")
	}
	if got := len(docs.ExcludesOr()); got == 0 {
		t.Error("ExcludesOr() empty, want the built-in deny-list")
	}
	if got, want := docs.MaxFileBytes(), int64(DefaultOpenVikingMaxFileKB)<<10; got != want {
		t.Errorf("MaxFileBytes() = %d, want %d", got, want)
	}
	if docs.UploadsBinaries() {
		t.Error("UploadsBinaries() = true by default, want false (skip)")
	}
	docs.BinaryMode = "Upload"
	if !docs.UploadsBinaries() {
		t.Error("UploadsBinaries() = false for \"Upload\", want true (case-insensitive)")
	}
	if got, want := docs.StateFile(), DefaultOpenVikingStatePath; got != want {
		t.Errorf("StateFile() = %q, want %q", got, want)
	}
	if got := docs.SyncInterval(); got != 0 {
		t.Errorf("SyncInterval() = %v, want 0 (off)", got)
	}
	docs.SyncIntervalSeconds = 30
	if got := docs.SyncInterval().Seconds(); got != 30 {
		t.Errorf("SyncInterval() = %vs, want 30s", got)
	}
}

func TestOpenVikingWorkspaceRootPrefersDocumentsOverride(t *testing.T) {
	docs := OpenVikingDocumentsConfig{}
	tools := ToolsConfig{Workspace: "/tmp/ws"}
	if got, want := docs.WorkspaceRoot(tools), filepath.Clean("/tmp/ws"); got != want {
		t.Errorf("WorkspaceRoot() = %q, want %q (tools.workspace)", got, want)
	}
	docs.WorkspaceDir = "/tmp/other/"
	if got, want := docs.WorkspaceRoot(tools), filepath.Clean("/tmp/other/"); got != want {
		t.Errorf("WorkspaceRoot() = %q, want %q (documents override)", got, want)
	}
	// Neither set: nothing to sync, which is what stops a sync from publishing
	// the process working directory by accident.
	if got := (OpenVikingDocumentsConfig{}).WorkspaceRoot(ToolsConfig{}); got != "" {
		t.Errorf("WorkspaceRoot() = %q, want empty", got)
	}
}

func TestApplyOpenVikingMCPRegistersHTTPEntry(t *testing.T) {
	cfg := Default()
	cfg.OpenViking.Enable = true
	cfg.OpenViking.BaseURL = "http://127.0.0.1:1933/"
	cfg.OpenViking.Account = "team"
	cfg.OpenViking.User = "huan"
	cfg.OpenViking.APIKey = "k"
	cfg.ApplyOpenVikingMCP()

	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(cfg.MCP.Servers))
	}
	s := cfg.MCP.Servers[0]
	if s.Name != DefaultOpenVikingMCPName {
		t.Errorf("Name = %q, want %q", s.Name, DefaultOpenVikingMCPName)
	}
	if s.Transport != "http" {
		t.Errorf("Transport = %q, want http", s.Transport)
	}
	if s.URL != "http://127.0.0.1:1933/mcp" {
		t.Errorf("URL = %q, want http://127.0.0.1:1933/mcp", s.URL)
	}
	want := map[string]bool{"X-API-Key: k": true, "X-OpenViking-Account: team": true, "X-OpenViking-User: huan": true}
	for _, h := range s.Headers {
		delete(want, h)
	}
	if len(want) != 0 {
		t.Errorf("headers missing: %v (got %v)", want, s.Headers)
	}
	if !s.IsEnabled() {
		t.Error("registered server is disabled, want enabled")
	}
}

func TestApplyOpenVikingMCPIsIdempotentAndRespectsOperatorEntry(t *testing.T) {
	cfg := Default()
	cfg.OpenViking.Enable = true
	cfg.ApplyOpenVikingMCP()
	cfg.ApplyOpenVikingMCP()
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("servers = %d after two applies, want 1 (idempotent)", len(cfg.MCP.Servers))
	}

	// An explicitly declared entry with the same name wins.
	cfg2 := Default()
	cfg2.OpenViking.Enable = true
	cfg2.MCP.Servers = []MCPServer{{Name: "openviking", Transport: "http", URL: "http://example.test/mcp"}}
	cfg2.ApplyOpenVikingMCP()
	if len(cfg2.MCP.Servers) != 1 {
		t.Fatalf("servers = %d, want 1 (operator entry kept)", len(cfg2.MCP.Servers))
	}
	if got := cfg2.MCP.Servers[0].URL; got != "http://example.test/mcp" {
		t.Errorf("URL = %q, want the operator's own definition", got)
	}
}

func TestApplyOpenVikingMCPNoopWhenDisabled(t *testing.T) {
	cfg := Default()
	cfg.OpenViking.MCP.Register = true // enabled switch still off
	cfg.ApplyOpenVikingMCP()
	if len(cfg.MCP.Servers) != 0 {
		t.Fatalf("servers = %d, want 0 when the integration is disabled", len(cfg.MCP.Servers))
	}

	cfg.OpenViking.Enable = true
	cfg.OpenViking.BaseURL = "  "
	cfg.ApplyOpenVikingMCP()
	if len(cfg.MCP.Servers) != 0 {
		t.Fatalf("servers = %d, want 0 when base_url is blank", len(cfg.MCP.Servers))
	}
}

func TestLoadAppliesOpenVikingMCPFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "openviking:\n  enable: true\n  base_url: \"http://127.0.0.1:1933\"\n  user: \"huan\"\nmcp:\n  servers: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OpenViking.Enabled() {
		t.Fatal("OpenViking.Enabled() = false, want true")
	}
	if got, want := cfg.OpenViking.Subtree(), "viking://user/huan/huan-agent"; got != want {
		t.Errorf("Subtree() = %q, want %q", got, want)
	}
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != DefaultOpenVikingMCPName {
		t.Fatalf("MCP servers = %+v, want the openviking entry", cfg.MCP.Servers)
	}
	headers := cfg.MCP.Servers[0].Headers
	if len(headers) != 2 {
		t.Fatalf("headers = %v, want the account and user identity headers only (no api_key)", headers)
	}
	for _, h := range headers {
		if h == "X-OpenViking-Account: default" || h == "X-OpenViking-User: huan" {
			continue
		}
		t.Errorf("unexpected header %q", h)
	}
}

func TestLoadOpenVikingEnvOverride(t *testing.T) {
	t.Setenv("HUAN_OPENVIKING_ENABLE", "true")
	t.Setenv("HUAN_OPENVIKING_BASE_URL", "http://10.0.0.5:1933")
	t.Setenv("HUAN_OPENVIKING_MEMORY_FLUSH_EVERY", "8")
	t.Setenv("HUAN_OPENVIKING_DOCUMENTS_SYNC_WORKSPACE", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OpenViking.Enabled() {
		t.Fatal("OpenViking.Enabled() = false, want true from env")
	}
	if got, want := cfg.OpenViking.BaseURL, "http://10.0.0.5:1933"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.OpenViking.Memory.FlushEvery, 8; got != want {
		t.Errorf("FlushEvery = %d, want %d", got, want)
	}
	if cfg.OpenViking.Documents.SyncWorkspace {
		t.Error("SyncWorkspace = true, want false from env")
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("MCP servers = %+v, want the openviking entry registered from env config", cfg.MCP.Servers)
	}
}
