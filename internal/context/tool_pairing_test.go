package context

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// asstToolCall builds the assistant message the runner appends after a step
// that decided to act: it carries tool_calls and, typically, no text.
func asstToolCall(ids ...string) *schema.Message {
	m := &schema.Message{Role: schema.Assistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, schema.ToolCall{
			ID:       id,
			Function: schema.FunctionCall{Name: "some_tool", Arguments: "{}"},
		})
	}
	return m
}

// toolObservation builds the tool-role message the runner appends per call.
func toolObservation(id string) *schema.Message {
	return &schema.Message{
		Role:       schema.Tool,
		Content:    strings.Repeat("observation ", 20),
		ToolCallID: id,
		ToolName:   "some_tool",
	}
}

// firstOrphanTool returns the index of the first tool message that is not
// preceded by an assistant message whose tool_calls include its id, or -1.
//
// This is the invariant providers enforce. DeepSeek rejects a violation with
// 400 "Messages with role 'tool' must be a response to a preceding message with
// 'tool_calls'", which fails the whole turn.
func firstOrphanTool(msgs []*schema.Message) int {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case schema.Assistant:
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case schema.Tool:
			if !pending[m.ToolCallID] {
				return i
			}
			delete(pending, m.ToolCallID)
		}
	}
	return -1
}

// assertToolPairing fails the test when the window contains an orphan tool
// result, printing the window so the offending alignment is visible.
func assertToolPairing(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	bad := firstOrphanTool(msgs)
	if bad < 0 {
		return
	}
	var b strings.Builder
	for i, m := range msgs {
		mark := " "
		if i == bad {
			mark = ">"
		}
		b.WriteString("\n  " + mark + " [" + itoa(i) + "] role=" + string(m.Role) +
			" tool_calls=" + itoa(len(m.ToolCalls)) + " tool_call_id=" + m.ToolCallID)
	}
	t.Errorf("window opens with an orphan tool result at index %d (tool_call_id=%q); "+
		"the assistant that requested it was summarized away:%s", bad, msgs[bad].ToolCallID, b.String())
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestCompressKeepsToolExchangePaired is the regression guard for the 400.
//
// CompressWith's kept tail is a plain slice boundary, and it can land inside a
// tool exchange: the assistant message carrying tool_calls falls into the
// foldable middle and is rolled into the summary, while the tool results it
// asked for stay in the kept tail. The window then opens with an orphan
// `role: "tool"` message and the provider rejects the request.
//
// Each case below is an alignment that used to produce exactly that.
func TestCompressKeepsToolExchangePaired(t *testing.T) {
	cases := []struct {
		name       string
		keepRecent int
		history    []*schema.Message
	}{
		{
			// One call per step: the split lands between the assistant and
			// its single result.
			name:       "sequential calls",
			keepRecent: 3,
			history: []*schema.Message{
				{Role: schema.System, Content: "you are an agent"},
				{Role: schema.User, Content: "do the thing"},
				asstToolCall("call_1"),
				toolObservation("call_1"),
				asstToolCall("call_2"),
				toolObservation("call_2"),
			},
		},
		{
			// Parallel calls: one assistant message, several results, so the
			// split can fall between two results of the same step.
			name:       "parallel calls",
			keepRecent: 2,
			history: []*schema.Message{
				{Role: schema.System, Content: "you are an agent"},
				{Role: schema.User, Content: "do the thing"},
				asstToolCall("a", "b", "c"),
				toolObservation("a"),
				toolObservation("b"),
				toolObservation("c"),
			},
		},
		{
			// The configured keep_recent (configs/config.yaml), with a step
			// that made two parallel calls astride the boundary.
			name:       "configured keep_recent",
			keepRecent: 12,
			history: append([]*schema.Message{
				{Role: schema.System, Content: "you are an agent"},
				{Role: schema.User, Content: "do the thing"},
				asstToolCall("x1", "x2"),
				toolObservation("x1"),
				toolObservation("x2"),
			}, func() []*schema.Message {
				var tail []*schema.Message
				for _, id := range []string{"a", "b", "c", "d", "e"} {
					tail = append(tail, asstToolCall(id), toolObservation(id))
				}
				return tail
			}()...),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(
				Budget{MaxTokens: 100, KeepRecent: tc.keepRecent, Summarizer: &fakeSummarizer{}},
				nil, chainEstimator)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}

			// head=1 is the leading system prompt, as the runner computes it.
			out, _, err := m.CompressKeeping(context.Background(), tc.history, 1, true)
			if err != nil {
				t.Fatalf("CompressKeeping: %v", err)
			}
			assertToolPairing(t, out)
		})
	}
}

// TestCompressFoldsWholeExchangeWhenBoundaryIsTight pins the trade-off the fix
// makes: when pulling the requesting assistant back would empty the middle,
// there is nothing left to summarize, so the window is returned whole rather
// than sent with a dangling tool result. An over-budget window is recoverable;
// a rejected request is not.
func TestCompressFoldsWholeExchangeWhenBoundaryIsTight(t *testing.T) {
	m, err := NewManager(
		Budget{MaxTokens: 100, KeepRecent: 2, Summarizer: &fakeSummarizer{}},
		nil, chainEstimator)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	history := []*schema.Message{
		{Role: schema.System, Content: "you are an agent"},
		{Role: schema.User, Content: "do the thing"},
		asstToolCall("call_1"),
		toolObservation("call_1"),
	}

	out, _, err := m.CompressKeeping(context.Background(), history, 1, true)
	if err != nil {
		t.Fatalf("CompressKeeping: %v", err)
	}
	assertToolPairing(t, out)
	if len(out) != len(history) {
		t.Errorf("want the window returned whole (%d messages), got %d", len(history), len(out))
	}
}
