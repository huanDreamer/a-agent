package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Restoring a turn.
//
// Three properties are what make this safe to offer as a button:
//
//   - **It can be undone.** The current state is captured before anything is
//     written, so "roll back" is itself a rollback away from where it started. A
//     one-way destructive operation is not something a person should be asked to
//     click.
//   - **It refuses to overwrite newer work by default.** A file changed since the
//     checkpoint — by a person, by a later turn, by anything — is a conflict, and
//     silently replacing it destroys the very thing the checkpoint exists to
//     protect. Conflicts are listed and skipping is the default.
//   - **It is atomic per file.** Every write goes through a temporary file and a
//     rename, so an interrupted restore cannot leave a half-written source file.

// RestoreOptions tunes one restore.
type RestoreOptions struct {
	// Force overwrites files that changed since the checkpoint. Without it they
	// are reported as conflicts and left alone.
	Force bool
	// Session is who did the restore, for the log.
	Session string
	// SkipPreCapture disables the "capture the current state first" step. It
	// exists for tests; production callers should not set it, because it is what
	// makes a rollback reversible.
	SkipPreCapture bool
}

// RestoreReport is what a restore did, so the caller can show it rather than
// claim success.
type RestoreReport struct {
	// Restored lists files put back to their pre-image.
	Restored []string `json:"restored"`
	// Deleted lists files removed because the turn created them.
	Deleted []string `json:"deleted"`
	// Unchanged lists files that already matched their pre-image.
	Unchanged []string `json:"unchanged"`
	// Conflicts lists files left alone because they changed since the checkpoint.
	Conflicts []Conflict `json:"conflicts,omitempty"`
	// Skipped lists entries the checkpoint never covered, with the reason.
	Skipped []SkippedFile `json:"skipped,omitempty"`
	// Undo, when set, names the checkpoint that holds the state before this
	// restore: restoring *that* turn puts everything back.
	Undo *TurnRef `json:"undo,omitempty"`
	// GitStatusBefore and GitStatusEnd are git's readings either side of the
	// restore, so the person can see what the restore did not touch.
	GitStatusBefore string `json:"git_status_before,omitempty"`
	GitStatusEnd    string `json:"git_status_end,omitempty"`
}

// TurnRef names a checkpoint.
type TurnRef struct {
	Session string `json:"session"`
	Turn    int    `json:"turn"`
}

// Conflict is a file the restore would have destroyed.
type Conflict struct {
	Path string `json:"path"`
	// Reason says what changed, in terms a person can act on.
	Reason string `json:"reason"`
}

// SkippedFile is an entry the checkpoint does not cover.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Summary renders the report for a person.
func (r RestoreReport) Summary() string {
	s := fmt.Sprintf("已恢复 %d 个文件，删除 %d 个本轮新建的文件，%d 个文件本来就一致",
		len(r.Restored), len(r.Deleted), len(r.Unchanged))
	if len(r.Conflicts) > 0 {
		s += fmt.Sprintf("；%d 个文件在检查点之后被改过，默认没有覆盖", len(r.Conflicts))
	}
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf("；%d 个条目不在检查点覆盖范围内", len(r.Skipped))
	}
	return s
}

