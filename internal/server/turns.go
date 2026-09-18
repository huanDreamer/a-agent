package server

import (
	"context"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// turnStoppedText is what a turn that the reader stopped is stored with.
//
// It is written into the message's error column, so it survives a reload: an
// answer that is only half there must say why, or the next reader takes it for
// the whole thing.
const turnStoppedText = "本轮已被停止，回答不完整"

// turnHub owns the turns that are in flight, one per conversation.
//
// A turn used to belong to the HTTP request that started it: the browser held
// the streaming response open, and closing that response was the only thing
// that could end the turn's delivery. That made the answer a property of a
// connection — so switching conversations, switching pages, or a refresh lost
// sight of a turn that was still running, and the reader could only wait for it
// to land in the database and be read back as a finished message.
//
// Here the turn belongs to the *conversation*. It runs on its own context, it
// accumulates everything it has produced, and any number of readers may attach
// to it, detach, and attach again — including a browser that was reloaded while
// it ran. What a reader sees the second time is the same event stream, replayed
// from the beginning and then followed live.
type turnHub struct {
	mu    sync.Mutex
	turns map[string]*liveTurn
	// runs counts the turn goroutines, so a shutdown can wait for them: a turn
	// persists its answer on the way out, and doing that after the store is
	// closed would lose it — and take the process down with it.
	runs sync.WaitGroup
}

func newTurnHub() *turnHub {
	return &turnHub{turns: make(map[string]*liveTurn)}
}

// shutdown cancels every turn in flight and waits for them to be stored.
//
// It is called before the store is closed. A turn interrupted this way is kept
// as a partial answer rather than dropped: the server was going away, not the
// work.
func (h *turnHub) shutdown(ctx context.Context) {
	h.mu.Lock()
	flying := make([]*liveTurn, 0, len(h.turns))
	for _, t := range h.turns {
		flying = append(flying, t)
	}
	h.mu.Unlock()

	for _, t := range flying {
		t.cancelRun()
	}

	done := make(chan struct{})
	go func() {
		h.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// turnRetain is how long a finished turn stays attachable.
//
// A turn can end before its reader manages to attach — a fast model, a slow
// round trip, a browser that sent the message and is still setting up its
// stream. Keeping the log for a while makes attaching the reliable way to read
// an answer: the reader gets the whole turn whether it arrives first or last,
// and never has to reason about which of the two happened.
const turnRetain = 2 * time.Minute

// begin registers a turn, refusing a second one for the same conversation.
//
// One turn per conversation is the contract the console is built on: the reader
// has a single composer, and two concurrent runs would interleave their answers
// into one transcript with no way to tell them apart. A finished turn that is
// still being retained does not count as busy.
func (h *turnHub) begin(t *liveTurn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, busy := h.turns[t.sessionID]; busy && !current.isDone() {
		return false
	}
	h.turns[t.sessionID] = t
	return true
}

// retire keeps a finished turn readable for a while, then forgets it.
func (h *turnHub) retire(t *liveTurn) {
	time.AfterFunc(turnRetain, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if current, ok := h.turns[t.sessionID]; ok && current == t {
			delete(h.turns, t.sessionID)
		}
	})
}

// find returns the turn for a conversation — running, or finished and still
// retained — or nil.
func (h *turnHub) find(sessionID string) *liveTurn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turns[sessionID]
}

// running reports whether a conversation has a turn *in flight*. The session
// list answers this for every row, which is what lets the sidebar say 生成中 for
// a conversation the reader is not looking at.
//
// A finished turn that is still retained is deliberately not running: it has
// nothing left to arrive, so marking its conversation as generating would be a
// lie that never clears.
func (h *turnHub) running(sessionID string) bool {
	t := h.find(sessionID)
	return t != nil && !t.isDone()
}

// stop asks the running turn for a conversation to end. Reports false when
// there was nothing left to stop.
func (h *turnHub) stop(sessionID string) bool {
	t := h.find(sessionID)
	if t == nil || t.isDone() {
		return false
	}
	t.stop()
	return true
}

// liveTurn is one turn in flight: its event log, its readers, and the handle
// that ends it.
type liveTurn struct {
	sessionID string
	startedAt time.Time

	// cancel ends the run. It is the request context's replacement: the turn
	// outlives every HTTP request that touches it, and only an explicit stop (or
	// the server shutting down) cancels it.
	cancel context.CancelFunc

	mu   sync.Mutex
	log  []chat.Event
	done bool
	// stopRequested records that a reader ended this turn, so the failure stored
	// with the answer can say so instead of reporting a bare context error.
	stopRequested bool
	// subs are the readers currently attached. Their channel carries no payload
	// — it is a doorbell. A reader that wakes reads the log from its own cursor,
	// so a slow reader coalesces its doorbells instead of losing events, and the
	// turn never blocks on a browser.
	subs map[*turnWatcher]struct{}
}

// turnWatcher is one attached reader's doorbell.
type turnWatcher struct {
	notify chan struct{}
}

func newLiveTurn(sessionID string, cancel context.CancelFunc) *liveTurn {
	return &liveTurn{
		sessionID: sessionID,
		startedAt: time.Now(),
		cancel:    cancel,
		subs:      make(map[*turnWatcher]struct{}),
	}
}

// emit records one event and rings every attached reader's doorbell.
//
// It never blocks: the run's progress must not depend on how fast a browser
// reads, and a reader that cannot keep up is caught by its next cursor read
// rather than by stalling the model.
func (t *liveTurn) emit(e chat.Event) {
	t.mu.Lock()
	t.log = appendEvent(t.log, e)
	for w := range t.subs {
		ring(w)
	}
	t.mu.Unlock()
}

// finish marks the turn over and wakes everyone still attached, which is how
// their response body ends.
func (t *liveTurn) finish() {
	t.mu.Lock()
	t.done = true
	for w := range t.subs {
		ring(w)
	}
	t.mu.Unlock()
}

// stop ends the run behind this turn.
func (t *liveTurn) stop() {
	t.mu.Lock()
	t.stopRequested = true
	t.mu.Unlock()
	t.cancel()
}

// cancelRun ends the run without recording it as a reader's stop, which is what
// a server shutdown does.
func (t *liveTurn) cancelRun() {
	t.cancel()
}

// stopped reports whether this turn was stopped by a reader rather than by the
// model finishing, so the failure stored with the answer can say so.
func (t *liveTurn) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopRequested
}

