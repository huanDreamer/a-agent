package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestChatPlan_RoundTrip covers the one piece of turn state that deliberately
// outlives its turn: without it, a resumed turn has no record of what was
// already done.
func TestChatPlan_RoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A conversation with no plan is ErrNotFound, not an empty plan: "never
	// planned anything" and "planned nothing" are different states.
	if _, err := st.GetChatPlan(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetChatPlan before any plan = %v, want ErrNotFound", err)
	}

	tasks := []map[string]string{
		{"id": "t1", "title": "读代码", "status": "done", "note": "已确认"},
		{"id": "t2", "title": "改代码", "status": "in_progress"},
	}
	raw, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("encode tasks: %v", err)
	}
	if err := st.SetChatPlan(ctx, ChatPlanRow{
		SessionID: "s1", Goal: "把续跑做出来", TasksJSON: string(raw), Revision: 3,
	}); err != nil {
		t.Fatalf("SetChatPlan: %v", err)
	}

	row, err := st.GetChatPlan(ctx, "s1")
	if err != nil {
		t.Fatalf("GetChatPlan: %v", err)
	}
	if row.Goal != "把续跑做出来" || row.Revision != 3 {
		t.Fatalf("row = %+v, want the goal and revision", row)
	}
	if row.UpdatedAt.IsZero() {
		t.Fatal("updated_at was not filled in")
	}
	var got []map[string]string
	if err := json.Unmarshal([]byte(row.TasksJSON), &got); err != nil {
		t.Fatalf("stored tasks are not JSON: %v", err)
	}
	if len(got) != 2 || got[1]["status"] != "in_progress" {
		t.Fatalf("tasks = %+v, want the two statuses", got)
	}
}

func TestChatPlan_SetIsAnUpsert(t *testing.T) {
	// The model's first plan_* call creates the row and every later one rewrites
	// it; the storage layer absorbing that keeps the planner free of a
	// "does it exist yet" branch that could race.
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, rev := range []int{1, 2, 3} {
		if err := st.SetChatPlan(ctx, ChatPlanRow{
			SessionID: "s1", Goal: "g", TasksJSON: `[{"id":"t1","title":"一件","status":"pending"}]`, Revision: rev,
		}); err != nil {
			t.Fatalf("SetChatPlan #%d: %v", rev, err)
		}
	}
	row, err := st.GetChatPlan(ctx, "s1")
	if err != nil {
		t.Fatalf("GetChatPlan: %v", err)
	}
	if row.Revision != 3 {
		t.Fatalf("revision = %d, want the last write to win", row.Revision)
	}

	if err := st.DeleteChatPlan(ctx, "s1"); err != nil {
		t.Fatalf("DeleteChatPlan: %v", err)
	}
	if _, err := st.GetChatPlan(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetChatPlan after delete = %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error: the caller's intent is met.
	if err := st.DeleteChatPlan(ctx, "s1"); err != nil {
		t.Fatalf("second DeleteChatPlan: %v", err)
	}
}

func TestChatPlan_GoesWithItsSession(t *testing.T) {
	// A plan is the conversation's working state; a conversation that no longer
	// exists must not leave one behind pointing at nothing.
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateChatSession(ctx, ChatSession{ID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.SetChatPlan(ctx, ChatPlanRow{
		SessionID: "s1", Goal: "g", TasksJSON: `[]`, Revision: 1,
	}); err != nil {
		t.Fatalf("SetChatPlan: %v", err)
	}
	if err := st.DeleteChatSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteChatSession: %v", err)
	}
	if _, err := st.GetChatPlan(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetChatPlan after the session was deleted = %v, want ErrNotFound", err)
	}
}

func TestChatPlan_RejectsAnEmptySessionID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.GetChatPlan(ctx, " "); err == nil {
		t.Fatal("GetChatPlan accepted an empty session id")
	}
	if err := st.SetChatPlan(ctx, ChatPlanRow{TasksJSON: "[]"}); err == nil {
		t.Fatal("SetChatPlan accepted an empty session id")
	}
	if err := st.DeleteChatPlan(ctx, ""); err == nil {
		t.Fatal("DeleteChatPlan accepted an empty session id")
	}
}