// Restore puts the workspace back to how it looked before a turn.
func (c *Checkpointer) Restore(session string, turn int, opts RestoreOptions) (RestoreReport, error) {
	if c == nil {
		return RestoreReport{}, ErrNoCheckpoint
	}

	c.mu.Lock()
	m, err := c.readManifest(session, turn)
	c.mu.Unlock()
	if err != nil {
		return RestoreReport{}, err
	}
	if m.Session == "" || len(m.Files) == 0 {
		return RestoreReport{}, ErrNoCheckpoint
	}

	report := RestoreReport{}
	_, report.GitStatusBefore = c.gitState(backgroundContext{})

	// Capture the current state first, so this restore is itself undoable. It goes
	// into a turn number one past the highest in use, which cannot collide with a
	// real turn of the session.
	if !opts.SkipPreCapture {
		undoTurn, err := c.nextTurnNumber(opts.Session)
		if err == nil {
			if err := c.captureCurrentState(opts.Session, undoTurn, m); err != nil {
				// A failed pre-capture is reported but does not stop the restore:
				// refusing to undo because the safety net could not be raised
				// would leave the person with no way out at all.
				if c.logger != nil {
					c.logger.Warn("checkpoint: 恢复前的快照失败，本次恢复将不可撤销",
						"error", err.Error())
				}
			} else {
				report.Undo = &TurnRef{Session: opts.Session, Turn: undoTurn}
			}
		}
	}

	// Phase one: decide. Nothing is written while this runs, so a conflict found
	// on the last file still leaves the first one untouched.
	type action struct {
		entry   FileEntry
		abs     string
		restore bool // true: write back; false: delete
		content []byte
	}
	var actions []action

	for _, entry := range m.Files {
		if entry.Skipped != "" {
			report.Skipped = append(report.Skipped, SkippedFile{Path: entry.Path, Reason: entry.Skipped})
			continue
		}
		abs, err := c.ws.Resolve(entry.Path)
		if err != nil {
			report.Skipped = append(report.Skipped, SkippedFile{Path: entry.Path, Reason: "路径已越界：" + err.Error()})
			continue
		}

		current, readErr := os.ReadFile(abs)
		exists := readErr == nil
		if readErr != nil && !os.IsNotExist(readErr) {
			report.Skipped = append(report.Skipped, SkippedFile{Path: entry.Path, Reason: "无法读取当前内容：" + readErr.Error()})
			continue
		}

		if !entry.Existed {
			// The turn created this file. Restoring means removing it — but only if
			// it is still the file the turn wrote, which is not knowable from the
			// manifest alone; a file that existed before the checkpoint is a
			// different story, and that case is the next branch.
			if !exists {
				report.Unchanged = append(report.Unchanged, entry.Path)
				continue
			}
			actions = append(actions, action{entry: entry, abs: abs, restore: false})
			continue
		}

		if !exists {
			// Deleted since the checkpoint: nothing to compare against, so it is
			// restored rather than treated as a conflict. Bringing back a file
			// somebody deleted is what the checkpoint is for.
			content, berr := c.readBlob(session, turn, entry.SHA256)
			if berr != nil {
				report.Skipped = append(report.Skipped, SkippedFile{Path: entry.Path, Reason: "前像丢失：" + berr.Error()})
				continue
			}
			actions = append(actions, action{entry: entry, abs: abs, restore: true, content: content})
			continue
		}

		if hashOf(current) == entry.SHA256 {
			report.Unchanged = append(report.Unchanged, entry.Path)
			continue
		}

		// The file is there and differs from its pre-image. Two cases, and the
		// after-image is what tells them apart:
		//
		//   - it still matches what this turn last wrote, so restoring simply
		//     undoes the turn's own edit — the ordinary case, no confirmation
		//     needed;
		//   - it matches neither, so something changed it after the turn, and
		//     overwriting that would destroy the very work a checkpoint exists to
		//     protect. That one asks.
		turnOwnEdit := entry.AfterSHA256 != "" && hashOf(current) == entry.AfterSHA256
		if !turnOwnEdit && !opts.Force {
			report.Conflicts = append(report.Conflicts, Conflict{
				Path:   entry.Path,
				Reason: "这一轮之后文件又被改过（不是本轮写入的内容）；确认要覆盖就加 force",
			})
			continue
		}
		content, berr := c.readBlob(session, turn, entry.SHA256)
		if berr != nil {
			report.Skipped = append(report.Skipped, SkippedFile{Path: entry.Path, Reason: "前像丢失：" + berr.Error()})
			continue
		}
		actions = append(actions, action{entry: entry, abs: abs, restore: true, content: content})
	}

	// Phase two: write. Each file atomically, and a failure is recorded rather
	// than aborting: a partial restore that says what it managed is more useful
	// than one that stopped and said nothing.
	for _, a := range actions {
		if a.restore {
			if err := writeFileAtomic(a.abs, a.content, os.FileMode(a.entry.Mode).Perm()); err != nil {
				report.Skipped = append(report.Skipped,
					SkippedFile{Path: a.entry.Path, Reason: "写回失败：" + err.Error()})
				continue
			}
			report.Restored = append(report.Restored, a.entry.Path)
			continue
		}
		if err := os.Remove(a.abs); err != nil {
			report.Skipped = append(report.Skipped,
				SkippedFile{Path: a.entry.Path, Reason: "删除失败：" + err.Error()})
			continue
		}
		report.Deleted = append(report.Deleted, a.entry.Path)
	}

	sort.Strings(report.Restored)
	sort.Strings(report.Deleted)
	sort.Strings(report.Unchanged)
	_, report.GitStatusEnd = c.gitState(backgroundContext{})

	if c.logger != nil {
		c.logger.Info("checkpoint: 已回退一轮",
			"session", session, "turn", turn,
			"restored", len(report.Restored), "deleted", len(report.Deleted),
			"conflicts", len(report.Conflicts), "forced", opts.Force)
	}
	return report, nil
}

// captureCurrentState records the pre-image of every file a checkpoint covers.
//
// It is how a restore becomes undoable: the files about to be written back are
// captured first, so restoring the undo checkpoint puts the newer content back.
func (c *Checkpointer) captureCurrentState(session string, turn int, m Manifest) error {
	for _, entry := range m.Files {
		if entry.Skipped != "" {
			continue
		}
		if err := c.Capture(backgroundContext{}, session, turn, entry.Path); err != nil {
			return err
		}
	}
	if head, status := c.gitState(backgroundContext{}); head != "" || status != "" {
		c.mu.Lock()
		um, err := c.readManifest(session, turn)
		if err == nil {
			um.Session, um.Turn = session, turn
			if um.CreatedAt.IsZero() {
				um.CreatedAt = c.now()
			}
			um.GitHead, um.GitStatus = head, status
			err = c.writeManifest(session, turn, um)
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// nextTurnNumber returns a turn number no checkpoint of this session uses yet.
func (c *Checkpointer) nextTurnNumber(session string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turns, err := c.listTurnDirs()
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, t := range turns {
		if t.session == sanitizeSegment(session) && t.turn > highest {
			highest = t.turn
		}
	}
	// The undo checkpoint goes past every real turn, so it cannot be mistaken for
	// one, and pruning's newest-first order keeps it around.
	return highest + 1, nil
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// backgroundContext is a context that is never cancelled, for the operations that
// have no caller context to honour (the git readings during a restore).
type backgroundContext struct{}

func (backgroundContext) Done() <-chan struct{} { return nil }
func (backgroundContext) Err() error            { return nil }

// RestoreDirPath is where a turn's blobs live, for a caller that wants to
// inspect them.
func (c *Checkpointer) RestoreDirPath(session string, turn int) string {
	return filepath.Join(c.turnDir(session, turn), filesDirName)
}
