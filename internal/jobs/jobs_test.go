package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// These tests start real processes. Nothing is faked, because the properties
// that matter here — that a job survives the call that started it, that a shell
// which forks and exits still counts as running, that SIGTERM is followed by
// SIGKILL, that output is bounded with the loss reported — only exist in the
// interaction with the operating system.

// testManager returns a manager rooted in a temporary directory, closed at the
// end of the test so a failing test cannot leak the processes it started.
func testManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	opts.Logger = zap.NewNop()
	if opts.StartGrace == 0 {
		opts.StartGrace = 50 * time.Millisecond
	}
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// testSpec is a shell command started in the process's own directory.
func testSpec(command string) Spec {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/"
	}
	return Spec{Command: command, Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: cwd, RequestedBy: "test"}
}

// mustStart starts a job and fails the test if it could not be started.
func mustStart(t *testing.T, m *Manager, command string) *Job {
	t.Helper()
	job, err := m.Start(context.Background(), testSpec(command))
	if err != nil {
		t.Fatalf("Start(%q): %v", command, err)
	}
	return job
}

// waitFor polls until cond holds, so a test does not depend on how fast a shell
// reaches a state — only on it getting there.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestManager_StartReportsAnImmediateFailure is the busy-port case: a command
// that dies at once must be reported in the start result, with its exit code,
// rather than stored as a job the caller has to poll to understand.
func TestManager_StartReportsAnImmediateFailure(t *testing.T) {
	m := testManager(t, Options{StartGrace: 2 * time.Second})

	job, err := m.Start(context.Background(), testSpec("echo oops >&2; exit 7"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Status != StatusExited {
		t.Fatalf("status = %q, want %q", job.Status, StatusExited)
	}
	if job.ExitCode == nil || *job.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", job.ExitCode)
	}
	if job.EndedAt == nil {
		t.Error("a finished job has no end time")
	}
	read, err := m.Output(context.Background(), job.ID, ReadOptions{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !strings.Contains(read.Data, "oops") {
		t.Errorf("output %q does not contain the message the command printed", read.Data)
	}
}

// TestManager_JobOutlivesTheCall is the whole point of the package: the process
// must still be running after Start returns, and must still be there when a
// later read comes back for its output.
func TestManager_JobOutlivesTheCall(t *testing.T) {
	m := testManager(t, Options{})

	job := mustStart(t, m, "echo started; sleep 30")
	if job.Status != StatusRunning {
		t.Fatalf("status = %q, want %q", job.Status, StatusRunning)
	}
	if job.PID <= 0 {
		t.Fatalf("pid = %d, want a real process", job.PID)
	}
	if job.LogPath == "" {
		t.Fatal("a running job has no log path")
	}

	// The log file is written by the manager's copy of the output, so it exists
	// as soon as the process has printed anything.
	waitFor(t, "the log file to contain the job's first line", func() bool {
		b, err := os.ReadFile(job.LogPath)
		return err == nil && strings.Contains(string(b), "started")
	})
	waitFor(t, "the output window to contain the job's first line", func() bool {
		read, err := m.Output(context.Background(), job.ID, ReadOptions{})
		return err == nil && strings.Contains(read.Data, "started")
	})

	// Reading from the returned offset and getting nothing new is the normal
	// case for a quiet process: it must not invent output.
	read, err := m.Output(context.Background(), job.ID, ReadOptions{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	again, err := m.Output(context.Background(), job.ID, ReadOptions{From: read.Next})
	if err != nil {
		t.Fatalf("Output from offset: %v", err)
	}
	if again.Data != "" {
		t.Errorf("a read past the end returned %q, want nothing", again.Data)
	}

	if _, err := m.Stop(context.Background(), job.ID, syscall.SIGTERM, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestManager_RunningMeansTheGroupIsAlive covers the case a single status flag
// gets wrong: the shell exits immediately and leaves the interesting process
// behind. The job must still be reported as running, because that is the
// question the user is asking.
func TestManager_RunningMeansTheGroupIsAlive(t *testing.T) {
	m := testManager(t, Options{})

	job := mustStart(t, m, "sleep 30 &")
	// The shell exits within milliseconds; give it that moment so the assertion
	// below is about the state that matters (shell reaped, child still in the
	// group) rather than about a race with the fork.
	time.Sleep(200 * time.Millisecond)

	got, err := m.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status = %q: a forked child is still in the group", got.Status)
	}
	if groupIsGone(got.PID) {
		t.Fatal("the process group is empty but the job is reported as running")
	}

	stopped, err := m.Stop(context.Background(), job.ID, syscall.SIGTERM, time.Second)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Errorf("status = %q, want %q", stopped.Status, StatusStopped)
	}
	if !groupIsGone(job.PID) {
		t.Error("the process group survived the stop")
	}
}

// TestManager_StopEscalatesToKill covers a process that ignores SIGTERM: the
// grace must expire and SIGKILL must follow, with the result saying it was the
// caller who ended it.
func TestManager_StopEscalatesToKill(t *testing.T) {
	m := testManager(t, Options{StopGrace: 200 * time.Millisecond})

	job := mustStart(t, m, "trap '' TERM; sleep 30")
	stopped, err := m.Stop(context.Background(), job.ID, syscall.SIGTERM, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Errorf("status = %q, want %q — the job was killed at our request", stopped.Status, StatusStopped)
	}
	if stopped.ExitCode == nil || *stopped.ExitCode != -1 {
		t.Errorf("exit code = %v, want -1 for a process killed by a signal", stopped.ExitCode)
	}
	if !groupIsGone(job.PID) {
		t.Error("the process group survived SIGKILL")
	}
}

// TestManager_StopOnAFinishedJobIsAConflict checks the reported outcome rather
// than a silent success: "I stopped it" would be a lie for a job that stopped
// itself.
func TestManager_StopOnAFinishedJobIsAConflict(t *testing.T) {
	m := testManager(t, Options{StartGrace: 2 * time.Second})

	job := mustStart(t, m, "exit 0")
	if _, err := m.Stop(context.Background(), job.ID, syscall.SIGTERM, time.Second); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop on a finished job: err = %v, want ErrNotRunning", err)
	}
}

// TestManager_UnknownAndMissingIdsAreRefused checks the error contract callers
// branch on, including the empty id a model can send.
func TestManager_UnknownAndMissingIdsAreRefused(t *testing.T) {
	m := testManager(t, Options{})
	for _, id := range []string{"job-nope", ""} {
		if _, err := m.Get(id); !errors.Is(err, ErrUnknownJob) {
			t.Errorf("Get(%q): err = %v, want ErrUnknownJob", id, err)
		}
		if _, err := m.Output(context.Background(), id, ReadOptions{}); !errors.Is(err, ErrUnknownJob) {
			t.Errorf("Output(%q): err = %v, want ErrUnknownJob", id, err)
		}
		if _, err := m.Stop(context.Background(), id, syscall.SIGTERM, time.Second); !errors.Is(err, ErrUnknownJob) {
			t.Errorf("Stop(%q): err = %v, want ErrUnknownJob", id, err)
		}
		if _, err := m.Forget(id); !errors.Is(err, ErrUnknownJob) {
			t.Errorf("Forget(%q): err = %v, want ErrUnknownJob", id, err)
		}
	}
}

// TestManager_RefusesBadSpecsBeforeSpawning keeps the promise that a refused
// start has no side effect: nothing was spawned, so there is nothing to undo.
func TestManager_RefusesBadSpecsBeforeSpawning(t *testing.T) {
	m := testManager(t, Options{})
	bad := []Spec{
		{Command: "", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: "/"},
		{Command: "echo hi", Shell: "", Cwd: "/"},
		{Command: "echo hi", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: ""},
		{Command: "echo hi", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: "/definitely/not/here"},
		{Command: "echo hi", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: filepath.Join(t.TempDir(), "file.txt")},
	}
	for i, spec := range bad {
		if _, err := m.Start(context.Background(), spec); err == nil {
			t.Errorf("spec %d was accepted: %+v", i, spec)
		}
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("a refused start left %d job(s) behind", got)
	}
}

// TestManager_MaxJobsRefusesBeforeSpawning is the bound that keeps a model from
// starting a dozen servers.
func TestManager_MaxJobsRefusesBeforeSpawning(t *testing.T) {
	m := testManager(t, Options{MaxJobs: 1})

	mustStart(t, m, "sleep 30")
	_, err := m.Start(context.Background(), testSpec("sleep 30"))
	if err == nil {
		t.Fatal("a second job was accepted with max_jobs = 1")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q does not say why the start was refused", err)
	}
	if got := m.Running(); got != 1 {
		t.Errorf("running = %d, want 1", got)
	}
	if got := len(m.List()); got != 1 {
		t.Errorf("%d jobs exist, want 1: the refused start left a record", got)
	}
}

// TestManager_StartOnAClosedManager checks shutdown ordering: once Close has run
// nothing new may start, because nothing would be left to supervise it.
func TestManager_StartOnAClosedManager(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{Dir: dir, Logger: zap.NewNop(), StartGrace: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Close()
	m.Close() // idempotent: a deferred Close plus an explicit one must be safe

	if _, err := m.Start(context.Background(), testSpec("sleep 30")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close: err = %v, want ErrClosed", err)
	}
}

// TestManager_CloseStopsEveryJob is the guarantee that nothing outlives the
// agent, including the jobs a user forgot about.
func TestManager_CloseStopsEveryJob(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{Dir: dir, Logger: zap.NewNop(), StartGrace: 20 * time.Millisecond, StopGrace: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := mustStart(t, m, "sleep 30")
	second := mustStart(t, m, "sleep 30 &") // the forked-child case as well
	m.Close()

	for _, id := range []string{first.ID, second.ID} {
		job, gerr := m.Get(id)
		if gerr != nil {
			t.Fatalf("Get(%s): %v", id, gerr)
		}
		if !job.Status.Terminal() {
			t.Errorf("job %s is %q after Close, want a terminal state", id, job.Status)
		}
		if !groupIsGone(job.PID) {
			t.Errorf("job %s left a process group behind", id)
		}
	}
}

// TestManager_ForgetDeletesOnlyAFinishedJob checks the one destructive operation:
// it refuses a running job, and it takes the log with it when it does run.
func TestManager_ForgetDeletesOnlyAFinishedJob(t *testing.T) {
	m := testManager(t, Options{})

	running := mustStart(t, m, "sleep 30")
	if _, err := m.Forget(running.ID); !errors.Is(err, ErrStillRunning) {
		t.Fatalf("Forget on a running job: err = %v, want ErrStillRunning", err)
	}
	if _, err := m.Get(running.ID); err != nil {
		t.Fatalf("the refused Forget dropped the record anyway: %v", err)
	}

	finished := mustStart(t, m, "echo done")
	waitFor(t, "the job to finish", func() bool {
		job, gerr := m.Get(finished.ID)
		return gerr == nil && job.Status.Terminal()
	})
	job, err := m.Forget(finished.ID)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := os.Stat(job.LogPath); !os.IsNotExist(err) {
		t.Errorf("log file %s still exists after Forget (err = %v)", job.LogPath, err)
	}
	if _, err := m.Get(finished.ID); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("Get after Forget: err = %v, want ErrUnknownJob", err)
	}
}

// TestManager_LogFileIsCappedAndSaysSo covers the disk bound: the file stops at
// the cap and the file itself records why, so a human reading it later is not
// left wondering where the output went.
func TestManager_LogFileIsCappedAndSaysSo(t *testing.T) {
	m := testManager(t, Options{MaxLogBytes: 32, StartGrace: 2 * time.Second})

	job := mustStart(t, m, "printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'")
	waitFor(t, "the job to finish", func() bool {
		got, err := m.Get(job.ID)
		return err == nil && got.Status.Terminal()
	})

	got, err := m.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LogCapped {
		t.Fatalf("LogCapped = false for a job with a 32-byte cap that printed 80 bytes")
	}
	if got.LogBytes > 32 {
		t.Errorf("LogBytes = %d, want at most the cap of 32", got.LogBytes)
	}
	content, err := os.ReadFile(got.LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(content), "log capped") {
		t.Errorf("the log file does not say it was capped: %q", content)
	}
	// The window is the part that keeps working: the tail is still readable even
	// though the file stopped.
	read, err := m.Output(context.Background(), job.ID, ReadOptions{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(read.Data), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Errorf("the window lost the tail: %q", read.Data)
	}
}

// TestManager_OutputReportsWhatItCouldNotServe is the honesty property: a reader
// must never mistake a gap for a continuous stream.
func TestManager_OutputReportsWhatItCouldNotServe(t *testing.T) {
	m := testManager(t, Options{WindowBytes: 128})

	// 200 lines of "line-<i>-abcdefghij" is 3890 bytes: waiting for the whole
	// stream to have arrived keeps every offset assertion below exact instead of
	// racing a process that is still printing.
	job := mustStart(t, m, "i=0; while [ $i -lt 200 ]; do echo \"line-$i-abcdefghij\"; i=$((i+1)); done; sleep 30")
	const printed = 3890
	waitFor(t, "the job to finish printing", func() bool {
		got, err := m.Get(job.ID)
		return err == nil && got.TotalBytes >= printed
	})

	tail, err := m.Output(context.Background(), job.ID, ReadOptions{MaxBytes: 64})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if tail.DiscardedBytes == 0 {
		t.Error("a read of a window that discarded bytes reports discarding none")
	}
	if tail.From <= 0 {
		t.Errorf("tail read started at %d, want an offset past the discarded beginning", tail.From)
	}
	if len(tail.Data) > 64 {
		t.Errorf("read returned %d bytes for max_bytes 64", len(tail.Data))
	}

	// Asking for an offset the window no longer holds must be answered with
	// what can be served plus the number of bytes that were skipped.
	early, err := m.Output(context.Background(), job.ID, ReadOptions{From: 1, MaxBytes: 32})
	if err != nil {
		t.Fatalf("Output from an early offset: %v", err)
	}
	if early.SkippedBytes != early.From-1 {
		t.Errorf("SkippedBytes = %d, want %d: the bytes between the requested offset and the window start",
			early.SkippedBytes, early.From-1)
	}
	if want := tail.TotalBytes - 128; early.From != want {
		t.Errorf("read started at %d, want the window's oldest byte %d", early.From, want)
	}

	// Continuing from the returned offset is contiguous and contains the newest
	// output, which is what a log view depends on.
	next, err := m.Output(context.Background(), job.ID, ReadOptions{From: early.Next, MaxBytes: 4096})
	if err != nil {
		t.Fatalf("Output from next: %v", err)
	}
	if next.From != early.Next {
		t.Errorf("continuation started at %d, want %d", next.From, early.Next)
	}
	if next.Next < next.From {
		t.Errorf("Next = %d went backwards from %d", next.Next, next.From)
	}
}

// TestManager_OutputWaitsForNewOutput is what makes "start a server and read what
// it prints" one call instead of a poll loop.
func TestManager_OutputWaitsForNewOutput(t *testing.T) {
	m := testManager(t, Options{})

	job := mustStart(t, m, "sleep 0.3; echo ready")
	start := time.Now()
	read, err := m.Output(context.Background(), job.ID, ReadOptions{Wait: 3 * time.Second})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !strings.Contains(read.Data, "ready") {
		t.Fatalf("waiting read returned %q, want the line the job printed", read.Data)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the read took %s, longer than its own wait", elapsed)
	}
}

// TestManager_OutputWaitExpiresWithoutOutput checks the other half: a wait that
// nothing satisfies must return, not hang.
func TestManager_OutputWaitExpiresWithoutOutput(t *testing.T) {
	m := testManager(t, Options{})

	job := mustStart(t, m, "sleep 30")
	start := time.Now()
	read, err := m.Output(context.Background(), job.ID, ReadOptions{Wait: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if read.Data != "" {
		t.Errorf("read returned %q for a silent job", read.Data)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("the read returned after %s without waiting", elapsed)
	}
}

// TestManager_PrunesFinishedRecordsButNotTheirLogs checks both halves of the
// housekeeping rule: records are bounded, and the evidence is not deleted behind
// the operator's back.
func TestManager_PrunesFinishedRecordsButNotTheirLogs(t *testing.T) {
	m := testManager(t, Options{MaxFinished: 2, StartGrace: 2 * time.Second})

	first := mustStart(t, m, "echo one")
	for i := 0; i < 3; i++ {
		mustStart(t, m, "echo more")
	}
	if _, err := m.Get(first.ID); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("the oldest finished record was not pruned: %v", err)
	}
	if _, err := os.Stat(first.LogPath); err != nil {
		t.Errorf("pruning a record deleted its log file: %v", err)
	}
	if got := len(m.List()); got > 3 {
		t.Errorf("%d records kept with max_finished = 2", got)
	}
}

// TestManager_ListOrdersOldestFirst keeps the console's listing stable.
func TestManager_ListOrdersOldestFirst(t *testing.T) {
	m := testManager(t, Options{})
	a := mustStart(t, m, "sleep 30")
	b := mustStart(t, m, "sleep 30")

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("%d jobs listed, want 2", len(list))
	}
	if list[0].ID != a.ID || list[1].ID != b.ID {
		t.Errorf("list order = %s, %s; want %s, %s", list[0].ID, list[1].ID, a.ID, b.ID)
	}
}

// TestManager_IdsAreNotReusedAcrossManagers guards the reason ids are random: a
// conversation outlives the agent process, so an id from a previous run must
// never name a different job.
func TestManager_IdsAreNotReusedAcrossManagers(t *testing.T) {
	seen := map[string]bool{}
	for round := 0; round < 3; round++ {
		m := testManager(t, Options{})
		for i := 0; i < 5; i++ {
			job := mustStart(t, m, "sleep 30")
			if seen[job.ID] {
				t.Fatalf("id %s was issued twice across managers", job.ID)
			}
			if !strings.HasPrefix(job.ID, "job-") {
				t.Fatalf("id %q does not carry the documented prefix", job.ID)
			}
			seen[job.ID] = true
		}
		m.Close()
	}
}

// TestManager_WorkspaceIsRecorded checks the fields the console and the tools
// scope on.
func TestManager_WorkspaceIsRecorded(t *testing.T) {
	m := testManager(t, Options{})
	spec := testSpec("sleep 30")
	spec.Name = "dev server"
	spec.Workspace = "blog"
	spec.WorkspaceRoot = "/tmp/blog"

	job, err := m.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Name != "dev server" || job.Workspace != "blog" || job.WorkspaceRoot != "/tmp/blog" {
		t.Errorf("job = %+v, want the spec's label, workspace and root", job)
	}
	if job.RequestedBy != "test" {
		t.Errorf("requested_by = %q, want test", job.RequestedBy)
	}
}

// TestManager_OutputOpenWhenAProcessEscapesTheGroup covers the one case where a
// job is over while something is still writing: a process that left the group
// (its own setsid) and kept the output pipe. The job must be declared finished
// rather than reported as running forever, and the fact must be visible.
func TestManager_OutputOpenWhenAProcessEscapesTheGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		// macOS does not ship the setsid command (only the syscall), so the
		// state machine this exercises is covered by the white-box test below on
		// this platform.
		t.Skip("setsid is not installed")
	}
	m := testManager(t, Options{StartGrace: 2 * time.Second})

	// The escaped writer holds the pipe well past the drain grace, so the job's
	// group empties while its output is still open. It records its pid so the
	// test can remove it precisely instead of hunting for it by name.
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	job := mustStart(t, m, fmt.Sprintf("(setsid sh -c 'echo $$ > %s; sleep 6; echo late' &) ; exit 0", pidFile))

	waitFor(t, "the job to be declared finished despite the escaped writer", func() bool {
		got, err := m.Get(job.ID)
		return err == nil && got.Status.Terminal() && got.OutputOpen
	})
	got, err := m.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusExited {
		t.Errorf("status = %q, want %q", got.Status, StatusExited)
	}
	// The escaped process is deliberately outside this manager's control, so the
	// test removes it itself; leaving it would keep a stray sleep alive for six
	// seconds after the suite finishes.
	killProcessFromFile(t, pidFile)
}

// killProcessFromFile kills the process whose pid is written in path, and
// tolerates the file not being there yet or the process having exited.
func killProcessFromFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// TestProcess_DrainGraceFinalizesAJobWhoseOutputStaysOpen pins the transition
// the integration test above can only reach where setsid exists: a process that
// left the job's group and kept the output pipe must not keep the job looking
// running forever, and the result must say that output is still open.
//
// It builds the process state directly rather than pretending to spawn an escape
// artist, which is both slower and platform-dependent.
func TestProcess_DrainGraceFinalizesAJobWhoseOutputStaysOpen(t *testing.T) {
	previous := groupDrainGrace
	groupDrainGrace = 50 * time.Millisecond
	t.Cleanup(func() { groupDrainGrace = previous })

	p := &process{
		id:        "job-whitebox",
		logger:    zap.NewNop(),
		window:    newByteWindow(64),
		done:      make(chan struct{}),
		poke:      make(chan struct{}, 1),
		stopPoll:  make(chan struct{}),
		startedAt: time.Now(),
		status:    StatusRunning,
	}
	// The shell has been reaped and the group is empty; only the output pipe is
	// still held, which is what an escaped process does.
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	p.markGroupGone()

	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the job was never finalized: an escaped process holding the pipe would report it as running forever")
	}
	snap := p.snapshot()
	if snap.Status != StatusExited {
		t.Errorf("status = %q, want %q", snap.Status, StatusExited)
	}
	if !snap.OutputOpen {
		t.Error("output_open = false, but a process outside the group still holds the pipe")
	}
}

// TestOptions_ApplyDefaults_StartGraceIsNotSilentlyZero is a regression test for
// a bug the end-to-end run found: leaving StartGrace unset produced a zero grace,
// so a start reported a job as running before the process had a chance to fail —
// which is precisely the case the startup grace exists to catch.
func TestOptions_ApplyDefaults_StartGraceIsNotSilentlyZero(t *testing.T) {
	o := Options{Dir: t.TempDir()}
	o.applyDefaults()
	if o.StartGrace != DefaultStartGrace {
		t.Errorf("StartGrace = %v, want the default %v", o.StartGrace, DefaultStartGrace)
	}
	if o.MaxJobs != DefaultMaxJobs || o.MaxLogBytes != DefaultMaxLogBytes ||
		o.WindowBytes != DefaultWindowBytes || o.MaxFinished != DefaultMaxFinished ||
		o.StopGrace != DefaultStopGrace {
		t.Errorf("defaults = %+v, want every field filled in", o)
	}

	// A negative value is the explicit "do not watch" and must survive.
	off := Options{StartGrace: -1}
	off.applyDefaults()
	if off.StartGrace != 0 {
		t.Errorf("StartGrace = %v for a negative value, want 0", off.StartGrace)
	}
}
