package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/workspace"
)

// These tests run real commands against a real *workspace.Workspace rooted in a
// temporary directory. Nothing is faked, because the properties that matter
// here — that a refused call executes nothing, that a timeout reaps the whole
// process group, that output is capped with an honest marker — only exist in
// the interaction with the operating system.
//
// Every process these tests start is expected to be reaped by the tool itself,
// and the background-child tests sleep long enough to notice if one was not.

// bashNewWorkspace returns a workspace rooted in a fresh temporary directory.
func bashNewWorkspace(t *testing.T, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// bashMustTool builds the tool or fails the test.
func bashMustTool(t *testing.T, ws *workspace.Workspace, policy BashPolicy) tool.InvokableTool {
	t.Helper()
	it, err := NewBashTool(ws, policy)
	if err != nil {
		t.Fatalf("NewBashTool: %v", err)
	}
	return it
}

// bashCall runs the tool and decodes its JSON result. A Go error is handed back
// to the caller rather than failing the test, because refusal is a result too.
func bashCall(t *testing.T, it tool.InvokableTool, in BashInput) (BashOutput, error) {
	t.Helper()
	args, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	raw, err := it.InvokableRun(context.Background(), string(args))
	if err != nil {
		return BashOutput{}, err
	}
	var out BashOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode result %q: %v", raw, err)
	}
	return out, nil
}

// bashMustCall runs the tool and fails the test on any Go error.
func bashMustCall(t *testing.T, it tool.InvokableTool, in BashInput) BashOutput {
	t.Helper()
	out, err := bashCall(t, it, in)
	if err != nil {
		t.Fatalf("bash %q: %v", in.Command, err)
	}
	return out
}

// bashAbsent fails the test if anything exists at abs.
func bashAbsent(t *testing.T, abs string) {
	t.Helper()
	if _, err := os.Stat(abs); err == nil {
		t.Errorf("%s exists: something ran when nothing should have", abs)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", abs, err)
	}
}

// bashMarkerCommand is a command that would prove it ran by creating marker.
// %q plays the same role as sh quoting for the temp paths used here.
func bashMarkerCommand(marker string) string {
	return fmt.Sprintf("echo ran > %q", marker)
}

func TestBashTool_SimpleCommand(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	started := time.Now()
	out := bashMustCall(t, it, BashInput{Command: "echo hi", Description: "  greet the user  "})
	elapsed := time.Since(started)

	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
	if out.Stdout != "hi\n" {
		t.Errorf("stdout = %q, want %q", out.Stdout, "hi\n")
	}
	if out.Stderr != "" {
		t.Errorf("stderr = %q, want empty", out.Stderr)
	}
	if out.TimedOut {
		t.Error("timed_out = true for a command that finished")
	}
	if out.StdoutTruncated || out.StderrTruncated {
		t.Error("truncation reported for two bytes of output")
	}
	if out.Command != "echo hi" {
		t.Errorf("command = %q, want it echoed back", out.Command)
	}
	if out.Description != "greet the user" {
		t.Errorf("description = %q, want it echoed back trimmed", out.Description)
	}
	if out.Cwd != "." {
		t.Errorf("cwd = %q, want %q for the workspace root", out.Cwd, ".")
	}
	if out.DurationMS < 0 || out.DurationMS > elapsed.Milliseconds()+1000 {
		t.Errorf("duration_ms = %d, want roughly %d", out.DurationMS, elapsed.Milliseconds())
	}
}

func TestBashTool_NonZeroExitIsAResult(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	out := bashMustCall(t, it, BashInput{Command: "exit 3"})
	if out.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", out.ExitCode)
	}
	if out.TimedOut {
		t.Error("timed_out = true, want false")
	}
}

