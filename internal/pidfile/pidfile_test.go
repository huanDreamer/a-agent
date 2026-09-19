package pidfile

// Tests for the single-instance guard.
//
// The interesting half of this package reaches outside the process — it reads a
// process table and signals what it finds — so the tests signal real children
// rather than a fake. Three of them exist only to pin the safety properties:
// a stale pid is not signalled, a pid naming another program is not signalled,
// and a SIGTERM'd process is gone before Acquire returns.
//
// The children are started as *this test binary re-running itself*, because the
// identity check reads the name the kernel recorded, and only a real executable
// with our name has one. (`exec -a` does not work here: it changes argv[0], which
// is not what /proc/<pid>/comm or p_comm report.)

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	helperEnv       = "HUAN_PIDFILE_TEST_CHILD"
	helperIgnoreEnv = "HUAN_PIDFILE_TEST_IGNORE_TERM"
	helperReadyEnv  = "HUAN_PIDFILE_TEST_READY"
)

// TestPIDFileHelperChild is not a test: it is the body of the child processes the
// tests below start. In the parent run it skips immediately; as a child it blocks
// until it is signalled (or killed), optionally after installing a SIGTERM
// handler that ignores the signal — the case the escalation to SIGKILL is for.
func TestPIDFileHelperChild(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process: only runs as a child of these tests")
	}
	if os.Getenv(helperIgnoreEnv) != "" {
		// Buffered so the runtime's own signal delivery cannot block on the
		// handler draining it; the point is only that the default disposition —
		// terminate — is not in effect.
		signal.Notify(make(chan os.Signal, 8), syscall.SIGTERM)
	}
	if ready := os.Getenv(helperReadyEnv); ready != "" {
		// Written after the handler is installed and before blocking, so the
		// parent's signal is guaranteed to arrive at a process that will ignore
		// it. It must not be a deferred call: this function never returns except
		// by being killed, and a killed process runs no defers.
		if err := os.WriteFile(ready, []byte("ready\n"), 0o644); err != nil {
			t.Errorf("child: write readiness file: %v", err)
		}
	}
	select {}
}

// child is a started process the tests can ask about.
//
// It is reaped in the background from the moment it starts, which is not
// tidiness: an unreaped child stays visible to kill(pid, 0) as a zombie, and the
// guard's "wait until it is gone" loop would then run into its timeout and report
// an escalation that never happened.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func start(t *testing.T, env []string, argv ...string) *child {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(c.stop)
	return c
}

func (c *child) pid() int { return c.cmd.Process.Pid }

// gone reports whether the process has exited and been reaped.
func (c *child) gone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// stop ends the child's life and waits for it, so a test that fails partway
// through does not leave a process behind.
func (c *child) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// startSelfChild starts another copy of this test binary, blocked in the helper.
// Its process-table name is therefore this binary's own, which is what the
// identity check is looking for.
func startSelfChild(t *testing.T, ignoreTerm bool) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := []string{helperEnv + "=1"}
	if ignoreTerm {
		ready := filepath.Join(t.TempDir(), "ready")
		env = append(env, helperIgnoreEnv+"=1", helperReadyEnv+"="+ready)
		c := start(t, env, exe, "-test.run=^TestPIDFileHelperChild$")
		waitForFile(t, ready)
		return c
	}
	return start(t, env, exe, "-test.run=^TestPIDFileHelperChild$")
}

// waitForFile waits until the child has written its readiness file. It is what
// keeps the TERM-ignoring case honest: the signal has to arrive after the handler
// is installed, or the test only proves the default disposition.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child never signalled readiness at %s", path)
}

// testPath returns a pid file path inside the test's own temporary directory, so
// a test can never touch the one this repository's working directory holds.
func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), DefaultPath)
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
}

func readRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	return string(b)
}

// quickOptions keeps the SIGTERM grace short: a test that has to wait ten
// seconds for a process that is going to be killed anyway is not worth running.
func quickOptions(path string) Options {
	return Options{Path: path, TermGrace: 500 * time.Millisecond}
}

// requireNameOf proves the premise the identity check rests on: that this
// platform really does report processes by the name processName reads. Without
// it, a wrong assumption would silently turn the tests below into "the guard
// refused to signal anything", which is a passing test that proves nothing.
func requireNameOf(t *testing.T, pid int) {
	t.Helper()
	name, err := processName(pid)
	if err != nil {
		t.Fatalf("processName(%d): %v", pid, err)
	}
	if !ourName(name) {
		t.Fatalf("processName(%d) = %q, which is not recognised as this program (%q)", pid, name, selfName)
	}
}

func TestAcquire_WritesTheCurrentPID(t *testing.T) {
	path := testPath(t)
	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file holds %q, want this process %d", got, os.Getpid())
	}
	if res.PreviousPID != 0 || res.Terminated || res.Stale {
		t.Errorf("first acquire reported %+v, want an empty file and no previous instance", res)
	}
	if h.PID() != os.Getpid() {
		t.Errorf("handle pid = %d, want %d", h.PID(), os.Getpid())
	}
}