// isDone reports whether the run has reported back and been persisted.
func (t *liveTurn) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// turnCursor is how far one reader has read.
//
// `index` counts the log entries whose delivery has started, and `offset` is how
// many bytes of the entry at index-1 that reader already has. The offset exists
// because a coalesced delta keeps growing in place: a reader that already has
// the first 40 KB of an answer must be handed the 41st, not the whole thing
// again.
type turnCursor struct {
	index  int
	offset int
}

// subscribe attaches a reader: it returns the doorbell, the log so far, and
// where that reader has read up to.
//
// The snapshot and the registration happen under one lock, so an event can
// neither be missed between them nor be delivered twice.
func (t *liveTurn) subscribe() (*turnWatcher, []chat.Event, turnCursor, bool) {
	w := &turnWatcher{notify: make(chan struct{}, 1)}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subs[w] = struct{}{}
	events, cur := readFrom(t.log, turnCursor{})
	return w, events, cur, t.done
}

func (t *liveTurn) unsubscribe(w *turnWatcher) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subs, w)
}

// since returns the events a reader has not seen yet, the cursor to carry into
// the next call, and whether the turn is over.
func (t *liveTurn) since(cur turnCursor) ([]chat.Event, turnCursor, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	events, next := readFrom(t.log, cur)
	return events, next, t.done
}

// readFrom returns everything after a cursor, plus the cursor for the result.
//
// A delta that has grown since the reader last looked is delivered as its new
// tail alone, because that is what the client's reducer appends; everything the
// reader has not started yet is delivered whole.
func readFrom(log []chat.Event, cur turnCursor) ([]chat.Event, turnCursor) {
	if cur.index > len(log) {
		cur.index, cur.offset = len(log), 0
	}
	out := make([]chat.Event, 0, len(log)-cur.index+1)

	if cur.index > 0 {
		last := log[cur.index-1]
		if isDelta(last.Type) && len(last.Text) > cur.offset {
			out = append(out, chat.Event{
				Type: last.Type, Step: last.Step, Text: last.Text[cur.offset:],
			})
			cur.offset = len(last.Text)
		}
	}
	for cur.index < len(log) {
		e := log[cur.index]
		out = append(out, e)
		cur.index++
		cur.offset = len(e.Text)
	}
	return out, cur
}

func isDelta(t chat.EventType) bool {
	return t == chat.EventTextDelta || t == chat.EventReasoningDelta
}

