package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What a task plan is, and what a surface has to be able to do with one.
//
// The vocabulary lives here, next to the Tool interface, for the same reason the
// ask_user vocabulary does (see ask.go): three packages have to agree on it and
// none of them owns the others. A tool writes a plan (internal/tool/builtin), a
// turn carries the planner on its context (internal/server), and the console
// renders and stores the same shape (internal/server, web/src/plan.js). Putting
// the types in any one of them would make the other two depend on it for no
// reason.
//
// Nothing here knows about SQLite, SSE or the browser: a Planner decides where a
// plan lives, who may change it, and what happens when the model asks for
// something impossible.

// TaskStatus is where one task of a plan stands.
//
// The five values are the whole vocabulary: a board renders them as three groups
// (执行中 / 待执行 / 已完成), and "failed" stays in 待执行 because a failed step is
// still a step to do — it is unfinished work with a reason attached, not a
// finished one.
type TaskStatus string

const (
	// TaskPending is not started.
	TaskPending TaskStatus = "pending"
	// TaskInProgress is being worked on right now. At most one task should be in
	// this state: it is what the board's 执行中 group and the model's own "where
	// am I" both read.
	TaskInProgress TaskStatus = "in_progress"
	// TaskDone is finished.
	TaskDone TaskStatus = "done"
	// TaskFailed was attempted and did not work.
	TaskFailed TaskStatus = "failed"
	// TaskSkipped was deliberately not done (no longer needed, superseded).
	TaskSkipped TaskStatus = "skipped"
)

// ValidTaskStatus reports whether s is one of the five statuses.
func ValidTaskStatus(s TaskStatus) bool {
	switch s {
	case TaskPending, TaskInProgress, TaskDone, TaskFailed, TaskSkipped:
		return true
	}
	return false
}

// TaskStatusList renders the vocabulary for a model-facing error message, so a
// model that invented a status is told the real ones instead of just "no".
func TaskStatusList() string {
	return string(TaskPending) + " / " + string(TaskInProgress) + " / " +
		string(TaskDone) + " / " + string(TaskFailed) + " / " + string(TaskSkipped)
}

// Task is one item of a plan. The JSON tags are the wire contract with the
// console (web/src/plan.js), so they are snake_case like the rest of the API.
type Task struct {
	// ID is stable for the life of the plan and is how the model addresses the
	// task when it updates it. The surface assigns it (a model that invents its
	// own ids produces duplicates across calls), so a Task coming from a tool
	// call may leave it empty.
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status TaskStatus `json:"status"`
	// Note is the optional one-line remark: what was found, why it failed, what
	// is left. It is what makes a finished task informative a turn later.
	Note string `json:"note,omitempty"`
}

// Plan is the task list for one conversation.
//
// It is deliberately flat and small: it is read by a model deciding what to do
// next and by a person glancing above the input box, and neither of them is
// served by a dependency graph.
type Plan struct {
	// Goal is what the whole plan is for, in the user's terms. It is what makes a
	// resumed turn possible at all — a task list with no subject is a list of
	// chores.
	Goal string `json:"goal"`
	// Tasks are the items, in the order the model intends to do them.
	Tasks []Task `json:"tasks"`
	// Revision increments on every change. The console uses it to tell a newer
	// plan from an older one; nothing else depends on it.
	Revision int `json:"revision"`
	// UpdatedAt is when the surface last changed this plan, in its own clock.
	UpdatedAt time.Time `json:"updated_at"`
}

// Empty reports whether the plan carries no tasks. An empty plan is treated as
// "no plan" everywhere: the board does not render, and 继续执行 has nothing to
// resume.
func (p Plan) Empty() bool { return len(p.Tasks) == 0 }

// Count returns how many tasks carry the given status.
func (p Plan) Count(status TaskStatus) int {
	n := 0
	for _, t := range p.Tasks {
		if t.Status == status {
			n++
		}
	}
	return n
}

// Progress returns how many tasks are done and how many there are.
func (p Plan) Progress() (done, total int) {
	return p.Count(TaskDone), len(p.Tasks)
}

// Unfinished returns how many tasks are not done. Skipped tasks count as
// finished: someone decided they were not needed, which is a decision, not a
// leftover.
func (p Plan) Unfinished() int {
	n := 0
	for _, t := range p.Tasks {
		if t.Status != TaskDone && t.Status != TaskSkipped {
			n++
		}
	}
	return n
}

// Task returns the task with this id and its position, or false when there is
// none. IDs are matched exactly: a model that passes "1" for task "t1" is
// corrected with the real ids rather than guessed at, because a plan where the
// wrong task is marked done is worse than a failed tool call.
func (p Plan) Task(id string) (Task, int, bool) {
	for i, t := range p.Tasks {
		if t.ID == id {
			return t, i, true
		}
	}
	return Task{}, -1, false
}

// IDs renders every task id, for an error message that lets the model fix its
// own call.
func (p Plan) IDs() string {
	if len(p.Tasks) == 0 {
		return "（计划为空）"
	}
	out := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		out = append(out, t.ID)
	}
	return strings.Join(out, ", ")
}

