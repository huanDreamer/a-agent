package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// testMCPDialer serves every spec from an in-process MCP server, so the whole
// MCP surface can be tested without spawning a process. tools maps a server id
// (or "default") to the tool names that server exposes; failFor makes a dial
// fail, which is how the failure paths are covered.
//
// Every field is guarded by mu: Dial runs on the server's request goroutines
// while the test mutates the maps between requests, and that is a genuine race
// even when the test's ordering happens to be correct.
type testMCPDialer struct {
	mu      sync.Mutex
	tools   map[string][]string
	failFor map[string]error
}

func newTestMCPDialer() *testMCPDialer {
	return &testMCPDialer{tools: map[string][]string{"default": {"echo", "add"}}}
}

// failWith makes every future dial of id fail.
func (d *testMCPDialer) failWith(id string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failFor == nil {
		d.failFor = map[string]error{}
	}
	d.failFor[id] = err
}

// recoverFrom lets future dials of id succeed again.
func (d *testMCPDialer) recoverFrom(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.failFor, id)
}

// setTools gives one server id its own tool set.
func (d *testMCPDialer) setTools(id string, names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tools == nil {
		d.tools = map[string][]string{}
	}
	d.tools[id] = names
}

// Dial implements mcp.Dialer.
func (d *testMCPDialer) Dial(ctx context.Context, spec mcp.ServerSpec) (*mcp.Client, error) {
	d.mu.Lock()
	failure := d.failFor[spec.ID]
	names := d.tools[spec.ID]
	if names == nil {
		names = d.tools["default"]
	}
	d.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	srv := mcpserver.NewMCPServer("test-"+spec.ID, "0.0.1")
	for _, n := range names {
		name := n
		srv.AddTool(mcppkg.NewTool(name, mcppkg.WithDescription("tool "+name)),
			func(_ context.Context, _ mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return mcppkg.NewToolResultText("ok:" + name), nil
			})
	}

	raw, err := mcpclient.NewInProcessClient(srv)
	if err != nil {
		return nil, err
	}
	if err := raw.Start(ctx); err != nil {
		return nil, err
	}
	if _, err := raw.Initialize(ctx, mcppkg.InitializeRequest{
		Params: mcppkg.InitializeParams{
			ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcppkg.Implementation{Name: "server-test", Version: "0"},
		},
	}); err != nil {
		return nil, err
	}
	return mcp.WrapClient(spec, raw), nil
}

// newMCPServer builds a server whose MCP runtime is backed by dialer, with an
// optional tool registry (a nil registry means chat is disabled).
func newMCPServer(t *testing.T, dialer mcp.Dialer, reg *tool.Registry, seed func(store.Store)) (*harness, *Server) {
	t.Helper()
	srv, st := buildServerWith(t, buildOpts{
		seed:    seed,
		mcpDial: dialer,
		chat:    ChatDeps{Tools: reg},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, srv
}

func TestMCPAPI_ListIsEmptyButExplainsItself(t *testing.T) {
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, tool.NewRegistry(), nil)

	var body struct {
		Servers    []mcpServerView `json:"servers"`
		Transports []string        `json:"transports"`
		Runtime    bool            `json:"runtime"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &body)

	if len(body.Servers) != 0 {
		t.Errorf("servers = %+v, want none", body.Servers)
	}
	if !body.Runtime {
		t.Error("runtime = false, want true when a tool registry exists")
	}
	// The vocabulary is served rather than hardcoded by the UI.
	if len(body.Transports) != 3 {
		t.Errorf("transports = %v, want stdio/sse/http", body.Transports)
	}
}

func TestMCPAPI_SaveConnectsAndRegistersTools(t *testing.T) {
	dialer := newTestMCPDialer()
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, dialer.Dial, reg, nil)

	resp := h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "transport": "stdio",
		"command": "node", "args": []string{"server.js"},
	})
	requireStatus(t, resp, http.StatusOK)

	// The tools are in the registry the chat reads, so the model can call them
	// on the next message without a restart.
	names := reg.Names()
	if len(names) != 2 {
		t.Fatalf("registry = %v, want the two MCP tools", names)
	}

	// …and the row reports its live state.
	var list struct {
		Servers []mcpServerView `json:"servers"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if len(list.Servers) != 1 {
		t.Fatalf("servers = %+v", list.Servers)
	}
	got := list.Servers[0]
	if got.Runtime == nil || !got.Runtime.Connected {
		t.Fatalf("runtime = %+v, want connected", got.Runtime)
	}
	if len(got.Runtime.Tools) != 2 {
		t.Errorf("tools = %v", got.Runtime.Tools)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want empty", got.LastError)
	}
}

func TestMCPAPI_SaveValidatesTheDefinition(t *testing.T) {
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, tool.NewRegistry(), nil)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no id", map[string]any{"command": "node"}, "id is required"},
		{"bad id slug", map[string]any{"id": "Bad Id", "command": "node"}, "slug"},
		{"stdio without command", map[string]any{"id": "x", "transport": "stdio"}, "command"},
		{"sse without url", map[string]any{"id": "x", "transport": "sse"}, "url"},
		{"unknown transport", map[string]any{"id": "x", "transport": "smoke", "command": "node"}, "transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.postJSON(t, "/api/mcp/servers", tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var out map[string]string
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if !strings.Contains(out["error"], tc.want) {
				t.Errorf("error = %q, want it to mention %q", out["error"], tc.want)
			}
		})
	}
}

