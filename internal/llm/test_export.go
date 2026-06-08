package llm

import "github.com/cloudwego/eino/components/model"

// OpenAIModelForTest is a thin alias for *openAIModel so test files
// in other packages (agent, mcp) can refer to the concrete type
// without reaching into unexported internals. Build-tagged to
// tests; do NOT use in production code.
type OpenAIModelForTest = openAIModel

// AsOpenAIModelForTest exposes the concrete type for tests in other
// packages. Production code should use the model.BaseChatModel /
// model.ToolCallingChatModel interfaces instead.
func AsOpenAIModelForTest(m model.BaseChatModel) *OpenAIModelForTest {
	if oam, ok := m.(*openAIModel); ok {
		return oam
	}
	return nil
}
