package main

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/prompt"
)

func TestSanitizeBotHistory_DropsToolMessages(t *testing.T) {
	history := []*schema.Message{
		{Role: schema.User, Content: "hi"},
		{Role: schema.Tool, Content: "some tool output", ToolCallID: "call_1"},
		{Role: schema.Assistant, Content: "oops", ToolCalls: []schema.ToolCall{{ID: "call_1", Type: "function"}}},
	}
	got := sanitizeBotHistory(history)
	// tool message dropped; assistant kept but stripped of ToolCalls.
	for _, m := range got {
		if m.Role == schema.Tool {
			t.Fatal("tool message should have been dropped")
		}
		if len(m.ToolCalls) != 0 {
			t.Errorf("assistant message still carries ToolCalls: %+v", m.ToolCalls)
		}
		if m.ToolCallID != "" {
			t.Errorf("message still carries ToolCallID: %q", m.ToolCallID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
}

func TestSanitizeBotHistory_DropsEmptyToolCallAssistant(t *testing.T) {
	history := []*schema.Message{
		{Role: schema.Assistant, Content: "", ToolCalls: []schema.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: schema.User, Content: "next"},
	}
	got := sanitizeBotHistory(history)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1 (empty tool-call assistant dropped)", len(got))
	}
	if got[0].Role != schema.User {
		t.Errorf("surviving message role = %v, want user", got[0].Role)
	}
}

func TestSanitizeBotHistory_PassthroughPlain(t *testing.T) {
	history := []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.User, Content: "q"},
		{Role: schema.Assistant, Content: "a"},
	}
	got := sanitizeBotHistory(history)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[1].Content != "q" || got[2].Content != "a" {
		t.Errorf("plain messages not preserved: %+v", got)
	}
}

func TestSanitizeBotHistory_PreservesContentWhenToolCallsPresent(t *testing.T) {
	// assistant with both content and ToolCalls keeps content, only ToolCalls stripped.
	history := []*schema.Message{
		{Role: schema.Assistant, Content: "thought plus answer", ToolCalls: []schema.ToolCall{{ID: "c1", Type: "function"}}},
	}
	got := sanitizeBotHistory(history)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].Content != "thought plus answer" {
		t.Errorf("content = %q, want preserved", got[0].Content)
	}
	if len(got[0].ToolCalls) != 0 {
		t.Errorf("ToolCalls should be stripped")
	}
}

func TestSanitizeBotHistory_DoesNotMutateInput(t *testing.T) {
	assistant := &schema.Message{Role: schema.Assistant, Content: "x", ToolCalls: []schema.ToolCall{{ID: "c1", Type: "function"}}}
	input := []*schema.Message{assistant}
	_ = sanitizeBotHistory(input)
	if len(assistant.ToolCalls) != 1 {
		t.Errorf("input message was mutated (ToolCalls = %d, want 1)", len(assistant.ToolCalls))
	}
}

func TestBotSystemPromptFollowsTheOperator(t *testing.T) {
	// The default is the general-purpose prompt plus the Feishu section, and a
	// configured chat.system_prompt wins over both — the same rule the console
	// applies, so a deployment that wrote its own prompt gets it in a chat
	// instead of the built-in text about cards and tables.
	h := &botHandler{cfg: &config.Config{}}
	if got := h.systemPrompt(); got != prompt.For(prompt.SurfaceFeishu) {
		t.Errorf("systemPrompt = %q, want the Feishu default", got)
	}

	h.cfg.Chat.SystemPrompt = "  你只回答天气，别的都说不清楚。  "
	if got, want := h.systemPrompt(), "你只回答天气，别的都说不清楚。"; got != want {
		t.Errorf("systemPrompt = %q, want %q", got, want)
	}

	// A whitespace-only value is not a prompt: it is an unset field, and the
	// model must not be left with no instructions at all.
	h.cfg.Chat.SystemPrompt = "   "
	if got := h.systemPrompt(); got != prompt.For(prompt.SurfaceFeishu) {
		t.Errorf("systemPrompt = %q, want the Feishu default", got)
	}

	// cfg nil is a supported shape for a handler built in a test or a partial
	// deployment; it must not panic on the way to the default.
	if got := (&botHandler{}).systemPrompt(); got != prompt.For(prompt.SurfaceFeishu) {
		t.Errorf("systemPrompt = %q, want the Feishu default", got)
	}
}
