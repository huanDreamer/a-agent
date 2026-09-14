package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap/zaptest"

	"github.com/huan/huan-agent/internal/tool"
)

// countingDialer wraps the in-process echo server as a Dialer, so Apply can be
// exercised without spawning a process. It counts dials per server id so a test
// can assert that an unchanged server is NOT reconnected.
type countingDialer struct {
	mu      sync.Mutex
	dials   map[string]int
	closed  int
	failFor map[string]error
	// servers maps a spec id to the tool set it should expose.
	servers map[string]*mcpserver.MCPServer
}

func newCountingDialer(t *testing.T, toolNames ...string) *countingDialer {
	t.Helper()
	return &countingDialer{
		dials:   map[string]int{},
		failFor: map[string]error{},
		servers: map[string]*mcpserver.MCPServer{"default": serverWith(t, toolNames...)},
	}
}

// serverWith builds an MCP server exposing one echo-like tool per name.
func serverWith(t *testing.T, names ...string) *mcpserver.MCPServer {
	t.Helper()
	srv := mcpserver.NewMCPServer("manager-test", "0.0.1")
	for _, name := range names {
		n := name
		t := mcppkg.NewTool(n, mcppkg.WithDescription("tool "+n))
		srv.AddTool(t, func(_ context.Context, _ mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
			return mcppkg.NewToolResultText("ok:" + n), nil
		})
	}
	return srv
}

func (d *countingDialer) dial(ctx context.Context, spec ServerSpec) (*Client, error) {
	d.mu.Lock()
	d.dials[spec.ID]++
	srv := d.servers[spec.ID]
	if srv == nil {
		srv = d.servers["default"]
	}
	fail := d.failFor[spec.ID]
	d.mu.Unlock()
	if fail != nil {
		return nil, fail
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
			ClientInfo:      mcppkg.Implementation{Name: "test", Version: "0"},
		},
	}); err != nil {
		return nil, err
	}
	return &Client{spec: spec, raw: raw}, nil
}

func (d *countingDialer) dialCount(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials[id]
}

func TestManager_ApplyRegistersTools(t *testing.T) {
	dialer := newCountingDialer(t, "echo", "add")
	reg := tool.NewRegistry()
	events := map[string]string{}
	m := NewManager(reg, ManagerOptions{
		Dial:   dialer.dial,
		Logger: zaptest.NewLogger(t),
		OnServerError: func(id, msg string) {
			events[id] = msg
		},
	})
	defer m.Close()

	spec := ServerSpec{ID: "demo", Name: "Demo", Command: "node", Args: []string{"server.js"}}
	status := m.Apply(context.Background(), []ServerSpec{spec})

	if len(status) != 1 || !status[0].Connected {
		t.Fatalf("status = %+v, want one connected server", status)
	}
	if got := reg.Names(); len(got) != 2 {
		t.Fatalf("registry names = %v, want 2 tools", got)
	}
	if !reg.IsAllowed("echo") {
		t.Error("echo should be permitted so the model can call it")
	}
	if msg, ok := events["demo"]; !ok || msg != "" {
		t.Errorf("observer events = %v, want a clean demo entry", events)
	}
}

func TestManager_ApplyIsIdempotentForUnchangedServer(t *testing.T) {
	dialer := newCountingDialer(t, "echo")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	spec := ServerSpec{ID: "demo", Name: "Demo", Command: "node"}
	m.Apply(context.Background(), []ServerSpec{spec})
	m.Apply(context.Background(), []ServerSpec{spec})

	if got := dialer.dialCount("demo"); got != 1 {
		t.Errorf("dials = %d, want 1: an unchanged server must keep its connection", got)
	}
	if got := len(reg.Names()); got != 1 {
		t.Errorf("tools = %d, want 1 (no duplicates from the second apply)", got)
	}
}