func TestBashTool_SeparatesStderr(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	out := bashMustCall(t, it, BashInput{Command: `printf 'to-stdout\n'; printf 'to-stderr\n' 1>&2`})
	if out.Stdout != "to-stdout\n" {
		t.Errorf("stdout = %q, want only the stdout line", out.Stdout)
	}
	if out.Stderr != "to-stderr\n" {
		t.Errorf("stderr = %q, want only the stderr line", out.Stderr)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
}

func TestBashTool_CwdHonoured(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	sub := filepath.Join(ws.Root(), "pkg", "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	it := bashMustTool(t, ws, DefaultBashPolicy())

	out := bashMustCall(t, it, BashInput{Command: "pwd", Cwd: "pkg/inner"})
	if got := strings.TrimSpace(out.Stdout); got != sub {
		t.Errorf("pwd = %q, want %q", got, sub)
	}
	if out.Cwd != "pkg/inner" {
		t.Errorf("cwd = %q, want the workspace-relative directory passed in", out.Cwd)
	}
}

func TestBashTool_CwdRelativeDotIsTheRoot(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	for _, cwd := range []string{"", ".", "./"} {
		out := bashMustCall(t, it, BashInput{Command: "pwd", Cwd: cwd})
		if got := strings.TrimSpace(out.Stdout); got != ws.Root() {
			t.Errorf("cwd %q: pwd = %q, want the workspace root %q", cwd, got, ws.Root())
		}
		if out.Cwd != "." {
			t.Errorf("cwd %q: reported cwd = %q, want %q", cwd, out.Cwd, ".")
		}
	}
}

func TestBashTool_CwdOutsideWorkspaceRefused(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())
	marker := filepath.Join(t.TempDir(), "marker")

	cases := map[string]string{
		"parent":        "..",
		"grandparent":   "../..",
		"absolute":      "/etc",
		"workspace dir": filepath.Dir(ws.Root()),
	}
	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := bashCall(t, it, BashInput{Command: bashMarkerCommand(marker), Cwd: cwd})
			if !errors.Is(err, workspace.ErrOutsideWorkspace) {
				t.Fatalf("cwd %q: err = %v, want workspace.ErrOutsideWorkspace", cwd, err)
			}
		})
	}
	bashAbsent(t, marker)
}

func TestBashTool_CwdIsNotADirectory(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())
	marker := filepath.Join(t.TempDir(), "marker")

	file := filepath.Join(ws.Root(), "notes.txt")
	if err := os.WriteFile(file, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := bashCall(t, it, BashInput{Command: bashMarkerCommand(marker), Cwd: "notes.txt"})
	if err == nil {
		t.Fatal("expected an error when cwd is a file")
	}
	if !errors.Is(err, workspace.ErrInvalidPath) {
		t.Errorf("err = %v, want it to wrap workspace.ErrInvalidPath", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want it to say the path is not a directory", err)
	}
	bashAbsent(t, marker)

	// A cwd that does not exist at all is a clear error too, not a silent run
	// in the wrong place.
	if _, err := bashCall(t, it, BashInput{Command: bashMarkerCommand(marker), Cwd: "missing/dir"}); err == nil {
		t.Error("expected an error for a missing cwd")
	}
	bashAbsent(t, marker)
}

func TestBashTool_ReadOnlyWorkspaceRefused(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{ReadOnly: true})
	it := bashMustTool(t, ws, DefaultBashPolicy())
	marker := filepath.Join(t.TempDir(), "marker")

	_, err := bashCall(t, it, BashInput{Command: bashMarkerCommand(marker)})
	if !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("err = %v, want workspace.ErrReadOnly", err)
	}
	bashAbsent(t, marker)
}

func TestBashTool_DisabledRefused(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	marker := filepath.Join(t.TempDir(), "marker")

	policies := map[string]BashPolicy{
		"explicitly disabled": func() BashPolicy { p := DefaultBashPolicy(); p.Enabled = false; return p }(),
		"zero policy":         {},
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			it := bashMustTool(t, ws, policy)
			_, err := bashCall(t, it, BashInput{Command: bashMarkerCommand(marker)})
			if err == nil {
				t.Fatal("expected a refusal when the tool is disabled")
			}
			if !strings.Contains(err.Error(), "disabled by policy") {
				t.Errorf("err = %v, want it to explain that policy disabled the tool", err)
			}
			bashAbsent(t, marker)
		})
	}
}