func ring(w *turnWatcher) {
	select {
	case w.notify <- struct{}{}:
	default: // already ringing; the reader will read past this one anyway
	}
}

// appendEvent adds one event to the log, merging it into the previous entry
// when both are the same kind of delta.
//
// Coalescing is what keeps a replay cheap: a long answer arrives as thousands
// of deltas, and a reader attaching halfway through should not be handed all of
// them. Merged, the log holds one text delta carrying everything said so far,
// which the client's reducer appends exactly as it would append the pieces.
func appendEvent(log []chat.Event, e chat.Event) []chat.Event {
	if e.Type != chat.EventTextDelta && e.Type != chat.EventReasoningDelta {
		return append(log, e)
	}
	if n := len(log); n > 0 && log[n-1].Type == e.Type {
		log[n-1].Text += e.Text
		return log
	}
	return append(log, e)
}

// sessionView is one conversation as the console reads it: the stored row plus
// the one fact that is not in the database — whether a turn is running for it
// right now.
//
// The flag comes from the live turn hub, so the sidebar can mark a conversation
// that is being answered while the reader looks at another one, and a reloaded
// page knows which conversation to attach to.
type sessionView struct {
	store.ChatSession
	Streaming bool `json:"streaming"`
}

// withStreaming decorates stored rows with their live state.
func (s *Server) withStreaming(sessions []store.ChatSession) []sessionView {
	out := make([]sessionView, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionView{ChatSession: sess, Streaming: s.turns.running(sess.ID)})
	}
	return out
}

// turnRun is everything one turn needs in order to run, resolved by the request
// that starts it and used after that request is gone.
type turnRun struct {
	session   store.ChatSession
	workspace string
	runner    *chat.Runner
	history   []*schema.Message
	scope     string

	// newTask marks a turn started by a *new request from the user*, as opposed
	// to one that continues an interrupted turn (see handleResumeTurn). A new
	// request is a new piece of work, so a plan that is already finished belongs
	// to the previous one and is cleared before this turn runs: leaving it up
	// would put a checklist of completed chores above a composer that is about to
	// be asked something else.
	newTask bool

	// The turn's budget, resolved once when the message was accepted: 设置 →
	// 对话预算 may have changed it since the runner was built, and reading it
	// once is what keeps a change made mid-turn from moving this turn's ceiling.
	maxSteps  int
	maxTokens int
	deadline  time.Duration
}

