package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// planStore opens a store with one conversation, which the plan's foreign key
// requires.
func planStore(t *testing.T, sessionID string) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateChatSession(context.Background(), store.ChatSession{ID: sessionID, Title: "t"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return st
}

// collectedEvents is an emitter that remembers what a turn published.
type collectedEvents struct {
	events []chat.Event
}

func (c *collectedEvents) emit(e chat.Event) { c.events = append(c.events, e) }

func (c *collectedEvents) plans() []*tool.Plan {
	var out []*tool.Plan
	for _, e := range c.events {
		if e.Type == chat.EventPlan && e.Plan != nil {
			out = append(out, e.Plan)
		}
	}
	return out
}

func TestTurnPlanner_CreatePublishesAndPersists(t *testing.T) {
	st := planStore(t, "s1")
	sink := &collectedEvents{}
	p := newTurnPlanner(st, "s1", 50, sink.emit, zap.NewNop())

	plan, err := p.Replace(context.Background(), tool.Plan{
		Goal: "把续跑做出来",
		Tasks: []tool.Task{
			{ID: "t1", Title: "读 runner", Status: tool.TaskDone, Note: "已确认 step 边界"},
			{ID: "t2", Title: "加重试", Status: tool.TaskInProgress},
			{ID: "t3", Title: "跑测试", Status: tool.TaskPending},
		},
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if plan.Revision != 1 {
		t.Fatalf("revision = %d, want 1", plan.Revision)
	}
	if plan.UpdatedAt.IsZero() {
		t.Fatal("updated_at was not set")
	}

	// Published: the board is what the console reads while the turn runs.
	published := sink.plans()
	if len(published) != 1 {
		t.Fatalf("published %d plans, want 1", len(published))
	}
	if published[0].Summary() != plan.Summary() {
		t.Fatalf("published %q, stored %q", published[0].Summary(), plan.Summary())
	}

	// Persisted: a reload and a restart both read this.
	row, err := st.GetChatPlan(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetChatPlan: %v", err)
	}
	if row.Goal != "把续跑做出来" || row.Revision != 1 {
		t.Fatalf("stored row = %+v, want the goal and revision", row)
	}
	var tasks []tool.Task
	if err := json.Unmarshal([]byte(row.TasksJSON), &tasks); err != nil {
		t.Fatalf("stored tasks are not JSON: %v", err)
	}
	if len(tasks) != 3 || tasks[1].Status != tool.TaskInProgress {
		t.Fatalf("stored tasks = %+v, want the three statuses", tasks)
	}
}

func TestTurnPlanner_UpdateMovesOneTaskAndBumpsTheRevision(t *testing.T) {
	st := planStore(t, "s1")
	sink := &collectedEvents{}
	p := newTurnPlanner(st, "s1", 50, sink.emit, zap.NewNop())
	ctx := context.Background()

	if _, err := p.Replace(ctx, tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "第一件", Status: tool.TaskPending},
		{ID: "t2", Title: "第二件", Status: tool.TaskPending},
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	note := "跑完了，全绿"
	updated, err := p.Update(ctx, "t1", tool.TaskPatch{Status: tool.TaskDone, Note: &note})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}
	if updated.Tasks[0].Status != tool.TaskDone || updated.Tasks[0].Note != note {
		t.Fatalf("task 1 = %+v, want done with its note", updated.Tasks[0])
	}
	if updated.Tasks[1].Status != tool.TaskPending {
		t.Fatalf("task 2 = %+v, want it untouched", updated.Tasks[1])
	}
	if got := len(sink.plans()); got != 2 {
		t.Fatalf("published %d plans, want 2 (one per change)", got)
	}
}

func TestTurnPlanner_UpdateRefusesAnUnknownID(t *testing.T) {
	st := planStore(t, "s1")
	p := newTurnPlanner(st, "s1", 50, nil, zap.NewNop())
	ctx := context.Background()
	if _, err := p.Replace(ctx, tool.Plan{Goal: "g", Tasks: []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskPending}}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	_, err := p.Update(ctx, "t9", tool.TaskPatch{Status: tool.TaskDone})
	if err == nil {
		t.Fatal("Update with an unknown id = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "t1") {
		t.Fatalf("error %q does not tell the model which ids exist", err)
	}
}

func TestTurnPlanner_RefusesAPlanOverTheLimit(t *testing.T) {
	st := planStore(t, "s1")
	p := newTurnPlanner(st, "s1", 2, nil, zap.NewNop())
	_, err := p.Replace(context.Background(), tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一"}, {ID: "t2", Title: "二"}, {ID: "t3", Title: "三"},
	}})
	if err == nil {
		t.Fatal("Replace over the limit = nil error, want a refusal")
	}
	if _, err := st.GetChatPlan(context.Background(), "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused plan was stored anyway (err = %v)", err)
	}
}

