package store

import (
	"context"
	"testing"
)

func TestRecordAndQueryInvocations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	events := []InvocationEvent{
		{SessionID: "s1", ToolName: "echo", Arguments: `{"text":"hi"}`, Result: `{"text":"hi"}`, DurationMs: 5},
		{SessionID: "s1", ToolName: "calc", Arguments: `{"expression":"1+1"}`, Result: `{"result":2}`, Err: "", DurationMs: 8},
		{SessionID: "s1", ToolName: "time", Arguments: `{}`, Err: "boom", DurationMs: 3},
		{SessionID: "s2", ToolName: "echo", Arguments: `{"text":"yo"}`, Result: `{"text":"yo"}`, DurationMs: 2},
	}
	for _, e := range events {
		if err := s.RecordInvocation(ctx, e); err != nil {
			t.Fatalf("RecordInvocation: %v", err)
		}
	}

	t.Run("all", func(t *testing.T) {
		out, err := s.QueryInvocations(ctx, InvocationFilter{})
		if err != nil {
			t.Fatalf("QueryInvocations: %v", err)
		}
		if len(out) != 4 {
			t.Errorf("len = %d, want 4", len(out))
		}
	})

	t.Run("filter session", func(t *testing.T) {
		out, _ := s.QueryInvocations(ctx, InvocationFilter{SessionID: "s1"})
		if len(out) != 3 {
			t.Errorf("len = %d, want 3", len(out))
		}
	})

	t.Run("filter tool", func(t *testing.T) {
		out, _ := s.QueryInvocations(ctx, InvocationFilter{ToolName: "echo"})
		if len(out) != 2 {
			t.Errorf("len = %d, want 2", len(out))
		}
	})

	t.Run("limit", func(t *testing.T) {
		out, _ := s.QueryInvocations(ctx, InvocationFilter{Limit: 2})
		if len(out) != 2 {
			t.Errorf("len = %d, want 2", len(out))
		}
	})

	t.Run("error row preserved", func(t *testing.T) {
		out, _ := s.QueryInvocations(ctx, InvocationFilter{ToolName: "time"})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0].Err != "boom" {
			t.Errorf("err = %q, want boom", out[0].Err)
		}
	})
}

func TestRecordInvocation_ValidationErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordInvocation(ctx, InvocationEvent{SessionID: "s1"}); err == nil {
		t.Error("expected error for missing tool_name")
	}
	if err := s.RecordInvocation(ctx, InvocationEvent{ToolName: "x"}); err == nil {
		t.Error("expected error for missing session_id")
	}
}
