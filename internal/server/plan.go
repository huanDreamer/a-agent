package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// defaultPlanMaxTasks bounds a plan when the config does not say. It duplicates
// config.DefaultPlanMaxTasks for the one caller that can be built without a
// config (a test, or a deployment that wired a planner by hand); the two values
// are asserted equal in the tests.
const defaultPlanMaxTasks = 50

// turnPlanner is the task plan of one conversation, as the turn's tools see it.
//
// It is the surface half of internal/tool's Planner interface: the tools decide
// what a model may ask for, and this decides what actually happens — the plan is
// written to the database (so it survives the turn that produced it, a reload,
// and a restart) and published on the turn's event log (so the board above the
// composer moves while the work happens rather than only after it).
//
// The write is deliberately synchronous and before the event is emitted: the
// console reloads the conversation the moment a turn ends, and a plan that only
// existed in an event would be gone from the reloaded page.
type turnPlanner struct {
	st        store.Store
	sessionID string
	// maxTasks is the plan size limit, from chat.plan.max_tasks. It is enforced
	// here rather than in the tool because it is a deployment setting, and the
	// tools do not read config.
	maxTasks int
	emit     chat.Emitter
	logger   *zap.Logger

	mu     sync.Mutex
	plan   tool.Plan
	loaded bool
}

// newTurnPlanner builds the planner for one turn. A nil emitter is replaced by a
// no-op, so a planner is safe to build in a test.
func newTurnPlanner(st store.Store, sessionID string, maxTasks int, emit chat.Emitter, logger *zap.Logger) *turnPlanner {
	if emit == nil {
		emit = func(chat.Event) {}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &turnPlanner{st: st, sessionID: sessionID, maxTasks: maxTasks, emit: emit, logger: logger}
}

// Current returns the plan as it stands.
func (p *turnPlanner) Current(ctx context.Context) (tool.Plan, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return tool.Plan{}, false, err
	}
	return p.plan, !p.plan.Empty(), nil
}

// load reads the plan from the store once per turn.
//
// Once is enough and is also the point: the planner is the only writer while a
// turn runs, so its in-memory copy is never stale, and re-reading per tool call
// would turn a chatty model into a stream of queries on a single-writer SQLite.
func (p *turnPlanner) load(ctx context.Context) error {
	if p.loaded {
		return nil
	}
	row, err := p.st.GetChatPlan(ctx, p.sessionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		p.plan = tool.Plan{}
		p.loaded = true
		return nil
	case err != nil:
		return fmt.Errorf("读取任务计划失败：%w", err)
	}
	plan, err := planFromRow(row)
	if err != nil {
		return err
	}
	p.plan = plan
	p.loaded = true
	return nil
}

// Replace creates or overwrites the plan.
func (p *turnPlanner) Replace(ctx context.Context, plan tool.Plan) (tool.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return tool.Plan{}, err
	}
	plan.Goal = strings.TrimSpace(plan.Goal)
	plan.Tasks = normalizeTasks(plan.Tasks)
	if err := p.checkSize(plan.Tasks); err != nil {
		return tool.Plan{}, err
	}
	// The revision counts changes to this conversation's plan, so it keeps
	// counting across a replacement: a new plan that restarted at 1 would look
	// older than the one it replaced to a client comparing revisions.
	plan.Revision = p.plan.Revision + 1
	plan.UpdatedAt = time.Now().UTC()
	if err := p.save(ctx, plan); err != nil {
		return tool.Plan{}, err
	}
	return plan, nil
}

// Append adds tasks to the current plan.
func (p *turnPlanner) Append(ctx context.Context, tasks []tool.Task) (tool.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return tool.Plan{}, err
	}
	if p.plan.Empty() {
		return tool.Plan{}, errors.New("当前还没有计划：先用 plan_create 建立计划")
	}
	next := p.plan
	next.Tasks = append(append([]tool.Task(nil), p.plan.Tasks...), normalizeTasks(tasks)...)
	if err := p.checkSize(next.Tasks); err != nil {
		return tool.Plan{}, err
	}
	if err := p.checkIDs(next.Tasks); err != nil {
		return tool.Plan{}, err
	}
	next.Revision = p.plan.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	if err := p.save(ctx, next); err != nil {
		return tool.Plan{}, err
	}
	return next, nil
}

