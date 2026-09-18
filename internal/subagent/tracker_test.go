package subagent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// The run list a header reads. Its job is to answer two different questions — "what
// is running now" and "what did it delegate" — and each of those shapes a decision
// below: running entries are never dropped, finished ones are kept but bounded, and
// a duration is live for one and fixed for the other.

func TestTrackerRecordsARunFromStartToFinish(t *testing.T) {
	tr := NewTracker(0)
	run := tr.Start("sess-1", "auth-survey", "调研一下鉴权怎么做的，需要读很多文件", "call-1")
	if run.Status != StatusRunning {
		t.Fatalf("status = %q, want running", run.Status)
	}
	if run.Session != "sess-1" || run.ParentToolCallID != "call-1" {
		t.Errorf("run = %+v", run)
	}
	if tr.Running("sess-1") != 1 || tr.RunningAll() != 1 {
		t.Errorf("running = %d / %d, want 1 / 1", tr.Running("sess-1"), tr.RunningAll())
	}

	tr.Finish(run.ID, Finish{Steps: 7, Tokens: 4321})
	runs := tr.List("sess-1")
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	got := runs[0]
	if got.Status != StatusOK || got.Steps != 7 || got.Tokens != 4321 {
		t.Errorf("finished run = %+v", got)
	}
	if got.EndedAt.IsZero() || got.DurationMs < 0 {
		t.Errorf("a finished run should carry its end: %+v", got)
	}
	if tr.Running("sess-1") != 0 {
		t.Error("a finished run still counts as running")
	}
	// It stays in the list: the header answers "what did it delegate" too.
	if tr.List("sess-1")[0].ID != run.ID {
		t.Error("a finished run was dropped from the list")
	}
}

// TestTrackerRunningDurationIsLive: a running entry reports how long it has been
// going, so the client does not have to know when it started.
func TestTrackerRunningDurationIsLive(t *testing.T) {
	tr := NewTracker(0)
	run := tr.Start("s", "slow", "x", "")
	time.Sleep(15 * time.Millisecond)
	got := tr.List("s")[0]
	if got.DurationMs < 10 {
		t.Errorf("a running run reported %d ms; it should report elapsed time", got.DurationMs)
	}
	if !got.EndedAt.IsZero() {
		t.Error("a running run should not have an end time")
	}
	// The tracker's own copy is not what the caller holds: mutating the returned
	// value must not change the record.
	_ = run
	got.Status = StatusFailed
	if tr.List("s")[0].Status != StatusRunning {
		t.Error("List returned a value the caller can use to mutate the record")
	}
}

// TestTrackerNeverDropsARunningRun: a header that lost the entry for a subagent
// that is still working would be worse than a long list.
func TestTrackerNeverDropsARunningRun(t *testing.T) {
	tr := NewTracker(3)

	// Three long runs that never finish, then ten finished ones.
	var live []Run
	for i := 0; i < 3; i++ {
		live = append(live, tr.Start("s", "live", "still working", ""))
	}
	for i := 0; i < 10; i++ {
		r := tr.Start("s", "done", "finished", "")
		tr.Finish(r.ID, Finish{Steps: 1})
	}

	runs := tr.List("s")
	for _, l := range live {
		found := false
		for _, r := range runs {
			if r.ID == l.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("a running run was trimmed away (keep=3, 3 live + 10 finished)")
		}
	}
	if tr.Running("s") != 3 {
		t.Errorf("running = %d, want 3", tr.Running("s"))
	}
}

// TestTrackerKeepsFinishedRunsBounded: the list is a record, not a leak.
func TestTrackerKeepsFinishedRunsBounded(t *testing.T) {
	tr := NewTracker(5)
	for i := 0; i < 40; i++ {
		r := tr.Start("s", "x", "y", "")
		tr.Finish(r.ID, Finish{Steps: 1})
	}
	runs := tr.List("s")
	if len(runs) > 5 {
		t.Errorf("kept %d finished runs with keep=5", len(runs))
	}
	if len(runs) == 0 {
		t.Error("everything was trimmed; the newest should survive")
	}
}

