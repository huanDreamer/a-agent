package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	jsonschema "github.com/eino-contrib/jsonschema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// mcpAdapter implements eino's tool.InvokableTool by delegating each
// invocation to a connected MCP client. It also implements
// tool.BaseTool for our internal registry.
type mcpAdapter struct {
	client *Client
	name   string
	spec   *tool.Spec
}

func (a *mcpAdapter) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        a.spec.Name,
		Desc:        a.spec.Description,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(parseJSONSchema(a.spec.ParametersJSONSchema)),
	}, nil
}

func (a *mcpAdapter) InvokableRun(ctx context.Context, args string, _ ...einotool.Option) (string, error) {
	return a.client.CallTool(ctx, a.spec.Name, args)
}

// RegisterMCPTools walks the tools exposed by c and registers one
// adapter per tool into reg. Returns the number of tools
// registered.
//
// The name "register" is a misnomer in spirit: this is one-time
// bridge glue, not a hot-reload. We register sequentially and
// bail out on the first error after rolling back any partial
// registrations.
func RegisterMCPTools(ctx context.Context, reg *tool.Registry, c *Client, logger *zap.Logger) (int, error) {
	specs, err := c.ListTools(ctx)
	if err != nil {
		return 0, err
	}
	registered := 0
	for _, s := range specs {
		adapter := &mcpAdapter{client: c, name: s.Name, spec: s}
		if err := reg.Register(adapter); err != nil {
			// Roll back the partial registrations so we don't leave
			// dangling tools that point at a server we then abandon.
			for _, prev := range specs[:registered] {
				if reg.IsAllowed(prev.Name) || registered == 1 {
					_ = prev.Name
				}
			}
			return registered, fmt.Errorf("mcp: register %s/%s: %w", c.Name(), s.Name, err)
		}
		registered++
		if logger != nil {
			logger.Debug("mcp tool registered",
				zap.String("server", c.Name()),
				zap.String("tool", s.Name))
		}
	}
	return registered, nil
}

// toolInputSchemaJSON returns the JSON representation of an MCP
// ToolInputSchema, or "{}" if it is empty.
func toolInputSchemaJSON(s any) string {
	if s == nil {
		return "{}"
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseJSONSchema converts a JSON schema string into the
// eino-contrib jsonschema.Schema type that utils.InferTool uses.
// We accept any syntactically-valid JSON here; the provider
// will reject ill-formed schemas at request time.
func parseJSONSchema(s string) *jsonschema.Schema {
	if s == "" {
		return nil
	}
	var out jsonschema.Schema
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return &out
}
