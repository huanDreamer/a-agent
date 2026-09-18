package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// namedTool is a tool.Tool that exists only to occupy a name, so a collision can
// be staged the way a builtin would cause one.
type namedTool struct{ name string }

func (s namedTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: "stub"}, nil
}

func (s namedTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "stub", nil
}

// boolPtr matches config.MCPServer.Enabled, where absent means enabled.
func boolPtr(v bool) *bool { return &v }

// httpMCPServer starts a real streamable-http MCP server exposing the given tool
// names, and returns its /mcp URL.
func httpMCPServer(t *testing.T, names ...string) string {
	t.Helper()

	srv := mcpserver.NewMCPServer("test-server", "0.0.1")
	for _, name := range names {
		srv.AddTool(
			mcppkg.NewTool(name, mcppkg.WithDescription(name)),
			func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return mcppkg.NewToolResultText("ok"), nil
			},
		)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.NewStreamableHTTPServer(srv))
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	return httpSrv.URL + "/mcp"
}

// closeAll closes the clients connectConfiguredMCP handed back.
func closeAll(clients []*mcp.Client) {
	for _, c := range clients {
		_ = c.Close()
	}
}

// TestMCPSpecFromConfig_FieldFidelity is the direct regression gate for the bug
// this file's production half exists to prevent: the CLI and Feishu loops used to
// copy only Name/Command/Args/Env, so Transport, URL and Headers were dropped.
//
// The failure mode was not a wrong value, it was a missing one: an http entry
// became a stdio entry with no command, and the resulting error named the config
// file — the one place the problem was not.
func TestMCPSpecFromConfig_FieldFidelity(t *testing.T) {
	tests := []struct {
		name       string
		in         config.MCPServer
		wantTrans  string
		wantCmd    string
		wantURL    string
		wantHeader []string
	}{
		{
			name: "http with headers (the openviking shape)",
			in: config.MCPServer{
				Name:      "openviking",
				Transport: "http",
				URL:       "http://127.0.0.1:1933/mcp",
				Headers:   []string{"X-API-Key: k", "X-OpenViking-Account: default"},
			},
			wantTrans:  "http",
			wantURL:    "http://127.0.0.1:1933/mcp",
			wantHeader: []string{"X-API-Key: k", "X-OpenViking-Account: default"},
		},
		{
			name: "sse",
			in: config.MCPServer{
				Name:      "hosted",
				Transport: "sse",
				URL:       "https://example.test/sse",
			},
			wantTrans: "sse",
			wantURL:   "https://example.test/sse",
		},
		{
			name: "stdio",
			in: config.MCPServer{
				Name:    "fs",
				Command: "uvx",
				Args:    []string{"mcp-server-filesystem", "/tmp/notes"},
				Env:     []string{"TOKEN=abc"},
			},
			wantTrans: "stdio",
			wantCmd:   "uvx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := mcpSpecFromConfig(tt.in)
			if err != nil {
				t.Fatalf("mcpSpecFromConfig: %v", err)
			}
			if got := string(spec.ResolvedTransport()); got != tt.wantTrans {
				t.Errorf("transport = %q, want %q", got, tt.wantTrans)
			}
			if spec.Command != tt.wantCmd {
				t.Errorf("command = %q, want %q", spec.Command, tt.wantCmd)
			}
			if spec.URL != tt.wantURL {
				t.Errorf("url = %q, want %q", spec.URL, tt.wantURL)
			}
			if len(spec.Args) != len(tt.in.Args) {
				t.Errorf("args = %v, want %v", spec.Args, tt.in.Args)
			}
			if len(spec.Env) != len(tt.in.Env) {
				t.Errorf("env = %v, want %v", spec.Env, tt.in.Env)
			}
			// Header order is part of the contract: it is what goes on the wire.
			if strings.Join(spec.Headers, "|") != strings.Join(tt.wantHeader, "|") {
				t.Errorf("headers = %v, want %v", spec.Headers, tt.wantHeader)
			}
			if spec.Name != tt.in.Name {
				t.Errorf("name = %q, want %q", spec.Name, tt.in.Name)
			}
			if spec.ID != mcp.ServerID(tt.in.Name) {
				t.Errorf("id = %q, want %q", spec.ID, mcp.ServerID(tt.in.Name))
			}

			// The point of all of it: the spec is complete enough to dial. An
			// empty transport falls back to stdio, which is what turned a
			// dropped field into "command is required for a stdio server".
			if err := spec.Validate(); err != nil {
				t.Errorf("spec is not dialable: %v", err)
			}
		})
	}
}