func TestTurnPlanner_AppendKeepsExistingProgress(t *testing.T) {
	st := planStore(t, "s1")
	p := newTurnPlanner(st, "s1", 50, nil, zap.NewNop())
	ctx := context.Background()
	if _, err := p.Replace(ctx, tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskDone, Note: "做完了"},
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	plan, err := p.Append(ctx, []tool.Task{{ID: "t2", Title: "新发现的活"}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(plan.Tasks))
	}
	if plan.Tasks[0].Status != tool.TaskDone || plan.Tasks[0].Note != "做完了" {
		t.Fatalf("appending lost the first task's progress: %+v", plan.Tasks[0])
	}
	if plan.Tasks[1].Status != tool.TaskPending {
		t.Fatalf("appended task = %+v, want it pending", plan.Tasks[1])
	}
}

func TestTurnPlanner_AppendWithoutAPlanRefuses(t *testing.T) {
	st := planStore(t, "s1")
	p := newTurnPlanner(st, "s1", 50, nil, zap.NewNop())
	if _, err := p.Append(context.Background(), []tool.Task{{ID: "t1", Title: "一件"}}); err == nil {
		t.Fatal("Append with no plan = nil error, want a refusal")
	}
}

func TestTurnPlanner_ReadsTheStoredPlanOnce(t *testing.T) {
	// A turn's planner is the only writer while it runs, and SQLite is a
	// single-writer database: re-reading per tool call would make a chatty model
	// into a stream of queries.
	st := planStore(t, "s1")
	sink := &collectedEvents{}
	ctx := context.Background()

	first := newTurnPlanner(st, "s1", 50, sink.emit, zap.NewNop())
	if _, err := first.Replace(ctx, tool.Plan{Goal: "g", Tasks: []tool.Task{{ID: "t1", Title: "一件"}}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// A second planner (the next turn) sees what the first one stored.
	second := newTurnPlanner(st, "s1", 50, nil, zap.NewNop())
	plan, ok, err := second.Current(ctx)
	if err != nil || !ok {
		t.Fatalf("Current() = (%v, %v), want the stored plan", ok, err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].Title != "一件" {
		t.Fatalf("plan = %+v, want the stored task", plan)
	}

	// A third turn's plan is gone with the conversation's plan row.
	if err := st.DeleteChatPlan(ctx, "s1"); err != nil {
		t.Fatalf("DeleteChatPlan: %v", err)
	}
	empty := newTurnPlanner(st, "s1", 50, nil, zap.NewNop())
	if _, ok, err := empty.Current(ctx); err != nil || ok {
		t.Fatalf("Current() after delete = (%v, %v), want no plan", ok, err)
	}
}

func TestPlanRow_RoundTripDegradesUnknownStatus(t *testing.T) {
	// A row written by a build that knew a status this one does not: "not done"
	// is the safe reading, and it must not fail the turn.
	row := store.ChatPlanRow{
		SessionID: "s1",
		Goal:      "g",
		TasksJSON: `[{"id":"t1","title":"一件","status":"exploded"},{"id":"t2","title":"两件","status":"done"}]`,
		Revision:  3,
	}
	plan, err := planFromRow(row)
	if err != nil {
		t.Fatalf("planFromRow: %v", err)
	}
	if plan.Tasks[0].Status != tool.TaskPending {
		t.Fatalf("unknown status survived as %q, want pending", plan.Tasks[0].Status)
	}
	if plan.Tasks[1].Status != tool.TaskDone {
		t.Fatalf("known status was rewritten: %q", plan.Tasks[1].Status)
	}

	if _, err := planFromRow(store.ChatPlanRow{SessionID: "s1", TasksJSON: "not json"}); err == nil {
		t.Fatal("unreadable tasks did not report an error")
	}
}

func TestSessionPlan_ReadsNullWhenThereIsNone(t *testing.T) {
	st := planStore(t, "s1")
	srv := &Server{store: st, logger: zap.NewNop()}
	if got := srv.sessionPlan(context.Background(), "s1"); got != nil {
		t.Fatalf("sessionPlan on a conversation with no plan = %+v, want nil", got)
	}
	if err := st.SetChatPlan(context.Background(), store.ChatPlanRow{
		SessionID: "s1", Goal: "g", TasksJSON: `[{"id":"t1","title":"一件","status":"pending"}]`, Revision: 1,
	}); err != nil {
		t.Fatalf("SetChatPlan: %v", err)
	}
	got := srv.sessionPlan(context.Background(), "s1")
	if got == nil || got.Goal != "g" || len(got.Tasks) != 1 {
		t.Fatalf("sessionPlan = %+v, want the stored plan", got)
	}
}

func TestPlanLimits_MatchTheConfigDefault(t *testing.T) {
	if defaultPlanMaxTasks != config.DefaultPlanMaxTasks {
		t.Fatalf("server default (%d) and config default (%d) disagree",
			defaultPlanMaxTasks, config.DefaultPlanMaxTasks)
	}
}

func TestTurnAccumulator_StepRetryRollsBackOnlyThatStep(t *testing.T) {
	// What the stored message must not contain: the half-answer of the attempt
	// that died followed by the answer of the one that succeeded.
	a := &turnAccumulator{}
	a.add(chat.Event{Type: chat.EventStepStart, Step: 1})
	a.add(chat.Event{Type: chat.EventReasoningDelta, Step: 1, Text: "想一下"})
	a.add(chat.Event{Type: chat.EventTextDelta, Step: 1, Text: "第一句。"})
	a.add(chat.Event{Type: chat.EventStepEnd, Step: 1})
	a.add(chat.Event{Type: chat.EventStepStart, Step: 2})
	a.add(chat.Event{Type: chat.EventReasoningDelta, Step: 2, Text: "第二步的思考"})
	a.add(chat.Event{Type: chat.EventTextDelta, Step: 2, Text: "半截话"})

	a.add(chat.Event{Type: chat.EventStepRetry, Step: 2, Attempt: 2, MaxAttempts: 3})

	if got := string(a.answer); got != "第一句。" {
		t.Fatalf("answer after the retry = %q, want only step 1's text", got)
	}
	if got := string(a.reasoning); got != "想一下" {
		t.Fatalf("reasoning after the retry = %q, want only step 1's thinking", got)
	}
	if n := len(a.steps); n != 2 {
		t.Fatalf("steps = %d, want 2 (a retry is not a new step)", n)
	}
	if len(a.steps[1].text) != 0 || len(a.steps[1].reasoning) != 0 {
		t.Fatalf("step 2 still holds %q / %q", a.steps[1].text, a.steps[1].reasoning)
	}

	// And the retry's own output lands cleanly.
	a.add(chat.Event{Type: chat.EventTextDelta, Step: 2, Text: "完整回答"})
	if got := string(a.answer); got != "第一句。完整回答" {
		t.Fatalf("answer = %q, want the recovered text appended once", got)
	}
}

func TestTurnAccumulator_StepRetryWithoutStepsIsIgnored(t *testing.T) {
	// A retry event for a step this accumulator never opened must not create a
	// phantom step: a reader of a stored turn has to see the iterations that ran.
	a := &turnAccumulator{}
	a.add(chat.Event{Type: chat.EventStepRetry, Step: 1, Attempt: 2})
	if len(a.steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(a.steps))
	}
}

func TestClearFinishedPlan_RemovesOnlyAFinishedPlan(t *testing.T) {
	cases := []struct {
		name      string
		tasks     []tool.Task
		wantGone  bool
		wantEvent bool
	}{
		{
			name:      "every task done",
			tasks:     []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskDone}, {ID: "t2", Title: "两件", Status: tool.TaskDone}},
			wantGone:  true,
			wantEvent: true,
		},
		{
			name:      "done and deliberately skipped",
			tasks:     []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskDone}, {ID: "t2", Title: "两件", Status: tool.TaskSkipped}},
			wantGone:  true,
			wantEvent: true,
		},
		{
			name:     "one task still pending",
			tasks:    []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskDone}, {ID: "t2", Title: "两件", Status: tool.TaskPending}},
			wantGone: false,
		},
		{
			name:     "one task in progress",
			tasks:    []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskInProgress}},
			wantGone: false,
		},
		{
			name:     "one task failed",
			tasks:    []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskFailed}},
			wantGone: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := planStore(t, "s1")
			srv := &Server{store: st, logger: zap.NewNop()}
			seedPlan(t, st, "s1", tc.tasks)

			sink := &collectedEvents{}
			srv.clearFinishedPlan(context.Background(), "s1", sink.emit)

			_, err := st.GetChatPlan(context.Background(), "s1")
			gone := errors.Is(err, store.ErrNotFound)
			if gone != tc.wantGone {
				t.Fatalf("plan deleted = %v, want %v (err = %v)", gone, tc.wantGone, err)
			}
			if got := len(sink.plans()); (got > 0) != tc.wantEvent {
				t.Fatalf("published %d plans, want event = %v", got, tc.wantEvent)
			}
			if tc.wantEvent {
				// The published plan is empty, which is how "no plan" travels on
				// the wire: the console normalises it to null and the board goes.
				published := sink.plans()[0]
				if !published.Empty() {
					t.Fatalf("published plan = %+v, want it empty", published)
				}
				if published.Revision <= 2 {
					t.Fatalf("revision = %d, want it past the plan it replaced (2)", published.Revision)
				}
			}
		})
	}
}

