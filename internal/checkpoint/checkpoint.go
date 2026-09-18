// Package checkpoint keeps the pre-image of every file a turn changes, so a turn
// can be undone.
//
// It is not a version control system and does not try to be one. The problem it
// solves is narrow and specific: an agent working for twenty minutes makes dozens
// of edits, and when it turns out to have been solving the wrong problem, the only
// way back is git — which the model has to have remembered to use. A checkpoint
// makes "undo what that turn did" one operation that does not depend on the model
// having been careful.
//
// Two properties shape everything here:
//
//   - **Copy-on-write, not a guess at the start.** Which files a turn will touch
//     is not knowable when the turn starts, so the pre-image is taken the moment
//     before the first change to a file, and only once per file per turn.
//   - **The covered range is stated, not implied.** Only writes that go through
//     the file tools are captured. A `bash -c 'sed -i ...'` bypasses all of it, so
//     the manifest also records git's view of the workspace at both ends of the
//     turn — which is how a person sees what the rest of it did.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huan/huan-agent/internal/workspace"
)

// ManifestVersion is the on-disk format version. It is written so a future change
// can tell what it is reading rather than guessing from the fields present.
const ManifestVersion = 1

// Defaults for the retention policy.
const (
	DefaultKeepTurns    = 20
	DefaultMaxTotalMB   = 512
	manifestFileName    = "manifest.json"
	filesDirName        = "files"
	maxCapturedFileSize = 32 << 20 // one file's pre-image is not worth 32 MiB of disk
)

// ErrNoCheckpoint means the turn has no checkpoint: nothing was changed through
// the file tools, or it has been pruned away.
var ErrNoCheckpoint = errors.New("checkpoint: 这一轮没有检查点")

// FileEntry is one file's pre-image record.
type FileEntry struct {
	// Path is workspace-relative and slash-separated, so a manifest read on one
	// platform makes sense on another.
	Path string `json:"path"`
	// Existed is false for a file the turn created: its "pre-image" is not
	// existing, and restoring means deleting it.
	Existed bool `json:"existed"`
	// Mode is the file's permission bits before the change.
	Mode uint32 `json:"mode,omitempty"`
	// SHA256 is of the pre-image content; empty when Existed is false.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// AfterSHA256 is the hash of what this turn last wrote to the file.
	//
	// It is what makes the common case work without a flag. Without it, "the file
	// differs from its pre-image" is ambiguous between "the turn changed it" and
	// "somebody changed it afterwards", so a rollback would have to ask for
	// confirmation every single time — or, worse, overwrite the second case
	// silently. With it, the two are distinguishable: matching this hash is the
	// turn's own work and is restored directly, and anything else is somebody
	// else's and is refused.
	AfterSHA256 string `json:"after_sha256,omitempty"`
	// Skipped explains why a file could not be captured (too large, unreadable).
	// It is recorded rather than dropped: a file the checkpoint does not cover has
	// to be visible in the report, or a rollback looks complete when it is not.
	Skipped string `json:"skipped,omitempty"`
	// CapturedAt is when the pre-image was taken.
	CapturedAt time.Time `json:"captured_at"`
}

// Manifest is everything known about one turn's checkpoint.
type Manifest struct {
	Version int    `json:"version"`
	Session string `json:"session"`
	Turn    int    `json:"turn"`
	// CreatedAt is when the first pre-image was captured.
	CreatedAt time.Time `json:"created_at"`
	// GitHead and GitStatus are the workspace's git state at the start of the
	// turn, when it is a repository. They are what makes the changes a checkpoint
	// does NOT cover visible: `git status --porcelain` after the turn shows what
	// bash did.
	GitHead   string `json:"git_head,omitempty"`
	GitStatus string `json:"git_status,omitempty"`
	// GitStatusEnd is the same reading at the end of the turn.
	GitStatusEnd string      `json:"git_status_end,omitempty"`
	Files        []FileEntry `json:"files"`
}

// Options configures a Checkpointer.
type Options struct {
	// Dir is the checkpoint root. Empty means the caller must supply one; the
	// data directory beside the database is the usual choice.
	Dir string
	// Workspace is the sandbox every path is resolved through. Required.
	Workspace *workspace.Workspace
	// KeepTurns bounds how many turns per session are kept. 0 uses the default.
	KeepTurns int
	// MaxTotalMB bounds the whole checkpoint directory. 0 uses the default.
	MaxTotalMB int
	// Logger receives what pruning dropped. Optional.
	Logger Logger
	// Enable turns checkpointing off (config). A disabled Checkpointer refuses
	// captures with ErrDisabled rather than silently doing nothing, so a caller
	// can tell the difference.
	Enable bool
}