// startTurn puts a turn in flight, off the request that asked for it.
//
// It returns nil when the conversation already has one, which the caller
// reports as a conflict rather than silently queueing: the second message would
// otherwise wait behind an answer the reader has not seen yet.
func (s *Server) startTurn(run turnRun) *liveTurn {
	runCtx, cancel := context.WithCancel(context.Background())
	t := newLiveTurn(run.session.ID, cancel)
	if !s.turns.begin(t) {
		cancel()
		return nil
	}
	s.turns.runs.Add(1)

	// One accumulator, two consumers: the log the readers replay, and the
	// summary persisted when the run ends.
	acc := &turnAccumulator{}
	emit := func(e chat.Event) {
		acc.add(e)
		t.emit(e)
	}

	// The asker is per turn, not per connection: the card has to reach whoever
	// is attached when the question is asked, and the answer comes back on a
	// request of its own.
	runCtx = tool.WithAsker(runCtx, &turnAsker{
		hub:     s.questions,
		session: run.session.ID,
		timeout: s.askTimeout(),
		logger:  s.logger,
		emit:    emit,
	})

	// The approver is per turn for the same reason, and so is the set of tools
	// this turn has already been allowed for: "allow for this turn" has to end
	// when the turn does, which the context does for us.
	runCtx = tool.WithApprover(runCtx, &turnApprover{
		hub:     s.approvals,
		session: run.session.ID,
		timeout: s.approvalTimeout(),
		logger:  s.logger,
		emit:    emit,
	})
	runCtx = tool.WithTurnAllowances(runCtx)

	// The plan store is per turn for the same reason, and it is installed here
	// rather than resolved by the tools because only the surface knows where a
	// plan lives (here: the database, so it survives the turn) and where it is
	// shown (here: this turn's event log, so the board moves as the work does).
	if s.chat.PlanEnable {
		// A new request starts a new task: drop the previous one's plan when it
		// has nothing left in it. Done before the planner is built, so the turn
		// that follows reads the cleared state rather than a plan it is about to
		// have taken away.
		if run.newTask {
			s.clearFinishedPlan(runCtx, run.session.ID, emit)
		}
		runCtx = tool.WithPlanner(runCtx, newTurnPlanner(
			s.store, run.session.ID, s.chat.PlanMaxTasks, emit, s.logger))
	}

	// The checkpoint for this turn is opened before the tools run and closed after
	// the turn ends, so every pre-image captured during it is filed under one turn
	// number and git's view is recorded on both sides.
	runCtx, endCheckpoint := s.beginCheckpointTurn(runCtx, run.session)

	go func() {
		defer s.turns.runs.Done()
		defer s.turns.retire(t)
		defer cancel()
		defer endCheckpoint()

		res, runErr := run.runner.Run(runCtx, chat.Request{
			Messages:  run.history,
			SessionID: run.session.ID,
			UserID:    run.session.UserID,
			// The scope is what the per-turn tool set is derived from, so this
			// conversation's tools resolve inside its own workspace even while
			// another conversation runs against a different one.
			Scope:     run.scope,
			MaxSteps:  run.maxSteps,
			MaxTokens: run.maxTokens,
			Deadline:  run.deadline,
		}, emit)

		failure := ""
		traceID := ""
		if res != nil {
			traceID = res.TraceID
		}
		if runErr != nil {
			// The trace id survives the failure: a failed turn is exactly when
			// someone wants to open the trace, and the store already holds it.
			failure = runErr.Error()
			res = &chat.Result{TraceID: traceID}
		} else if acc.errText != "" {
			// A runner that reported the failure on the stream rather than
			// through its return value: the message still has to carry it.
			failure = acc.errText
		}

		// A turn a reader stopped is recorded as stopped whatever the runner
		// made of the cancellation.
		//
		// Cancelling a model call can look like an ordinary end to the runner —
		// the response simply stops arriving — and an answer that is short
		// because someone asked it to be must not be stored as though the model
		// had finished.
		if t.stopped() {
			failure = turnStoppedText
		}
		if failure != "" {
			emit(chat.Event{Type: chat.EventError, Error: failure})
		}

		// Persist before the readers are told the turn is over: the console
		// reloads the conversation the moment the stream ends, and the row it
		// reads back has to be there.
		s.persistTurn(context.Background(), run.session, run.workspace, acc.summary(res), failure, traceID)

		t.finish()
	}()

	return t
}

// turnAccumulator folds a turn's events into the message that will be stored.
//
// It is addressed by step, not just appended to: the console renders the turn as
// a sequence of steps, so the reasoning and the tool calls of one iteration have
// to stay together from the moment they arrive.
type turnAccumulator struct {
	// mu serialises the folds. Parallel-safe tool calls emit their events from
	// their own goroutines, so "the run loop is the only writer" stopped being
	// true the moment tools could overlap — and a lost tool row here is a step the
	// console shows as never having run.
	mu         sync.Mutex
	answer     []byte
	reasoning  []byte
	tools      []chat.ToolRun
	steps      []*turnStep
	step       int
	usage      chat.Usage
	stopReason string
	errText    string
}

