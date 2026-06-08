package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap/zaptest"

	"github.com/huan/huan-agent/internal/tool"
)

// inProcessClient builds a *Client backed by an in-process MCP server
// so we can exercise ListTools / CallTool without spawning a real
// subprocess. The returned cleanup must run at the end of the test.
func inProcessClient(t *testing.T, srv *mcpserver.MCPServer) (*Client, func()) {
	t.Helper()
	raw, err := mcpclient.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := raw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := raw.Initialize(ctx, mcppkg.InitializeRequest{
		Params: mcppkg.InitializeParams{
			ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcppkg.Implementation{
				Name:    "huan-agent-test",
				Version: "0.0.1",
			},
		},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c := &Client{spec: ServerSpec{Name: "inproc"}, raw: raw}
	return c, func() { _ = c.Close() }
}

// newEchoServer builds an MCP server with a single "echo" tool that
// returns its `msg` argument, plus a "boom" tool that always errors.
func newEchoServer(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	srv := mcpserver.NewMCPServer("echo-test", "0.0.1")

	echoTool := mcppkg.NewTool("echo",
		mcppkg.WithDescription("Echo a message back to the caller"),
		mcppkg.WithString("msg", mcppkg.Description("the message")),
	)
	srv.AddTool(echoTool, func(_ context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		args, _ := req.Params.Arguments.(map[string]any)
		msg, _ := args["msg"].(string)
		return mcppkg.NewToolResultText("echo:" + msg), nil
	})

	boomTool := mcppkg.NewTool("boom",
		mcppkg.WithDescription("Always errors"),
	)
	srv.AddTool(boomTool, func(_ context.Context, _ mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		return &mcppkg.CallToolResult{
			IsError: true,
			Content: []mcppkg.Content{
				mcppkg.TextContent{Type: "text", Text: "boom failed"},
			},
		}, nil
	})

	return srv
}

func TestClient_ListAndCall_InProcess(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()

	ctx := context.Background()
	specs, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	var echoSpec *tool.Spec
	for _, s := range specs {
		if s.Name == "echo" {
			echoSpec = s
			break
		}
	}
	if echoSpec == nil {
		t.Fatal("missing echo in list")
	}
	if !strings.Contains(echoSpec.Description, "Echo") {
		t.Errorf("description = %q", echoSpec.Description)
	}
	if !strings.Contains(echoSpec.ParametersJSONSchema, `"msg"`) {
		t.Errorf("schema = %q", echoSpec.ParametersJSONSchema)
	}

	got, err := c.CallTool(ctx, "echo", `{"msg":"hi"}`)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "echo:hi" {
		t.Errorf("got %q, want %q", got, "echo:hi")
	}
}

func TestClient_CallTool_ServerError(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()

	_, err := c.CallTool(context.Background(), "boom", "{}")
	if err == nil {
		t.Fatal("expected error from boom")
	}
	if !strings.Contains(err.Error(), "boom failed") {
		t.Errorf("error missing server text: %v", err)
	}
}

func TestClient_CallTool_BadJSON(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()

	// Invalid JSON should still reach the server with the `_raw`
	// fallback key — echo treats missing "msg" as empty string.
	got, err := c.CallTool(context.Background(), "echo", `not json`)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "echo:" {
		t.Errorf("got %q, want %q", got, "echo:")
	}
}

func TestClient_CallTool_EmptyArgs(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()

	got, err := c.CallTool(context.Background(), "echo", "")
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "echo:" {
		t.Errorf("got %q, want %q", got, "echo:")
	}
}

func TestClient_Name(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()
	if c.Name() != "inproc" {
		t.Errorf("Name = %q", c.Name())
	}
}

func TestClient_DoubleClose(t *testing.T) {
	c, _ := inProcessClient(t, newEchoServer(t))
	if err := c.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestRegisterMCPTools_InProcess(t *testing.T) {
	c, cleanup := inProcessClient(t, newEchoServer(t))
	defer cleanup()

	reg := tool.NewRegistry()
	n, err := RegisterMCPTools(context.Background(), reg, c, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("RegisterMCPTools: %v", err)
	}
	if n != 2 {
		t.Errorf("registered = %d, want 2", n)
	}
	if got := reg.Names(); len(got) != 2 {
		t.Errorf("registry has %d tools, want 2", len(got))
	}
}

func TestRegisterMCPTools_DisconnectedError(t *testing.T) {
	reg := tool.NewRegistry()
	_, err := RegisterMCPTools(context.Background(), reg, nil, nil)
	if err == nil {
		t.Error("expected error for nil client")
	}
}
