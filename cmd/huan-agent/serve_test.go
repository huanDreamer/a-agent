package main

import (
	"testing"

	"github.com/cloudwego/eino/schema"
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
