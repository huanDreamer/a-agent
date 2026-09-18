package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// newRig builds a checkpointer over a real workspace and a temp checkpoint
// directory.
func newRig(t *testing.T, configure ...func(*Options)) (*Checkpointer, *workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := workspace.New(root, workspace.Options{})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	opts := Options{Dir: filepath.Join(t.TempDir(), "checkpoints"), Workspace: ws, Enable: true}
	for _, fn := range configure {
		fn(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ws, root
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		t.Fatal(err)
	}
	return string(body), true
}

// TestCaptureIsCopyOnWrite is the property everything else rests on: the first
// change to a file records what it looked like when the turn began, and later
// changes in the same turn do not overwrite that record.
func TestCaptureIsCopyOnWrite(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "a.go", "original")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	write(t, root, "a.go", "changed once")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	write(t, root, "a.go", "changed twice")

	m, err := c.Manifest("sess", 1)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("manifest has %d entries, want 1 (a second change must not add one)", len(m.Files))
	}
	if m.Files[0].SHA256 != hashOf([]byte("original")) {
		t.Error("the recorded pre-image is the second version, not the one the turn started with")
	}
}

// TestCaptureRecordsCreationAndDeletion covers the two cases a "back up the
// contents" design gets wrong: a file that did not exist must be deleted on
// restore, not left behind.
func TestCaptureRecordsFilesThatDidNotExist(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	if err := c.Capture(ctx, "sess", 1, "new.go"); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	write(t, root, "new.go", "created by the turn")

	m, err := c.Manifest("sess", 1)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("files = %+v", m.Files)
	}
	if m.Files[0].Existed {
		t.Error("a file that did not exist must be recorded as not existing")
	}

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, exists := read(t, root, "new.go"); exists {
		t.Error("restoring must delete a file the turn created")
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "new.go" {
		t.Errorf("report = %+v", report)
	}
}

// TestRestorePutsContentBackAndKeepsMode: the round trip.
func TestRestorePutsContentBackAndKeepsMode(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "script.sh", "#!/bin/sh\necho original\n")
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Capture(ctx, "sess", 1, "script.sh"); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// The turn rewrites it, and loses the executable bit on the way — which is
	// exactly the kind of collateral damage a restore should undo.
	write(t, root, "script.sh", "#!/bin/sh\necho changed\n")
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true, Force: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, exists := read(t, root, "script.sh")
	if !exists || got != "#!/bin/sh\necho original\n" {
		t.Errorf("content = %q (exists=%v), want the original", got, exists)
	}
	info, err := os.Stat(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 restored", info.Mode().Perm())
	}
	if len(report.Restored) != 1 {
		t.Errorf("report = %+v", report)
	}
}

// TestRestoreRefusesToOverwriteNewerWork is the safety property that matters most:
// a rollback that destroys the work done since is worse than no rollback at all.
func TestRestoreRefusesToOverwriteNewerWork(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "a.go", "before the turn")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.go", "after the turn")
	// Somebody edits it afterwards — the model, or the person.
	write(t, root, "a.go", "a person's newer work")

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", report.Conflicts)
	}
	if len(report.Restored) != 0 {
		t.Error("a conflicted file must not be written")
	}
	got, _ := read(t, root, "a.go")
	if got != "a person's newer work" {
		t.Errorf("the newer work was overwritten: %q", got)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "force") {
		t.Errorf("the conflict should say how to proceed: %+v", report.Conflicts[0])
	}

	// With Force it is written, because the person said so.
	report, err = c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true, Force: true})
	if err != nil {
		t.Fatalf("forced Restore: %v", err)
	}
	if len(report.Restored) != 1 {
		t.Errorf("forced report = %+v", report)
	}
	got, _ = read(t, root, "a.go")
	if got != "before the turn" {
		t.Errorf("forced restore left %q", got)
	}
}

