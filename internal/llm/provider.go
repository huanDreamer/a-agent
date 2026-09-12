// Package llm provides a multi-provider LLM client built on eino's
// BaseChatModel abstraction. All providers speak the OpenAI HTTP protocol,
// so a single adapter handles DeepSeek / Qwen / GLM / OpenAI / Ollama.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
)

// Provider is the configuration for a single LLM backend.
type Provider struct {
	Name    string // unique identifier (deepseek, qwen, glm, openai, ollama)
	BaseURL string // OpenAI-compatible API root
	APIKey  string // empty for local providers (e.g. ollama)
	Model   string // default model name
	// DisableUsageRequest suppresses stream_options.include_usage. Streaming
	// needs it to report token usage, but it is an OpenAI extension: a provider
	// that rejects unknown request fields would fail every streamed call, so it
	// can be turned off per provider.
	DisableUsageRequest bool
}

// LLMError wraps an upstream LLM call failure with provider context.
type LLMError struct {
	Provider   string
	Model      string
	StatusCode int
	Body       string
	Err        error
}

func (e *LLMError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("llm %s/%s: status=%d body=%q: %v", e.Provider, e.Model, e.StatusCode, e.Body, e.Err)
	}
	return fmt.Sprintf("llm %s/%s: %v", e.Provider, e.Model, e.Err)
}

func (e *LLMError) Unwrap() error { return e.Err }

// New constructs a BaseChatModel for the given provider.
func New(p Provider) (model.BaseChatModel, error) {
	if p.Name == "" {
		return nil, errors.New("llm: provider name is required")
	}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("llm: provider %q: base_url is required", p.Name)
	}
	if p.Model == "" {
		return nil, fmt.Errorf("llm: provider %q: model is required", p.Name)
	}
	cfg := openai.DefaultConfig(p.APIKey)
	cfg.BaseURL = p.BaseURL
	return &openAIModel{
		provider: p,
		client:   openai.NewClientWithConfig(cfg),
	}, nil
}

// openAIModel is the eino adapter around go-openai.
type openAIModel struct {
	provider Provider
	client   *openai.Client
	tools    []*schema.ToolInfo // bound tools (immutable; use WithTools to derive)
}

// Name returns the provider's configured model name. It is how tracing and
// logging attribute a call to a specific model without the caller having to
// thread the name through; without it a trace records no model at all.
func (m *openAIModel) Name() string { return m.provider.Model }

// Provider returns the provider name (deepseek, qwen, ...).
func (m *openAIModel) Provider() string { return m.provider.Name }

// WithTools returns a copy of m with the given tools bound. It
// implements model.ToolCallingChatModel so the same model can be
// reused by the ReAct agent (which calls WithTools per request)
// without mutating shared state.
func (m *openAIModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	cp := *m
	cp.tools = append([]*schema.ToolInfo(nil), tools...)
	return &cp, nil
}

// toOpenAITools converts eino ToolInfo into the openai SDK shape used
// by ChatCompletionRequest.Tools. Parameters are serialized from
// the eino ParamsOneOf via ToJSONSchema, which handles both
// struct-derived and raw-JSON-Schema tool definitions.
func toOpenAITools(specs []*schema.ToolInfo) []openai.Tool {
	out := make([]openai.Tool, 0, len(specs))
	for _, s := range specs {
		var params json.RawMessage
		if s.ParamsOneOf != nil {
			js, err := s.ParamsOneOf.ToJSONSchema()
			if err == nil && js != nil {
				if b, mErr := json.Marshal(js); mErr == nil {
					params = b
				}
			}
		}
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        s.Name,
				Description: s.Desc,
				Parameters:  params,
			},
		})
	}
	return out
}

// toOpenAIMessages converts eino messages into the openai SDK shape.
//
// ToolCallID and ToolCalls MUST be carried across. A tool result is only
// meaningful as the answer to a specific call, and providers enforce it: a
// `role: "tool"` message without `tool_call_id` is rejected outright
// ("missing field `tool_call_id`"), and an assistant message whose tool_calls
// were dropped leaves the following results answering nothing.
//
// ReasoningContent is deliberately NOT sent back. Providers that emit it reject
// it on input, and the stored reasoning is for display only.
func (m *openAIModel) toOpenAIMessages(msgs []*schema.Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, msg := range msgs {
		converted := openai.ChatCompletionMessage{
			Role:       string(msg.Role),
			Content:    msg.Content,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}
		if len(msg.ToolCalls) > 0 {
			converted.ToolCalls = make([]openai.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				call := openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolType(tc.Type),
					Function: openai.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
				// The SDK omits Index when nil, which is what the request
				// format wants; only carry it when the caller set it.
				if tc.Index != nil {
					idx := *tc.Index
					call.Index = &idx
				}
				if call.Type == "" {
					call.Type = openai.ToolTypeFunction
				}
				converted.ToolCalls = append(converted.ToolCalls, call)
			}
		}
		out = append(out, converted)
	}
	return out
}

func (m *openAIModel) wrapErr(err error, statusCode int, body string) error {
	return &LLMError{
		Provider:   m.provider.Name,
		Model:      m.provider.Model,
		StatusCode: statusCode,
		Body:       body,
		Err:        err,
	}
}

