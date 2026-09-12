package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	openai "github.com/sashabaranov/go-openai"
)

// TestGenerate_Success runs an actual HTTP roundtrip against a mock OpenAI
// server. This is the only way to exercise the Generate code path without
// hitting a real provider.
func TestGenerate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request shape.
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req openai.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		if req.Model != "mock-model" {
			t.Errorf("model = %q, want mock-model", req.Model)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("messages = %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Model: "mock-model",
			Choices: []openai.ChatCompletionChoice{
				{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "hello back"}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		})
	}))
	defer srv.Close()

	m, err := New(Provider{
		Name:    "mock",
		BaseURL: srv.URL,
		APIKey:  "test",
		Model:   "mock-model",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := m.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Content != "hello back" {
		t.Errorf("content = %q", got.Content)
	}
	if got.ResponseMeta == nil || got.ResponseMeta.Usage == nil {
		t.Fatal("missing usage")
	}
	if got.ResponseMeta.Usage.TotalTokens != 7 {
		t.Errorf("total = %d", got.ResponseMeta.Usage.TotalTokens)
	}
}

func TestGenerate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth"}}`))
	}))
	defer srv.Close()

	m, _ := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	_, err := m.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var le *LLMError
	if !errorsAs(err, &le) {
		t.Fatalf("not *LLMError: %T", err)
	}
	if le.StatusCode != 401 {
		t.Errorf("status = %d, want 401", le.StatusCode)
	}
}

func errorsAs(err error, target any) bool {
	// Avoid pulling errors into the test file's import block; just re-export.
	return stdErrorsAs(err, target)
}

