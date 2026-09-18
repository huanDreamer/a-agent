package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/tool"
)

// fakePlanner is an in-memory plan store, so the tools can be exercised without
// a database: what is under test here is the model-facing contract, not the
// persistence.
type fakePlanner struct {
	plan tool.Plan
	has  bool
	// mirrored records the plan the tools asked to store, so a test can tell a
	// refusal from a write.
	mirrored []tool.Plan
	err      error
}

func (f *fakePlanner) Current(context.Context) (tool.Plan, bool, error) {
	if f.err != nil {
		return tool.Plan{}, false, f.err
	}
	return f.plan, f.has, nil
}

func (f *fakePlanner) Replace(_ context.Context, plan tool.Plan) (tool.Plan, error) {
	if f.err != nil {
		return tool.Plan{}, f.err
	}
	f.plan, f.has = plan, true
	f.mirrored = append(f.mirrored, plan)
	return plan, nil
}

func (f *fakePlanner) Append(_ context.Context, tasks []tool.Task) (tool.Plan, error) {
	if f.err != nil {
		return tool.Plan{}, f.err
	}
	f.plan.Tasks = append(f.plan.Tasks, tasks...)
	f.has = true
	f.mirrored = append(f.mirrored, f.plan)
	return f.plan, nil
}

func (f *fakePlanner) Update(_ context.Context, taskID string, patch tool.TaskPatch) (tool.Plan, error) {
	if f.err != nil {
		return tool.Plan{}, f.err
	}
	task, idx, ok := f.plan.Task(taskID)
	if !ok {
		return tool.Plan{}, errors.New("no such task")
	}
	if patch.Title != "" {
		task.Title = patch.Title
	}
	if patch.Status != "" {
		task.Status = patch.Status
	}
	if patch.Note != nil {
		task.Note = *patch.Note
	}
	f.plan.Tasks[idx] = task
	f.mirrored = append(f.mirrored, f.plan)
	return f.plan, nil
}

// planTool looks one tool up by name and runs it with the given JSON arguments.
func planTool(t *testing.T, ctx context.Context, name, args string) (PlanOutput, error) {
	t.Helper()
	tools, err := NewPlanTools()
	if err != nil {
		t.Fatalf("NewPlanTools: %v", err)
	}
	for _, tl := range tools {
		info, ierr := tl.Info(context.Background())
		if ierr != nil {
			t.Fatalf("Info: %v", ierr)
		}
		if info.Name != name {
			continue
		}
		out, rerr := tl.InvokableRun(ctx, args)
		if rerr != nil {
			return PlanOutput{}, rerr
		}
		var decoded PlanOutput
		if uerr := json.Unmarshal([]byte(out), &decoded); uerr != nil {
			t.Fatalf("tool %s returned %q, which is not a PlanOutput: %v", name, out, uerr)
		}
		return decoded, nil
	}
	t.Fatalf("no tool named %s", name)
	return PlanOutput{}, nil
}

func TestPlanCreate_BuildsAPlanWithAssignedIDs(t *testing.T) {
	p := &fakePlanner{}
	out, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanCreateToolName,
		`{"goal":"  把续跑做出来  ","tasks":[{"title":"读 runner"},{"title":"加重试"},{"title":"跑测试"}]}`)
	if err != nil {
		t.Fatalf("plan_create: %v", err)
	}
	if len(p.mirrored) != 1 {
		t.Fatalf("the planner was asked to store %d plans, want 1", len(p.mirrored))
	}
	got := p.mirrored[0]
	if got.Goal != "把续跑做出来" {
		t.Fatalf("goal = %q, want it trimmed", got.Goal)
	}
	wantIDs := []string{"t1", "t2", "t3"}
	for i, want := range wantIDs {
		if got.Tasks[i].ID != want {
			t.Fatalf("task %d id = %q, want %q", i, got.Tasks[i].ID, want)
		}
		if got.Tasks[i].Status != tool.TaskPending {
			t.Fatalf("task %d status = %q, want pending", i, got.Tasks[i].Status)
		}
	}
	// The model reads the checklist back, so it never has to spend a step on
	// plan_read just to see what it wrote.
	if !strings.Contains(out.Checklist, "t1 读 runner") {
		t.Fatalf("checklist = %q, want the assigned ids", out.Checklist)
	}
	if out.Summary != "已完成 0/3 · 待执行 3 项" {
		t.Fatalf("summary = %q", out.Summary)
	}
	if !strings.Contains(out.Note, "in_progress") {
		t.Fatalf("note = %q, want it to say what to do next", out.Note)
	}
}