func TestBashTool_TimeoutKillsAndReports(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	policy := DefaultBashPolicy()
	policy.Timeout = 300 * time.Millisecond
	it := bashMustTool(t, ws, policy)

	started := time.Now()
	out, err := bashCall(t, it, BashInput{Command: "sleep 5"})
	elapsed := time.Since(started)
	if err != nil {
		// A timeout is a result to report, not a bare context error.
		t.Fatalf("timeout returned a Go error: %v", err)
	}
	if !out.TimedOut {
		t.Error("timed_out = false, want true")
	}
	if out.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 for a killed process", out.ExitCode)
	}
	if elapsed < policy.Timeout/2 {
		t.Errorf("returned after %s, far short of the %s timeout: the command cannot have been allowed to run", elapsed, policy.Timeout)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %s, want well under the 5s sleep", elapsed)
	}
	if !strings.Contains(out.Stderr, "timed out") || !strings.Contains(out.Stderr, policy.Timeout.String()) {
		t.Errorf("stderr = %q, want a note naming the %s timeout", out.Stderr, policy.Timeout)
	}
}

func TestBashTool_PerCallTimeoutIsClamped(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	policy := DefaultBashPolicy()
	policy.Timeout = 300 * time.Millisecond
	it := bashMustTool(t, ws, policy)

	started := time.Now()
	out, err := bashCall(t, it, BashInput{Command: "sleep 5", TimeoutMS: 60_000})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("timeout returned a Go error: %v", err)
	}
	if !out.TimedOut {
		t.Errorf("timed_out = false after %s: a per-call override extended the policy timeout", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %s, want the clamped %s timeout to have applied", elapsed, policy.Timeout)
	}

	// Shortening is allowed and is what the override is for.
	started = time.Now()
	out, err = bashCall(t, it, BashInput{Command: "sleep 5", TimeoutMS: 50})
	if err != nil {
		t.Fatalf("timeout returned a Go error: %v", err)
	}
	if !out.TimedOut {
		t.Error("timed_out = false for a 50ms override against a 5s command")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want the 50ms override to have applied", elapsed)
	}
}

// TestBashTool_BackgroundChildKilled covers both ways a backgrounded child can
// outlive its shell, because they need different machinery.
func TestBashTool_BackgroundChildKilled(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})

	// The literal shape: the child inherits the captured pipes, so Wait cannot
	// return until either the child exits or the timeout kills the group.
	t.Run("child holds the output pipes", func(t *testing.T) {
		policy := DefaultBashPolicy()
		policy.Timeout = 300 * time.Millisecond
		it := bashMustTool(t, ws, policy)

		began := filepath.Join(ws.Root(), "began")
		finished := filepath.Join(ws.Root(), "finished")
		command := fmt.Sprintf("( : > %q; sleep 1; : > %q ) &", began, finished)

		started := time.Now()
		if _, err := bashCall(t, it, BashInput{Command: command}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("call took %s, want it to return on the timeout", elapsed)
		}
		if _, err := os.Stat(began); err != nil {
			t.Fatalf("the background child never ran, so the test proves nothing: %v", err)
		}
		time.Sleep(1300 * time.Millisecond) // past the child's 1s sleep
		bashAbsent(t, finished)
	})

	// The other shape: the child redirects its output, so the shell's exit ends
	// Wait immediately and the timeout never fires. Nothing but an explicit
	// post-exit kill of the process group can stop this child.
	t.Run("child redirects its output", func(t *testing.T) {
		policy := DefaultBashPolicy()
		policy.Timeout = 10 * time.Second // long, so the timeout cannot be what saves us
		it := bashMustTool(t, ws, policy)

		began := filepath.Join(ws.Root(), "detached-began")
		finished := filepath.Join(ws.Root(), "detached-finished")
		// sleep 0.4 in the shell gives the child a wide margin to record that it
		// started, while still exiting well before the child's own 1s sleep ends.
		command := fmt.Sprintf("( : > %q; sleep 1; : > %q ) >/dev/null 2>&1 & sleep 0.4; echo launched", began, finished)

		started := time.Now()
		out := bashMustCall(t, it, BashInput{Command: command})
		elapsed := time.Since(started)
		if out.ExitCode != 0 || !strings.Contains(out.Stdout, "launched") {
			t.Fatalf("exit_code = %d stdout = %q, want the shell to have exited 0", out.ExitCode, out.Stdout)
		}
		if out.TimedOut {
			t.Fatalf("timed_out = true after %s: the timeout should not have fired", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Errorf("call took %s, want it to return when the shell exits", elapsed)
		}
		if _, err := os.Stat(began); err != nil {
			t.Fatalf("the background child never ran, so the test proves nothing: %v", err)
		}
		time.Sleep(1200 * time.Millisecond) // past the child's 1s sleep
		bashAbsent(t, finished)
	})
}