// TestStream_ToolCallChunk verifies the Stream path correctly forwards
// tool-call deltas in a streaming response. This is the path the
// agent loop hits when the LLM decides to invoke a tool.
func TestStream_ToolCallChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// Single chunk carrying the full tool call (ID, name, type, and
		// arguments all in one delta). Our adapter does not currently
		// merge multi-chunk deltas by index; the agent loop is responsible
		// for that, so the wire format here mirrors what a real provider
		// sends in the simpler "first chunk carries everything" shape.
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]}}]}` + "\n\n"))
		flusher.Flush()

		// Final chunk: finish reason
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := m.Stream(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "call echo"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	// Drain: we expect at least one chunk with tool_calls, then EOF.
	var gotToolCall bool
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk == nil {
			continue
		}
		if len(chunk.ToolCalls) > 0 {
			gotToolCall = true
			if chunk.ToolCalls[0].Function.Name != "echo" {
				t.Errorf("tool name = %q, want echo (chunk=%+v)", chunk.ToolCalls[0].Function.Name, chunk)
			}
		}
	}
	if !gotToolCall {
		t.Error("never observed a tool call in the stream")
	}
}

// TestStream_ReasoningContent verifies reasoning deltas reach the caller.
// Reasoning models (e.g. deepseek-reasoner) stream their thinking in a separate
// field; dropping it makes the model's reasoning invisible to any UI.
func TestStream_ReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先分析"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"答案是 42"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "q"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var reasoning, content []string
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk == nil {
			continue
		}
		if chunk.ReasoningContent != "" {
			reasoning = append(reasoning, chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			content = append(content, chunk.Content)
		}
	}
	if len(reasoning) == 0 {
		t.Fatal("reasoning_content was dropped; the UI cannot show the thinking process")
	}
	if strings.Join(reasoning, "") != "先分析" {
		t.Errorf("reasoning = %q, want 先分析", strings.Join(reasoning, ""))
	}
	if strings.Join(content, "") != "答案是 42" {
		t.Errorf("content = %q", strings.Join(content, ""))
	}
}

// TestStream_UsageIsRequestedAndCaptured verifies that streaming asks for usage
// and forwards it. Without IncludeUsage the provider never reports tokens, so a
// streamed conversation would appear to cost nothing.
func TestStream_UsageIsRequestedAndCaptured(t *testing.T) {
	var sawIncludeUsage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openai.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err == nil {
			if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
				sawIncludeUsage = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
		flusher.Flush()
		// The usage-only chunk carries no choices, which is exactly how
		// OpenAI-compatible providers send it.
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "q"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var usage *schema.TokenUsage
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk != nil && chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			usage = chunk.ResponseMeta.Usage
		}
	}

	if !sawIncludeUsage {
		t.Error("stream_options.include_usage was not requested; the provider will never report tokens")
	}
	if usage == nil {
		t.Fatal("usage was dropped from the usage-only chunk")
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 4 || usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want 11/4/15", usage)
	}
}

// TestModelName verifies the model reports its name, which tracing uses to
// attribute a call.
func TestModelName(t *testing.T) {
	m, err := New(Provider{Name: "deepseek", BaseURL: "https://example.invalid/v1", APIKey: "x", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	named, ok := m.(interface{ Name() string })
	if !ok {
		t.Fatal("the model does not expose Name(); traces would record no model")
	}
	if got := named.Name(); got != "deepseek-chat" {
		t.Errorf("Name() = %q, want deepseek-chat", got)
	}
	if p, ok := m.(interface{ Provider() string }); ok {
		if got := p.Provider(); got != "deepseek" {
			t.Errorf("Provider() = %q, want deepseek", got)
		}
	}
}

// TestWithTools_KeepsName verifies a tool-bound copy still identifies its model,
// so per-request tracing keeps attributing calls correctly.
func TestWithTools_KeepsName(t *testing.T) {
	m, err := New(Provider{Name: "p", BaseURL: "https://example.invalid/v1", APIKey: "x", Model: "m1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tcm, ok := m.(model.ToolCallingChatModel)
	if !ok {
		t.Fatal("the model does not implement ToolCallingChatModel")
	}
	bound, err := tcm.WithTools([]*schema.ToolInfo{{Name: "t", Desc: "d"}})
	if err != nil {
		t.Fatalf("WithTools: %v", err)
	}
	named, ok := bound.(interface{ Name() string })
	if !ok {
		t.Fatal("the tool-bound copy lost Name()")
	}
	if got := named.Name(); got != "m1" {
		t.Errorf("Name() after WithTools = %q, want m1", got)
	}
}

// TestStream_UsageOnlyChunkWithoutChoices verifies that a usage-only chunk is
// forwarded even though it carries no choices. Providers send usage exactly
// this way, so skipping choice-less chunks would silently drop all token
// accounting for streamed calls.
func TestStream_UsageOnlyChunkWithoutChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "q"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	chunks := 0
	var total int
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk == nil {
			continue
		}
		chunks++
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			total = chunk.ResponseMeta.Usage.TotalTokens
		}
	}
	if total != 3 {
		t.Errorf("total tokens = %d, want 3 (the usage-only chunk must be forwarded)", total)
	}
	if chunks != 2 {
		t.Errorf("forwarded %d chunks, want 2 (content + usage)", chunks)
	}
}

// TestStream_UsageRequestCanBeDisabled verifies the opt-out: a provider that
// rejects the stream_options extension would otherwise fail every streamed call.
func TestStream_UsageRequestCanBeDisabled(t *testing.T) {
	var sawStreamOptions bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "stream_options") {
			sawStreamOptions = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{
		Name: "strict", BaseURL: srv.URL, APIKey: "x", Model: "m",
		DisableUsageRequest: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "q"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
	}
	if sawStreamOptions {
		t.Error("stream_options was sent despite the provider disabling it")
	}
}

// TestRequest_ToolCallProtocolIsWellFormed drives a full tool round trip and
// validates the request against the contract providers actually enforce:
//
//   - every `role: "tool"` message carries a non-empty tool_call_id
//   - every tool_call_id answers an id declared by a preceding assistant
//     tool_calls entry
//   - an assistant message with tool_calls keeps them on the wire
//
// A lenient mock that merely reads `role` accepts a broken request, which is
// exactly how "missing field `tool_call_id`" reached a real provider.
func TestRequest_ToolCallProtocolIsWellFormed(t *testing.T) {
	var second []map[string]any

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request is not valid JSON: %v", err)
		}
		calls++
		if calls == 1 {
			// First turn: ask for a tool.
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]}}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		second = req.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"done"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "mock", BaseURL: srv.URL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bound, err := m.(model.ToolCallingChatModel).WithTools([]*schema.ToolInfo{{
		Name: "echo", Desc: "echoes",
		ParamsOneOf: mustParams(t, `{"type":"object","properties":{"text":{"type":"string"}}}`),
	}})
	if err != nil {
		t.Fatalf("WithTools: %v", err)
	}

	// Turn 1: the model asks for a tool.
	stream, err := bound.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "echo hi"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var assistant *schema.Message
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("Recv: %v", rerr)
		}
		if chunk == nil {
			continue
		}
		if assistant == nil {
			cp := *chunk
			assistant = &cp
		} else {
			merged, cerr := schema.ConcatMessages([]*schema.Message{assistant, chunk})
			if cerr != nil {
				t.Fatalf("concat: %v", cerr)
			}
			assistant = merged
		}
	}
	stream.Close()
	if assistant == nil || len(assistant.ToolCalls) == 0 {
		t.Fatal("no tool call came back")
	}

	// Turn 2: send the tool result back, which is where a dropped id shows up.
	history := []*schema.Message{
		{Role: schema.User, Content: "echo hi"},
		assistant,
		{Role: schema.Tool, Content: `{"text":"hi"}`, ToolCallID: assistant.ToolCalls[0].ID},
	}
	stream2, err := bound.Stream(context.Background(), history)
	if err != nil {
		t.Fatalf("Stream 2: %v", err)
	}
	for {
		_, rerr := stream2.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("Recv 2: %v", rerr)
		}
	}
	stream2.Close()

	if second == nil {
		t.Fatal("the second request never reached the server")
	}

	// Validate the contract.
	declared := map[string]bool{}
	sawAssistantCalls := false
	sawTool := false
	for i, msg := range second {
		role, _ := msg["role"].(string)
		if callsRaw, ok := msg["tool_calls"]; ok && role == string(schema.Assistant) {
			arr, _ := callsRaw.([]any)
			if len(arr) > 0 {
				sawAssistantCalls = true
			}
			for _, c := range arr {
				cm, _ := c.(map[string]any)
				if id, _ := cm["id"].(string); id != "" {
					declared[id] = true
				}
				fn, _ := cm["function"].(map[string]any)
				if fn == nil {
					t.Errorf("messages[%d]: tool_call has no function", i)
					continue
				}
				if name, _ := fn["name"].(string); name == "" {
					t.Errorf("messages[%d]: tool_call function has no name", i)
				}
			}
		}
		if role == string(schema.Tool) {
			sawTool = true
			id, _ := msg["tool_call_id"].(string)
			if id == "" {
				t.Errorf("messages[%d]: a tool message has no tool_call_id — this is the "+
					"exact request a provider rejects with \"missing field `tool_call_id`\"", i)
				continue
			}
			if !declared[id] {
				t.Errorf("messages[%d]: tool_call_id %q answers no assistant tool call", i, id)
			}
		}
	}
	if !sawAssistantCalls {
		t.Error("the assistant message lost its tool_calls on the wire")
	}
	if !sawTool {
		t.Error("no tool message was sent")
	}
}

// mustParams builds a ParamsOneOf from a JSON schema literal.
func mustParams(t *testing.T, raw string) *schema.ParamsOneOf {
	t.Helper()
	var js jsonschema.Schema
	if err := json.Unmarshal([]byte(raw), &js); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return schema.NewParamsOneOfByJSONSchema(&js)
}
