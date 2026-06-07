package llm

import (
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
)

func TestNew_Validations(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
	}{
		{"empty name", Provider{BaseURL: "x", Model: "y"}},
		{"empty base url", Provider{Name: "x", Model: "y"}},
		{"empty model", Provider{Name: "x", BaseURL: "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.p); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNew_Success(t *testing.T) {
	m, err := New(Provider{
		Name:    "test",
		BaseURL: "https://example.com/v1",
		APIKey:  "k",
		Model:   "test-model",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("nil model")
	}
	// Sanity: it implements BaseChatModel (compile-time check).
	var _ model.BaseChatModel = m
}

func TestRegistry_Defaults(t *testing.T) {
	r := NewRegistry(map[string]Provider{
		"deepseek": {APIKey: "k"}, // no BaseURL/Model; should inherit defaults
		"qwen":     {APIKey: "k", Model: "qwen-turbo"},
	}, "deepseek")

	got := r.Names()
	if len(got) != 2 {
		t.Errorf("Names = %v, want 2 entries", got)
	}

	m, err := r.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if m == nil {
		t.Fatal("nil default model")
	}

	// Inherited defaults
	if p, ok := r.Provider("deepseek"); !ok {
		t.Error("deepseek missing")
	} else if p.BaseURL == "" || p.Model == "" {
		t.Errorf("deepseek missing defaults: %+v", p)
	}
	// User override preserved
	if p, _ := r.Provider("qwen"); p.Model != "qwen-turbo" {
		t.Errorf("qwen model = %q, want qwen-turbo", p.Model)
	}
}

func TestRegistry_UnknownProvider(t *testing.T) {
	r := NewRegistry(map[string]Provider{
		"deepseek": {APIKey: "k"},
	}, "")
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestRegistry_NoDefault(t *testing.T) {
	r := NewRegistry(map[string]Provider{}, "")
	_, err := r.Default()
	if err == nil {
		t.Fatal("expected error when no default configured")
	}
}

func TestToOpenAIMessages(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "x"}}
	got := m.toOpenAIMessages(nil)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}

	got = m.toOpenAIMessages([]*schema.Message{
		{Role: schema.System, Content: "you are helpful"},
		{Role: schema.User, Content: "hello", Name: "alice"},
		{Role: schema.Assistant, Content: "hi"},
	})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Role != "system" || got[0].Content != "you are helpful" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Name != "alice" {
		t.Errorf("got[1].Name = %q, want alice", got[1].Name)
	}
}

func TestToTokenUsage(t *testing.T) {
	if u := toTokenUsage(openai.Usage{}); u != nil {
		t.Errorf("empty usage should return nil, got %+v", u)
	}
	u := toTokenUsage(openai.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30})
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 20 || u.TotalTokens != 30 {
		t.Errorf("usage = %+v", u)
	}
}

func TestLLMError_Error(t *testing.T) {
	e := &LLMError{Provider: "p", Model: "m", StatusCode: 401, Body: "auth failed", Err: errors.New("boom")}
	got := e.Error()
	if got == "" {
		t.Error("empty error string")
	}
	if !errors.Is(e, errors.Unwrap(e)) {
		t.Error("Unwrap broken")
	}

	// No body branch
	e2 := &LLMError{Provider: "p", Model: "m", Err: errors.New("x")}
	if e2.Error() == "" {
		t.Error("empty body-less error string")
	}
}

func TestWrapErr(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "p", Model: "m"}}
	e := m.wrapErr(errors.New("x"), 500, "internal")
	var le *LLMError
	if !errors.As(e, &le) {
		t.Fatalf("not *LLMError: %T", e)
	}
	if le.Provider != "p" || le.Model != "m" || le.StatusCode != 500 || le.Body != "internal" {
		t.Errorf("le = %+v", le)
	}
}

func TestNewMessagePipe(t *testing.T) {
	sr, sw := newMessagePipe(8)
	if sr == nil || sw == nil {
		t.Fatal("nil pipe parts")
	}
	// Send a chunk and close.
	if closed := sw.Send(&schema.Message{Role: schema.Assistant, Content: "hi"}, nil); closed {
		t.Error("Send returned closed=true on a live writer")
	}
	sw.Close()

	// Recv should yield the chunk, then EOF.
	got, err := sr.Recv()
	if err != nil {
		t.Fatalf("Recv chunk: %v", err)
	}
	if got == nil || got.Content != "hi" {
		t.Errorf("got = %+v", got)
	}
	if _, err := sr.Recv(); err == nil {
		t.Error("expected EOF after close")
	}
}

func TestApplyOptions(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "p", Model: "m"}}
	req := openai.ChatCompletionRequest{}
	m.applyOptions(&req, []model.Option{
		model.WithTemperature(0.3),
		model.WithMaxTokens(256),
	})
	if req.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3", req.Temperature)
	}
	if req.MaxTokens != 256 {
		t.Errorf("max_tokens = %v, want 256", req.MaxTokens)
	}
}
