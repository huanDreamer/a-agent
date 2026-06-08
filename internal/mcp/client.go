// Package mcp integrates with Model Context Protocol servers. Phase 2
// supports only the stdio transport; SSE / HTTP will land in a
// later phase. The package is intentionally small — it wraps
// mark3labs/mcp-go and bridges MCP tools into the internal/tool
// registry used by the agent loop.
package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	mcpclient "github.com/mark3labs/mcp-go/client"

	"github.com/huan/huan-agent/internal/tool"
)

// ServerSpec describes how to spawn a single MCP server. Mirrors
// config.MCPServer so this package does not need to import config.
type ServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     []string // merged onto os.Environ()
}

// Client wraps an mcp-go client and the subprocess it owns.
type Client struct {
	spec   ServerSpec
	raw    *mcpclient.Client
	mu     sync.Mutex
	closed bool
}

// Connect spawns the MCP server subprocess, performs the
// initialize handshake, and returns a ready Client.
//
// The returned Client MUST be Close()d by the caller to kill the
// subprocess and release its pipes.
func Connect(ctx context.Context, spec ServerSpec) (*Client, error) {
	if spec.Command == "" {
		return nil, fmt.Errorf("mcp: %s: command is required", spec.Name)
	}
	env := append(os.Environ(), spec.Env...)

	c, err := mcpclient.NewStdioMCPClient(spec.Command, env, spec.Args...)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: spawn: %w", spec.Name, err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.Initialize(initCtx, mcppkg.InitializeRequest{
		Params: mcppkg.InitializeParams{
			ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcppkg.Implementation{
				Name:    "huan-agent",
				Version: "0.1.0",
			},
			Capabilities: mcppkg.ClientCapabilities{},
		},
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp: %s: initialize: %w", spec.Name, err)
	}

	return &Client{spec: spec, raw: c}, nil
}

// ListTools returns the tool metadata for this server, adapted to
// the eino schema.ToolInfo shape.
func (c *Client) ListTools(ctx context.Context) ([]*tool.Spec, error) {
	if c == nil || c.raw == nil {
		return nil, fmt.Errorf("mcp: client not connected")
	}
	res, err := c.raw.ListTools(ctx, mcppkg.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: list tools: %w", c.spec.Name, err)
	}
	out := make([]*tool.Spec, 0, len(res.Tools))
	for i := range res.Tools {
		t := &res.Tools[i]
		paramsJSON := toolInputSchemaJSON(t.InputSchema)
		out = append(out, &tool.Spec{
			Name:                t.Name,
			Description:         t.Description,
			ParametersJSONSchema: paramsJSON,
		})
	}
	return out, nil
}

// CallTool invokes a tool on the server and returns its text content
// as a string (multiple text blocks are joined with newlines). If
// the tool returned a non-text content type or an isError flag, the
// result is wrapped in an error.
func (c *Client) CallTool(ctx context.Context, name string, argsJSON string) (string, error) {
	if c == nil || c.raw == nil {
		return "", fmt.Errorf("mcp: client not connected")
	}

	// Parse the args JSON into a map so the mcp-go call dispatches it
	// as a structured object, not a stringified value.
	var args map[string]any
	if argsJSON != "" && argsJSON != "null" {
		if err := jsonUnmarshal([]byte(argsJSON), &args); err != nil {
			// Fall back to passing the raw string under a single key
			// so we still get a useful error from the server.
			args = map[string]any{"_raw": argsJSON}
		}
	}

	res, err := c.raw.CallTool(ctx, mcppkg.CallToolRequest{
		Params: mcppkg.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		return "", fmt.Errorf("mcp: %s: call %s: %w", c.spec.Name, name, err)
	}
	if res.IsError {
		// Server-side tool failure: surface the textual content as the
		// error message so the agent loop can feed it back to the LLM.
		text := extractText(res.Content)
		if text == "" {
			text = "tool returned an error"
		}
		return "", fmt.Errorf("mcp: %s: tool %s failed: %s", c.spec.Name, name, text)
	}
	text := extractText(res.Content)
	if text == "" && res.StructuredContent != nil {
		// If the server returned structured content, JSON-encode it.
		b, _ := jsonMarshal(res.StructuredContent)
		text = string(b)
	}
	return text, nil
}

// Close terminates the MCP session and kills the subprocess. Safe
// to call multiple times.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.raw != nil {
		return c.raw.Close()
	}
	return nil
}

// Name returns the server's name from its spec.
func (c *Client) Name() string { return c.spec.Name }

// extractText joins the Text blocks from a CallToolResult Content
// slice. Non-text content is ignored for now; a future phase may
// surface image / audio / embedded resources.
func extractText(content []mcppkg.Content) string {
	parts := make([]string, 0, len(content))
	for _, c := range content {
		if tc, ok := c.(mcppkg.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