func TestBashTool_OutputCapped(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	const capBytes = 4096
	const produced = 20000
	policy := DefaultBashPolicy()
	policy.MaxOutputBytes = capBytes
	it := bashMustTool(t, ws, policy)

	out := bashMustCall(t, it, BashInput{Command: fmt.Sprintf("yes a | head -c %d", produced)})
	if !out.StdoutTruncated {
		t.Fatal("stdout_truncated = false for output over the cap")
	}
	if out.StderrTruncated {
		t.Error("stderr_truncated = true, want false: stderr produced nothing")
	}
	if len(out.Stdout) > capBytes+200 {
		t.Errorf("captured %d bytes, want the cap of %d plus a short marker", len(out.Stdout), capBytes)
	}
	// The head is what is kept, so the first bytes must be the real output.
	for _, r := range out.Stdout[:1024] {
		if r != 'a' && r != '\n' {
			t.Fatalf("captured head contains %q, want only the command's own output", r)
		}
	}
	want := fmt.Sprintf("discarded %d bytes", produced-capBytes)
	if !strings.Contains(out.Stdout, want) {
		t.Errorf("stdout is missing the marked discard count %q: %q", want, out.Stdout[len(out.Stdout)-120:])
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
}

func TestBashTool_DenyPatternRefused(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	marker := filepath.Join(t.TempDir(), "marker")
	policy := DefaultBashPolicy()
	// A pattern without backslashes, so the assertion on the message can be a
	// plain substring check rather than a study in escaping.
	policy.DenyPatterns = []string{`echo +forbidden`}
	it := bashMustTool(t, ws, policy)

	_, err := bashCall(t, it, BashInput{Command: fmt.Sprintf("echo    forbidden > %q", marker)})
	if err == nil {
		t.Fatal("expected a refusal for a command matching a deny pattern")
	}
	if !strings.Contains(err.Error(), `echo +forbidden`) {
		t.Errorf("err = %v, want it to name the configured pattern", err)
	}
	bashAbsent(t, marker)

	// A deny list is not a blanket refusal: everything else still runs.
	out := bashMustCall(t, it, BashInput{Command: "echo allowed"})
	if out.ExitCode != 0 || !strings.Contains(out.Stdout, "allowed") {
		t.Errorf("exit_code = %d stdout = %q, want the allowed command to run", out.ExitCode, out.Stdout)
	}
}

// TestBashTool_DenyListAcceptsBlankPatterns covers the tolerant reading of a
// policy assembled from configuration, where an empty line is a typo rather
// than a pattern that should match everything.
func TestBashTool_DenyListAcceptsBlankPatterns(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	policy := DefaultBashPolicy()
	policy.DenyPatterns = []string{"", "   "}
	it := bashMustTool(t, ws, policy)

	out := bashMustCall(t, it, BashInput{Command: "echo still-allowed"})
	if out.ExitCode != 0 || !strings.Contains(out.Stdout, "still-allowed") {
		t.Errorf("exit_code = %d stdout = %q, want the command to run", out.ExitCode, out.Stdout)
	}
}

func TestBashTool_CommandNotFound(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	out := bashMustCall(t, it, BashInput{Command: "huan-bash-test-no-such-command"})
	if out.ExitCode != 127 {
		t.Errorf("exit_code = %d, want 127 for a command the shell cannot find", out.ExitCode)
	}
	if !strings.Contains(out.Stderr, "not found") {
		t.Errorf("stderr = %q, want the shell's complaint", out.Stderr)
	}
	if out.TimedOut {
		t.Error("timed_out = true for a command that failed immediately")
	}
}

func TestBashTool_SignalDeathReportsNegativeExit(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	// A command that kills itself is a result, not a Go error, and -1 is the
	// only honest exit code for a process that never chose one.
	out := bashMustCall(t, it, BashInput{Command: "kill -9 $$"})
	if out.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 for a process killed by a signal", out.ExitCode)
	}
	if out.TimedOut {
		t.Error("timed_out = true, want false: no deadline was involved")
	}
}