// Logger is the slice of logging this package needs.
type Logger interface {
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
}

// ErrDisabled means checkpointing is turned off in the configuration.
var ErrDisabled = errors.New("checkpoint: 检查点功能已关闭")

// Checkpointer captures and restores pre-images for one workspace.
type Checkpointer struct {
	dir       string
	ws        *workspace.Workspace
	keepTurns int
	maxBytes  int64
	enabled   bool
	logger    Logger
	now       func() time.Time
	runGit    func(ctx contextContext, root string, args ...string) (string, error)

	// mu serialises the read-modify-write of a manifest. Two tools writing at
	// once is normal (the runner runs reads in parallel and a future version runs
	// more), and a lost update here would drop a pre-image — which is silent until
	// the day it is needed.
	mu sync.Mutex
}

// New builds a Checkpointer.
func New(opts Options) (*Checkpointer, error) {
	if opts.Workspace == nil {
		return nil, errors.New("checkpoint: workspace is required")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("checkpoint: directory is required")
	}
	if opts.KeepTurns <= 0 {
		opts.KeepTurns = DefaultKeepTurns
	}
	if opts.MaxTotalMB <= 0 {
		opts.MaxTotalMB = DefaultMaxTotalMB
	}
	return &Checkpointer{
		dir:       opts.Dir,
		ws:        opts.Workspace,
		keepTurns: opts.KeepTurns,
		maxBytes:  int64(opts.MaxTotalMB) << 20,
		enabled:   opts.Enable,
		logger:    opts.Logger,
		now:       time.Now,
		runGit:    runGitCommand,
	}, nil
}

// Dir returns the checkpoint root.
func (c *Checkpointer) Dir() string { return c.dir }

// Enabled reports whether captures do anything.
func (c *Checkpointer) Enabled() bool { return c != nil && c.enabled }

// turnDir is where one turn's checkpoint lives.
func (c *Checkpointer) turnDir(session string, turn int) string {
	return filepath.Join(c.dir, sanitizeSegment(session), fmt.Sprintf("turn-%04d", turn))
}

// BeginTurn records the workspace's git state for a turn.
//
// It runs before any change, so the reading is a "before" picture. A failure is
// not fatal: not being a git repository is normal, and a checkpoint is still
// useful without one — it is just narrower than the person might assume, which is
// why the manifest says which case it is.
func (c *Checkpointer) BeginTurn(ctx contextContext, session string, turn int) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	head, status := c.gitState(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()

	m, err := c.readManifest(session, turn)
	if err != nil {
		return err
	}
	m.Session, m.Turn = session, turn
	if m.CreatedAt.IsZero() {
		m.CreatedAt = c.now()
	}
	m.GitHead, m.GitStatus = head, status
	return c.writeManifest(session, turn, m)
}

// EndTurn records git's view at the end of the turn, so bash's changes are visible
// next to the checkpoint's narrower coverage.
func (c *Checkpointer) EndTurn(ctx contextContext, session string, turn int) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	_, status := c.gitState(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()

	m, err := c.readManifest(session, turn)
	if err != nil {
		return err
	}
	m.GitStatusEnd = status
	return c.writeManifest(session, turn, m)
}