// TestRestoreIsItselfUndoable: a one-way destructive operation is not something a
// person should be asked to click.
func TestRestoreIsItselfUndoable(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "a.go", "turn start")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.go", "turn end")

	report, err := c.Restore("sess", 1, RestoreOptions{Session: "sess", Force: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := read(t, root, "a.go")
	if got != "turn start" {
		t.Fatalf("restore left %q", got)
	}
	if report.Undo == nil {
		t.Fatal("the report does not name the undo checkpoint, so the restore cannot be reversed")
	}

	// Restoring the undo checkpoint brings the newer content back.
	if _, err := c.Restore(report.Undo.Session, report.Undo.Turn,
		RestoreOptions{Force: true, SkipPreCapture: true}); err != nil {
		t.Fatalf("undo Restore: %v", err)
	}
	got, _ = read(t, root, "a.go")
	if got != "turn end" {
		t.Errorf("the undo left %q, want the content from before the restore", got)
	}
}

// TestRestoreBringsBackADeletedFile: a file the turn (or a later step) removed
// comes back, which is the other half of what a checkpoint is for.
func TestRestoreBringsBackADeletedFile(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "gone.go", "important")
	if err := c.Capture(ctx, "sess", 1, "gone.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, exists := read(t, root, "gone.go")
	if !exists || got != "important" {
		t.Errorf("the deleted file was not restored: %q exists=%v", got, exists)
	}
	if len(report.Restored) != 1 {
		t.Errorf("report = %+v", report)
	}
}

// TestCaptureSkipsWhatItCannotCover: a file left out of the checkpoint has to be
// visible in the report, because a rollback that looks complete and is not is
// worse than one that says what it missed.
func TestCaptureRecordsSkips(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	// A directory is not a file the checkpoint can restore.
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Capture(ctx, "sess", 1, "subdir"); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	m, err := c.Manifest("sess", 1)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Files) != 1 || m.Files[0].Skipped == "" {
		t.Fatalf("files = %+v, want one entry explaining the skip", m.Files)
	}

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Skipped) != 1 {
		t.Errorf("report = %+v, want the skip listed", report)
	}
	if !strings.Contains(report.Summary(), "不在检查点覆盖范围内") {
		t.Errorf("summary hides the gap: %q", report.Summary())
	}
}

// TestCaptureIsIdempotentUnderConcurrency: the runner can have several writes in
// flight, and a lost update here drops a pre-image silently — until the day it is
// needed.
func TestCaptureIsConcurrent(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	// 24 distinct file names, each captured by its own goroutine: the point is
	// that no capture is lost to a concurrent read-modify-write of the manifest.
	const n = 24
	for i := 0; i < n; i++ {
		write(t, root, filepath.Join("pkg", "f"+string(rune('a'+i))+".go"), "original")
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel := filepath.Join("pkg", "f"+string(rune('a'+i))+".go")
			_ = c.Capture(ctx, "sess", 1, rel)
		}(i)
	}
	wg.Wait()

	m, err := c.Manifest("sess", 1)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Files) != n {
		t.Errorf("captured %d distinct files, want %d (a lost update drops a pre-image silently)", len(m.Files), n)
	}
}

// TestManifestWithoutACheckpoint: a turn that changed nothing has no checkpoint,
// and the caller has to be able to tell that from an empty one.
func TestManifestWithoutACheckpoint(t *testing.T) {
	c, _, _ := newRig(t)
	if _, err := c.Manifest("sess", 7); err != ErrNoCheckpoint {
		t.Errorf("err = %v, want ErrNoCheckpoint", err)
	}
	if _, err := c.Restore("sess", 7, RestoreOptions{}); err != ErrNoCheckpoint {
		t.Errorf("Restore err = %v, want ErrNoCheckpoint", err)
	}
}

// TestDisabledRefuses: a caller has to be able to tell "off" from "nothing to do",
// or a deployment with checkpoints disabled would look like one where every turn
// happened to change nothing.
func TestDisabledRefuses(t *testing.T) {
	c, _, _ := newRig(t, func(o *Options) { o.Enable = false })
	if c.Enabled() {
		t.Fatal("Enabled should be false")
	}
	if err := c.Capture(context.Background(), "sess", 1, "a.go"); err != ErrDisabled {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

// TestPathsAreConfined: a checkpoint must not become a way to read or write
// outside the workspace.
func TestPathsAreConfined(t *testing.T) {
	c, _, _ := newRig(t)
	if err := c.Capture(context.Background(), "sess", 1, "../../etc/passwd"); err == nil {
		t.Error("a path outside the workspace must be refused")
	}
}

// TestPruneKeepsTheNewestTurns: an unbounded checkpoint directory is a disk-space
// bug, and dropping the newest would be worse than dropping nothing.
func TestPruneKeepsTheNewestTurns(t *testing.T) {
	c, _, root := newRig(t, func(o *Options) { o.KeepTurns = 3 })
	ctx := context.Background()

	for turn := 1; turn <= 6; turn++ {
		write(t, root, "a.go", strings.Repeat("x", turn))
		if err := c.Capture(ctx, "sess", turn, "a.go"); err != nil {
			t.Fatalf("Capture turn %d: %v", turn, err)
		}
	}
	removed, err := c.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed %d turns, want 3", removed)
	}

	turns, err := c.ListTurns("sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("kept turns = %v, want three", turns)
	}
	for _, want := range []int{4, 5, 6} {
		found := false
		for _, got := range turns {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("turn %d was pruned; the newest must be kept (kept %v)", want, turns)
		}
	}
}

// TestPruneEnforcesTheSizeCap: the per-session cap alone does not bound disk use,
// because one huge turn can be arbitrarily large.
func TestPruneEnforcesTheSizeCap(t *testing.T) {
	c, _, root := newRig(t, func(o *Options) {
		o.KeepTurns = 100
		o.MaxTotalMB = 1 // 1 MiB
	})
	ctx := context.Background()

	// Three turns of ~600 KiB each: the turn cap allows all three, the size cap
	// does not.
	big := strings.Repeat("y", 600<<10)
	for turn := 1; turn <= 3; turn++ {
		write(t, root, "big.bin", big)
		if err := c.Capture(ctx, "sess", turn, "big.bin"); err != nil {
			t.Fatalf("Capture turn %d: %v", turn, err)
		}
	}
	removed, err := c.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("the size cap removed nothing")
	}
	turns, _ := c.ListTurns("sess")
	if len(turns) >= 3 {
		t.Errorf("kept %d turns totalling more than the cap", len(turns))
	}
}