func TestPlanCreate_KeepsModelProvidedIDsAndRefusesDuplicates(t *testing.T) {
	p := &fakePlanner{}
	_, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanCreateToolName,
		`{"goal":"g","tasks":[{"id":"read","title":"读"},{"id":"write","title":"写"}]}`)
	if err != nil {
		t.Fatalf("plan_create with explicit ids: %v", err)
	}
	if p.mirrored[0].Tasks[0].ID != "read" {
		t.Fatalf("explicit id was rewritten to %q", p.mirrored[0].Tasks[0].ID)
	}

	q := &fakePlanner{}
	_, err = planTool(t, tool.WithPlanner(context.Background(), q), PlanCreateToolName,
		`{"goal":"g","tasks":[{"id":"t1","title":"一"},{"id":"t1","title":"二"}]}`)
	if err == nil {
		t.Fatal("duplicate ids were accepted")
	}
	if len(q.mirrored) != 0 {
		t.Fatal("a refused plan was stored anyway")
	}
}

func TestPlanCreate_MixesProvidedAndAssignedIDsWithoutCollision(t *testing.T) {
	// The model named the *second* task t1. An auto-assigned id must not walk
	// into it.
	p := &fakePlanner{}
	if _, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanCreateToolName,
		`{"goal":"g","tasks":[{"title":"第一件"},{"id":"t1","title":"第二件"}]}`); err != nil {
		t.Fatalf("plan_create: %v", err)
	}
	tasks := p.mirrored[0].Tasks
	if tasks[0].ID == tasks[1].ID {
		t.Fatalf("both tasks got id %q", tasks[0].ID)
	}
	if tasks[0].ID != "t2" {
		t.Fatalf("assigned id = %q, want t2 (t1 is taken)", tasks[0].ID)
	}
}

