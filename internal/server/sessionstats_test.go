package server

import (
	"context"
	"net/http"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

// statsBody is the wire shape of the `stats` object on GET /api/chat/sessions/{id}.
type statsBody struct {
	Messages         int `json:"messages"`
	Turns            int `json:"turns"`
	LlmCalls         int `json:"llm_calls"`
	LlmDurationMs    int `json:"llm_duration_ms"`
	ToolCalls        int `json:"tool_calls"`
	ToolDurationMs   int `json:"tool_duration_ms"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// seedStatsSession writes a conversation with a known shape: two turns, three
// model calls, two tool calls, and usage that sums to numbers a test can state.
func seedStatsSession(t *testing.T, st store.Store, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: sessionID, Title: "统计"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Turn 1: the model asks for a tool, gets the result, then answers.
	write := func(m store.ChatMessage) {
		t.Helper()
		if _, err := st.AppendChatMessage(ctx, sessionID, m); err != nil {
			t.Fatalf("append %s: %v", m.Role, err)
		}
	}
	write(store.ChatMessage{Role: store.RoleUser, Content: "第一个问题"})
	write(store.ChatMessage{
		Role:      store.RoleAssistant,
		ToolCalls: `[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]`,
		UsageJSON: `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"duration_ms":1000}`,
	})
	write(store.ChatMessage{Role: store.RoleTool, Content: "ok", ToolCallID: "c1", ToolName: "bash"})
	write(store.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   "答案一",
		UsageJSON: `{"prompt_tokens":200,"completion_tokens":30,"total_tokens":230,"duration_ms":2000}`,
	})
	// Turn 2: two tools in one step, then a final answer.
	write(store.ChatMessage{Role: store.RoleUser, Content: "第二个问题"})
	write(store.ChatMessage{
		Role: store.RoleAssistant,
		ToolCalls: `[{"id":"c2","type":"function","function":{"name":"bash","arguments":"{}"}},` +
			`{"id":"c3","type":"function","function":{"name":"read_file","arguments":"{}"}}]`,
		UsageJSON: `{"prompt_tokens":300,"completion_tokens":40,"total_tokens":340,"duration_ms":3000}`,
	})
	write(store.ChatMessage{Role: store.RoleTool, Content: "ok", ToolCallID: "c2", ToolName: "bash"})
	write(store.ChatMessage{Role: store.RoleTool, Content: "ok", ToolCallID: "c3", ToolName: "read_file"})
	write(store.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   "答案二",
		UsageJSON: `{"prompt_tokens":400,"completion_tokens":50,"total_tokens":450,"duration_ms":4000}`,
	})
	// The audit log carries the tool timing, which the messages do not.
	for _, e := range []store.InvocationEvent{
		{SessionID: sessionID, ToolName: "bash", DurationMs: 500},
		{SessionID: sessionID, ToolName: "bash", DurationMs: 700},
		{SessionID: sessionID, ToolName: "read_file", DurationMs: 300},
	} {
		if err := st.RecordInvocation(ctx, e); err != nil {
			t.Fatalf("record invocation: %v", err)
		}
	}
}

func TestSessionStats_AggregatesTheConversation(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)
	const sessionID = "stats-session"
	seedStatsSession(t, h.store, sessionID)

	var got struct {
		Stats statsBody `json:"stats"`
	}
	h.getJSON(t, "/api/chat/sessions/"+sessionID, http.StatusOK, &got)

	// The numbers the header shows, each one stated rather than derived from the
	// implementation: 9 stored messages, 2 turns, 4 model calls, 3 tool calls.
	if got.Stats.Messages != 9 {
		t.Errorf("messages = %d, want 9", got.Stats.Messages)
	}
	if got.Stats.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Stats.Turns)
	}
	if got.Stats.LlmCalls != 4 {
		t.Errorf("llm_calls = %d, want 4 (one per assistant message)", got.Stats.LlmCalls)
	}
	if got.Stats.LlmDurationMs != 10000 {
		t.Errorf("llm_duration_ms = %d, want 1000+2000+3000+4000", got.Stats.LlmDurationMs)
	}
	if got.Stats.ToolCalls != 3 {
		t.Errorf("tool_calls = %d, want 3", got.Stats.ToolCalls)
	}
	if got.Stats.ToolDurationMs != 1500 {
		t.Errorf("tool_duration_ms = %d, want 500+700+300 from the audit log", got.Stats.ToolDurationMs)
	}
	if got.Stats.PromptTokens != 1000 || got.Stats.CompletionTokens != 140 || got.Stats.TotalTokens != 1140 {
		t.Errorf("tokens = %d/%d/%d, want 1000/140/1140",
			got.Stats.PromptTokens, got.Stats.CompletionTokens, got.Stats.TotalTokens)
	}
}

func TestSessionStats_EmptyConversationIsZero(t *testing.T) {
	h := newChatHarness(t, nil, nil)
	h.login(t)
	id := createSession(t, h)

	var got struct {
		Stats statsBody `json:"stats"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	// Every field zero, and no division anywhere downstream that would make an
	// empty conversation render as an error instead of as "0".
	if got.Stats != (statsBody{}) {
		t.Errorf("stats = %+v, want the zero value for a new conversation", got.Stats)
	}
}