func TestAcquire_StalePIDIsReplacedWithoutSignalling(t *testing.T) {
	path := testPath(t)

	// A pid that cannot be running: far beyond any pid the kernel hands out, so
	// the file is unambiguously stale.
	stale := strconv.Itoa(1 << 30)
	writeRaw(t, path, stale+"\n")

	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if !res.Stale {
		t.Errorf("res.Stale = false, want true (pid %s is not running)", stale)
	}
	if res.Terminated {
		t.Errorf("a stale pid must not be reported as terminated: %+v", res)
	}
	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file holds %q, want it replaced by %d", got, os.Getpid())
	}
}

func TestAcquire_EmptyOrGarbageFileIsNotAPreviousInstance(t *testing.T) {
	for _, content := range []string{"", "\n", "   \n", "not a pid\n", "0\n", "-5\n"} {
		path := testPath(t)
		writeRaw(t, path, content)

		h, res, err := Acquire(quickOptions(path))
		if err != nil {
			t.Fatalf("Acquire over %q: %v", content, err)
		}
		t.Cleanup(func() { _ = h.Release() })

		if res.PreviousPID != 0 || res.Terminated || res.Stale {
			t.Errorf("content %q gave %+v, want it treated as no previous instance", content, res)
		}
		// Garbage is a runtime artifact, not a reason to refuse to start.
		if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
			t.Errorf("content %q left %q in the file, want %d", content, got, os.Getpid())
		}
	}
}

func TestAcquire_StopsThePreviousInstance(t *testing.T) {
	path := testPath(t)
	prev := startSelfChild(t, false)
	requireNameOf(t, prev.pid())
	writeRaw(t, path, strconv.Itoa(prev.pid())+"\n")

	started := time.Now()
	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if !res.Terminated {
		t.Fatalf("res.Terminated = false, want true (pid %d was running)", prev.pid())
	}
	if res.Escalated {
		t.Errorf("res.Escalated = true, want false: a blocked process exits on SIGTERM")
	}
	if !prev.gone() {
		t.Errorf("the previous instance is still running after Acquire returned")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("stopping a SIGTERM-responsive process took %v", elapsed)
	}
	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file holds %q, want %d", got, os.Getpid())
	}
}

func TestAcquire_KeepsAPIDThatIsNotThisProgram(t *testing.T) {
	path := testPath(t)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	// A live process that is emphatically not huan-agent: the guard must leave it
	// alone rather than kill whatever the file happened to name.
	other := start(t, nil, "/bin/sh", "-c", "exec sleep 300")
	name, err := processName(other.pid())
	if err != nil {
		t.Fatalf("processName(%d): %v", other.pid(), err)
	}
	if ourName(name) {
		t.Fatalf("the decoy is named %q, which the guard treats as ours", name)
	}
	writeRaw(t, path, strconv.Itoa(other.pid())+"\n")

	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if res.Terminated {
		t.Errorf("res.Terminated = true: an unrelated process was killed")
	}
	if res.Kept == "" {
		t.Errorf("res.Kept is empty, want the reason the pid was left alone: %+v", res)
	}
	if res.PreviousPID != other.pid() {
		t.Errorf("res.PreviousPID = %d, want %d", res.PreviousPID, other.pid())
	}
	if other.gone() {
		t.Errorf("pid %d was signalled; a pid naming another program must be left alone", other.pid())
	}
	// The file is still replaced, so the next start asks about this process and
	// not about a pid that was never ours.
	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file holds %q, want %d", got, os.Getpid())
	}
}

func TestAcquire_KillsAPreviousInstanceThatIgnoresSIGTERM(t *testing.T) {
	path := testPath(t)
	prev := startSelfChild(t, true)
	requireNameOf(t, prev.pid())
	writeRaw(t, path, strconv.Itoa(prev.pid())+"\n")

	opts := quickOptions(path)
	opts.TermGrace = 300 * time.Millisecond
	h, res, err := Acquire(opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if !res.Escalated {
		t.Errorf("res.Escalated = false, want true: the child ignores SIGTERM")
	}
	if !res.Terminated {
		t.Errorf("res.Terminated = false, want true")
	}
	if !prev.gone() {
		t.Errorf("the TERM-ignoring instance is still running after SIGKILL")
	}
}

func TestAcquire_RecordsItselfWhenTheFileAlreadyNamesIt(t *testing.T) {
	path := testPath(t)
	writeRaw(t, path, strconv.Itoa(os.Getpid())+"\n")

	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if res.Terminated {
		t.Fatalf("Acquire signalled its own process: %+v", res)
	}
	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file holds %q, want %d", got, os.Getpid())
	}
}

