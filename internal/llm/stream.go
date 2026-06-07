package llm

import "github.com/cloudwego/eino/schema"

// newMessagePipe wraps schema.Pipe so callers don't need to import eino schema
// for the boilerplate.
func newMessagePipe(cap int) (*schema.StreamReader[*schema.Message], *schema.StreamWriter[*schema.Message]) {
	return schema.Pipe[*schema.Message](cap)
}