// Capture records a file's pre-image if this turn has not recorded it yet.
//
// Idempotent by design: the second change to a file in one turn must not overwrite
// the first pre-image, because the first one is what the file looked like when the
// turn began. That is the single most important property here, and it is why the
// caller can call this on every write without thinking about it.
func (c *Checkpointer) Capture(ctx contextContext, session string, turn int, path string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	abs, rel, err := c.resolve(path)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	m, err := c.readManifest(session, turn)
	if err != nil {
		return err
	}
	if m.Session == "" {
		m.Session, m.Turn, m.CreatedAt = session, turn, c.now()
	}
	for _, f := range m.Files {
		if f.Path == rel {
			return nil // already captured: the first pre-image is the right one
		}
	}

	entry := FileEntry{Path: rel, CapturedAt: c.now()}
	info, statErr := os.Lstat(abs)
	switch {
	case os.IsNotExist(statErr):
		// The file does not exist yet: restoring means deleting it.
		entry.Existed = false
	case statErr != nil:
		entry.Skipped = "无法读取文件信息：" + statErr.Error()
	default:
		if !info.Mode().IsRegular() {
			entry.Skipped = "不是普通文件（目录、符号链接或设备），未纳入检查点"
			break
		}
		if info.Size() > maxCapturedFileSize {
			entry.Skipped = fmt.Sprintf("文件过大（%d 字节），未纳入检查点", info.Size())
			break
		}
		content, readErr := os.ReadFile(abs)
		if readErr != nil {
			entry.Skipped = "读取失败：" + readErr.Error()
			break
		}
		entry.Existed = true
		entry.Mode = uint32(info.Mode().Perm())
		entry.SHA256 = hashBytes(content)
		entry.Size = int64(len(content))

		// Content-addressed: two files with the same content are stored once, and
		// a file captured twice is stored once.
		if err := os.MkdirAll(filepath.Join(c.turnDir(session, turn), filesDirName), 0o755); err != nil {
			return fmt.Errorf("checkpoint: create files dir: %w", err)
		}
		blob := filepath.Join(c.turnDir(session, turn), filesDirName, entry.SHA256)
		if err := writeFileAtomic(blob, content, 0o644); err != nil {
			return fmt.Errorf("checkpoint: store pre-image of %s: %w", rel, err)
		}
	}

	m.Files = append(m.Files, entry)
	sort.SliceStable(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	if err := c.writeManifest(session, turn, m); err != nil {
		return err
	}
	return nil
}

// RecordAfter notes what the turn left in a file, so a later restore can tell the
// turn's own edit from a change made after it.
//
// It is called after a successful write and never fails the write: this is
// bookkeeping about a hash, and a write that already happened must not be reported
// as failed because the record of it could not be updated.
func (c *Checkpointer) RecordAfter(ctx contextContext, session string, turn int, path string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	abs, rel, err := c.resolve(path)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		// A write tool that also deleted the file leaves nothing to hash; the
		// absence of an after-hash is itself meaningful (the restore will treat the
		// file as the turn's own deletion).
		if os.IsNotExist(err) {
			content = nil
		} else {
			return err
		}
	}
	sum := ""
	if content != nil {
		sum = hashBytes(content)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.readManifest(session, turn)
	if err != nil {
		return err
	}
	for i := range m.Files {
		if m.Files[i].Path == rel {
			m.Files[i].AfterSHA256 = sum
			return c.writeManifest(session, turn, m)
		}
	}
	return nil
}

// Manifest returns one turn's checkpoint, or ErrNoCheckpoint.
func (c *Checkpointer) Manifest(session string, turn int) (Manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.readManifest(session, turn)
	if err != nil {
		return Manifest{}, err
	}
	if m.Session == "" || len(m.Files) == 0 && m.GitHead == "" && m.GitStatus == "" {
		return Manifest{}, ErrNoCheckpoint
	}
	return m, nil
}

// ListTurns returns the turn numbers that have a checkpoint for a session, oldest
// first.
func (c *Checkpointer) ListTurns(session string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(c.dir, sanitizeSegment(session)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: list %s: %w", session, err)
	}
	var turns []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.Name(), "turn-%04d", &n); err == nil {
			turns = append(turns, n)
		}
	}
	sort.Ints(turns)
	return turns, nil
}

// resolve puts a caller-supplied path through the sandbox and returns both the
// absolute path and the workspace-relative one a manifest stores.
func (c *Checkpointer) resolve(path string) (string, string, error) {
	abs, err := c.ws.Resolve(path)
	if err != nil {
		return "", "", err
	}
	rel, err := c.ws.RelWithin(abs)
	if err != nil {
		return "", "", err
	}
	return abs, rel, nil
}

// readManifest loads a turn's manifest, returning an empty one when it does not
// exist yet. Caller holds mu.
func (c *Checkpointer) readManifest(session string, turn int) (Manifest, error) {
	m := Manifest{Version: ManifestVersion}
	path := filepath.Join(c.turnDir(session, turn), manifestFileName)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		// A corrupt manifest is reported rather than treated as empty: silently
		// starting over would capture a pre-image that is already the changed
		// file, which is worse than no checkpoint at all.
		return m, fmt.Errorf("checkpoint: %s 已损坏（%w）；请删除该目录后重试", path, err)
	}
	if m.Version != ManifestVersion {
		return m, fmt.Errorf("checkpoint: %s 的版本 %d 不受支持（本版本支持 %d）", path, m.Version, ManifestVersion)
	}
	return m, nil
}

// writeManifest stores a turn's manifest. Caller holds mu.
func (c *Checkpointer) writeManifest(session string, turn int, m Manifest) error {
	m.Version = ManifestVersion
	dir := c.turnDir(session, turn)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal manifest: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, manifestFileName), body, 0o644)
}

