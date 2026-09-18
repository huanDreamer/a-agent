package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/tool"
)

// The four tools that make a plan visible and keep it honest.
//
// They are four and not one, because each of them answers a different question
// the model asks at a different moment: what am I doing (plan_create), what did
// I forget (plan_add), where am I now (plan_update), and where did I leave off
// (plan_read). A single "plan" tool with an action parameter is the same code
// with a dispatch the model has to get right on every call.
const (
	// PlanCreateToolName builds the plan.
	PlanCreateToolName = "plan_create"
	// PlanAddToolName appends tasks to it.
	PlanAddToolName = "plan_add"
	// PlanUpdateToolName moves one task.
	PlanUpdateToolName = "plan_update"
	// PlanReadToolName reads the plan back.
	PlanReadToolName = "plan_read"
)

// maxTaskTitleRunes bounds one task's title. It is a readability limit rather
// than a storage one: a title longer than this is a paragraph, and the board
// renders titles on one line.
const maxTaskTitleRunes = 200

// maxGoalRunes bounds the plan's goal. Two hundred characters is one sentence in
// any language this console is used in.
const maxGoalRunes = 400

// planCreateDescription is the model-facing contract of plan_create.
//
// It is written as rules, because the failure mode of a planning tool is a model
// that either plans everything (including "say hello") or plans nothing and
// narrates the plan in prose nobody can follow. The three sentences that matter
// are: when to call it, how big the tasks should be, and that the list has to be
// maintained afterwards.
func planCreateDescription() string {
	return "为本次任务建立一份计划（任务清单），它会显示在用户输入框上方的任务看板里，并随执行逐步更新。" +
		"需要多步、多次工具调用或改动多个文件的任务，动手之前先调用它：goal 用一句话写清最后要交付什么，tasks 拆成 3-10 条能独立完成、能判断做完没做完的条目（按执行顺序）。" +
		"一步就能答完的问题、闲聊、纯问答不要调用它。" +
		"计划建立之后，每完成一条就立刻用 plan_update 把它标成 done 并写上结果；不要等到全部做完再一次性补记。" +
		"任务 id 由系统分配，请使用返回的 checklist 里的真实 id，不要自己编。"
}

func planAddDescription() string {
	return "在现有计划末尾追加任务。执行过程中发现新的必做事项（一个报错暴露出另一个问题、用户补了一个要求）时用它。" +
		"不要用 plan_create 覆盖已有计划：那会把已经完成的进度一起丢掉。"
}

func planUpdateDescription() string {
	return "更新计划里某一条任务的状态或备注，这是把计划与实际进展对齐的唯一方式。" +
		"开始做一条时标 in_progress（同一时间最多一条），做完标 done 并把结果或结论写进 note，做不下去标 failed 并把错误写进 note，" +
		"确认不再需要时标 skipped。每完成一步就调用一次，不要攒到最后。" +
		"status 取值：" + tool.TaskStatusList() + "。task_id 必须是 plan_create 或 plan_read 返回的真实 id。"
}

func planReadDescription() string {
	return "读回当前计划：目标、每条任务的状态与备注。一轮任务被中断（报错、预算用尽、用户停止）后重新接手，" +
		"或者不确定还剩哪些没做时，先调用它再决定从哪继续；已经标成 done 的任务不要重做。"
}

// PlanTaskInput is one task as the model writes it.
type PlanTaskInput struct {
	// Optional id, only accepted from plan_create. plan_add assigns its own.
	ID    string `json:"id,omitempty" jsonschema:"description=可选。留空由系统分配（推荐留空）；只有在你要引用已有计划里的任务时才填写"`
	Title string `json:"title" jsonschema:"description=一条任务的标题：一句话说清要做什么、做完的标准是什么。不要写编号或前缀,required"`
}