func TestMCPAPI_SaveRefusesToClobberAnExistingID(t *testing.T) {
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, tool.NewRegistry(), nil)

	first := map[string]any{"id": "demo", "name": "First", "command": "node"}
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", first), http.StatusOK)

	// A second create with the same id must not silently replace the first: a
	// generated id colliding with a real server is exactly how a working
	// configuration would disappear.
	second := map[string]any{"id": "demo", "name": "Second", "command": "python3"}
	resp := h.postJSON(t, "/api/mcp/servers", second)
	requireStatus(t, resp, http.StatusConflict)

	var list struct {
		Servers []mcpServerView `json:"servers"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if len(list.Servers) != 1 || list.Servers[0].Name != "First" {
		t.Fatalf("servers = %+v, want the original row intact", list.Servers)
	}

	// An explicit edit works.
	second["overwrite"] = true
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", second), http.StatusOK)
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if list.Servers[0].Name != "Second" || list.Servers[0].Command != "python3" {
		t.Fatalf("servers = %+v, want the edited row", list.Servers)
	}
}

func TestMCPAPI_DisableUnregistersTools(t *testing.T) {
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, reg, nil)

	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node",
	}), http.StatusOK)
	if len(reg.Names()) != 2 {
		t.Fatalf("registry = %v", reg.Names())
	}

	disabled := false
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node", "enabled": disabled, "overwrite": true,
	}), http.StatusOK)

	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want empty after disabling", got)
	}
}

// TestMCPAPI_ToggleNeedsNoOverwrite is the rule the list's switch depends on: a
// body carrying only id + enabled is a toggle, so it may update an existing
// server without restating (and risking) its definition.
func TestMCPAPI_ToggleNeedsNoOverwrite(t *testing.T) {
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, reg, nil)

	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node", "args": []string{"a.js"},
	}), http.StatusOK)

	off := false
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "enabled": off,
	}), http.StatusOK)

	// The definition survived the toggle untouched…
	saved, err := h.store.GetMCPServer(context.Background(), "demo")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if saved.Enabled || saved.Command != "node" || len(saved.Args) != 1 || saved.Name != "Demo" {
		t.Fatalf("row = %+v, want the definition intact and disabled", saved)
	}
	// …and its tools are gone from the registry while it is off.
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want empty", got)
	}

	// A definition-only body still needs overwrite, so a create cannot clobber.
	resp := h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "transport": "stdio", "command": "python3", "enabled": true,
	})
	requireStatus(t, resp, http.StatusConflict)
}

func TestMCPAPI_DeleteRemovesServerAndTools(t *testing.T) {
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, reg, nil)

	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node",
	}), http.StatusOK)
	requireStatus(t, h.deleteJSON(t, "/api/mcp/servers/demo"), http.StatusOK)

	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want empty after delete", got)
	}
	requireStatus(t, h.deleteJSON(t, "/api/mcp/servers/demo"), http.StatusNotFound)
}

func TestMCPAPI_ConnectFailureIsReportedNotFatal(t *testing.T) {
	dialer := newTestMCPDialer()
	dialer.failWith("broken", fmt.Errorf("spawn: no such file or directory"))
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, dialer.Dial, reg, nil)

	// Saving a server that cannot connect still succeeds: the definition is
	// valid, the connection is not, and the console must show the difference.
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "broken", "name": "Broken", "command": "nope",
	}), http.StatusOK)

	var list struct {
		Servers []mcpServerView `json:"servers"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	st := list.Servers[0]
	if st.Runtime == nil || st.Runtime.Connected {
		t.Fatalf("runtime = %+v, want unconnected", st.Runtime)
	}
	if !strings.Contains(st.LastError, "no such file") {
		t.Errorf("last_error = %q, want the dial failure", st.LastError)
	}
	// The failure is also persisted, so it survives a restart of the console.
	saved, err := h.store.GetMCPServer(context.Background(), "broken")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if !strings.Contains(saved.LastError, "no such file") {
		t.Errorf("stored last_error = %q", saved.LastError)
	}
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want empty", got)
	}
}