// TestBashTool_CallerCancellationIsReported pins a decision the spec does not
// cover: when the caller's own context is cancelled the run was abandoned, not
// timed out by this tool, so it is reported as an error rather than as a
// meaningless exit code.
func TestBashTool_CallerCancellationIsReported(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	policy := DefaultBashPolicy()
	policy.Timeout = 30 * time.Second
	it := bashMustTool(t, ws, policy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	args, err := json.Marshal(BashInput{Command: "sleep 5"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	started := time.Now()
	raw, err := it.InvokableRun(ctx, string(args))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("InvokableRun = %q, want an error after the caller cancelled", raw)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %s, want the cancellation to have stopped the command at once", elapsed)
	}
}

// TestBashKillGroupKillsTheWholeGroup tests the group kill directly as well as
// through the tool, so the mechanism is pinned even if a future change to the
// tool stops exercising it: killing only the shell would leave `sleep 30` alive
// in the group and this test would notice.
func TestBashKillGroupKillsTheWholeGroup(t *testing.T) {
	if err := bashKillGroup(nil); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("bashKillGroup(nil) = %v, want os.ErrProcessDone", err)
	}
	bashReapGroup(nil) // must not panic

	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Whatever happens below, do not leave the 30 second sleep behind.
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	if err := bashKillGroup(cmd); err != nil {
		t.Fatalf("bashKillGroup: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the killed command did not exit: the kill did not reach it")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process group %d still has members (err = %v): a child survived the group kill", pid, err)
	}
	// Nothing is left to reap, so a second pass must be a quiet no-op.
	bashReapGroup(cmd)
}

func TestBashTool_DefaultDenyListIsWired(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	// The command is harmless on its own; it only matters that the default list
	// is consulted at all. It is never spelled as a real destructive command
	// here, because a test that depends on a deny rule firing must not be able
	// to run the command if the rule regresses.
	_, err := bashCall(t, it, BashInput{Command: "echo reboot"})
	if err == nil {
		t.Fatal("expected DefaultBashPolicy's deny list to refuse a command matching a default pattern")
	}
	if !strings.Contains(err.Error(), "deny pattern") {
		t.Errorf("err = %v, want it to name the deny list as the reason", err)
	}
}

func TestBashTool_EnvironmentInherited(t *testing.T) {
	t.Setenv("HUAN_BASH_TEST_MARKER", "inherited-value")
	ws := bashNewWorkspace(t, workspace.Options{})
	if err := os.MkdirAll(filepath.Join(ws.Root(), "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	it := bashMustTool(t, ws, DefaultBashPolicy())

	out := bashMustCall(t, it, BashInput{Command: `printf '%s' "$HUAN_BASH_TEST_MARKER"`})
	if out.Stdout != "inherited-value" {
		t.Errorf("inherited variable = %q, want %q", out.Stdout, "inherited-value")
	}

	out = bashMustCall(t, it, BashInput{Command: `printf '%s' "$HOME"`})
	if strings.TrimSpace(out.Stdout) == "" {
		t.Error("HOME is empty in the child: the parent environment was not inherited")
	}

	// PWD follows the working directory, so tools that trust it agree with the
	// process's actual cwd.
	out = bashMustCall(t, it, BashInput{Command: `printf '%s' "$PWD"`})
	if strings.TrimSpace(out.Stdout) != ws.Root() {
		t.Errorf("PWD = %q, want the workspace root %q", out.Stdout, ws.Root())
	}
	out = bashMustCall(t, it, BashInput{Command: `printf '%s' "$PWD"`, Cwd: "sub"})
	if got := strings.TrimSpace(out.Stdout); got != filepath.Join(ws.Root(), "sub") {
		t.Errorf("PWD = %q, want %q", got, filepath.Join(ws.Root(), "sub"))
	}
}

func TestBashTool_EmptyCommand(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	it := bashMustTool(t, ws, DefaultBashPolicy())

	for _, command := range []string{"", "   ", "\t\n"} {
		if _, err := bashCall(t, it, BashInput{Command: command}); err == nil {
			t.Errorf("command %q: expected an error", command)
		}
	}
}

func TestBashTool_ConstructionErrors(t *testing.T) {
	if _, err := NewBashTool(nil, DefaultBashPolicy()); err == nil {
		t.Error("NewBashTool(nil, ...) = nil error, want a refusal instead of a panic later")
	}

	ws := bashNewWorkspace(t, workspace.Options{})
	if _, err := NewBashTool(ws, BashPolicy{Enabled: true, DenyPatterns: []string{"("}}); err == nil {
		t.Error("expected an error for an uncompilable deny pattern")
	}
}

func TestBashTool_NilWorkspaceDoesNotPanic(t *testing.T) {
	// The exported constructor is the front door, but the tool must also
	// survive a nil workspace reaching the run function.
	out, err := bashRun(context.Background(), nil, bashConfig{enabled: true, timeout: time.Second, maxOutput: 128}, BashInput{Command: "echo hi"})
	if err == nil {
		t.Fatal("expected an error for a nil workspace")
	}
	if out != (BashOutput{}) {
		t.Errorf("output = %+v, want the zero value", out)
	}
}

func TestBashTool_DefaultsAppliedByConstructor(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	// A policy that only says "on" must still get the documented limits.
	it := bashMustTool(t, ws, BashPolicy{Enabled: true})

	out := bashMustCall(t, it, BashInput{Command: "echo defaults"})
	if out.ExitCode != 0 || !strings.Contains(out.Stdout, "defaults") {
		t.Errorf("exit_code = %d stdout = %q", out.ExitCode, out.Stdout)
	}

	info, err := it.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	want := fmt.Sprintf("%d bytes", DefaultBashMaxOutputBytes)
	if !strings.Contains(info.Desc, want) {
		t.Errorf("description does not mention the default cap %q: %s", want, info.Desc)
	}
	if !strings.Contains(info.Desc, DefaultBashTimeout.String()) {
		t.Errorf("description does not mention the default timeout %q", DefaultBashTimeout)
	}
}

func TestBashTool_NameSchemaAndDescription(t *testing.T) {
	ws := bashNewWorkspace(t, workspace.Options{})
	policy := DefaultBashPolicy()
	policy.Timeout = 2 * time.Second
	policy.MaxOutputBytes = 4096
	it := bashMustTool(t, ws, policy)

	info, err := it.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "bash" {
		t.Errorf("name = %q, want %q", info.Name, "bash")
	}

	// The description has to say what the tool is for and be honest about the
	// confinement and the limits; a description that oversells the sandbox is
	// worse than none, because the model acts on it.
	for _, want := range []string{
		"builds", "git",
		"working directory is confined to the workspace",
		"not sandboxed",
		"4096 bytes", "2s",
		"deny pattern", "not a security boundary",
		"discarded",
		"exit_code",
	} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("description is missing %q:\n%s", want, info.Desc)
		}
	}
	lower := strings.ToLower(info.Desc)
	for _, banned := range []string{"cannot access", "no access to the network", "sandboxed from", "guarantees"} {
		if strings.Contains(lower, banned) {
			t.Errorf("description claims %q, which the implementation does not enforce:\n%s", banned, info.Desc)
		}
	}

	schema := bashSchemaJSON(t, it)
	var parsed struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(schema), &parsed); err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	if parsed.Type != "object" {
		t.Errorf("schema type = %q, want object", parsed.Type)
	}
	for _, field := range []string{"command", "description", "cwd", "timeout_ms"} {
		if _, ok := parsed.Properties[field]; !ok {
			t.Errorf("schema is missing property %q: %s", field, schema)
		}
	}
	required := strings.Join(parsed.Required, ",")
	if required != "command" {
		t.Errorf("required = %v, want only command: the rest are optional", parsed.Required)
	}
}

