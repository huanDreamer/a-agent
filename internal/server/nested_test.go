package server

import (
	"testing"

	"github.com/huan/huan-agent/internal/chat"
)

// What a subagent's events do to the turn it was spawned from.
//
// The rule under test is the one that makes a subagent worth having: its working
// out stays out of the parent's context. A console that appended a subagent's text
// to the answer would be showing the reader something the model never saw, and —
// worse — the stored turn would not be what the model produced, so the next turn
// reading it back would be reasoning about text the model had never written.

func TestNestedTextDoesNotBecomeTheAnswer(t *testing.T) {
	acc := &turnAccumulator{}

	// The turn: one step, a call to spawn_agent.
	acc.add(chat.Event{Type: chat.EventStepStart, Step: 1})
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "我来派一个子 agent。"})
	acc.add(chat.Event{Type: chat.EventToolCall, Step: 1, ToolCallID: "call-1", ToolName: "spawn_agent", ToolArgs: "{}"})

	// The subagent: thinking, talking, and calling tools of its own.
	acc.add(chat.Event{Type: chat.EventReasoningDelta, Step: 1, Text: "先看看目录结构。", ParentToolCallID: "call-1"})
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "它自己的中间结论。", ParentToolCallID: "call-1"})
	acc.add(chat.Event{Type: chat.EventToolCall, Step: 1, ToolCallID: "call-2", ToolName: "grep", ParentToolCallID: "call-1"})
	acc.add(chat.Event{Type: chat.EventToolResult, Step: 1, ToolCallID: "call-2", ToolResult: "24 matches", ParentToolCallID: "call-1"})

	// The subagent's report comes back as the tool result of the parent call.
	acc.add(chat.Event{Type: chat.EventToolResult, Step: 1, ToolCallID: "call-1", ToolResult: "子 agent 的结论。"})
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 2, Text: "它说：子 agent 的结论。"})

	summary := acc.summary(nil)

	// The answer is the turn's own words, and only those.
	want := "我来派一个子 agent。它说：子 agent 的结论。"
	if summary.answer != want {
		t.Errorf("answer = %q, want %q", summary.answer, want)
	}
	if contains([]string{summary.answer}, "中间结论") {
		t.Error("the subagent's intermediate text leaked into the answer")
	}

	// The turn has exactly one tool run — the call that spawned the subagent. The
	// subagent's own calls are not runs of this turn; they are entries under that
	// card.
	if len(summary.tools) != 1 {
		t.Fatalf("tools = %+v, want only the spawning call", summary.tools)
	}
	parent := summary.tools[0]
	if parent.ID != "call-1" || parent.Name != "spawn_agent" {
		t.Fatalf("first tool = %+v", parent)
	}
	// Three entries: a reasoning run, a text run, and one tool call that has its
	// result attached (a call and its result are one thing a reader sees, not two).
	if len(parent.Nested) != 3 {
		t.Errorf("nested entries = %d, want 3 (reasoning, text, tool): %+v",
			len(parent.Nested), parent.Nested)
	}
	// The nested tool call is not a step of the turn.
	for _, run := range summary.tools {
		if run.ID == "call-2" {
			t.Error("a nested tool call was recorded as one of the turn's own tool runs")
		}
	}
	// And the nested entries carry what happened.
	var sawTool, sawResult bool
	for _, n := range parent.Nested {
		if n.Kind == "tool" && n.Name == "grep" {
			sawTool = true
		}
		if n.ID == "call-2" && n.Result == "24 matches" {
			sawResult = true
		}
	}
	if !sawTool {
		t.Errorf("the nested tool call lost its name: %+v", parent.Nested)
	}
	if !sawResult {
		t.Errorf("the nested tool result was not attached to its call: %+v", parent.Nested)
	}
}

// TestNestedDeltasOfOneKindCoalesce: a subagent's paragraph is one block, not one
// entry per token.
func TestNestedDeltasCoalesce(t *testing.T) {
	acc := &turnAccumulator{}
	acc.add(chat.Event{Type: chat.EventToolCall, Step: 1, ToolCallID: "call-1", ToolName: "spawn_agent"})
	for _, part := range []string{"结论", "是", "这样的", "。"} {
		acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: part, ParentToolCallID: "call-1"})
	}
	acc.add(chat.Event{Type: chat.EventReasoningDelta, Step: 1, Text: "想了想", ParentToolCallID: "call-1"})
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "补充一句", ParentToolCallID: "call-1"})

	summary := acc.summary(nil)
	nested := summary.tools[0].Nested
	if len(nested) != 3 {
		t.Fatalf("nested = %+v, want text, reasoning, text", nested)
	}
	if nested[0].Text != "结论是这样的。" {
		t.Errorf("the text run was not coalesced: %q", nested[0].Text)
	}
	if nested[1].Kind != "reasoning" || nested[2].Text != "补充一句" {
		t.Errorf("the kind boundary was not respected: %+v", nested)
	}
}

// TestNestedWithoutAParentIsDropped: an event whose card is gone is dropped rather
// than invented into the wrong place.
func TestNestedWithoutAParentIsDropped(t *testing.T) {
	acc := &turnAccumulator{}
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "orphan", ParentToolCallID: "call-gone"})
	acc.add(chat.Event{Type: chat.EventToolResult, Step: 1, ToolCallID: "call-gone", ToolResult: "x", ParentToolCallID: "call-gone"})

	summary := acc.summary(nil)
	if summary.answer != "" {
		t.Errorf("an orphan nested event became the answer: %q", summary.answer)
	}
	if len(summary.tools) != 0 {
		t.Errorf("an orphan nested event invented a tool run: %+v", summary.tools)
	}
}

// TestTopLevelEventsAreUnaffected is the regression gate: every existing
// conversation behaves exactly as it did.
func TestTopLevelEventsAreUnaffected(t *testing.T) {
	acc := &turnAccumulator{}
	acc.add(chat.Event{Type: chat.EventStepStart, Step: 1})
	acc.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "答案"})
	acc.add(chat.Event{Type: chat.EventReasoningDelta, Step: 1, Text: "思考"})
	acc.add(chat.Event{Type: chat.EventToolCall, Step: 1, ToolCallID: "c1", ToolName: "grep"})
	acc.add(chat.Event{Type: chat.EventToolResult, Step: 1, ToolCallID: "c1", ToolResult: "ok"})

	summary := acc.summary(nil)
	if summary.answer != "答案" {
		t.Errorf("answer = %q", summary.answer)
	}
	if len(summary.tools) != 1 || summary.tools[0].Result != "ok" {
		t.Errorf("tools = %+v", summary.tools)
	}
	if len(summary.tools[0].Nested) != 0 {
		t.Errorf("a top-level call gained nested entries: %+v", summary.tools[0].Nested)
	}
}
