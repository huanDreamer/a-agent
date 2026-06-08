package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"

	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// toolCallResponse is a minimal OpenAI response shape carrying a
// single tool_call. The agent loop will pick this up, dispatch to
// the registered tool, then loop back to the model.
func toolCallResponse(callID, name, args string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Model: "m",
		Choices: []openai.ChatCompletionChoice{
			{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{
						{ID: callID, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: name, Arguments: args}},
					},
				},
				FinishReason: "tool_calls",
			},
		},
		Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}
}

func textResponse(text string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Model: "m",
		Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Role: "assistant", Content: text}, FinishReason: "stop"},
		},
		Usage: openai.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
}

// newMockChatServer returns an httptest server that emits the given
// responses in order, one per POST. The server also records the
// number of requests it has served.
func newMockChatServer(t *testing.T, responses ...openai.ChatCompletionResponse) (*httptest.Server, *int) {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		idx := calls - 1
		if idx >= len(responses) {
			t.Errorf("unexpected extra request #%d", calls)
			_ = json.NewEncoder(w).Encode(textResponse(""))
			return
		}
		_ = json.NewEncoder(w).Encode(responses[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestModel(t *testing.T, baseURL string) *llm.OpenAIModelForTest {
	t.Helper()
	m, err := llm.New(llm.Provider{Name: "mock", BaseURL: baseURL, APIKey: "x", Model: "m"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return llm.AsOpenAIModelForTest(m)
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAgent_ReActLoop(t *testing.T) {
	srv, calls := newMockChatServer(t,
		toolCallResponse("call_1", "echo", `{"text":"hi"}`),
		textResponse("echoed: hi"),
	)

	m := newTestModel(t, srv.URL)
	reg := tool.NewRegistry()
	echo, err := builtin.NewEchoTool()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	a, err := New(context.Background(), Config{
		Model: m,
		Tools: reg,
		Audit: st,
		SessionIDFn: func() string { return "s1" },
		MaxSteps:    5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := a.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "echo hi"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Content != "echoed: hi" {
		t.Errorf("content = %q, want %q", out.Content, "echoed: hi")
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2 (one tool call, one final)", *calls)
	}

	// Audit row should exist.
	rows, err := st.QueryInvocations(context.Background(), store.InvocationFilter{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("invocations = %d, want 1", len(rows))
	}
	if rows[0].ToolName != "echo" {
		t.Errorf("tool = %q, want echo", rows[0].ToolName)
	}
	if !strings.Contains(rows[0].Result, "hi") {
		t.Errorf("result missing payload: %q", rows[0].Result)
	}
}

func TestAgent_NoTools_FallsBackToChat(t *testing.T) {
	srv, _ := newMockChatServer(t, textResponse("just a chat reply"))

	m := newTestModel(t, srv.URL)
	reg := tool.NewRegistry() // empty

	a, err := New(context.Background(), Config{Model: m, Tools: reg})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Content != "just a chat reply" {
		t.Errorf("content = %q", out.Content)
	}
}

func TestAgent_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hello "}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m := newTestModel(t, srv.URL)
	reg := tool.NewRegistry()
	a, err := New(context.Background(), Config{Model: m, Tools: reg})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := a.Stream(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk != nil {
			sb.WriteString(chunk.Content)
		}
	}
	if !strings.Contains(sb.String(), "hello world") {
		t.Errorf("stream content = %q", sb.String())
	}
}

func TestAgent_NilModelErrors(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("expected error for nil model")
	}
}

func TestAgent_Defaults(t *testing.T) {
	srv, _ := newMockChatServer(t, textResponse("ok"))
	m := newTestModel(t, srv.URL)
	// MaxSteps = 0 → default 12; pass nil logger to hit the default branch.
	a, err := New(context.Background(), Config{Model: m, Tools: tool.NewRegistry(), MaxSteps: 100})
	if err != nil {
		t.Fatal(err)
	}
	if a.maxStep != 25 {
		t.Errorf("maxStep = %d, want 25 (capped)", a.maxStep)
	}
}
