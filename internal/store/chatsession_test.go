package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChatSession_CRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sess := ChatSession{
		ID: "sess-1", Title: "First chat", UserID: "ou_alice",
		Provider: "deepseek", Model: "deepseek-chat",
	}
	if err := st.CreateChatSession(ctx, sess); err != nil {
		t.Fatalf("CreateChatSession: %v", err)
	}

	got, err := st.GetChatSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	if got.Title != "First chat" || got.UserID != "ou_alice" ||
		got.Provider != "deepseek" || got.Model != "deepseek-chat" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", got)
	}
	if got.MessageCount != 0 {
		t.Errorf("MessageCount = %d, want 0", got.MessageCount)
	}

	// Renaming must not clear the model: a partial patch is a real hazard.
	newTitle := "Renamed"
	if err := st.UpdateChatSession(ctx, "sess-1", ChatSessionPatch{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateChatSession: %v", err)
	}
	got, err = st.GetChatSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	if got.Title != "Renamed" {
		t.Errorf("Title = %q, want Renamed", got.Title)
	}
	if got.Model != "deepseek-chat" {
		t.Errorf("Model = %q, want it preserved by a title-only patch", got.Model)
	}

	if err := st.DeleteChatSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteChatSession: %v", err)
	}
	if _, err := st.GetChatSession(ctx, "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestChatSession_ErrorsAndEdgeCases(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	t.Run("empty id rejected", func(t *testing.T) {
		if err := st.CreateChatSession(ctx, ChatSession{}); err == nil {
			t.Error("want an error for an empty session id")
		}
	})
	t.Run("missing session is not found", func(t *testing.T) {
		if _, err := st.GetChatSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("updating a missing session is not found", func(t *testing.T) {
		title := "x"
		if err := st.UpdateChatSession(ctx, "nope", ChatSessionPatch{Title: &title}); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("deleting a missing session is not found", func(t *testing.T) {
		if err := st.DeleteChatSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("duplicate id rejected", func(t *testing.T) {
		if err := st.CreateChatSession(ctx, ChatSession{ID: "dup"}); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := st.CreateChatSession(ctx, ChatSession{ID: "dup"}); err == nil {
			t.Error("want an error when inserting a duplicate session id")
		}
	})
}

func TestChatSession_List(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC()
	for _, s := range []ChatSession{
		{ID: "old", UserID: "u1", CreatedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)},
		{ID: "mid", UserID: "u1", CreatedAt: base.Add(-time.Hour), UpdatedAt: base.Add(-time.Hour)},
		{ID: "new", UserID: "u2", CreatedAt: base, UpdatedAt: base},
	} {
		if err := st.CreateChatSession(ctx, s); err != nil {
			t.Fatalf("create %s: %v", s.ID, err)
		}
	}

	all, err := st.ListChatSessions(ctx, ChatSessionFilter{})
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("sessions = %d, want 3", len(all))
	}
	// Most recently updated first.
	if all[0].ID != "new" || all[1].ID != "mid" || all[2].ID != "old" {
		t.Errorf("order = %s/%s/%s, want new/mid/old", all[0].ID, all[1].ID, all[2].ID)
	}

	byUser, err := st.ListChatSessions(ctx, ChatSessionFilter{UserID: "u1"})
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(byUser) != 2 {
		t.Errorf("user filter = %d sessions, want 2", len(byUser))
	}

	limited, err := st.ListChatSessions(ctx, ChatSessionFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit = %d sessions, want 1", len(limited))
	}
}

func TestChatSession_TouchReorders(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	for _, s := range []ChatSession{
		{ID: "a", UpdatedAt: base},
		{ID: "b", UpdatedAt: base.Add(time.Minute)},
	} {
		if err := st.CreateChatSession(ctx, s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	first, _ := st.ListChatSessions(ctx, ChatSessionFilter{})
	if first[0].ID != "b" {
		t.Fatalf("expected b first, got %s", first[0].ID)
	}

	// Touching "a" must bring it to the top, which is how an active
	// conversation stays at the head of the list.
	if err := st.TouchChatSession(ctx, "a"); err != nil {
		t.Fatalf("TouchChatSession: %v", err)
	}
	after, _ := st.ListChatSessions(ctx, ChatSessionFilter{})
	if after[0].ID != "a" {
		t.Errorf("after touch order starts with %s, want a", after[0].ID)
	}
}

func TestChatMessages_AppendAndList(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	msgs := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi", Reasoning: "thinking", UsageJSON: `{"total_tokens":10}`},
		{Role: "assistant", ToolCalls: `[{"id":"c1","function":{"name":"time"}}]`},
		{Role: "tool", Content: "12:00", ToolCallID: "c1", ToolName: "time"},
		{Role: "assistant", Content: "done"},
	}
	for i, m := range msgs {
		if _, err := st.AppendChatMessage(ctx, "s", m); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := st.ListChatMessages(ctx, "s", 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("messages = %d, want %d", len(got), len(msgs))
	}
	// Oldest first, so a UI can render the conversation in order.
	for i := range msgs {
		if got[i].Role != msgs[i].Role {
			t.Errorf("message %d role = %q, want %q", i, got[i].Role, msgs[i].Role)
		}
	}
	if got[1].Reasoning != "thinking" {
		t.Errorf("reasoning not persisted: %q", got[1].Reasoning)
	}
	if got[1].UsageJSON != `{"total_tokens":10}` {
		t.Errorf("usage not persisted: %q", got[1].UsageJSON)
	}
	if got[2].ToolCalls == "" {
		t.Error("tool calls not persisted")
	}
	if got[3].ToolCallID != "c1" || got[3].ToolName != "time" {
		t.Errorf("tool result linkage lost: %+v", got[3])
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("created_at not set")
	}

	// Ids must be increasing so ordering is stable.
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Errorf("ids not ascending: %d then %d", got[i-1].ID, got[i].ID)
		}
	}
}

func TestChatMessages_MessageCountAndLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := st.AppendChatMessage(ctx, "s", ChatMessage{Role: "user", Content: "m"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// The session carries its message count so a list view need not fetch them.
	sess, err := st.GetChatSession(ctx, "s")
	if err != nil {
		t.Fatalf("GetChatSession: %v", err)
	}
	if sess.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", sess.MessageCount)
	}
	listed, _ := st.ListChatSessions(ctx, ChatSessionFilter{})
	if len(listed) != 1 || listed[0].MessageCount != 5 {
		t.Errorf("list MessageCount = %+v, want 5", listed)
	}

	limited, err := st.ListChatMessages(ctx, "s", 2)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit = %d messages, want 2", len(limited))
	}
}

func TestChatMessages_ClearKeepsSession(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s", Title: "keep me"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.AppendChatMessage(ctx, "s", ChatMessage{Role: "user", Content: "x"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := st.DeleteChatMessages(ctx, "s"); err != nil {
		t.Fatalf("DeleteChatMessages: %v", err)
	}

	// "Clear conversation" must empty the history but keep the session.
	sess, err := st.GetChatSession(ctx, "s")
	if err != nil {
		t.Fatalf("session should survive clearing: %v", err)
	}
	if sess.MessageCount != 0 {
		t.Errorf("MessageCount = %d, want 0", sess.MessageCount)
	}
	if sess.Title != "keep me" {
		t.Errorf("Title = %q, want it preserved", sess.Title)
	}
	msgs, _ := st.ListChatMessages(ctx, "s", 0)
	if len(msgs) != 0 {
		t.Errorf("messages = %d, want 0", len(msgs))
	}
}

func TestChatSession_DeleteCascadesMessages(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AppendChatMessage(ctx, "s", ChatMessage{Role: "user", Content: "x"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if err := st.DeleteChatSession(ctx, "s"); err != nil {
		t.Fatalf("DeleteChatSession: %v", err)
	}

	// Messages must not be left orphaned behind the session.
	msgs, err := st.ListChatMessages(ctx, "s", 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("orphaned messages = %d, want 0", len(msgs))
	}
	var count int
	if err := st.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chat_messages WHERE session_id = ?", "s").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("chat_messages still has %d rows for the deleted session", count)
	}
}

func TestChatMessages_EmptySessionIDRejected(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AppendChatMessage(context.Background(), "", ChatMessage{Role: "user"}); err == nil {
		t.Error("want an error for an empty session id")
	}
}

func TestChatMessages_ErrorPersisted(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	longErr := strings.Repeat("boom ", 50)
	if _, err := st.AppendChatMessage(ctx, "s", ChatMessage{
		Role: "assistant", Error: longErr,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ := st.ListChatMessages(ctx, "s", 0)
	if len(got) != 1 || got[0].Error != longErr {
		t.Errorf("error text not persisted intact: %q", got[0].Error)
	}
}
