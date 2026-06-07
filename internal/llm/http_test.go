package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