func TestPlanCreate_RefusesEmptyInput(t *testing.T) {
	cases := map[string]string{
		"empty goal":     `{"goal":"   ","tasks":[{"title":"一"}]}`,
		"no tasks":       `{"goal":"g","tasks":[]}`,
		"empty title":    `{"goal":"g","tasks":[{"title":"一"},{"title":"  "}]}`,
		"overlong title": `{"goal":"g","tasks":[{"title":"` + strings.Repeat("字", maxTaskTitleRunes+1) + `"}]}`,
		"overlong goal":  `{"goal":"` + strings.Repeat("字", maxGoalRunes+1) + `","tasks":[{"title":"一"}]}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			p := &fakePlanner{}
			_, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanCreateToolName, args)
			if err == nil {
				t.Fatal("bad input was accepted")
			}
			if len(p.mirrored) != 0 {
				t.Fatal("a refused plan was stored anyway")
			}
		})
	}
}

func TestPlanTools_RefuseWithoutAPlanner(t *testing.T) {
	// A surface that registers the tools without installing a store gets a
	// sentence the model can act on, not a silent no-op.
	for _, name := range []string{PlanCreateToolName, PlanAddToolName, PlanUpdateToolName, PlanReadToolName} {
		t.Run(name, func(t *testing.T) {
			_, err := planTool(t, context.Background(), name, `{"goal":"g","tasks":[{"title":"一"}]}`)
			if err == nil {
				t.Fatal("the tool worked without a planner")
			}
			if !strings.Contains(err.Error(), "网页控制台") {
				t.Fatalf("error %q does not explain where plans exist", err)
			}
		})
	}
}

func TestPlanAdd_AppendsWithFreshIDs(t *testing.T) {
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "已完成的", Status: tool.TaskDone, Note: "做完了"},
	}}}
	out, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanAddToolName,
		`{"tasks":[{"title":"新发现的活"}]}`)
	if err != nil {
		t.Fatalf("plan_add: %v", err)
	}
	got := p.mirrored[0].Tasks
	if len(got) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got))
	}
	if got[0].Status != tool.TaskDone || got[0].Note != "做完了" {
		t.Fatalf("appending lost the first task's progress: %+v", got[0])
	}
	if got[1].ID != "t2" || got[1].Status != tool.TaskPending {
		t.Fatalf("appended task = %+v, want t2 pending", got[1])
	}
	if !strings.Contains(out.Checklist, "t2 新发现的活") {
		t.Fatalf("checklist = %q", out.Checklist)
	}
}

func TestPlanAdd_WithoutAPlanSaysSo(t *testing.T) {
	p := &fakePlanner{}
	_, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanAddToolName,
		`{"tasks":[{"title":"一件"}]}`)
	if err == nil {
		t.Fatal("plan_add worked with no plan")
	}
	if !strings.Contains(err.Error(), "plan_create") {
		t.Fatalf("error %q does not tell the model what to do first", err)
	}
}

func TestPlanUpdate_MovesOneTask(t *testing.T) {
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskPending},
		{ID: "t2", Title: "两件", Status: tool.TaskPending},
	}}}
	out, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanUpdateToolName,
		`{"task_id":"t1","status":"done","note":"跑完了，全绿"}`)
	if err != nil {
		t.Fatalf("plan_update: %v", err)
	}
	got := p.mirrored[0].Tasks
	if got[0].Status != tool.TaskDone || got[0].Note != "跑完了，全绿" {
		t.Fatalf("task 1 = %+v", got[0])
	}
	if got[1].Status != tool.TaskPending {
		t.Fatalf("task 2 = %+v, want it untouched", got[1])
	}
	if !strings.Contains(out.Checklist, "[x] t1 一件 — 跑完了，全绿") {
		t.Fatalf("checklist = %q", out.Checklist)
	}
}

func TestPlanUpdate_ClearsANoteOnlyWhenAsked(t *testing.T) {
	// "no note" and "remove the note" are different requests: a model that
	// re-opens a task usually wants the stale reason gone, and one that omits the
	// field wants it kept.
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskFailed, Note: "上次超时"},
	}}}
	ctx := tool.WithPlanner(context.Background(), p)
	if _, err := planTool(t, ctx, PlanUpdateToolName, `{"task_id":"t1","status":"in_progress"}`); err != nil {
		t.Fatalf("plan_update: %v", err)
	}
	if got := p.mirrored[0].Tasks[0].Note; got != "上次超时" {
		t.Fatalf("note = %q, want it kept when the call did not mention it", got)
	}
	if _, err := planTool(t, ctx, PlanUpdateToolName, `{"task_id":"t1","clear_note":true}`); err != nil {
		t.Fatalf("plan_update with clear_note: %v", err)
	}
	if got := p.mirrored[1].Tasks[0].Note; got != "" {
		t.Fatalf("note = %q, want it cleared", got)
	}
}

func TestPlanUpdate_RefusesTheShapesTheModelCanFix(t *testing.T) {
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskPending},
	}}}
	ctx := tool.WithPlanner(context.Background(), p)
	cases := map[string]struct {
		args    string
		mention string
	}{
		"unknown id":    {`{"task_id":"t9","status":"done"}`, "t1"},
		"bad status":    {`{"task_id":"t1","status":"finished"}`, "in_progress"},
		"nothing to do": {`{"task_id":"t1"}`, "status"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := planTool(t, ctx, PlanUpdateToolName, tc.args)
			if err == nil {
				t.Fatal("the call was accepted")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("error %q does not mention %q, so the model cannot fix it", err, tc.mention)
			}
			if len(p.mirrored) != 0 {
				t.Fatal("a refused update was stored anyway")
			}
		})
	}
}

func TestPlanRead_ReportsAnEmptyPlanWithoutInventingOne(t *testing.T) {
	p := &fakePlanner{}
	out, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanReadToolName, `{}`)
	if err != nil {
		t.Fatalf("plan_read: %v", err)
	}
	if out.Status != "empty" {
		t.Fatalf("status = %q, want empty", out.Status)
	}
	if !strings.Contains(out.Note, "plan_create") {
		t.Fatalf("note = %q, want the next move", out.Note)
	}
	if len(p.mirrored) != 0 {
		t.Fatal("reading created a plan")
	}
}

func TestPlanRead_SaysHowMuchIsLeft(t *testing.T) {
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskDone},
		{ID: "t2", Title: "两件", Status: tool.TaskPending},
	}}}
	out, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanReadToolName, `{}`)
	if err != nil {
		t.Fatalf("plan_read: %v", err)
	}
	if !strings.Contains(out.Note, "1 条任务没有完成") {
		t.Fatalf("note = %q, want the count of what is left", out.Note)
	}
	if !strings.Contains(out.Checklist, "[x] t1") || !strings.Contains(out.Checklist, "[ ] t2") {
		t.Fatalf("checklist = %q", out.Checklist)
	}
}

func TestPlanTools_DescriptionsGuideTheModel(t *testing.T) {
	tools, err := NewPlanTools()
	if err != nil {
		t.Fatalf("NewPlanTools: %v", err)
	}
	if len(tools) != 4 {
		t.Fatalf("built %d tools, want 4", len(tools))
	}
	for _, tl := range tools {
		info, ierr := tl.Info(context.Background())
		if ierr != nil {
			t.Fatalf("Info: %v", ierr)
		}
		desc := strings.TrimSpace(info.Desc)
		if len([]rune(desc)) < 40 {
			t.Errorf("%s: description is too short to guide the model: %q", info.Name, desc)
		}
		if !strings.HasSuffix(desc, "。") {
			t.Errorf("%s: description does not end in a sentence: %q", info.Name, desc)
		}
		// The declaration has to survive into a schema a provider accepts.
		spec, serr := tool.SpecOf(context.Background(), tl)
		if serr != nil {
			t.Fatalf("%s: SpecOf: %v", info.Name, serr)
		}
		var schema map[string]any
		if uerr := json.Unmarshal([]byte(spec.ParametersJSONSchema), &schema); uerr != nil {
			t.Fatalf("%s: schema is not JSON: %v", info.Name, uerr)
		}
		if schema["type"] != "object" {
			t.Errorf("%s: schema type = %v, want object", info.Name, schema["type"])
		}
	}
}

// TestPlanUpdateIgnoresAnEmptyNoteString keeps the wire contract honest: an
// empty note without clear_note is "the model did not say", not "erase it".
func TestPlanUpdateIgnoresAnEmptyNoteString(t *testing.T) {
	p := &fakePlanner{has: true, plan: tool.Plan{Goal: "g", Tasks: []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskFailed, Note: "失败原因"},
	}}}
	if _, err := planTool(t, tool.WithPlanner(context.Background(), p), PlanUpdateToolName,
		`{"task_id":"t1","status":"pending","note":""}`); err != nil {
		t.Fatalf("plan_update: %v", err)
	}
	if got := p.mirrored[0].Tasks[0].Note; got != "失败原因" {
		t.Fatalf("note = %q, want it kept", got)
	}
}

var _ = einotool.InvokableTool(nil)