// bashSchemaJSON renders the tool's parameter schema for the assertions above.
func bashSchemaJSON(t *testing.T, it tool.InvokableTool) string {
	t.Helper()
	info, err := it.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("tool publishes no parameter schema")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	b, err := json.Marshal(js)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

func TestBashTool_ShellPerPlatform(t *testing.T) {
	shell, flags, err := bashShell()
	switch runtime.GOOS {
	case "darwin", "linux":
		if err != nil {
			t.Fatalf("bashShell on %s: %v", runtime.GOOS, err)
		}
		if shell != "/bin/sh" || len(flags) != 1 || flags[0] != "-c" {
			t.Errorf("bashShell = %q %v, want /bin/sh [-c]", shell, flags)
		}
	default:
		if err == nil {
			t.Errorf("bashShell on %s = %q %v, want an error for an unmapped platform", runtime.GOOS, shell, flags)
		}
	}
}

func TestDefaultBashPolicy(t *testing.T) {
	policy := DefaultBashPolicy()
	if !policy.Enabled {
		t.Error("Enabled = false, want the default policy to enable the tool")
	}
	if policy.Timeout != DefaultBashTimeout {
		t.Errorf("Timeout = %s, want %s", policy.Timeout, DefaultBashTimeout)
	}
	if policy.MaxOutputBytes != DefaultBashMaxOutputBytes {
		t.Errorf("MaxOutputBytes = %d, want %d", policy.MaxOutputBytes, DefaultBashMaxOutputBytes)
	}
	if len(policy.DenyPatterns) == 0 {
		t.Fatal("DenyPatterns is empty, want the conservative defaults")
	}
	// Each caller gets its own slice: mutating one policy must not change what
	// the next caller receives.
	policy.DenyPatterns[0] = "mutated"
	if DefaultBashPolicy().DenyPatterns[0] == "mutated" {
		t.Error("DefaultBashPolicy shares its deny slice between callers")
	}
}

func TestBashTimeoutClamping(t *testing.T) {
	policy := 5 * time.Second
	cases := []struct {
		name     string
		override int
		want     time.Duration
	}{
		{"absent", 0, policy},
		{"negative", -1, policy},
		{"shorter", 250, 250 * time.Millisecond},
		{"equal", 5000, policy},
		{"longer", 60_000, policy},
		{"overflowing", int(^uint(0) >> 1), policy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bashTimeout(policy, tc.override); got != tc.want {
				t.Errorf("bashTimeout(%s, %d) = %s, want %s", policy, tc.override, got, tc.want)
			}
		})
	}
}