func TestClearFinishedPlan_WithoutAPlanIsANoOp(t *testing.T) {
	st := planStore(t, "s1")
	srv := &Server{store: st, logger: zap.NewNop()}
	sink := &collectedEvents{}
	srv.clearFinishedPlan(context.Background(), "s1", sink.emit)
	if len(sink.events) != 0 {
		t.Fatalf("published %d events for a conversation with no plan", len(sink.events))
	}
}

// TestTurn_NewMessageClearsAFinishedPlan is the reported behaviour end to end:
// the model finishes every task, the reader sends the next message, and the
// board is gone — from the store, from the fresh turn's event log, and therefore
// from every open page.
func TestTurn_NewMessageClearsAFinishedPlan(t *testing.T) {
	h, sessionID := planFlowHarness(t, [][]*schema.Message{
		{planToolCall("p1", builtin.PlanCreateToolName,
			`{"goal":"只做一件","tasks":[{"title":"唯一的一件"}]}`)},
		{planToolCall("p2", builtin.PlanUpdateToolName, `{"task_id":"t1","status":"done"}`)},
		{{Role: schema.Assistant, Content: "做完了。"}},
		// The next request gets its own script: it is a different piece of work.
		{{Role: schema.Assistant, Content: "第二件事也答完了。"}},
	})

	first := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "做一件事"})
	if !strings.Contains(answerText(first), "做完了。") {
		t.Fatalf("the first turn did not finish: %v", eventTypes(first))
	}
	// The finished plan is on record after the first turn.
	var after struct {
		Plan *tool.Plan `json:"plan"`
	}
	decode(t, h.get(t, "/api/chat/sessions/"+sessionID), &after)
	if after.Plan == nil || after.Plan.Unfinished() != 0 {
		t.Fatalf("plan after the first turn = %+v, want a finished one", after.Plan)
	}

	second := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "另外一件事"})

	// The new turn announces the cleared board, and does so before it answers —
	// the reader must not be looking at a stale checklist while the next reply
	// streams in.
	var cleared *tool.Plan
	for _, e := range second {
		raw, err := json.Marshal(e["plan"])
		if err != nil {
			continue
		}
		var plan tool.Plan
		if err := json.Unmarshal(raw, &plan); err == nil && len(plan.Tasks) == 0 {
			cleared = &plan
			break
		}
	}
	if cleared == nil {
		t.Fatalf("the new turn did not publish an empty plan: %v", eventTypes(second))
	}

	// And the stored state agrees, so a reload shows no board either.
	if _, err := h.store.GetChatPlan(context.Background(), sessionID); err == nil {
		t.Fatal("the finished plan is still stored after a new message")
	}
	var afterSecond struct {
		Plan *tool.Plan `json:"plan"`
	}
	decode(t, h.get(t, "/api/chat/sessions/"+sessionID), &afterSecond)
	if afterSecond.Plan != nil {
		t.Fatalf("GET session still reports a plan: %+v", afterSecond.Plan)
	}
}