// add folds one event in, the same way for every consumer.
func (a *turnAccumulator) add(e chat.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// A nested event is a subagent's, not the turn's. It is recorded under the call
	// that spawned it and **nowhere else**:
	//
	//   - its text is not the answer. The reason to spawn a subagent is that its
	//     working-out stays out of the parent's context; a console that appended it
	//     to the answer would show the reader something the model never saw, and
	//     the stored turn would not be what the model produced.
	//   - its tool calls are not the turn's steps. They belong to the subagent's own
	//     little loop, and putting them in the parent's step list would make a plan
	//     look like it did work it delegated.
	if e.ParentToolCallID != "" {
		a.recordNested(e)
		return
	}

	switch e.Type {
	case chat.EventStepStart:
		a.openStep(e.Step)

	case chat.EventTextDelta:
		step := a.openStep(e.Step)
		step.text = append(step.text, e.Text...)
		a.answer = append(a.answer, e.Text...)

	case chat.EventReasoningDelta:
		step := a.openStep(e.Step)
		step.reasoning = append(step.reasoning, e.Text...)
		a.reasoning = append(a.reasoning, e.Text...)

	case chat.EventToolCall:
		run := chat.ToolRun{ID: e.ToolCallID, Name: e.ToolName, Args: e.ToolArgs, Step: e.Step}
		a.tools = append(a.tools, run)
		a.openStep(e.Step).calls = append(a.openStep(e.Step).calls, run)

	case chat.EventToolResult:
		for i := range a.tools {
			if a.tools[i].ID == e.ToolCallID {
				a.tools[i].Result = e.ToolResult
				a.tools[i].Err = e.ToolError
				a.tools[i].DurationMs = e.DurationMs
				break
			}
		}
		for i := range a.steps {
			for j := range a.steps[i].calls {
				if a.steps[i].calls[j].ID != e.ToolCallID {
					continue
				}
				a.steps[i].calls[j].Result = e.ToolResult
				a.steps[i].calls[j].Err = e.ToolError
				a.steps[i].calls[j].DurationMs = e.DurationMs
			}
		}

	case chat.EventUsage:
		if e.Usage != nil {
			a.usage = *e.Usage
		}

	case chat.EventStepRetry:
		// This step's model call failed and is about to run again from the same
		// history, so everything the failed attempt contributed is dropped. Left
		// in, the stored message would carry the half-answer of the attempt that
		// died followed by the answer of the one that succeeded — one answer
		// saying the same thing twice, which is exactly what a reader would blame
		// the model for.
		a.retryStep(e.Step)

	case chat.EventBudgetStop:
		a.stopReason = e.Reason

	case chat.EventDone:
		if e.Text != "" {
			a.answer = append(a.answer[:0], e.Text...)
		}

	case chat.EventError:
		a.errText = e.Error
	}
}

// openStep returns the step an event belongs to, opening it when it is new.
//
// An event that names no step — an older emitter, a delta that arrived without
// its step_start — joins the newest step rather than opening one ahead of it: a
// client reading a persisted turn must see the order the work actually happened
// in, and a phantom step would invent an iteration nobody ran.
func (a *turnAccumulator) openStep(n int) *turnStep {
	if len(a.steps) == 0 {
		a.steps = append(a.steps, a.newStep(max(n, 1)))
		return a.steps[0]
	}
	if n > a.step {
		a.steps = append(a.steps, a.newStep(n))
	}
	return a.steps[len(a.steps)-1]
}

// newStep opens a step, remembering how much of the turn's text and reasoning
// existed before it. Those marks are what makes a step retry able to undo exactly
// its own output: see retryStep.
func (a *turnAccumulator) newStep(index int) *turnStep {
	a.step = index
	return &turnStep{
		index:         index,
		answerMark:    len(a.answer),
		reasoningMark: len(a.reasoning),
	}
}

// retryStep discards what one step produced before its retry.
//
// Only the step's own text and reasoning are dropped, plus the tail those added
// to the turn's totals. Earlier steps are left alone: their thoughts really
// happened, and the model they were sent to is the same one that is about to
// answer again. Tool calls never need undoing — a step whose model call failed
// never reached the point of running one.
func (a *turnAccumulator) retryStep(n int) {
	var step *turnStep
	switch {
	case len(a.steps) == 0:
		return
	case n > 0 && n < a.step:
		// A retry for a step that is not the newest one. Nothing to drop: the
		// stream has already moved past it, and rewriting history here would
		// contradict what the readers were shown.
		return
	default:
		step = a.steps[len(a.steps)-1]
	}
	step.text = step.text[:0]
	step.reasoning = step.reasoning[:0]
	a.answer = truncateTo(a.answer, step.answerMark)
	a.reasoning = truncateTo(a.reasoning, step.reasoningMark)
}

// truncateTo shortens b to n bytes, tolerating an n that is past its end (which
// a retry event for a step that was never opened can produce).
func truncateTo(b []byte, n int) []byte {
	if n >= len(b) {
		return b
	}
	if n <= 0 {
		return b[:0]
	}
	return b[:n]
}

// turnStep is one iteration while it is being accumulated.
type turnStep struct {
	index     int
	reasoning []byte
	text      []byte
	calls     []chat.ToolRun
	// answerMark and reasoningMark are how long the turn's text and reasoning
	// buffers were when this step opened, so a retry of this step can roll them
	// back to exactly where it started (see retryStep).
	answerMark    int
	reasoningMark int
}

// built freezes the accumulated step into what gets stored.
func (s *turnStep) built() chat.Step {
	return chat.Step{
		Index:     s.index,
		Reasoning: string(s.reasoning),
		Text:      string(s.text),
		Tools:     s.calls,
	}
}

