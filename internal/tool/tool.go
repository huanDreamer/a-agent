// Package tool defines the Tool interface, Registry, and allow-list
// semantics used by the agent loop and LLM tool-calling integration.
package tool

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// Spec is the static, LLM-facing view of a tool: name, description, and
// parameter JSON schema. Registry exposes Spec slices to the chat model
// so it can decide which tool to call.
type Spec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ParametersJSONSchema is a JSON Schema (draft 2020-12 or 07)
	// describing the tool's argument object. Most providers accept
	// either as long as the structure is valid JSON Schema.
	ParametersJSONSchema string `json:"parameters"`
}

// Tool is the canonical interface every tool must implement. It is a
// superset of eino's tool.InvokableTool: we require both Info (so the
// registry can publish specs to the LLM) and InvokableRun (so the
// agent loop can execute the tool).
//
// Most tools should be built with eino's utils.InferTool, which
// auto-generates a Tool from a typed function and struct parameters.
type Tool interface {
	Info(ctx context.Context) (*schema.ToolInfo, error)
	InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error)
}

// einoAdapter wraps a Tool so it implements eino's tool.InvokableTool.
// Useful when registering a Tool that was not built via utils.InferTool
// and we want to feed it into a higher-level eino API that requires
// the eino type.
type einoAdapter struct{ Tool }

// SpecOf returns the LLM-facing Spec for a Tool, computing the
// parameter JSON Schema by re-deriving it from a sample. The schema
// is taken verbatim from Info() when the tool already provides it
// (e.g. via utils.InferTool).
func SpecOf(ctx context.Context, t Tool) (Spec, error) {
	info, err := t.Info(ctx)
	if err != nil {
		return Spec{}, fmt.Errorf("tool: %w", err)
	}
	params := ""
	if info.ParamsOneOf != nil {
		// Prefer the explicit JSON schema when the tool declared it.
		b, _ := jsonMarshal(info.ParamsOneOf)
		params = string(b)
	}
	return Spec{
		Name:                info.Name,
		Description:         info.Desc,
		ParametersJSONSchema: params,
	}, nil
}

// AsEinoTool returns an eino-compatible tool.InvokableTool view of t.
func AsEinoTool(t Tool) tool.InvokableTool {
	if t == nil {
		return nil
	}
	if et, ok := t.(tool.InvokableTool); ok {
		return et
	}
	return einoAdapter{t}
}