// Summary is one line about the plan, for a board header and for the model.
func (p Plan) Summary() string {
	if p.Empty() {
		return "计划为空"
	}
	done, total := p.Progress()
	out := fmt.Sprintf("已完成 %d/%d", done, total)
	var doing []string
	for _, t := range p.Tasks {
		if t.Status == TaskInProgress {
			doing = append(doing, t.Title)
		}
	}
	switch {
	case len(doing) > 0:
		out += " · 进行中：" + strings.Join(doing, "、")
	case p.Unfinished() == 0:
		out += " · 全部完成"
	default:
		out += fmt.Sprintf(" · 待执行 %d 项", p.Unfinished())
	}
	return out
}

// Render is the plan as a checklist, which is what a model reads back.
//
// The markers are text rather than the raw status values on purpose: a model that
// sees "[x]" and "[ ]" recognises the convention, while "status=done" makes it
// guess whether the rest of the line still matters.
func (p Plan) Render() string {
	var b strings.Builder
	if strings.TrimSpace(p.Goal) != "" {
		b.WriteString("目标：" + p.Goal + "\n")
	}
	if p.Empty() {
		b.WriteString("（还没有任务）")
		return b.String()
	}
	b.WriteString("进度：" + p.Summary() + "\n")
	for i, t := range p.Tasks {
		b.WriteString(strconv.Itoa(i+1) + ". " + taskMarker(t.Status) + " " + t.ID + " " + t.Title)
		if strings.TrimSpace(t.Note) != "" {
			b.WriteString(" — " + t.Note)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// taskMarker renders one status as the checklist glyph above.
func taskMarker(s TaskStatus) string {
	switch s {
	case TaskDone:
		return "[x]"
	case TaskInProgress:
		return "[>]"
	case TaskFailed:
		return "[!]"
	case TaskSkipped:
		return "[-]"
	default:
		return "[ ]"
	}
}

// NextTaskID returns an id that no task in tasks is using, in the "t<n>" form the
// surface assigns.
//
// It walks past the highest numeric suffix rather than counting tasks, so a plan
// that lost a task to a validation error — or one created in several calls —
// cannot hand out the same id twice.
func NextTaskID(tasks []Task) string {
	highest := 0
	for _, t := range tasks {
		n, ok := taskIDSuffix(t.ID)
		if ok && n > highest {
			highest = n
		}
	}
	return "t" + strconv.Itoa(highest+1)
}

// taskIDSuffix parses the number out of a "t<n>" id.
func taskIDSuffix(id string) (int, bool) {
	if len(id) < 2 || id[0] != 't' {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// TaskPatch is one change to one task. Every field is optional, and the zero
// value of each means "leave it alone" — which is why Note is a pointer: "no
// note" and "remove the note" are different requests, and a planner that
// conflated them would silently erase the reason a task failed.
type TaskPatch struct {
	Title  string
	Status TaskStatus
	Note   *string
}

// Empty reports whether the patch would change nothing, which the tool refuses
// rather than writing a no-op that looks like work.
func (p TaskPatch) Empty() bool {
	return strings.TrimSpace(p.Title) == "" && p.Status == "" && p.Note == nil
}

// Planner is a surface's plan store for one conversation.
//
// It is read from the turn's context (PlannerFrom) rather than captured by the
// tools, for the same reason the Asker is: one tool set is shared by every
// conversation of a surface, and each turn belongs to its own.
//
// Implementations own the limits — how many tasks a plan may hold, what an id
// looks like, whether a change is persisted at all — because those are
// deployment decisions. The tools own the parts the model has to fix itself: an
// empty title, a status that is not in the vocabulary, a task id that does not
// exist.
type Planner interface {
	// Current returns the plan as it stands, and whether there is one.
	Current(ctx context.Context) (Plan, bool, error)
	// Replace creates or overwrites the plan wholesale. It is what plan_create
	// calls, and it is the only way a plan comes into existence.
	Replace(ctx context.Context, plan Plan) (Plan, error)
	// Append adds tasks to the end of the current plan.
	Append(ctx context.Context, tasks []Task) (Plan, error)
	// Update changes one task, addressed by id.
	Update(ctx context.Context, taskID string, patch TaskPatch) (Plan, error)
}

// plannerKey is the context key carrying a turn's Planner. Unexported so the
// only way to install one is WithPlanner.
type plannerKey struct{}

// WithPlanner returns ctx carrying p, the plan store for this turn.
//
// A nil Planner returns ctx unchanged: "this surface has no plans" is the
// absence of the value, not a value every reader has to nil-check.
func WithPlanner(ctx context.Context, p Planner) context.Context {
	if p == nil || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, plannerKey{}, p)
}

// PlannerFrom returns the Planner installed on ctx, and whether there is one.
//
// A tool that gets false must say so instead of pretending to record a plan: the
// tools are only registered on surfaces that install a planner, so this is the
// belt to that braces — a surface that adds the tools without the store gets a
// readable refusal rather than a plan nobody kept.
func PlannerFrom(ctx context.Context) (Planner, bool) {
	if ctx == nil {
		return nil, false
	}
	p, ok := ctx.Value(plannerKey{}).(Planner)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}