// Update changes one task.
func (p *turnPlanner) Update(ctx context.Context, taskID string, patch tool.TaskPatch) (tool.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return tool.Plan{}, err
	}
	if p.plan.Empty() {
		return tool.Plan{}, errors.New("当前还没有计划：先用 plan_create 建立计划")
	}
	next := p.plan
	next.Tasks = append([]tool.Task(nil), p.plan.Tasks...)
	target, idx, ok := next.Task(taskID)
	if !ok {
		return tool.Plan{}, fmt.Errorf("计划里没有任务 %q（现有：%s）", taskID, next.IDs())
	}
	if patch.Title != "" {
		target.Title = patch.Title
	}
	if patch.Status != "" {
		if !tool.ValidTaskStatus(patch.Status) {
			return tool.Plan{}, fmt.Errorf("status %q 不是合法状态", patch.Status)
		}
		target.Status = patch.Status
	}
	if patch.Note != nil {
		target.Note = *patch.Note
	}
	next.Tasks[idx] = target
	next.Revision = p.plan.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	if err := p.save(ctx, next); err != nil {
		return tool.Plan{}, err
	}
	return next, nil
}

// normalizeTasks gives every task a status the vocabulary knows.
//
// The tools already do this, and it is repeated here because the planner is the
// last thing standing between a task list and the database: a task stored with an
// empty status would render as unknown on the board, and the plan would be
// persisted in a shape nothing else can read. "pending" is the safe reading of
// "not done".
func normalizeTasks(tasks []tool.Task) []tool.Task {
	out := make([]tool.Task, len(tasks))
	copy(out, tasks)
	for i := range out {
		if !tool.ValidTaskStatus(out[i].Status) {
			out[i].Status = tool.TaskPending
		}
	}
	return out
}

// checkSize refuses a plan longer than the deployment allows.
//
// It refuses rather than truncating: a model that sent twenty tasks and is told
// "eight were dropped" can fix its call, while a silent truncation leaves it
// believing work was planned that no board will ever show.
func (p *turnPlanner) checkSize(tasks []tool.Task) error {
	limit := p.maxTasks
	if limit <= 0 {
		limit = defaultPlanMaxTasks
	}
	if len(tasks) > limit {
		return fmt.Errorf("计划最多 %d 条任务，收到 %d 条：请合并或删减后再试", limit, len(tasks))
	}
	return nil
}

// checkIDs refuses a plan whose ids are not unique, which would make
// plan_update ambiguous.
func (p *turnPlanner) checkIDs(tasks []tool.Task) error {
	seen := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		if strings.TrimSpace(t.ID) == "" {
			return errors.New("任务缺少 id：请由系统分配，不要手写空 id")
		}
		if _, dup := seen[t.ID]; dup {
			return fmt.Errorf("任务 id %q 重复", t.ID)
		}
		seen[t.ID] = struct{}{}
	}
	return nil
}

// save persists the plan and publishes it, in that order.
func (p *turnPlanner) save(ctx context.Context, plan tool.Plan) error {
	row, err := planToRow(p.sessionID, plan)
	if err != nil {
		return err
	}
	if err := p.st.SetChatPlan(ctx, row); err != nil {
		return fmt.Errorf("保存任务计划失败：%w", err)
	}
	p.plan = plan
	p.loaded = true

	published := plan
	p.logger.Info("chat: task plan updated",
		zap.String("session", p.sessionID),
		zap.Int("revision", plan.Revision),
		zap.Int("tasks", len(plan.Tasks)),
		zap.String("summary", plan.Summary()),
	)
	p.emit(chat.Event{Type: chat.EventPlan, Plan: &published})
	return nil
}