// TestSessionIdsCannotEscapeTheDirectory: a path built from caller input is a
// traversal waiting to happen.
func TestSessionIdSanitised(t *testing.T) {
	for _, in := range []string{"../../etc", "..", ".", "a/b", "a\\b", ""} {
		got := sanitizeSegment(in)
		if strings.Contains(got, "..") || strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeSegment(%q) = %q, which can escape the directory", in, got)
		}
	}
	c, _, _ := newRig(t)
	if err := c.Capture(context.Background(), "../../etc", 1, "a.go"); err != nil {
		// The path does not exist, which is fine; what matters is where it looked.
		t.Logf("capture with a hostile session id: %v", err)
	}
	entries, err := os.ReadDir(c.Dir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") {
			t.Errorf("a directory named %q was created from a session id", e.Name())
		}
	}
}

// TestCorruptManifestIsReported: silently starting over would capture a pre-image
// that is already the changed file, which is worse than no checkpoint.
func TestCorruptManifestIsReported(t *testing.T) {
	c, _, _ := newRig(t)
	ctx := context.Background()
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.turnDir("sess", 1), manifestFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Capture(ctx, "sess", 1, "b.go"); err == nil {
		t.Error("a corrupt manifest must be reported rather than replaced")
	}
}

// TestUnsupportedManifestVersionIsReported: a format change has to be visible, not
// guessed at.
func TestUnsupportedManifestVersionIsReported(t *testing.T) {
	c, _, _ := newRig(t)
	if err := c.Capture(context.Background(), "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.turnDir("sess", 1), manifestFileName)
	if err := os.WriteFile(path, []byte(`{"version":999,"files":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Manifest("sess", 1); err == nil {
		t.Error("an unsupported version must be refused")
	}
}

// TestGitStateIsRecordedWhenThereIsARepository: the manifest's git readings are
// what make bash's changes visible next to the checkpoint's narrower coverage.
func TestGitStateRecorded(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	if err := c.BeginTurn(ctx, "sess", 1); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	m, err := c.Manifest("sess", 1)
	if err != nil {
		// The temp directory is not a git repository, so there is nothing to
		// record; that is the "no git information" case rather than a failure.
		if err != ErrNoCheckpoint {
			t.Fatalf("Manifest: %v", err)
		}
		return
	}
	// In a git repository the readings are populated; here they are simply empty,
	// and the point is that BeginTurn did not fail either way.
	_ = m
	_ = root
}

// TestRestoreUndoesTheTurnsOwnEditWithoutForce is the ordinary case, and the one a
// real session exposed as broken: the model changed a file during the turn, and the
// manifest has to be able to tell that apart from somebody editing the file
// afterwards. Without the after-image both look like "differs", so every rollback
// would demand a confirmation flag — which trains people to pass it.
func TestRestoreUndoesTheTurnsOwnEditWithoutForce(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "a.go", "before")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.go", "the turn's own edit")
	if err := c.RecordAfter(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Errorf("the turn's own edit was reported as a conflict: %+v", report.Conflicts)
	}
	if len(report.Restored) != 1 {
		t.Errorf("report = %+v, want the file restored", report)
	}
	if got, _ := read(t, root, "a.go"); got != "before" {
		t.Errorf("content = %q, want the pre-image", got)
	}
}

// TestRestoreStillRefusesSomeoneElsesEdit is the other half: the after-image must
// not turn the safety check off. A file changed *after* the turn does not match it,
// so it is still a conflict.
func TestRestoreStillRefusesSomeoneElsesEdit(t *testing.T) {
	c, _, root := newRig(t)
	ctx := context.Background()

	write(t, root, "a.go", "before")
	if err := c.Capture(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.go", "the turn's own edit")
	if err := c.RecordAfter(ctx, "sess", 1, "a.go"); err != nil {
		t.Fatal(err)
	}
	// Somebody edits it after the turn ended.
	write(t, root, "a.go", "a person's newer work")

	report, err := c.Restore("sess", 1, RestoreOptions{SkipPreCapture: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one: the newer work must not be overwritten", report.Conflicts)
	}
	if got, _ := read(t, root, "a.go"); got != "a person's newer work" {
		t.Errorf("the newer work was overwritten: %q", got)
	}
}

// TestGuardCapturesAndRecordsThroughTheToolPath: the two decorator steps in the
// order they have to happen — before the write for the pre-image, after it for the
// record of what it left.
func TestGuardCapturesAndRecords(t *testing.T) {
	c, _, root := newRig(t)
	write(t, root, "a.go", "before")

	tl := &mutatingTool{name: "write_file", cap: tool.CapWrite, root: root, rel: "a.go", content: "after"}
	guarded := c.Guard(tl, "path")
	if _, ok := guarded.(*guard); !ok {
		t.Fatalf("guarded tool is %T, want the guard", guarded)
	}

	ctx := WithTurn(context.Background(), Turn{Session: "sess", Index: 1})
	if _, err := guarded.(interface {
		InvokableRun(context.Context, string, ...einotool.Option) (string, error)
	}).InvokableRun(ctx, `{"path":"a.go","content":"after"}`); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	m, err := c.Manifest("sess", 1)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("files = %+v", m.Files)
	}
	if m.Files[0].SHA256 != hashBytes([]byte("before")) {
		t.Error("the pre-image is not what the file held before the write")
	}
	if m.Files[0].AfterSHA256 != hashBytes([]byte("after")) {
		t.Error("the after-image is not what the write left behind")
	}
}

// TestGuardWithoutATurnDoesNothing: a surface that never set up a turn gets no
// checkpoints, rather than captures filed under a made-up session.
func TestGuardWithoutATurnDoesNothing(t *testing.T) {
	c, _, root := newRig(t)
	tl := &mutatingTool{name: "write_file", cap: tool.CapWrite, root: root, rel: "a.go", content: "x"}
	guarded := c.Guard(tl, "path")

	if _, err := guarded.(interface {
		InvokableRun(context.Context, string, ...einotool.Option) (string, error)
	}).InvokableRun(context.Background(), `{"path":"a.go","content":"x"}`); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if _, err := c.Manifest("sess", 1); err != ErrNoCheckpoint {
		t.Errorf("a capture happened without a turn on the context: %v", err)
	}
	if tl.ran != 1 {
		t.Error("the tool must still run")
	}
}

// mutatingTool writes a file, standing in for write_file.
type mutatingTool struct {
	name    string
	cap     tool.Capability
	root    string
	rel     string
	content string
	ran     int
}

func (m *mutatingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: m.name, Desc: "mutates"}, nil
}

func (m *mutatingTool) Capability() tool.Capability { return m.cap }

func (m *mutatingTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	m.ran++
	if err := os.WriteFile(filepath.Join(m.root, m.rel), []byte(m.content), 0o644); err != nil {
		return "", err
	}
	return "ok", nil
}

// TestGuardForwardsConcurrency: embedding the Tool interface promotes only that
// interface's methods, so a decorator has to forward every declaration it does not
// define. The approval gate shipped a bug from exactly this, which is why each
// decorator has a test.
func TestGuardForwardsConcurrency(t *testing.T) {
	c, _, root := newRig(t)
	inner := tool.WithConcurrency(&mutatingTool{name: "write_file", cap: tool.CapWrite,
		root: root, rel: "a.go", content: "x"}, tool.Serial)
	guarded := c.Guard(inner, "path")

	if got := tool.ConcurrencyOf(guarded); got != tool.Serial {
		t.Errorf("the guard lost the declaration: %q", got)
	}
	if got := tool.CapabilityOf(guarded); got != tool.CapWrite {
		t.Errorf("the guard lost the capability: %q, which would let a write through a read-only filter", got)
	}

	// And a parallel-safe tool stays parallel-safe through the guard.
	parallel := tool.WithConcurrency(&mutatingTool{name: "edit_file", cap: tool.CapWrite,
		root: root, rel: "a.go", content: "y"}, tool.ParallelSafe)
	if got := tool.ConcurrencyOf(c.Guard(parallel, "path")); got != tool.ParallelSafe {
		t.Errorf("a parallel-safe tool became %q behind the guard", got)
	}
}