// TestTurn_NewMessageKeepsAnUnfinishedPlan is the other half of the rule: work
// that is still open is not swept away by asking something else.
func TestTurn_NewMessageKeepsAnUnfinishedPlan(t *testing.T) {
	h, sessionID := planFlowHarness(t, [][]*schema.Message{
		{planToolCall("p1", builtin.PlanCreateToolName,
			`{"goal":"两件事","tasks":[{"title":"做完的一件"},{"title":"没做的一件"}]}`)},
		{planToolCall("p2", builtin.PlanUpdateToolName, `{"task_id":"t1","status":"done"}`)},
		{{Role: schema.Assistant, Content: "先做到这里。"}},
		{{Role: schema.Assistant, Content: "回答另一个问题。"}},
	})

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "做两件事"})

	var before struct {
		Plan *tool.Plan `json:"plan"`
	}
	decode(t, h.get(t, "/api/chat/sessions/"+sessionID), &before)
	if before.Plan == nil || before.Plan.Unfinished() != 1 {
		t.Fatalf("plan before the new message = %+v, want one task left", before.Plan)
	}

	readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "顺便问一句"})

	row, err := h.store.GetChatPlan(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("an unfinished plan was cleared by a new message: %v", err)
	}
	if !strings.Contains(row.TasksJSON, "没做的一件") {
		t.Fatalf("stored plan = %s, want the unfinished task", row.TasksJSON)
	}
}

