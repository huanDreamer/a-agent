package subagent

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// What a delegated subagent is doing, for the surfaces that show it.
//
// The reason this exists as its own object rather than being derived from the
// turn's events: a subagent outlives a step, several may be in flight at once, and
// the console wants to answer "which ones did I delegate, and what are they doing"
// the same way it answers it for background processes — from a list, not from
// reconstructing a stream.
//
// It is deliberately a record of *runs*, not a log: a run that finished stays in
// the list (bounded, newest first) so the header keeps showing what happened after
// the work is over. A list that emptied itself the moment the last subagent
// returned would answer "is anything running" and nothing else.

// Status is how a run ended, or that it has not.
type Status string

const (
	// StatusRunning is a subagent that is still working.
	StatusRunning Status = "running"
	// StatusOK is one that finished and returned a report.
	StatusOK Status = "ok"
	// StatusFailed is one that did not complete. The parent carries on either way;
	// this is what the reader sees.
	StatusFailed Status = "failed"
)

// Run is one delegated subagent, as a surface shows it.
type Run struct {
	ID string `json:"id"`
	// Session is the conversation that asked for it, so a header can show its own.
	Session string `json:"session,omitempty"`
	// Name is the short display name: the model's if it gave one, otherwise derived
	// from the task.
	Name string `json:"name"`
	// Prompt is the task, truncated for display. The full brief is in the trace.
	Prompt string `json:"prompt,omitempty"`
	// ParentToolCallID is the call that spawned it, which is how the console ties a
	// run in the header to the card in the conversation.
	ParentToolCallID string `json:"parent_tool_call_id,omitempty"`
	// Status is running until the run ends.
	Status Status `json:"status"`
	// StartedAt and EndedAt bound the run; EndedAt is zero while it runs.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// DurationMs is what the header shows for a finished run, and how long it has
	// been going for a running one.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Steps and Tokens describe a finished run. Steps is -1 when the runner cannot
	// count them (Eino's ReAct loop returns only the final message) — a header that
	// showed "0 steps" for a minute of work would be lying.
	Steps  int `json:"steps"`
	Tokens int `json:"tokens,omitempty"`
	// Error says why a failed run failed.
	Error string `json:"error,omitempty"`
	// StopReason is set when the run ended on its own budget rather than finishing.
	StopReason string `json:"stop_reason,omitempty"`
}

// Tracker records the runs a process has delegated.
//
// It is safe for concurrent use, which it has to be: parallel subagents finish from
// their own goroutines while the console reads the list.
type Tracker struct {
	mu    sync.Mutex
	keep  int
	byID  map[string]*Run
	order map[string][]string
}

// DefaultTrackerKeep is how many finished runs a session keeps.
//
// Enough to answer "what did it delegate while I was reading" after the fact, and
// few enough that a long session's header does not become a scroll.
const DefaultTrackerKeep = 20

// NewTracker builds a tracker. A keep of 0 uses the default.
func NewTracker(keep int) *Tracker {
	if keep <= 0 {
		keep = DefaultTrackerKeep
	}
	return &Tracker{
		keep:  keep,
		byID:  map[string]*Run{},
		order: map[string][]string{},
	}
}

// promptPreview bounds the task text a surface shows.
const promptPreview = 160

// Start records a run that is about to begin and returns it.
//
// The returned value is a copy: a caller that kept the pointer could mutate a
// record the console is reading.
func (t *Tracker) Start(session, name, prompt, parentCallID string) Run {
	if t == nil {
		return Run{}
	}
	run := Run{
		ID:               newRunID(),
		Session:          session,
		Name:             name,
		Prompt:           preview(prompt),
		ParentToolCallID: parentCallID,
		Status:           StatusRunning,
		StartedAt:        time.Now(),
		// Unknown until a runner says otherwise: this loop's default is "cannot
		// count", and a runner that counts overwrites it.
		Steps: -1,
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	stored := run
	t.byID[run.ID] = &stored
	t.order[session] = append([]string{run.ID}, t.order[session]...)
	// Trim the oldest finished runs, never a running one: a header that dropped a
	// subagent that is still working would be worse than a long list.
	t.trimLocked(session)
	return run
}

// Finish records how a run ended.
func (t *Tracker) Finish(id string, res Finish) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	run, ok := t.byID[id]
	if !ok {
		return
	}
	run.Status = StatusOK
	if res.Failed {
		run.Status = StatusFailed
	}
	// A caller that reports no step count means "unknown", not "zero": keeping the
	// marker is what stops a zero from being invented on the way to the header.
	if res.Steps != 0 {
		run.Steps = res.Steps
	} else if res.Failed {
		run.Steps = StepsUnknown
	}
	run.EndedAt = time.Now()
	run.DurationMs = run.EndedAt.Sub(run.StartedAt).Milliseconds()
	run.Tokens = res.Tokens
	run.StopReason = res.StopReason
	run.Error = res.Error
}

// Finish describes the end of a run.
type Finish struct {
	Failed     bool
	Steps      int
	Tokens     int
	StopReason string
	Error      string
}

// List returns a session's runs, newest first.
func (t *Tracker) List(session string) []Run {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := t.order[session]
	out := make([]Run, 0, len(ids))
	for _, id := range ids {
		run, ok := t.byID[id]
		if !ok {
			continue
		}
		copied := *run
		// A running entry reports how long it has been going, so a header can show
		// a live duration without the client having to know when it started.
		if copied.Status == StatusRunning {
			copied.DurationMs = time.Since(copied.StartedAt).Milliseconds()
		}
		out = append(out, copied)
	}
	return out
}

// Running counts a session's live runs.
func (t *Tracker) Running(session string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, id := range t.order[session] {
		if run, ok := t.byID[id]; ok && run.Status == StatusRunning {
			n++
		}
	}
	return n
}

// RunningAll counts live runs across every session, for the process-wide gate.
func (t *Tracker) RunningAll() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, run := range t.byID {
		if run.Status == StatusRunning {
			n++
		}
	}
	return n
}

// trimLocked drops the oldest finished runs beyond the keep limit.
func (t *Tracker) trimLocked(session string) {
	ids := t.order[session]
	if len(ids) <= t.keep {
		return
	}
	kept := make([]string, 0, len(ids))
	over := len(ids) - t.keep
	for _, id := range ids {
		run, ok := t.byID[id]
		if !ok {
			continue
		}
		// Running runs are never dropped, and never count against the limit: the
		// list's job is to show what is happening now.
		if over > 0 && run.Status != StatusRunning {
			over--
			delete(t.byID, id)
			continue
		}
		kept = append(kept, id)
	}
	t.order[session] = kept
}

// preview truncates a task for display, on a rune boundary.
func preview(prompt string) string {
	prompt = strings.Join(strings.Fields(prompt), " ")
	runes := []rune(prompt)
	if len(runes) <= promptPreview {
		return prompt
	}
	return string(runes[:promptPreview]) + "…"
}

// runSeq makes run ids unique within a process.
//
// A counter **and** a timestamp, because neither alone is enough: two subagents
// spawned in the same microsecond get the same timestamp, and a plain counter loses
// its meaning across a restart (the id appears in logs and in the API, so it should
// be readable on its own). The collision is not theoretical — it was found by this
// package's own test, and a collided id is not just a cosmetic duplicate: the second
// run overwrites the first in the map, so the header shows one subagent where two
// were delegated.
var runSeq atomic.Uint64

// newRunID is a short, sortable id for one run.
func newRunID() string {
	return fmt.Sprintf("sa-%s-%03d",
		time.Now().UTC().Format("20060102T150405.000000"), runSeq.Add(1)%1000)
}