func TestSessionStats_ToleratesUnreadableRows(t *testing.T) {
	// A tool_calls blob that does not parse and a usage blob that does not parse:
	// the message is still a model call, and neither broken field may turn a
	// readable conversation into a failure or invent a number.
	srv := &Server{logger: zap.NewNop()}
	msgs := []store.ChatMessage{
		{Role: store.RoleAssistant, ToolCalls: "not json", UsageJSON: "not json"},
		{Role: store.RoleAssistant, ToolCalls: "", UsageJSON: ""},
	}
	// sessionStatsFor needs a store for the tool timing; the counts do not.
	srv.store = emptyStatsStore{}
	got := srv.sessionStatsFor(context.Background(), "s", msgs)
	if got.LlmCalls != 2 {
		t.Errorf("llm_calls = %d, want 2", got.LlmCalls)
	}
	if got.ToolCalls != 0 {
		t.Errorf("tool_calls = %d, want 0 for an unreadable blob", got.ToolCalls)
	}
	if got.TotalTokens != 0 || got.LlmDurationMs != 0 {
		t.Errorf("usage = %d tokens / %d ms, want 0", got.TotalTokens, got.LlmDurationMs)
	}
}

func TestSessionStats_StoreFailureStillReportsCounts(t *testing.T) {
	// The tool timing comes from a second query. If that one fails, the page must
	// still open with the counts, the tokens and the model durations it already
	// has — the tool duration is simply 0.
	srv := &Server{logger: zap.NewNop(), store: failingBudgetStore{}}
	got := srv.sessionStatsFor(context.Background(), "s", []store.ChatMessage{
		{Role: store.RoleUser},
		{Role: store.RoleAssistant, UsageJSON: `{"total_tokens":10,"duration_ms":5}`},
	})
	if got.Turns != 1 || got.LlmCalls != 1 || got.TotalTokens != 10 || got.LlmDurationMs != 5 {
		t.Errorf("stats = %+v, want the counts to survive a failed aggregate query", got)
	}
	if got.ToolDurationMs != 0 {
		t.Errorf("tool_duration_ms = %d, want 0 when the aggregate query failed", got.ToolDurationMs)
	}
}

// emptyStatsStore answers the aggregate query with zeros; the embedded nil
// interface panics on anything else, which is what a test wants from a double it
// does not expect to be used further.
type emptyStatsStore struct {
	store.Store
}

func (emptyStatsStore) QueryInvocationTotals(context.Context, string) (store.InvocationTotals, error) {
	return store.InvocationTotals{}, nil
}