// turnSummary is everything one finished turn writes down.
type turnSummary struct {
	answer     string
	reasoning  string
	tools      []chat.ToolRun
	plan       []chat.Step
	usage      chat.Usage
	stopReason string
}

// summary folds the runner's own result over the accumulated events.
//
// The result is authoritative where it has something to say — it is the whole
// answer rather than the pieces that were streamed, the ReAct loop may have
// produced a preamble before calling a tool, and its plan is built by the loop
// itself — and the accumulated events are what a turn that ended badly has
// instead. Keeping the fallback matters: a turn that was stopped or that failed
// on its last model call still ran real steps, and those are exactly the turns
// whose process a reader wants to see.
func (a *turnAccumulator) summary(res *chat.Result) turnSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := turnSummary{
		answer:     string(a.answer),
		reasoning:  string(a.reasoning),
		tools:      a.tools,
		plan:       a.plan(),
		usage:      a.usage,
		stopReason: a.stopReason,
	}
	if res == nil {
		return out
	}
	if res.Text != "" {
		out.answer = res.Text
	}
	if res.Reasoning != "" {
		out.reasoning = res.Reasoning
	}
	if len(res.Tools) > 0 {
		out.tools = res.Tools
	}
	if len(res.Plan) > 0 {
		out.plan = res.Plan
	}
	if res.Usage.TotalTokens > 0 {
		out.usage = res.Usage
	}
	if res.StopReason != "" {
		out.stopReason = res.StopReason
	}
	return out
}

// plan freezes the accumulated steps, or nil when nothing was recorded.
func (a *turnAccumulator) plan() []chat.Step {
	if len(a.steps) == 0 {
		return nil
	}
	out := make([]chat.Step, 0, len(a.steps))
	for _, step := range a.steps {
		out = append(out, step.built())
	}
	return out
}

// recordNested files an event from a subagent under the call that spawned it.
//
// It is best-effort by design: a nested event whose parent call is unknown (a
// subagent outliving its card, a reload in the middle) is dropped rather than
// invented into the wrong place.
func (a *turnAccumulator) recordNested(e chat.Event) {
	switch e.Type {
	case chat.EventTextDelta:
		a.appendNested(e.ParentToolCallID, "text", e.Text, "")
	case chat.EventReasoningDelta:
		a.appendNested(e.ParentToolCallID, "reasoning", e.Text, "")
	case chat.EventToolCall:
		a.appendNested(e.ParentToolCallID, "tool", "", e.ToolCallID)
		a.nameNestedTool(e.ParentToolCallID, e.ToolCallID, e.ToolName)
	case chat.EventToolResult:
		a.closeNestedTool(e.ParentToolCallID, e.ToolCallID, e.ToolResult, e.ToolError)
	}
}

// appendNested adds or extends a nested entry on the parent tool run.
func (a *turnAccumulator) appendNested(parentID, kind, text, toolID string) {
	for i := range a.tools {
		if a.tools[i].ID != parentID {
			continue
		}
		// Consecutive deltas of the same kind extend one entry, so a subagent's
		// paragraph is one block rather than one entry per token.
		if n := len(a.tools[i].Nested); n > 0 && toolID == "" {
			last := &a.tools[i].Nested[n-1]
			if last.Kind == kind {
				last.Text += text
				return
			}
		}
		a.tools[i].Nested = append(a.tools[i].Nested, chat.NestedCall{Kind: kind, Text: text, ID: toolID})
		return
	}
}

// nameNestedTool records the name of a nested tool call.
func (a *turnAccumulator) nameNestedTool(parentID, toolID, name string) {
	for i := range a.tools {
		if a.tools[i].ID != parentID {
			continue
		}
		for j := range a.tools[i].Nested {
			if a.tools[i].Nested[j].ID == toolID {
				a.tools[i].Nested[j].Name = name
				return
			}
		}
	}
}

// closeNestedTool fills in a nested tool call's result.
func (a *turnAccumulator) closeNestedTool(parentID, toolID, result, errText string) {
	for i := range a.tools {
		if a.tools[i].ID != parentID {
			continue
		}
		for j := range a.tools[i].Nested {
			if a.tools[i].Nested[j].ID == toolID {
				a.tools[i].Nested[j].Result = result
				a.tools[i].Nested[j].Err = errText
				return
			}
		}
	}
}