// TestTurn_ResumeKeepsThePlan: the resume path is the one that must not clear,
// because the plan it would clear is the context it was asked to continue from.
func TestTurn_ResumeKeepsThePlan(t *testing.T) {
	mdl := &recordingModel{}
	h, sessionID := resumeHarness(t, mdl)
	seedFailedTurn(t, h.store, sessionID, "把这件事做完", "boom")
	seedPlan(t, h.store, sessionID, []tool.Task{
		{ID: "t1", Title: "做完的一件", Status: tool.TaskDone},
		{ID: "t2", Title: "没做的一件", Status: tool.TaskPending},
	})

	resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/resume", map[string]string{})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume status = %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()
	waitForCondition(t, 5*time.Second, "the resumed turn to be stored", func() bool {
		msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 0)
		return err == nil && len(msgs) > 0 && msgs[len(msgs)-1].Role == store.RoleAssistant
	})

	if _, err := h.store.GetChatPlan(context.Background(), sessionID); err != nil {
		t.Fatalf("resume cleared the plan it was continuing from: %v", err)
	}
	// The briefing the model got still carries the checklist.
	if history := messagesOf(mdl.lastSeen()); !strings.Contains(history, "没做的一件") {
		t.Errorf("the resumed turn was not told the plan:\n%s", history)
	}
}