func TestMCPAPI_ProbeDoesNotSaveOrRegister(t *testing.T) {
	dialer := newTestMCPDialer()
	dialer.setTools("probe", "echo", "add", "search")
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, dialer.Dial, reg, nil)

	var out mcpProbeResult
	resp := h.postJSON(t, "/api/mcp/probe", map[string]any{
		"id": "probe", "name": "Probe", "transport": "stdio", "command": "node",
	})
	decode(t, resp, &out)

	if !out.OK || out.Count != 3 {
		t.Fatalf("probe = %+v, want 3 tools", out)
	}
	if len(out.Tools) != 3 || out.Tools[0].Name == "" {
		t.Errorf("tools = %+v", out.Tools)
	}
	// A definition being tried out must not reach the runtime.
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want untouched", got)
	}
	var list struct {
		Servers []mcpServerView `json:"servers"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if len(list.Servers) != 0 {
		t.Errorf("servers = %+v, want nothing saved", list.Servers)
	}
}

func TestMCPAPI_ProbeReportsAFailedConnection(t *testing.T) {
	dialer := newTestMCPDialer()
	dialer.failWith("probe", fmt.Errorf("dial tcp: connection refused"))
	h, _ := newMCPServer(t, dialer.Dial, tool.NewRegistry(), nil)

	var out mcpProbeResult
	resp := h.postJSON(t, "/api/mcp/probe", map[string]any{
		"id": "probe", "name": "Probe", "transport": "sse", "url": "http://127.0.0.1:1/sse",
	})
	// A failed test is a result, not an API error.
	decode(t, resp, &out)
	if out.OK || !strings.Contains(out.Error, "connection refused") {
		t.Fatalf("probe = %+v, want a reported failure", out)
	}
}

func TestMCPAPI_TestSavedServer(t *testing.T) {
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, tool.NewRegistry(), nil)
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node",
	}), http.StatusOK)

	var out mcpProbeResult
	resp := h.postJSON(t, "/api/mcp/servers/demo/test", nil)
	decode(t, resp, &out)
	if !out.OK || out.Count != 2 {
		t.Fatalf("test = %+v, want 2 tools", out)
	}

	requireStatus(t, h.postJSON(t, "/api/mcp/servers/missing/test", nil), http.StatusNotFound)
}

func TestMCPAPI_ReloadReconnectsEverything(t *testing.T) {
	dialer := newTestMCPDialer()
	dialer.failWith("demo", fmt.Errorf("boom"))
	h, _ := newMCPServer(t, dialer.Dial, tool.NewRegistry(), nil)

	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node",
	}), http.StatusOK)

	dialer.recoverFrom("demo")
	var out struct {
		Servers []mcp.ServerStatus `json:"servers"`
		Runtime bool               `json:"runtime"`
	}
	resp := h.postJSON(t, "/api/mcp/reload", nil)
	decode(t, resp, &out)

	if !out.Runtime || len(out.Servers) != 1 || !out.Servers[0].Connected {
		t.Fatalf("reload = %+v, want the server reconnected", out)
	}
}

func TestMCPAPI_ConfigSourcedServerIsReadOnly(t *testing.T) {
	seed := func(st store.Store) {
		if err := st.UpsertMCPServer(context.Background(), store.MCPServer{
			ID: "from-config", Name: "From Config", Command: "node",
			Source: store.SourceConfig, Enabled: true,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	reg := tool.NewRegistry()
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, reg, seed)

	// The config row is connected at startup (SyncMCP is called by the harness).
	if len(reg.Names()) != 2 {
		t.Fatalf("registry = %v, want the config server's tools", reg.Names())
	}

	// Editing anything but `enabled` is refused, because the next start would
	// overwrite it from the file.
	resp := h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "from-config", "name": "Renamed", "command": "python3",
	})
	requireStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()

	// Deleting it is refused for the same reason.
	resp = h.deleteJSON(t, "/api/mcp/servers/from-config")
	requireStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()

	// Toggling enabled is allowed and takes effect.
	off := false
	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "from-config", "enabled": off,
	}), http.StatusOK)
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want empty after disabling a config server", got)
	}
}

func TestMCPAPI_WorksWithoutAChatRegistry(t *testing.T) {
	// chat.enable = false leaves no registry, so nothing can be registered — but
	// the console must still be able to store and test a definition.
	h, srv := newMCPServer(t, newTestMCPDialer().Dial, nil, nil)
	if srv.mcp != nil {
		t.Fatal("a server without a tool registry must not build an MCP manager")
	}

	requireStatus(t, h.postJSON(t, "/api/mcp/servers", map[string]any{
		"id": "demo", "name": "Demo", "command": "node",
	}), http.StatusOK)

	var list struct {
		Servers []mcpServerView `json:"servers"`
		Runtime bool            `json:"runtime"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if list.Runtime {
		t.Error("runtime = true, want false without a registry")
	}
	if len(list.Servers) != 1 {
		t.Fatalf("servers = %+v", list.Servers)
	}

	// 测试连接 still works: it connects and disconnects.
	var probe mcpProbeResult
	resp := h.postJSON(t, "/api/mcp/servers/demo/test", nil)
	decode(t, resp, &probe)
	if !probe.OK || probe.Count != 2 {
		t.Fatalf("probe = %+v", probe)
	}
}