// gitState reads HEAD and porcelain status, tolerating every failure: a
// non-repository, a missing git binary and a timeout are all "no git information",
// which the manifest records as empty.
func (c *Checkpointer) gitState(ctx contextContext) (head, status string) {
	root := c.ws.Root()
	if out, err := c.runGit(ctx, root, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}
	if out, err := c.runGit(ctx, root, "status", "--porcelain"); err == nil {
		status = strings.TrimRight(out, "\n")
	}
	return head, status
}

// Prune enforces the retention policy: at most KeepTurns turns per session and at
// most MaxTotalMB across the whole directory.
//
// The oldest go first, and what was dropped is logged: a rollback that silently
// disappears is worse than one that was never offered, because the person
// discovers it at the moment they need it.
func (c *Checkpointer) Prune() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	turns, err := c.listTurnDirs()
	if err != nil {
		return 0, err
	}

	// Stage one: the per-session turn cap, newest kept.
	bySession := map[string][]turnDirInfo{}
	for _, t := range turns {
		bySession[t.session] = append(bySession[t.session], t)
	}
	doomed := map[string]turnDirInfo{}
	for _, list := range bySession {
		sort.SliceStable(list, func(i, j int) bool { return list[i].turn > list[j].turn })
		for i := c.keepTurns; i < len(list); i++ {
			doomed[list[i].dir] = list[i]
		}
	}

	// Stage two: the total size cap over what is left, oldest first.
	var total int64
	var remaining []turnDirInfo
	for _, t := range turns {
		if _, drop := doomed[t.dir]; drop {
			continue
		}
		total += t.size
		remaining = append(remaining, t)
	}
	sort.SliceStable(remaining, func(i, j int) bool { return remaining[i].modTime.Before(remaining[j].modTime) })
	for i := 0; total > c.maxBytes && i < len(remaining); i++ {
		doomed[remaining[i].dir] = remaining[i]
		total -= remaining[i].size
	}

	removed := 0
	for _, t := range doomed {
		if err := os.RemoveAll(t.dir); err != nil {
			if c.logger != nil {
				c.logger.Warn("checkpoint: 淘汰失败", "dir", t.dir, "error", err.Error())
			}
			continue
		}
		removed++
		if c.logger != nil {
			c.logger.Info("checkpoint: 已淘汰旧检查点", "session", t.session, "turn", t.turn)
		}
	}
	return removed, nil
}

// turnDirInfo is one turn's directory with what the retention policy needs to
// decide about it.
type turnDirInfo struct {
	session string
	turn    int
	dir     string
	modTime time.Time
	size    int64
}

// listTurnDirs walks the checkpoint root once. Caller holds mu.
func (c *Checkpointer) listTurnDirs() ([]turnDirInfo, error) {
	sessions, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: read %s: %w", c.dir, err)
	}
	var out []turnDirInfo
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(c.dir, s.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(e.Name(), "turn-%04d", &n); err != nil {
				continue
			}
			dir := filepath.Join(c.dir, s.Name(), e.Name())
			size, newest := dirSize(dir)
			out = append(out, turnDirInfo{session: s.Name(), turn: n, dir: dir, modTime: newest, size: size})
		}
	}
	return out, nil
}

// dirSize returns the total size of a directory tree and the newest modification
// time in it.
func dirSize(dir string) (int64, time.Time) {
	var total int64
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !d.IsDir() {
			total += info.Size()
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return total, newest
}

// sanitizeSegment makes a session id safe to use as one path element.
//
// Session ids are uuids in practice, but a path built from a caller-supplied
// string is a path traversal waiting to happen: ".." is a valid session id as far
// as this function can tell.
func sanitizeSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "." || out == ".." || strings.Contains(out, "..") {
		return "unknown"
	}
	return out
}

// writeFileAtomic writes a file through a temporary file and a rename, so an
// interrupted write cannot leave a half-written pre-image or manifest behind.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename succeeded
	}()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// contextContext is the part of context.Context this package uses.
type contextContext interface {
	Done() <-chan struct{}
	Err() error
}

// runGitCommand runs one git command in a directory.
//
// It is a shell-out rather than a git library because this project has no git
// dependency anywhere else, and the two readings it needs (HEAD, porcelain status)
// are stable interfaces.
func runGitCommand(ctx contextContext, root string, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", err
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	// A hook or a prompt would hang the turn; nothing here is interactive.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// hashBytes is the content hash used for both the pre-image and the after-image.
func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// readBlob reads a stored pre-image.
func (c *Checkpointer) readBlob(session string, turn int, sum string) ([]byte, error) {
	if sum == "" {
		return nil, errors.New("checkpoint: 缺少内容哈希")
	}
	return os.ReadFile(filepath.Join(c.turnDir(session, turn), filesDirName, sum))
}
