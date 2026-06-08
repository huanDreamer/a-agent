package mcp

import (
	"context"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"

	"github.com/huan/huan-agent/internal/tool"
)

func TestConnect_EmptyCommand(t *testing.T) {
	_, err := Connect(context.Background(), ServerSpec{Name: "x"})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestConnect_NotFound(t *testing.T) {
	_, err := Connect(context.Background(), ServerSpec{
		Name:    "x",
		Command: "/this/binary/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestExtractText(t *testing.T) {
	got := extractText([]mcppkg.Content{
		mcppkg.TextContent{Type: "text", Text: "hello"},
		mcppkg.TextContent{Type: "text", Text: "world"},
	})
	if got != "hello\nworld" {
		t.Errorf("got %q, want %q", got, "hello\nworld")
	}
}

func TestExtractText_Empty(t *testing.T) {
	if got := extractText(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := extractText([]mcppkg.Content{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestToolInputSchemaJSON(t *testing.T) {
	type obj struct {
		Type       string `json:"type"`
		Properties map[string]any `json:"properties"`
	}
	if got := toolInputSchemaJSON(nil); got != "{}" {
		t.Errorf("nil = %q, want {}", got)
	}
	if got := toolInputSchemaJSON(obj{Type: "object"}); !strings.Contains(got, `"type":"object"`) {
		t.Errorf("got %q", got)
	}
}

func TestParseJSONSchema(t *testing.T) {
	const s = `{"type":"object","properties":{"x":{"type":"string"}}}`
	got := parseJSONSchema(s)
	if got == nil {
		t.Fatal("nil result for valid schema")
	}
	if got.Type != "object" {
		t.Errorf("type = %q", got.Type)
	}
	if got.Properties == nil {
		t.Fatal("missing properties")
	}
	if v, ok := got.Properties.Get("x"); !ok || v == nil {
		t.Error("missing properties.x")
	}

	if parseJSONSchema("") != nil {
		t.Error("empty input should return nil")
	}
	if parseJSONSchema("not json") != nil {
		t.Error("invalid input should return nil")
	}
}

func TestClient_NotConnectedErrors(t *testing.T) {
	var c *Client
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Error("nil client should error")
	}
	if _, err := c.CallTool(context.Background(), "x", "{}"); err == nil {
		t.Error("nil client should error")
	}
	if err := c.Close(); err != nil {
		t.Errorf("nil client Close: %v", err)
	}
}

func TestRegisterMCPTools_Empty(t *testing.T) {
	// Use a fake "Client" that has no underlying mcp-go client.
	// We can't construct one through the public API, so we test
	// the bridge with a *Client that points to a non-existent
	// server: ListTools will fail, RegisterMCPTools will return
	// the wrapped error. The empty / no-tools case is covered
	// implicitly by integration tests in later phases.
	t.Skip("requires mcp-go in-process server; covered manually")
	_ = tool.NewRegistry()
}