func TestMCPAPI_DraftBuildsADefinitionFromTheModel(t *testing.T) {
	model := &jsonModel{answer: `{
	  "name": "GitHub",
	  "summary": "访问 GitHub 仓库，需要把 token 换成真实值",
	  "transport": "stdio",
	  "command": "npx",
	  "args": ["-y", "@modelcontextprotocol/server-github"],
	  "env": ["GITHUB_PERSONAL_ACCESS_TOKEN="],
	  "url": "",
	  "headers": []
	}`}
	srv, st := buildServerWith(t, buildOpts{
		mcpDial: newTestMCPDialer().Dial,
		chat:    ChatDeps{Tools: tool.NewRegistry(), Builder: &staticBuilder{provider: "deepseek", model: "deepseek-chat", cm: model}},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var out struct {
		Draft    mcpServerRequest  `json:"draft"`
		Warnings []string          `json:"warnings"`
		Notes    string            `json:"notes"`
		Raw      string            `json:"raw"`
		Model    map[string]string `json:"model"`
	}
	resp := h.postJSON(t, "/api/mcp/draft", map[string]any{"description": "帮我接入 GitHub 的 MCP"})
	decode(t, resp, &out)

	if out.Draft.Name != "GitHub" || out.Draft.ID != "github" {
		t.Errorf("draft = %+v", out.Draft)
	}
	if out.Draft.Transport != "stdio" || out.Draft.Command != "npx" {
		t.Errorf("draft = %+v", out.Draft)
	}
	if len(out.Draft.Args) != 2 {
		t.Errorf("args = %v", out.Draft.Args)
	}
	// The empty secret is called out rather than saved silently.
	if len(out.Warnings) == 0 || !strings.Contains(strings.Join(out.Warnings, " "), "GITHUB_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("warnings = %v, want a note about the unset token", out.Warnings)
	}
	if out.Notes == "" {
		t.Error("notes = empty, want the model's summary")
	}
	if out.Model["provider"] != "deepseek" {
		t.Errorf("model = %v", out.Model)
	}
	// A draft is never persisted.
	var list struct {
		Servers []mcpServerView `json:"servers"`
	}
	h.getJSON(t, "/api/mcp/servers", http.StatusOK, &list)
	if len(list.Servers) != 0 {
		t.Errorf("servers = %+v, want nothing saved by a draft", list.Servers)
	}
}

func TestMCPAPI_DraftParsesJSONOutOfProse(t *testing.T) {
	model := &jsonModel{answer: "好的，这是配置：\n```json\n{\"name\":\"Files\",\"transport\":\"stdio\",\"command\":\"npx\",\"args\":[\"-y\",\"@modelcontextprotocol/server-filesystem\",\"/tmp\"],\"env\":[],\"summary\":\"本地文件系统\"}\n```\n需要 Node.js。"}
	srv, st := buildServerWith(t, buildOpts{
		chat: ChatDeps{Builder: &staticBuilder{provider: "p", model: "m", cm: model}},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var out struct {
		Draft mcpServerRequest `json:"draft"`
	}
	resp := h.postJSON(t, "/api/mcp/draft", map[string]any{"description": "本地文件"})
	decode(t, resp, &out)
	if out.Draft.ID != "files" || out.Draft.Command != "npx" {
		t.Fatalf("draft = %+v", out.Draft)
	}
}

func TestMCPAPI_DraftWithoutAModel(t *testing.T) {
	// No builder: the endpoint must say what to configure instead of failing
	// with an opaque error.
	h, _ := newMCPServer(t, newTestMCPDialer().Dial, tool.NewRegistry(), nil)
	resp := h.postJSON(t, "/api/mcp/draft", map[string]any{"description": "x"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out["error"], "模型") {
		t.Errorf("error = %q, want a message naming the missing model", out["error"])
	}
}

func TestMCPAPI_DraftNeedsARequest(t *testing.T) {
	model := &jsonModel{answer: `{}`}
	srv, st := buildServerWith(t, buildOpts{
		chat: ChatDeps{Builder: &staticBuilder{provider: "p", model: "m", cm: model}},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	requireStatus(t, h.postJSON(t, "/api/mcp/draft", map[string]any{}), http.StatusBadRequest)
}

func TestSpecFromStoreDefaultsToStdio(t *testing.T) {
	spec := specFromStore(store.MCPServer{ID: "x", Name: "X", Command: "node", Transport: "nonsense"})
	if spec.Transport != mcp.TransportStdio {
		t.Errorf("transport = %q, want stdio for an unreadable value", spec.Transport)
	}
}

func TestMCPDraftToRequestFixesUpModelOutput(t *testing.T) {
	taken := map[string]bool{"github": true}
	req, warnings := mcpDraftToRequest(map[string]any{
		"name":      "GitHub",
		"transport": "stdio",
		"command":   "npx",
		"args":      "not-a-list",
		"url":       "https://example.com",
		"env":       []any{"TOKEN=", "OK=1"},
	}, "GitHub MCP", taken)

	if req.ID != "github-2" {
		t.Errorf("id = %q, want a free id", req.ID)
	}
	if len(req.Args) != 1 || req.Args[0] != "not-a-list" {
		t.Errorf("args = %v, want a single-element list", req.Args)
	}
	if req.URL != "" {
		t.Errorf("url = %q, want it cleared for stdio", req.URL)
	}
	joined := strings.Join(warnings, " | ")
	for _, want := range []string{"url", "TOKEN"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %v, want a note about %q", warnings, want)
		}
	}
}

func TestMCPDraftToRequestRemoteTransport(t *testing.T) {
	req, warnings := mcpDraftToRequest(map[string]any{
		"name":      "Remote",
		"transport": "sse",
		"url":       "https://example.com/sse",
		"command":   "npx",
		"args":      []any{"-y"},
		"headers":   []any{"Authorization: Bearer "},
	}, "远程", map[string]bool{})

	if req.Command != "" || len(req.Args) != 0 {
		t.Errorf("a remote transport must not carry a command: %+v", req)
	}
	if !strings.Contains(strings.Join(warnings, " | "), "Authorization") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestMCPDraftToRequestWithoutName(t *testing.T) {
	req, warnings := mcpDraftToRequest(map[string]any{"transport": "stdio", "command": "node"},
		"帮我接入某个东西\n第二行", map[string]bool{})
	if req.Name == "" || req.ID == "" {
		t.Errorf("req = %+v, want a fallback name", req)
	}
	if len(warnings) == 0 {
		t.Error("want a warning that the name was guessed")
	}
}

// jsonModel answers Generate with a fixed string. It implements eino's
// BaseChatModel, which is all an AI-assist endpoint needs (it never streams).
type jsonModel struct {
	answer string
	err    error
	seen   []*schema.Message
}

func (m *jsonModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.seen = in
	if m.err != nil {
		return nil, m.err
	}
	return &schema.Message{Role: schema.Assistant, Content: m.answer}, nil
}

func (m *jsonModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, fmt.Errorf("streaming is not used by the config assistant")
}

// TestSyncConfigServersRemovesStaleRows covers the whole sync contract against a
// real store, including the rule that a console-created row is never touched.
func TestSyncConfigServersRemovesStaleRows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	servers := []config.MCPServer{{Name: "GitHub", Command: "npx", Args: []string{"-y", "pkg"}}}
	n, err := SyncConfigServers(ctx, st, servers, zap.NewNop())
	if err != nil {
		t.Fatalf("SyncConfigServers: %v", err)
	}
	if n != 1 {
		t.Errorf("synced = %d, want 1", n)
	}
	row, err := st.GetMCPServer(ctx, "github")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if row.Source != store.SourceConfig || row.Name != "GitHub" || len(row.Args) != 2 {
		t.Errorf("row = %+v", row)
	}
	// An entry with no enabled flag is on, which is what it meant before the
	// flag existed.
	if !row.Enabled {
		t.Error("Enabled = false, want true by default")
	}

	if err := st.UpsertMCPServer(ctx, store.MCPServer{
		ID: "user-made", Name: "User", Command: "node", Source: store.SourceUser, Enabled: true,
	}); err != nil {
		t.Fatalf("seed user row: %v", err)
	}

	// Removing the entry from the config drops its row, but never the user's.
	if _, err := SyncConfigServers(ctx, st, nil, zap.NewNop()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := st.GetMCPServer(ctx, "github"); err == nil {
		t.Error("the config row should have been dropped")
	}
	if _, err := st.GetMCPServer(ctx, "user-made"); err != nil {
		t.Errorf("the user row must survive a sync: %v", err)
	}
}

func TestSyncConfigServersHonoursDisabled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	off := false
	if _, err := SyncConfigServers(ctx, st, []config.MCPServer{
		{Name: "Off", Command: "node", Enabled: &off},
	}, zap.NewNop()); err != nil {
		t.Fatalf("SyncConfigServers: %v", err)
	}
	row, err := st.GetMCPServer(ctx, "off")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if row.Enabled {
		t.Error("Enabled = true, want the config's false to win")
	}
}

// TestMCPAPI_DraftIsBounded documents that drafting carries its own deadline
// rather than inheriting an open-ended one from the engine.
func TestMCPAPI_DraftIsBounded(t *testing.T) {
	if draftTimeout <= 0 || draftTimeout > 5*time.Minute {
		t.Errorf("draftTimeout = %s, want a bounded value", draftTimeout)
	}
}