// TestMCPSpecFromConfig_EmptyTransportIsStdio pins the compatibility rule: an
// entry written before the transport field existed is a stdio server. Fixing the
// dropped-field bug must not change that.
func TestMCPSpecFromConfig_EmptyTransportIsStdio(t *testing.T) {
	spec, err := mcpSpecFromConfig(config.MCPServer{Name: "legacy", Command: "srv"})
	if err != nil {
		t.Fatalf("mcpSpecFromConfig: %v", err)
	}
	if got := string(spec.ResolvedTransport()); got != "stdio" {
		t.Errorf("transport = %q, want stdio", got)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestMCPSpecFromConfig_InvalidTransportFailsLoudly: an unparseable transport must
// not be quietly treated as stdio. Falling back is what made the original error
// point at the config file instead of at the code.
func TestMCPSpecFromConfig_InvalidTransportFailsLoudly(t *testing.T) {
	_, err := mcpSpecFromConfig(config.MCPServer{
		Name:      "weird",
		Transport: "websocket",
		URL:       "ws://example.test",
	})
	if err == nil {
		t.Fatal("expected an error for an unparseable transport")
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("error should name the server: %v", err)
	}
	if !strings.Contains(err.Error(), "websocket") {
		t.Errorf("error should quote the offending value: %v", err)
	}
}

// TestMCPSpecFromConfig_HTTPWithoutURL: the missing field is reported, and it is
// url — not command.
func TestMCPSpecFromConfig_HTTPWithoutURL(t *testing.T) {
	spec, err := mcpSpecFromConfig(config.MCPServer{Name: "openviking", Transport: "http"})
	if err != nil {
		t.Fatalf("mcpSpecFromConfig: %v", err)
	}
	err = spec.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject an http server with no url")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should be about url, got: %v", err)
	}
}

// TestConnectConfiguredMCP_SkipsDisabled: a disabled entry is never dialed. The
// feishu path always did this and the CLI path did not, which is the other half of
// the "two hand-written loops drifted apart" problem.
func TestConnectConfiguredMCP_SkipsDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.Servers = []config.MCPServer{{
		Name:    "off",
		Command: "definitely-not-a-real-command-xyz",
		Enabled: boolPtr(false),
	}}

	clients, err := connectConfiguredMCP(context.Background(), tool.NewRegistry(), cfg,
		zaptest.NewLogger(t), "cli")
	if err != nil {
		t.Fatalf("a disabled server must not be connected, got: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("clients = %d, want 0", len(clients))
	}
}

// TestConnectConfiguredMCP_HTTPEntryReachesTheServer is the end-to-end form of the
// regression: an entry declaring transport http must actually connect over http.
// Before the fix this entry became a stdio spec and failed validation, so
// `chat --tools` refused to start while the console — which builds its spec
// correctly — connected the same server without complaint.
func TestConnectConfiguredMCP_HTTPEntryReachesTheServer(t *testing.T) {
	url := httpMCPServer(t, "ping")

	cfg := &config.Config{}
	cfg.MCP.Servers = []config.MCPServer{{
		Name:      "openviking",
		Transport: "http",
		URL:       url,
		Headers:   []string{"X-API-Key: k"},
	}}

	reg := tool.NewRegistry()
	clients, err := connectConfiguredMCP(context.Background(), reg, cfg, zaptest.NewLogger(t), "cli")
	if err != nil {
		t.Fatalf("connectConfiguredMCP over http: %v", err)
	}
	defer closeAll(clients)

	if len(clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(clients))
	}
	if _, ok := reg.Get("ping"); !ok {
		t.Errorf("the server's tool was not registered; registry = %v", reg.Names())
	}
}

// TestConnectConfiguredMCP_NameCollisionIsNotFatal pins the rule mcp-runtime
// states: a tool whose name is already registered is skipped and the rest of the
// server stays usable. Aborting instead would mean that one colliding name
// (OpenViking really does expose "grep") keeps the whole agent from starting.
func TestConnectConfiguredMCP_NameCollisionIsNotFatal(t *testing.T) {
	url := httpMCPServer(t, "taken", "free")

	cfg := &config.Config{}
	cfg.MCP.Servers = []config.MCPServer{{
		Name: "collide", Transport: "http", URL: url,
	}}

	reg := tool.NewRegistry()
	if err := reg.Register(namedTool{name: "taken"}); err != nil {
		t.Fatalf("pre-register: %v", err)
	}

	clients, err := connectConfiguredMCP(context.Background(), reg, cfg, zaptest.NewLogger(t), "cli")
	if err != nil {
		t.Fatalf("connectConfiguredMCP must survive a name collision: %v", err)
	}
	defer closeAll(clients)

	if _, ok := reg.Get("free"); !ok {
		t.Error("the non-colliding tool was not registered")
	}
	if _, ok := reg.Get("taken"); !ok {
		t.Error("the pre-existing tool disappeared; a skip must not unregister anything")
	}
}