func TestBashClipRune(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"ascii", []byte("hello"), "hello"},
		{"whole rune at the end", []byte("héllo"), "héllo"},
		{"cut inside a two byte rune", []byte("héllo")[:2], "h"},
		{"cut after a three byte rune's lead byte", []byte("日")[:1], ""},
		{"cut after a complete three byte rune", []byte("日本")[:4], "日"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(bashClipRune(tc.in)); got != tc.want {
				t.Errorf("bashClipRune(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBashCaptureKeepsHeadAndCounts(t *testing.T) {
	c := &bashCapture{limit: 8}
	if n, err := c.Write([]byte("0123456789abc")); n != 13 || err != nil {
		t.Fatalf("Write = %d, %v, want the full length and no error", n, err)
	}
	text, discarded := c.text()
	if discarded != 5 {
		t.Errorf("discarded = %d, want 5", discarded)
	}
	if !strings.HasPrefix(text, "01234567") {
		t.Errorf("text = %q, want the first 8 bytes kept", text)
	}
	if !strings.Contains(text, "discarded 5 bytes") {
		t.Errorf("text = %q, want a marker naming the discarded byte count", text)
	}

	small := &bashCapture{limit: 8}
	if _, err := small.Write([]byte("abc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if text, discarded := small.text(); discarded != 0 || text != "abc" {
		t.Errorf("text = %q discarded = %d, want %q and 0", text, discarded, "abc")
	}
}