// PlanCreateInput is the parameter schema for plan_create.
type PlanCreateInput struct {
	Goal  string          `json:"goal" jsonschema:"description=这次任务的总体目标：一句话，从用户的角度说清最后要交付什么,required"`
	Tasks []PlanTaskInput `json:"tasks" jsonschema:"description=任务清单，3-10 条，按执行顺序排列；每条都应是能独立判断完成与否的动作,required"`
}

// PlanAddInput is the parameter schema for plan_add.
type PlanAddInput struct {
	Tasks []PlanTaskInput `json:"tasks" jsonschema:"description=要追加的任务，按顺序排在现有任务之后,required"`
}

// PlanUpdateInput is the parameter schema for plan_update.
type PlanUpdateInput struct {
	TaskID string `json:"task_id" jsonschema:"description=要更新的任务 id：plan_create 或 plan_read 返回的那个（如 t1、t2）,required"`
	Status string `json:"status,omitempty" jsonschema:"description=新状态，取值：pending / in_progress / done / failed / skipped。不改状态时留空"`
	Note   string `json:"note,omitempty" jsonschema:"description=这一条的结果、结论或失败原因，一句话。标记 done 或 failed 时请务必填写"`
	Title  string `json:"title,omitempty" jsonschema:"description=修正标题时填写；不改动标题就留空"`
	// A pointer so "no note" and "clear the note" stay distinguishable: a model
	// that re-opens a task it had marked done usually wants the stale note gone,
	// and a plain string would make that request indistinguishable from silence.
	ClearNote *bool `json:"clear_note,omitempty" jsonschema:"description=设为 true 可清空该任务的备注。默认 false（保留原备注）"`
}

// PlanReadInput is the parameter schema for plan_read. It takes nothing, and the
// struct exists so the generated schema is still an object, which providers
// require.
type PlanReadInput struct{}

// PlanOutput is what every plan tool returns.
//
// Checklist is the whole point of repeating it on every call: the model's next
// decision depends on what is still open, and making it spend a separate
// plan_read for that would burn a step per update.
type PlanOutput struct {
	Status string `json:"status"`
	// Summary is one line: 已完成 2/5 · 进行中：...
	Summary string `json:"summary"`
	// Checklist is the plan rendered as a checklist with real ids.
	Checklist string `json:"checklist"`
	// Note is what the model should do next, when there is something to say.
	Note string `json:"note,omitempty"`
}

// NewPlanTools returns the four plan tools.
//
// The plan store is read from the turn's context (tool.PlannerFrom) rather than
// captured here: one tool set is shared by every conversation of a surface, and
// each turn belongs to its own.
func NewPlanTools() ([]einotool.InvokableTool, error) {
	create, err := utils.InferTool(PlanCreateToolName, planCreateDescription(), planCreate)
	if err != nil {
		return nil, err
	}
	add, err := utils.InferTool(PlanAddToolName, planAddDescription(), planAdd)
	if err != nil {
		return nil, err
	}
	update, err := utils.InferTool(PlanUpdateToolName, planUpdateDescription(), planUpdate)
	if err != nil {
		return nil, err
	}
	read, err := utils.InferTool(PlanReadToolName, planReadDescription(), planRead)
	if err != nil {
		return nil, err
	}
	return []einotool.InvokableTool{create, add, update, read}, nil
}

// planner reads the turn's plan store, or refuses in words the model can act on.
func planner(ctx context.Context, toolName string) (tool.Planner, error) {
	p, ok := tool.PlannerFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("当前通道没有任务计划（%s 只在网页控制台可用）：请直接用文字说明你要做什么、做到哪一步了", toolName)
	}
	return p, nil
}