// planFromRow decodes a stored plan.
//
// A row that cannot be decoded degrades to "no plan" rather than failing the
// turn: it is display state, and refusing to answer a question because a task
// list is unreadable would be the wrong trade. The caller that reads plans for
// the resume brief treats the same case the same way.
func planFromRow(row store.ChatPlanRow) (tool.Plan, error) {
	plan := tool.Plan{Goal: row.Goal, Revision: row.Revision, UpdatedAt: row.UpdatedAt}
	if strings.TrimSpace(row.TasksJSON) == "" {
		return plan, nil
	}
	tasks := make([]tool.Task, 0, 8)
	if err := json.Unmarshal([]byte(row.TasksJSON), &tasks); err != nil {
		return tool.Plan{}, fmt.Errorf("任务计划无法解析：%w", err)
	}
	for i := range tasks {
		if !tool.ValidTaskStatus(tasks[i].Status) {
			// An unknown status is a stored row this build does not understand.
			// "pending" is the safe reading: the task is not done.
			tasks[i].Status = tool.TaskPending
		}
	}
	plan.Tasks = tasks
	return plan, nil
}

// planToRow encodes a plan for storage.
func planToRow(sessionID string, plan tool.Plan) (store.ChatPlanRow, error) {
	tasks := plan.Tasks
	if tasks == nil {
		tasks = []tool.Task{}
	}
	b, err := json.Marshal(tasks)
	if err != nil {
		return store.ChatPlanRow{}, fmt.Errorf("任务计划无法序列化：%w", err)
	}
	return store.ChatPlanRow{
		SessionID: sessionID,
		Goal:      plan.Goal,
		TasksJSON: string(b),
		Revision:  plan.Revision,
		UpdatedAt: plan.UpdatedAt,
	}, nil
}

// clearFinishedPlan drops the plan of a task that is over, when a new request
// starts.
//
// A plan describes one piece of work. Once every task in it is done (or
// deliberately skipped), the checklist has said everything it has to say: the
// model has answered, and the next message is a different request. Keeping it
// would leave a board of completed chores above a composer that is about to be
// asked something else, and the reader would have to work out for themselves
// that it is a receipt rather than work in progress.
//
// A plan with anything left in it is kept, and that asymmetry is the point: an
// unfinished plan is still somewhere to continue from (继续执行 is offered for
// it), while a finished one is only a record. Unfinished work is also the one
// case where the reader may still want to see what the model thought it was
// doing.
//
// It publishes an empty plan on the new turn's log, so a reader that is already
// open — another tab, a conversation being polled — drops the board now rather
// than at the end of the turn.
func (s *Server) clearFinishedPlan(ctx context.Context, sessionID string, emit chat.Emitter) {
	plan := s.sessionPlan(ctx, sessionID)
	if plan == nil || plan.Unfinished() > 0 {
		return
	}
	if err := s.store.DeleteChatPlan(ctx, sessionID); err != nil {
		// A plan that could not be deleted is a board that stays up one turn too
		// long. Nothing about the request that is starting depends on it, so this
		// is logged and the turn carries on.
		s.logger.Warn("chat: clearing a finished plan failed",
			zapString("session", sessionID), zapError(err))
		return
	}
	if emit == nil {
		return
	}
	s.logger.Info("chat: cleared the finished plan of the previous task",
		zapString("session", sessionID),
		zap.Int("tasks", len(plan.Tasks)),
		zap.String("summary", plan.Summary()),
	)
	// An empty plan is how "there is no plan" travels on the wire (the console
	// normalises it to null), so this needs no new event type — and a client one
	// version behind already knows how to render it.
	cleared := tool.Plan{Revision: plan.Revision + 1}
	emit(chat.Event{Type: chat.EventPlan, Plan: &cleared})
}

// sessionPlan reads a conversation's plan for the API. A conversation with no
// plan — or one whose stored plan cannot be read — answers nil, which the
// console renders as "no board".
func (s *Server) sessionPlan(ctx context.Context, sessionID string) *tool.Plan {
	row, err := s.store.GetChatPlan(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.logger.Warn("chat: read session plan failed", zapString("session", sessionID), zapError(err))
		return nil
	}
	plan, err := planFromRow(row)
	if err != nil {
		s.logger.Warn("chat: stored plan is unreadable", zapString("session", sessionID), zapError(err))
		return nil
	}
	if plan.Empty() {
		return nil
	}
	return &plan
}