func TestManager_ApplyReconnectsOnEdit(t *testing.T) {
	dialer := newCountingDialer(t, "echo")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	ctx := context.Background()
	m.Apply(ctx, []ServerSpec{{ID: "demo", Name: "Demo", Command: "node", Args: []string{"a.js"}}})
	// A rename alone must not drop the connection…
	m.Apply(ctx, []ServerSpec{{ID: "demo", Name: "Renamed", Command: "node", Args: []string{"a.js"}}})
	if got := dialer.dialCount("demo"); got != 1 {
		t.Errorf("dials after rename = %d, want 1", got)
	}

	// …but changing how it is reached must.
	m.Apply(ctx, []ServerSpec{{ID: "demo", Name: "Renamed", Command: "node", Args: []string{"b.js"}}})
	if got := dialer.dialCount("demo"); got != 2 {
		t.Errorf("dials after argument change = %d, want 2", got)
	}
	if got := reg.Names(); len(got) != 1 {
		t.Errorf("tools = %v, want exactly one (the old registration must go)", got)
	}
}

func TestManager_ApplyRemovesDeletedServer(t *testing.T) {
	dialer := newCountingDialer(t, "echo")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	ctx := context.Background()
	m.Apply(ctx, []ServerSpec{{ID: "demo", Name: "Demo", Command: "node"}})
	status := m.Apply(ctx, nil)

	if len(status) != 0 {
		t.Fatalf("status = %+v, want empty after removal", status)
	}
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("tools = %v, want none: a deleted server must take its tools with it", got)
	}
}

func TestManager_ApplyReportsConnectFailure(t *testing.T) {
	dialer := newCountingDialer(t, "echo")
	dialer.failFor["broken"] = errors.New("spawn: no such file")
	reg := tool.NewRegistry()

	type event struct{ id, msg string }
	var events []event
	m := NewManager(reg, ManagerOptions{
		Dial:   dialer.dial,
		Logger: zaptest.NewLogger(t),
		OnServerError: func(id, msg string) {
			events = append(events, event{id, msg})
		},
	})
	defer m.Close()

	status := m.Apply(context.Background(), []ServerSpec{
		{ID: "broken", Name: "Broken", Command: "nope"},
		{ID: "ok", Name: "Ok", Command: "node"},
	})

	if len(status) != 2 {
		t.Fatalf("status = %+v, want both servers reported", status)
	}
	byID := map[string]ServerStatus{}
	for _, st := range status {
		byID[st.ID] = st
	}
	if byID["broken"].Connected || !strings.Contains(byID["broken"].Error, "no such file") {
		t.Errorf("broken = %+v, want a recorded failure", byID["broken"])
	}
	if !byID["ok"].Connected {
		t.Errorf("ok = %+v, want connected: one broken server must not stop the others", byID["ok"])
	}
	if len(events) != 2 {
		t.Errorf("events = %+v, want a report for each server", events)
	}
}

func TestManager_ApplyRejectsInvalidSpec(t *testing.T) {
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: newCountingDialer(t, "echo").dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	status := m.Apply(context.Background(), []ServerSpec{{ID: "empty", Name: "Empty"}})
	if len(status) != 1 || status[0].Connected {
		t.Fatalf("status = %+v, want an unconnected server", status)
	}
	if !strings.Contains(status[0].Error, "command is required") {
		t.Errorf("error = %q, want it to name the missing field", status[0].Error)
	}
}

func TestManager_ToolNameCollisionIsReported(t *testing.T) {
	// Two servers both exposing "echo": the second must not steal the first's
	// registration, and the collision must be visible rather than silent.
	dialer := newCountingDialer(t, "echo")
	dialer.servers["second"] = serverWith(t, "echo", "other")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	status := m.Apply(context.Background(), []ServerSpec{
		{ID: "first", Name: "First", Command: "node"},
		{ID: "second", Name: "Second", Command: "node"},
	})

	byID := map[string]ServerStatus{}
	for _, st := range status {
		byID[st.ID] = st
	}
	if !strings.Contains(byID["second"].Error, "already registered") {
		t.Errorf("second.Error = %q, want the collision reported", byID["second"].Error)
	}
	if len(byID["second"].Tools) != 1 || byID["second"].Tools[0] != "other" {
		t.Errorf("second.Tools = %v, want just the tool that could be added", byID["second"].Tools)
	}
	if got := len(reg.Names()); got != 2 {
		t.Errorf("registry size = %d, want 2 (echo + other)", got)
	}
}

