package builtin

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// EchoInput is the parameter schema for the "echo" tool.
type EchoInput struct {
	Text string `json:"text" jsonschema:"description=Text to echo back, required"`
}

// EchoOutput is what the echo tool returns.
type EchoOutput struct {
	Text string `json:"text"`
}

// NewEchoTool returns a Tool that simply returns its input. Useful
// for plumbing tests and ad-hoc debugging.
func NewEchoTool() (tool.InvokableTool, error) {
	return utils.InferTool("echo",
		"Return the input text unchanged. Useful for debugging the tool-call path.",
		func(_ context.Context, in EchoInput) (EchoOutput, error) {
			if in.Text == "" {
				return EchoOutput{}, fmt.Errorf("text is required")
			}
			return EchoOutput{Text: in.Text}, nil
		})
}