// planCreate builds a fresh plan, replacing whatever was there.
func planCreate(ctx context.Context, in PlanCreateInput) (PlanOutput, error) {
	p, err := planner(ctx, PlanCreateToolName)
	if err != nil {
		return PlanOutput{}, err
	}
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return PlanOutput{}, errors.New("goal 不能为空：一句话说清这次任务最后要交付什么")
	}
	if len([]rune(goal)) > maxGoalRunes {
		return PlanOutput{}, fmt.Errorf("goal 太长（%d 字，上限 %d）：把细节放进任务标题里",
			len([]rune(goal)), maxGoalRunes)
	}
	if len(in.Tasks) == 0 {
		return PlanOutput{}, errors.New("tasks 不能为空：计划至少要有一条任务；如果这件事不需要计划，就不要调用 plan_create")
	}

	tasks, err := planTasks(in.Tasks, true)
	if err != nil {
		return PlanOutput{}, err
	}
	plan, err := p.Replace(ctx, tool.Plan{Goal: goal, Tasks: tasks})
	if err != nil {
		return PlanOutput{}, err
	}
	return planOutput("created", plan,
		"计划已建立。开始第一条时用 plan_update 把它标成 in_progress。"), nil
}

// planAdd appends tasks to the plan that exists.
func planAdd(ctx context.Context, in PlanAddInput) (PlanOutput, error) {
	p, err := planner(ctx, PlanAddToolName)
	if err != nil {
		return PlanOutput{}, err
	}
	if len(in.Tasks) == 0 {
		return PlanOutput{}, errors.New("tasks 不能为空：说明要追加哪些任务")
	}
	current, ok, err := p.Current(ctx)
	if err != nil {
		return PlanOutput{}, err
	}
	if !ok || current.Empty() {
		return PlanOutput{}, errors.New("当前还没有计划：先用 plan_create 建立计划，再用 plan_add 追加")
	}

	tasks, err := planTasks(in.Tasks, false)
	if err != nil {
		return PlanOutput{}, err
	}
	// Ids are minted from the current plan so an appended task can never collide
	// with one that is already there — including when the model supplied an id
	// that looks like ours.
	next := current.Tasks
	for i := range tasks {
		tasks[i].ID = tool.NextTaskID(next)
		next = append(next, tasks[i])
	}

	plan, err := p.Append(ctx, tasks)
	if err != nil {
		return PlanOutput{}, err
	}
	return planOutput("appended", plan, ""), nil
}

// planUpdate moves one task.
func planUpdate(ctx context.Context, in PlanUpdateInput) (PlanOutput, error) {
	p, err := planner(ctx, PlanUpdateToolName)
	if err != nil {
		return PlanOutput{}, err
	}
	current, ok, err := p.Current(ctx)
	if err != nil {
		return PlanOutput{}, err
	}
	if !ok || current.Empty() {
		return PlanOutput{}, errors.New("当前还没有计划：先用 plan_create 建立计划")
	}

	id := strings.TrimSpace(in.TaskID)
	if id == "" {
		return PlanOutput{}, errors.New("task_id 不能为空：用 checklist 里的真实 id（如 t1）")
	}
	if _, _, found := current.Task(id); !found {
		return PlanOutput{}, fmt.Errorf("计划里没有任务 %q。现有 id：%s", id, current.IDs())
	}

	patch := tool.TaskPatch{Title: strings.TrimSpace(in.Title)}
	if s := strings.TrimSpace(in.Status); s != "" {
		status := tool.TaskStatus(s)
		if !tool.ValidTaskStatus(status) {
			return PlanOutput{}, fmt.Errorf("status %q 不是合法状态，取值只能是：%s", s, tool.TaskStatusList())
		}
		patch.Status = status
	}
	if note := strings.TrimSpace(in.Note); note != "" {
		patch.Note = &note
	} else if in.ClearNote != nil && *in.ClearNote {
		empty := ""
		patch.Note = &empty
	}
	if patch.Empty() {
		return PlanOutput{}, errors.New("这次调用没有要改的内容：至少要给 status、note 或 title 之一")
	}
	if len([]rune(patch.Title)) > maxTaskTitleRunes {
		return PlanOutput{}, fmt.Errorf("title 太长（%d 字，上限 %d）", len([]rune(patch.Title)), maxTaskTitleRunes)
	}

	plan, err := p.Update(ctx, id, patch)
	if err != nil {
		return PlanOutput{}, err
	}
	return planOutput("updated", plan, planUpdateNote(plan)), nil
}