// TestTrackerIsPerSession: one conversation's chip must not show another's work.
func TestTrackerIsPerSession(t *testing.T) {
	tr := NewTracker(0)
	tr.Start("a", "one", "x", "")
	tr.Start("b", "two", "y", "")
	if got := len(tr.List("a")); got != 1 {
		t.Errorf("session a has %d runs, want 1", got)
	}
	if got := tr.List("a")[0].Name; got != "one" {
		t.Errorf("session a shows %q", got)
	}
	if tr.Running("a") != 1 || tr.Running("b") != 1 {
		t.Errorf("running = %d / %d", tr.Running("a"), tr.Running("b"))
	}
	if tr.RunningAll() != 2 {
		t.Errorf("RunningAll = %d, want 2 (the gate is process-wide)", tr.RunningAll())
	}
}

// TestTrackerConcurrentUse: parallel subagents finish from their own goroutines
// while the console reads the list.
func TestTrackerConcurrentUse(t *testing.T) {
	tr := NewTracker(50)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := tr.Start("s", "x", "y", "")
			tr.Finish(r.ID, Finish{Steps: 1, Tokens: 10})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.List("s")
			_ = tr.Running("s")
			_ = tr.RunningAll()
		}()
	}
	wg.Wait()
	if tr.Running("s") != 0 {
		t.Errorf("running = %d after every run finished", tr.Running("s"))
	}
}

// TestTrackerPreviewsALongTask: the header shows a task, and it must be readable
// rather than a wall of text.
func TestTrackerPreviewsALongTask(t *testing.T) {
	tr := NewTracker(0)
	long := strings.Repeat("很长的任务描述 ", 100)
	run := tr.Start("s", "x", long, "")
	if len([]rune(run.Prompt)) > promptPreview+1 {
		t.Errorf("the prompt preview is %d runes", len([]rune(run.Prompt)))
	}
	if !strings.HasSuffix(run.Prompt, "…") {
		t.Errorf("a truncated preview should say so: %q", run.Prompt)
	}
	// Whitespace is collapsed so a multi-line brief stays one line in the header.
	multi := tr.Start("s", "y", "第一行\n第二行\t第三行", "")
	if strings.Contains(multi.Prompt, "\n") {
		t.Errorf("the preview kept a newline: %q", multi.Prompt)
	}
}

// TestNewTrackerWithNoKeepUsesTheDefault.
func TestNewTrackerWithNoKeepUsesTheDefault(t *testing.T) {
	tr := NewTracker(0)
	if tr.keep != DefaultTrackerKeep {
		t.Errorf("keep = %d, want %d", tr.keep, DefaultTrackerKeep)
	}
	// A nil tracker is usable: the surfaces that have no subagents still call these.
	var nilTracker *Tracker
	if nilTracker.List("s") != nil || nilTracker.Running("s") != 0 || nilTracker.RunningAll() != 0 {
		t.Error("a nil tracker should answer empty rather than panic")
	}
	nilTracker.Finish("x", Finish{})
}

// TestRunIDsAreUniqueUnderTheClock: two subagents spawned in the same microsecond
// must not share an id.
//
// This is not hypothetical tidiness. The first version of this file built ids from
// a timestamp alone, and a collision is not a cosmetic duplicate: the second run
// overwrote the first in the map, so a header showed one subagent where two had been
// delegated, and the earlier run vanished from the list. The package's own
// session-isolation test found it.
func TestRunIDsAreUniqueUnderTheClock(t *testing.T) {
	tr := NewTracker(100)
	seen := map[string]bool{}
	// No sleeps: the point is to spawn within one clock tick.
	for i := 0; i < 200; i++ {
		run := tr.Start("s", "busy", "x", "")
		if seen[run.ID] {
			t.Fatalf("duplicate run id %q at iteration %d", run.ID, i)
		}
		seen[run.ID] = true
	}
	if got := len(tr.List("s")); got != 200 {
		t.Errorf("List returned %d runs, want 200: an id collision dropped one", got)
	}
	if tr.RunningAll() != 200 {
		t.Errorf("RunningAll = %d, want 200", tr.RunningAll())
	}
}