// TestConnectConfiguredMCP_RollsBackOnFailure: when a server cannot be converted,
// nothing is left behind. The caller is about to abort, and for a stdio server
// each client is a subprocess.
func TestConnectConfiguredMCP_RollsBackOnFailure(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.Servers = []config.MCPServer{{Name: "bad", Transport: "websocket"}}

	clients, err := connectConfiguredMCP(context.Background(), tool.NewRegistry(), cfg,
		zaptest.NewLogger(t), "cli")
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(clients) != 0 {
		t.Errorf("clients = %v, want none returned on failure", clients)
	}
}

// TestConnectConfiguredMCP_NilInputs: callers may pass a nil config or registry
// (a process with no workspace, no tools). That is not an error.
func TestConnectConfiguredMCP_NilInputs(t *testing.T) {
	if _, err := connectConfiguredMCP(context.Background(), nil, &config.Config{}, zap.NewNop(), "cli"); err != nil {
		t.Errorf("nil registry: %v", err)
	}
	if _, err := connectConfiguredMCP(context.Background(), tool.NewRegistry(), nil, zap.NewNop(), "cli"); err != nil {
		t.Errorf("nil config: %v", err)
	}
}

// TestShippedExampleConfig_ServersAreDialable starts from the shape an operator
// actually writes: the repo's own example config, with the OpenViking integration
// turned on the way its own docs describe. Every server it declares must survive
// the conversion.
//
// This is the "the function is right but the call site is not wired" guard. The
// original bug was not a wrong conversion, it was that nobody called one, so a
// test that only exercises mcpSpecFromConfig would not have caught it.
func TestShippedExampleConfig_ServersAreDialable(t *testing.T) {
	t.Setenv("HUAN_OPENVIKING_ENABLE", "true")
	t.Setenv("HUAN_OPENVIKING_BASE_URL", "http://127.0.0.1:1933")
	t.Setenv("HUAN_OPENVIKING_API_KEY", "test-key")

	cfg, err := config.Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("load configs/config.example.yaml: %v", err)
	}
	if len(cfg.MCP.Servers) == 0 {
		t.Fatal("the example config declares no MCP servers; check that turning OpenViking on registers one")
	}

	for _, s := range cfg.MCP.Servers {
		spec, err := mcpSpecFromConfig(s)
		if err != nil {
			t.Errorf("server %q: %v", s.Name, err)
			continue
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("server %q is not dialable as configured: %v", s.Name, err)
		}
	}
}

// TestFeishuToolingConnectsHTTPEntry covers the second surface that had the bug.
//
// The Feishu bot built its spec the same way the CLI did, so an http entry failed
// there too — the bot simply never got a tool registry. It needs a real store and
// a real workspace directory, which is what makes this the honest end-to-end form
// of the fix rather than another unit test of the converter.
func TestFeishuToolingConnectsHTTPEntry(t *testing.T) {
	url := httpMCPServer(t, "ping")

	cfg := &config.Config{}
	cfg.Tools.Workspace = t.TempDir()
	cfg.MCP.Servers = []config.MCPServer{{
		Name: "openviking", Transport: "http", URL: url,
	}}

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "feishu.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	tooling, err := buildFeishuTooling(context.Background(), cfg, st, zaptest.NewLogger(t), nil, nil)
	if err != nil {
		t.Fatalf("buildFeishuTooling with an http MCP entry: %v", err)
	}
	defer tooling.close()

	if len(tooling.clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(tooling.clients))
	}
	if _, ok := tooling.base.Get("ping"); !ok {
		t.Errorf("the MCP tool was not registered; registry = %v", tooling.base.Names())
	}
}

// TestOneMCPSpecBuilderInCmd is a structural guard: cmd/ must contain exactly one
// place that builds a mcp.ServerSpec from configuration.
//
// It reads the source rather than the behaviour on purpose. The bug this phase
// fixes was not a wrong conversion but a *duplicated* one — three call sites, two
// of them missing three fields — and duplication is invisible to behavioural
// tests, because each copy looks fine on its own until you compare them. A grep is
// the only assertion that actually catches a fourth copy appearing later.
func TestOneMCPSpecBuilderInCmd(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	const allowed = "mcp.go" // the one conversion, plus its own test file
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == allowed {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(body), "mcp.ServerSpec{") {
			t.Errorf("%s builds an mcp.ServerSpec by hand. Build it with mcpSpecFromConfig "+
				"(cmd/huan-agent/mcp.go) instead: a hand-written field list is a field list "+
				"that can drop one, which is how the CLI and the Feishu bot stopped being "+
				"able to connect an http MCP server while the console could.", f)
		}
	}
}
