// Package mcp integrates with Model Context Protocol servers. It supports the
// three transports an operator can configure: stdio (spawn a local command),
// sse (the legacy HTTP+SSE protocol) and http (streamable HTTP). The package is
// intentionally small — it wraps mark3labs/mcp-go and bridges MCP tools into the
// internal/tool registry used by the agent loop.
package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcppkg "github.com/mark3labs/mcp-go/mcp"

	"github.com/huan/huan-agent/internal/tool"
)

// Transport is how a server is reached. The values match store.MCPTransport,
// which is what the console stores; the two cannot share a type because the
// client package must not depend on the database layer.
type Transport string

const (
	// TransportStdio spawns a command and talks over its stdin/stdout.
	TransportStdio Transport = "stdio"
	// TransportSSE dials the base URL and uses the HTTP+SSE transport.
	TransportSSE Transport = "sse"
	// TransportHTTP dials the base URL and uses streamable HTTP.
	TransportHTTP Transport = "http"
)

// ParseTransport normalises a configured transport name. An empty value means
// stdio, which is what every pre-existing config entry is.
func ParseTransport(s string) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "stdio", "local", "command":
		return TransportStdio, nil
	case "sse":
		return TransportSSE, nil
	case "http", "streamable-http", "streamable_http", "streamablehttp":
		return TransportHTTP, nil
	default:
		return "", fmt.Errorf("mcp: unsupported transport %q (want stdio, sse or http)", s)
	}
}

// Remote reports whether the transport dials a URL rather than spawning a
// command.
func (t Transport) Remote() bool { return t != TransportStdio && t != "" }

// ServerSpec describes one MCP server: how to reach it and how the runtime
// keys it.
//
// ID is the stable identity (a database row's id, or a slug derived from the
// name for a config-file entry); Name is the display name. They are separate
// because renaming a server must not orphan its connection state.
type ServerSpec struct {
	ID        string
	Name      string
	Transport Transport
	// Command, Args and Env describe a stdio server. Env entries follow the
	// "KEY=value" shape and are merged onto os.Environ() at spawn.
	Command string
	Args    []string
	Env     []string
	// URL and Headers describe a remote server. Headers entries are
	// "Name: value" (an HTTP header line).
	URL     string
	Headers []string
}

// ResolvedTransport returns the spec's transport, defaulting to stdio.
func (s ServerSpec) ResolvedTransport() Transport {
	if s.Transport == "" {
		return TransportStdio
	}
	return s.Transport
}

// Validate reports what a connection actually needs. It runs before a dial so
// the error names the missing field rather than surfacing as a spawn or dial
// failure with no context.
func (s ServerSpec) Validate() error {
	switch s.ResolvedTransport() {
	case TransportStdio:
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("mcp: %s: command is required for a stdio server", s.Name)
		}
	case TransportSSE, TransportHTTP:
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("mcp: %s: url is required for a %s server", s.Name, s.ResolvedTransport())
		}
	default:
		return fmt.Errorf("mcp: %s: unsupported transport %q", s.Name, s.Transport)
	}
	return nil
}

// Fingerprint renders the connection-relevant fields, so the runtime can tell
// whether a server needs reconnecting after an edit. Display-only fields (the
// name) are deliberately excluded: renaming a server should not drop its
// connection.
func (s ServerSpec) Fingerprint() string {
	return strings.Join([]string{
		string(s.ResolvedTransport()),
		s.Command,
		strings.Join(s.Args, "\x00"),
		strings.Join(s.Env, "\x00"),
		s.URL,
		strings.Join(s.Headers, "\x00"),
	}, "\x01")
}

// Client wraps an mcp-go client and the subprocess it owns.
type Client struct {
	spec   ServerSpec
	raw    *mcpclient.Client
	mu     sync.Mutex
	closed bool
}

// connectTimeout bounds the initialize handshake. A server that spawns but
// never answers must not hold a request (or the startup sync) open.
const connectTimeout = 10 * time.Second

// Connect builds a client for the spec, performs the initialize handshake, and
// returns it ready to use.
//
// The returned Client MUST be Close()d by the caller: it owns a subprocess or a
// live HTTP stream.
func Connect(ctx context.Context, spec ServerSpec) (*Client, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", spec.Name, err)
	}

	var (
		raw *mcpclient.Client
		err error
	)
	switch spec.ResolvedTransport() {
	case TransportStdio:
		env := append(os.Environ(), spec.Env...)
		raw, err = mcpclient.NewStdioMCPClient(spec.Command, env, spec.Args...)
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: spawn: %w", spec.Name, err)
		}
	case TransportSSE:
		raw, err = mcpclient.NewSSEMCPClient(spec.URL, mcpclient.WithHeaders(headerMap(spec.Headers)))
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: sse client: %w", spec.Name, err)
		}
	case TransportHTTP:
		raw, err = mcpclient.NewStreamableHttpClient(spec.URL,
			transport.WithHTTPHeaders(headerMap(spec.Headers)),
			transport.WithHTTPTimeout(connectTimeout))
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: http client: %w", spec.Name, err)
		}
	}

	// A remote transport needs an explicit Start. Its connection outlives the
	// call that opened it (the runtime holds it until the server is edited or
	// deleted), so the caller's cancellation is deliberately dropped: cancelling
	// an admin request must not tear down a shared connection.
	if spec.ResolvedTransport().Remote() {
		if err := raw.Start(context.WithoutCancel(ctx)); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("mcp: %s: start %s transport: %w", spec.Name, spec.ResolvedTransport(), err)
		}
	}

	initCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if _, err := raw.Initialize(initCtx, mcppkg.InitializeRequest{
		Params: mcppkg.InitializeParams{
			ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcppkg.Implementation{
				Name:    "huan-agent",
				Version: "0.1.0",
			},
			Capabilities: mcppkg.ClientCapabilities{},
		},
	}); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("mcp: %s: initialize: %w", spec.Name, err)
	}

	return &Client{spec: spec, raw: raw}, nil
}

// headerMap converts "Name: value" lines into the map mcp-go expects. A line
// without a colon is skipped: an unmatched entry would otherwise become a
// header with a name and no value, which some servers reject outright.
func headerMap(lines []string) map[string]string {
	if len(lines) == 0 {
		return nil
	}
	out := make(map[string]string, len(lines))
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		out[name] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WrapClient adapts an already-initialized mcp-go client, so a caller that
// built its own transport can still use this package's ListTools / CallTool
// bridge.
//
// It is how the in-process transport is used (mcp-go's NewInProcessClient),
// which is what makes the runtime and the API above it testable without
// spawning a process: the same Manager, registry and routes are exercised
// either way.
func WrapClient(spec ServerSpec, raw *mcpclient.Client) *Client {
	return &Client{spec: spec, raw: raw}
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
			Name:                 t.Name,
			Description:          t.Description,
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