// planRead returns the plan as it stands.
func planRead(ctx context.Context, _ PlanReadInput) (PlanOutput, error) {
	p, err := planner(ctx, PlanReadToolName)
	if err != nil {
		return PlanOutput{}, err
	}
	plan, ok, err := p.Current(ctx)
	if err != nil {
		return PlanOutput{}, err
	}
	if !ok || plan.Empty() {
		return PlanOutput{
			Status:    "empty",
			Summary:   "当前没有计划",
			Checklist: "（还没有计划）",
			Note:      "如果这件事需要多步完成，先用 plan_create 建立计划。",
		}, nil
	}
	return planOutput("ok", plan, planResumeNote(plan)), nil
}

// planTasks normalises the model's task list: every title non-empty and short
// enough, and — when the model is allowed to name tasks — no duplicate id.
func planTasks(in []PlanTaskInput, withIDs bool) ([]tool.Task, error) {
	out := make([]tool.Task, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, raw := range in {
		title := strings.TrimSpace(raw.Title)
		if title == "" {
			return nil, fmt.Errorf("第 %d 条任务的 title 为空：每条都要写清做什么", i+1)
		}
		if len([]rune(title)) > maxTaskTitleRunes {
			return nil, fmt.Errorf("第 %d 条任务的 title 太长（%d 字，上限 %d）：一句话即可",
				i+1, len([]rune(title)), maxTaskTitleRunes)
		}
		task := tool.Task{Title: title, Status: tool.TaskPending}
		if withIDs {
			id := strings.TrimSpace(raw.ID)
			if id != "" {
				if _, dup := seen[id]; dup {
					return nil, fmt.Errorf("任务 id %q 重复：请留空 id 由系统分配（推荐），或给每条一个不同的 id", id)
				}
				seen[id] = struct{}{}
				task.ID = id
			}
		}
		out = append(out, task)
	}
	// Ids the model left empty are minted here, in order, so a plan created in
	// one call has stable ids from the moment the model sees them. Ids the model
	// *did* supply are already in the pool, so an auto-assigned one can never
	// collide with them.
	if withIDs {
		pool := make([]tool.Task, 0, len(out))
		for _, t := range out {
			if t.ID != "" {
				pool = append(pool, tool.Task{ID: t.ID})
			}
		}
		for i := range out {
			if out[i].ID != "" {
				continue
			}
			out[i].ID = tool.NextTaskID(pool)
			pool = append(pool, tool.Task{ID: out[i].ID})
		}
	}
	return out, nil
}

// planOutput renders one tool result.
func planOutput(status string, plan tool.Plan, note string) PlanOutput {
	return PlanOutput{
		Status:    status,
		Summary:   plan.Summary(),
		Checklist: plan.Render(),
		Note:      note,
	}
}

// planUpdateNote tells the model what the update implies for its next move. It
// is what turns the tool from a bookkeeping chore into a step in a loop.
func planUpdateNote(plan tool.Plan) string {
	doing := plan.Count(tool.TaskInProgress)
	switch {
	case plan.Unfinished() == 0:
		return "所有任务都已完成：请用一段话总结结果交给用户。"
	case doing > 1:
		return "同时有多个任务处于 in_progress：请把不做的那些改回 pending，看板上的执行中只应有一个。"
	case doing == 0:
		return "现在没有 in_progress 的任务：把接下来要做的那条标成 in_progress 再动手。"
	default:
		return ""
	}
}

// planResumeNote is what plan_read says when the plan still has work in it. A
// model that has just read a half-finished plan is exactly the one that needs to
// be told not to start over.
func planResumeNote(plan tool.Plan) string {
	if plan.Unfinished() == 0 {
		return "计划里的任务都已完成；如果用户还要继续，请说明这一点，必要时用 plan_add 追加新任务。"
	}
	return fmt.Sprintf("还有 %d 条任务没有完成：从第一个未完成的开始继续，已经标成 done 的不要重做。", plan.Unfinished())
}
