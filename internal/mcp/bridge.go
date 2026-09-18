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

// RegisterMCPTools walks the tools exposed by c and registers one adapter per
// tool into reg.
//
// It returns the names it registered and one message per tool it could not
// register; the error is reserved for a failure to list tools at all (a
// disconnected client, an unreachable server), where there is nothing to bridge.
//
// This is one-shot bridge glue for a caller that connects a server and keeps it
// for the life of the process (the CLI and the Feishu bot). A runtime that adds
// and removes servers while running uses Manager, which tracks which names it
// registered so it can unregister exactly those.
//
// A name that is already taken is a skip, not a failure — see registerToolSpecs.
func RegisterMCPTools(ctx context.Context, reg *tool.Registry, c *Client, logger *zap.Logger) ([]string, []string, error) {
	specs, err := c.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	names, skipped := registerToolSpecs(reg, c, specs, logger)
	return names, skipped, nil
}

// registerToolSpecs registers one adapter per spec into reg, returning the names
// it added and one message per spec it could not add.
//
// A name that is already taken — by a builtin, or by another MCP server — is
// reported and skipped rather than failing the whole server: the registry
// refuses duplicate names, and a server with one colliding tool is still worth
// connecting. Rolling back the rest and aborting is the tempting alternative,
// and it is wrong: a single colliding name (OpenViking exposes one called
// "grep") would make `chat --tools` refuse to start at all.
//
// This is the single implementation of "bridge this server's tools into the
// registry". The lifecycle manager (manager.go) and the one-shot bridge above
// both go through it, because they once disagreed: the console connected a
// server whose tool collided and kept going, while the CLI aborted the whole
// agent build. Two implementations of one rule is how that happens.
func registerToolSpecs(reg *tool.Registry, c *Client, specs []*tool.Spec, logger *zap.Logger) ([]string, []string) {
	if reg == nil {
		return nil, nil
	}

	var (
		names   = make([]string, 0, len(specs))
		skipped []string
	)
	for _, s := range specs {
		adapter := &mcpAdapter{client: c, name: s.Name, spec: s}
		if err := reg.Register(adapter); err != nil {
			msg := fmt.Sprintf("tool %q not registered: %v", s.Name, err)
			skipped = append(skipped, msg)
			if logger != nil {
				logger.Warn("mcp tool not registered",
					zap.String("server", c.Name()),
					zap.String("tool", s.Name),
					zap.Error(err))
			}
			continue
		}
		names = append(names, s.Name)
		if logger != nil {
			logger.Debug("mcp tool registered",
				zap.String("server", c.Name()),
				zap.String("tool", s.Name))
		}
	}
	return names, skipped
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