func TestManager_RefreshReconnectsFailedServer(t *testing.T) {
	dialer := newCountingDialer(t, "echo")
	dialer.failFor["demo"] = errors.New("boom")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	ctx := context.Background()
	spec := ServerSpec{ID: "demo", Name: "Demo", Command: "node"}
	if st := m.Apply(ctx, []ServerSpec{spec}); st[0].Connected {
		t.Fatal("first apply should have failed")
	}

	// The failure is fixed and the operator clicks 重连: Refresh must retry a
	// server whose spec never changed.
	dialer.mu.Lock()
	delete(dialer.failFor, "demo")
	dialer.mu.Unlock()

	st := m.Refresh(ctx)
	if len(st) != 1 || !st[0].Connected {
		t.Fatalf("status after refresh = %+v, want connected", st)
	}
	if got := reg.Names(); len(got) != 1 {
		t.Errorf("tools = %v, want the refreshed server's tool", got)
	}
}

func TestManager_CloseUnregistersTools(t *testing.T) {
	dialer := newCountingDialer(t, "echo", "add")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})

	m.Apply(context.Background(), []ServerSpec{{ID: "demo", Name: "Demo", Command: "node"}})
	m.Close()

	if got := reg.Names(); len(got) != 0 {
		t.Errorf("tools = %v, want none after Close", got)
	}
}

func TestManager_InspectLeavesRuntimeAlone(t *testing.T) {
	dialer := newCountingDialer(t, "echo", "add")
	reg := tool.NewRegistry()
	m := NewManager(reg, ManagerOptions{Dial: dialer.dial, Logger: zaptest.NewLogger(t)})
	defer m.Close()

	specs, err := m.Inspect(context.Background(), ServerSpec{ID: "probe", Name: "Probe", Command: "node"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(specs) != 2 {
		t.Errorf("specs = %d, want 2", len(specs))
	}
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registry = %v, want untouched: 测试连接 must not register anything", got)
	}
	if got := m.Status(); len(got) != 0 {
		t.Errorf("status = %v, want empty", got)
	}
}

func TestManager_InspectRejectsInvalidSpec(t *testing.T) {
	m := NewManager(tool.NewRegistry(), ManagerOptions{Dial: newCountingDialer(t, "echo").dial})
	if _, err := m.Inspect(context.Background(), ServerSpec{Name: "NoURL", Transport: TransportSSE}); err == nil {
		t.Fatal("expected url validation error")
	}
}

func TestParseTransport(t *testing.T) {
	cases := map[string]Transport{
		"":                TransportStdio,
		"stdio":           TransportStdio,
		"STDIO":           TransportStdio,
		" sse ":           TransportSSE,
		"http":            TransportHTTP,
		"streamable-http": TransportHTTP,
	}
	for in, want := range cases {
		got, err := ParseTransport(in)
		if err != nil {
			t.Fatalf("ParseTransport(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseTransport(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseTransport("carrier-pigeon"); err == nil {
		t.Error("expected an error for an unknown transport")
	}
}

func TestServerID(t *testing.T) {
	cases := map[string]string{
		"GitHub MCP":     "github-mcp",
		"  filesystem  ": "filesystem",
		"123":            "mcp-123",
		"!!!":            "mcp-server",
		"a/b":            "a-b",
	}
	for in, want := range cases {
		if got := ServerID(in); got != want {
			t.Errorf("ServerID(%q) = %q, want %q", in, got, want)
		}
	}
}
