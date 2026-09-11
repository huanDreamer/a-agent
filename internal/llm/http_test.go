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