func TestRelease_RemovesItsOwnFile(t *testing.T) {
	path := testPath(t)
	h, _, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pid file still exists after Release: %v", err)
	}
	// Idempotent: a second Release on the same handle is not an error, because
	// the deferred one always runs.
	if err := h.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

func TestRelease_DoesNotDeleteAnotherInstanceClaim(t *testing.T) {
	path := testPath(t)
	h, _, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A successor started while this handle was held: it has already written its
	// own pid. Deleting the file now would leave it running unguarded.
	successor := 424242
	writeRaw(t, path, strconv.Itoa(successor)+"\n")

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := strings.TrimSpace(readRaw(t, path)); got != strconv.Itoa(successor) {
		t.Errorf("pid file holds %q, want the successor's pid %d to survive", got, successor)
	}
	// A handle must not keep claiming after Release: releasing twice is fine, and
	// the file it declined to remove must not become removable later.
	if err := h.Release(); err != nil {
		t.Errorf("Release after a declined removal: %v", err)
	}
}

func TestAcquire_ResolvesThePathToAbsolute(t *testing.T) {
	// A relative path resolves against the caller's working directory, which is
	// the whole reason the default is a bare name: the next start has to find it.
	path := testPath(t)
	h, res, err := Acquire(quickOptions(path))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if !filepath.IsAbs(res.Path) {
		t.Errorf("res.Path = %q, want an absolute path", res.Path)
	}
	if !strings.HasSuffix(res.Path, DefaultPath) {
		t.Errorf("res.Path = %q, want it to end in %s", res.Path, DefaultPath)
	}
}

// TestCommandName_CutsAtTheNUL pins the byte pattern the kernel really produces.
//
// These inputs are not invented: they are what `kern.proc.pid` returned for live
// processes on darwin (P_comm is a fixed 17-byte buffer, MAXCOMLEN+1). A `sleep`
// that took over the pid slot a `tmp_commprobe` had occupied came back as
// "sleep\x00mmprobe", and trimming the NUL padding alone left the residue in the
// name — which is what fed "mmprobe" to the prefix match and let a process that
// is not ours be recognised as one.
func TestCommandName_CutsAtTheNUL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"huan-agent\x00\x00\x00\x00\x00\x00\x00", "huan-agent"},
		{"sleep\x00mmprobe\x00\x00\x00\x00", "sleep"},
		{"sleep\x00mmprobe", "sleep"},
		// /proc/<pid>/comm is newline-terminated, which is why the name is
		// whitespace-trimmed as well as NUL-cut.
		{"huan-agent\n", "huan-agent"},
		{"huan-agent", "huan-agent"},
		{"\x00mmprobe", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := commandName([]byte(c.in)); got != c.want {
			t.Errorf("commandName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOurName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{selfName, true},
		{"huan-agent", true},
		{"huan-agent.exe", true},
		{"sleep", false},
		{"", false},
		// The kernel hands the name over in a fixed-size buffer and stops it at
		// a NUL, so the bytes after that NUL are residue from whatever occupied
		// the slot before. Only the part before it is the name: this is the
		// "sleep\x00mmprobe" a previous process called tmp_commprobe left behind
		// on darwin, and reading past the NUL would feed "mmprobe" to the prefix
		// match below.
		{"sleep\x00mmprobe", false},
		{"huan-agent\x00mmprobe", true},
		{"\x00huan-agent", false},
		{"\x00", false},
	}
	for _, c := range cases {
		if got := ourName(c.name); got != c.want {
			t.Errorf("ourName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOurName_StopsAtTheNUL is the same rule stated as the safety property it
// protects: a name whose residue *is* ours must not be mistaken for our process,
// and the residue of an unrelated process must not make us claim its name.
func TestOurName_StopsAtTheNUL(t *testing.T) {
	if ourName("sleep\x00" + selfName) {
		t.Errorf("a process name that only carries %q after the NUL was accepted", selfName)
	}
	if got := ourName("huan-agent\x00somethingelse"); !got {
		t.Errorf("the bytes after the NUL should be ignored, so huan-agent\\x00... is ours")
	}
}

func TestAlive(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Errorf("alive(this process) = false")
	}
	if alive(1 << 30) {
		t.Errorf("alive(1<<30) = true, want false for a pid that cannot exist")
	}
	if alive(0) || alive(-1) {
		t.Errorf("alive must reject non-positive pids")
	}
}

func TestRead_ToleratesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	pid, err := read(filepath.Join(dir, "absent.pid"))
	if err != nil {
		t.Fatalf("read of a missing file: %v", err)
	}
	if pid != 0 {
		t.Errorf("read of a missing file = %d, want 0", pid)
	}
	// A directory is not a pid file and an error is the honest answer.
	if _, err := read(dir); err == nil {
		t.Errorf("read of a directory returned no error")
	}
}