// applyOptions copies eino options onto the request. Phase 1 only honors
// temperature and max tokens; tool/JSON-mode/etc. come in later phases.
func (m *openAIModel) applyOptions(req *openai.ChatCompletionRequest, opts []model.Option) {
	o := model.GetCommonOptions(nil, opts...)
	if o.Temperature != nil {
		req.Temperature = *o.Temperature
	}
	if o.MaxTokens != nil {
		req.MaxTokens = *o.MaxTokens
	}
}

// Generate implements model.BaseChatModel.
func (m *openAIModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	req := openai.ChatCompletionRequest{
		Model:    m.provider.Model,
		Messages: m.toOpenAIMessages(msgs),
	}
	if len(m.tools) > 0 {
		req.Tools = toOpenAITools(m.tools)
	}
	m.applyOptions(&req, opts)

	resp, err := m.client.CreateChatCompletion(ctx, req)
	if err != nil {
		var apiErr *openai.APIError
		if errors.As(err, &apiErr) {
			return nil, m.wrapErr(err, apiErr.HTTPStatusCode, apiErr.Message)
		}
		return nil, m.wrapErr(err, 0, "")
	}
	if len(resp.Choices) == 0 {
		return nil, m.wrapErr(errors.New("no choices in response"), 0, "")
	}
	c := resp.Choices[0]
	out := &schema.Message{
		Role:    schema.Assistant,
		Content: c.Message.Content,
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: string(c.FinishReason),
			Usage:        toTokenUsage(resp.Usage),
		},
	}
	if len(c.Message.ToolCalls) > 0 {
		out.ToolCalls = toEinoToolCalls(c.Message.ToolCalls)
	}
	return out, nil
}

// Stream implements model.BaseChatModel.
func (m *openAIModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	req := openai.ChatCompletionRequest{
		Model:    m.provider.Model,
		Messages: m.toOpenAIMessages(msgs),
		Stream:   true,
	}
	// Ask for token usage on the final chunk. Without this the stream never
	// reports usage, so a streamed conversation would show zero tokens and cost
	// nothing. It is opt-out because it is an OpenAI extension that a strict
	// provider may reject.
	if !m.provider.DisableUsageRequest {
		req.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	if len(m.tools) > 0 {
		req.Tools = toOpenAITools(m.tools)
	}
	m.applyOptions(&req, opts)

	stream, err := m.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		var apiErr *openai.APIError
		if errors.As(err, &apiErr) {
			return nil, m.wrapErr(err, apiErr.HTTPStatusCode, apiErr.Message)
		}
		return nil, m.wrapErr(err, 0, "")
	}

	sr, sw := newMessagePipe(64)
	go func() {
		defer sw.Close()
		defer stream.Close()
		for {
			chunk, recvErr := stream.Recv()
			if errors.Is(recvErr, io.EOF) {
				return
			}
			if recvErr != nil {
				_ = sw.Send(nil, m.wrapErr(recvErr, 0, ""))
				return
			}
			// The usage-only chunk carries no choices, so it must be handled
			// before the choice checks or the token counts are lost.
			if chunk.Usage != nil && len(chunk.Choices) == 0 {
				if closed := sw.Send(&schema.Message{
					Role:         schema.Assistant,
					ResponseMeta: &schema.ResponseMeta{Usage: toTokenUsage(*chunk.Usage)},
				}, nil); closed {
					return
				}
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			c := chunk.Choices[0]
			msg := &schema.Message{
				Role:    schema.Assistant,
				Content: c.Delta.Content,
				// Reasoning models stream their thinking in a separate field;
				// dropping it would hide the model's reasoning entirely.
				ReasoningContent: c.Delta.ReasoningContent,
			}
			if len(c.Delta.ToolCalls) > 0 {
				msg.ToolCalls = toEinoToolCalls(c.Delta.ToolCalls)
			}
			meta := &schema.ResponseMeta{}
			if c.FinishReason != "" {
				meta.FinishReason = string(c.FinishReason)
			}
			if u := toTokenUsagePtr(chunk.Usage); u != nil {
				meta.Usage = u
			}
			if meta.FinishReason != "" || meta.Usage != nil {
				msg.ResponseMeta = meta
			}
			if closed := sw.Send(msg, nil); closed {
				return
			}
		}
	}()
	return sr, nil
}

// toTokenUsagePtr converts an optional usage payload.
func toTokenUsagePtr(u *openai.Usage) *schema.TokenUsage {
	if u == nil {
		return nil
	}
	return toTokenUsage(*u)
}

func toTokenUsage(u openai.Usage) *schema.TokenUsage {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return nil
	}
	return &schema.TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// toEinoToolCalls adapts the streaming delta ToolCalls from go-openai
// into the eino schema representation. The openai SDK emits one chunk
// per call index per delta; eino passes each delta through to its
// own ReAct loop, which is responsible for merging same-index chunks.
func toEinoToolCalls(deltas []openai.ToolCall) []schema.ToolCall {
	out := make([]schema.ToolCall, 0, len(deltas))
	for _, d := range deltas {
		tc := schema.ToolCall{
			ID:       d.ID,
			Type:     string(d.Type),
			Function: schema.FunctionCall{Name: d.Function.Name, Arguments: d.Function.Arguments},
		}
		if d.Index != nil {
			idx := *d.Index
			tc.Index = &idx
		}
		out = append(out, tc)
	}
	return out
}
