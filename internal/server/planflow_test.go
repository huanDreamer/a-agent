package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// planFlowHarness is the whole wiring a deployment has: the plan tools in the
// registry and the per-turn planner installed by the server. It is what makes
// the board move, and nothing smaller can show that.
func planFlowHarness(t *testing.T, turns [][]*schema.Message) (*harness, string) {
	t.Helper()

	reg := tool.NewRegistry()
	planTools, err := builtin.NewPlanTools()
	if err != nil {
		t.Fatalf("NewPlanTools: %v", err)
	}
	for _, tl := range planTools {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register plan tool: %v", err)
		}
	}

	runner, err := chat.New(chat.Config{
		Model:    &scriptedModel{turns: turns},
		Tools:    reg,
		MaxSteps: 6,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Runner:       runner,
		Tools:        reg,
		PlanEnable:   true,
		PlanMaxTasks: defaultPlanMaxTasks,
	}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "plan"}), &created)
	if created.Session.ID == "" {
		t.Fatal("no session id")
	}
	return h, created.Session.ID
}

// planToolCall is one model-issued plan_* call.
func planToolCall(id, name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: id, Type: "function",
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// TestTurn_PlanToolsDriveTheBoardAndTheStore is the feature end to end: the
// model builds a plan, updates a task, and both the live event stream and the
// stored conversation carry it.
func TestTurn_PlanToolsDriveTheBoardAndTheStore(t *testing.T) {
	h, sessionID := planFlowHarness(t, [][]*schema.Message{
		{planToolCall("p1", builtin.PlanCreateToolName,
			`{"goal":"把续跑做出来","tasks":[{"title":"读 runner"},{"title":"加重试"},{"title":"跑测试"}]}`)},
		{planToolCall("p2", builtin.PlanUpdateToolName,
			`{"task_id":"t1","status":"done","note":"看完了"}`)},
		{{Role: schema.Assistant, Content: "第一件做完了，接着做第二件。"}},
	})

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "把续跑做出来"})

	// The board is fed by plan events, one per change, each carrying the whole
	// plan as it now stands.
	var plans []*tool.Plan
	for _, e := range events {
		kind, _ := e["type"].(string)
		if kind != string(chat.EventPlan) {
			continue
		}
		raw, err := json.Marshal(e["plan"])
		if err != nil {
			t.Fatalf("encode plan event: %v", err)
		}
		var plan tool.Plan
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatalf("decode plan event: %v", err)
		}
		plans = append(plans, &plan)
	}
	if len(plans) != 2 {
		t.Fatalf("plan events = %d, want 2 (one per change)", len(plans))
	}
	if len(plans[0].Tasks) != 3 || plans[0].Tasks[0].Status != tool.TaskPending {
		t.Fatalf("first plan event = %+v, want three pending tasks", plans[0])
	}
	if plans[1].Tasks[0].Status != tool.TaskDone || plans[1].Tasks[0].Note != "看完了" {
		t.Fatalf("second plan event = %+v, want t1 done with its note", plans[1].Tasks[0])
	}
	if plans[1].Revision <= plans[0].Revision {
		t.Fatalf("revisions did not advance: %d then %d", plans[0].Revision, plans[1].Revision)
	}

	// The tool results reached the model as observations, so it can keep working
	// from the checklist it was handed.
	var sawChecklist bool
	for _, e := range events {
		if kind, _ := e["type"].(string); kind != "tool_result" {
			continue
		}
		result, _ := e["tool_result"].(string)
		if name, _ := e["tool_name"].(string); name == builtin.PlanCreateToolName &&
			strings.Contains(result, "t1 读 runner") {
			sawChecklist = true
		}
	}
	if !sawChecklist {
		t.Error("plan_create did not hand the model the checklist with real ids")
	}

	// And the stored state is what a reload reads.
	var body struct {
		Plan *tool.Plan `json:"plan"`
	}
	decode(t, h.get(t, "/api/chat/sessions/"+sessionID), &body)
	if body.Plan == nil || len(body.Plan.Tasks) != 3 {
		t.Fatalf("stored plan = %+v, want the three tasks", body.Plan)
	}
	if body.Plan.Tasks[0].Status != tool.TaskDone {
		t.Fatalf("stored task 1 = %+v, want done", body.Plan.Tasks[0])
	}
	if got := body.Plan.Goal; got != "把续跑做出来" {
		t.Fatalf("stored goal = %q", got)
	}
	// The FK row exists too, which is what survives a restart.
	row, err := h.store.GetChatPlan(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetChatPlan: %v", err)
	}
	if row.Revision != 2 {
		t.Fatalf("stored revision = %d, want 2", row.Revision)
	}
}

// TestTurn_PlanToolsAreWithheldWhenDisabled keeps the switch honest: with the
// feature off the model is not offered tools that could only fail.
func TestTurn_PlanToolsAreWithheldWhenDisabled(t *testing.T) {
	reg := tool.NewRegistry()
	planTools, err := builtin.NewPlanTools()
	if err != nil {
		t.Fatalf("NewPlanTools: %v", err)
	}
	for _, tl := range planTools {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	// PlanEnable is false: the tools exist in the registry (as they would if a
	// deployment turned the feature off at runtime) but no planner is installed,
	// so a call is refused in words rather than recorded nowhere.
	runner, err := chat.New(chat.Config{
		Model: &scriptedModel{turns: [][]*schema.Message{
			{planToolCall("p1", builtin.PlanCreateToolName, `{"goal":"g","tasks":[{"title":"一件"}]}`)},
			{{Role: schema.Assistant, Content: "我建不了计划，直接做。"}},
		}},
		Tools:    reg,
		MaxSteps: 4,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner, Tools: reg}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "off"}), &created)
	sessionID := created.Session.ID

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "做点事"})
	for _, e := range events {
		if kind, _ := e["type"].(string); kind == string(chat.EventPlan) {
			t.Fatal("a plan event was emitted although the feature is off")
		}
		if kind, _ := e["type"].(string); kind == "tool_result" {
			if name, _ := e["tool_name"].(string); name == builtin.PlanCreateToolName {
				if errText, _ := e["tool_error"].(string); !strings.Contains(errText, "网页控制台") {
					t.Fatalf("the refusal was %q, want an explanation the model can act on", errText)
				}
			}
		}
	}
	if _, err := h.store.GetChatPlan(context.Background(), sessionID); err == nil {
		t.Fatal("a refused plan was stored")
	}
}
