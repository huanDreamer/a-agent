package documents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateVersion is the on-disk state's schema version. A file written by a newer
// version is treated as absent (a full sync) rather than parsed optimistically:
// re-uploading a workspace is cheap, acting on fields we do not understand is
// not.
const stateVersion = 1

// Entry is one tracked document or workspace file. It is exported because the
// console lists them; the map that holds them stays private.
type Entry struct {
	// Path is stable identity: the workspace-relative path for a workspace
	// file, or the documents/<year>/<month>/<name> path for a saved document.
	Path string `json:"path"`
	// URI is the viking:// location it was written to.
	URI string `json:"uri"`
	// Hash is the sha256 of the content, which is what makes a sync incremental.
	Hash string `json:"hash"`
	// Size is the content size in bytes.
	Size int64 `json:"size"`
	// SyncedAt is when it was last written.
	SyncedAt time.Time `json:"synced_at"`
	// Kind is "workspace" or "document".
	Kind string `json:"kind,omitempty"`
	// Title is set for a saved document.
	Title string `json:"title,omitempty"`
}

// state is the syncer's persisted view of what OpenViking already has.
type state struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// put records an entry, keyed by its path.
func (s *state) put(key string, e Entry) {
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	s.Entries[key] = e
}

// loadState reads the state file. A missing, unreadable, corrupt or
// newer-versioned file yields an empty state, which degrades a sync to a full
// one instead of failing it: the state file is an optimization, not a source of
// truth.
func loadState(path string) state {
	empty := state{Version: stateVersion, Entries: map[string]Entry{}}
	if path == "" {
		return empty
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Missing (never synced) and unreadable are the same outcome here.
		return empty
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return empty
	}
	if st.Version != stateVersion {
		return empty
	}
	if st.Entries == nil {
		st.Entries = map[string]Entry{}
	}
	return st
}

// saveState writes the state file atomically: a temp file in the same directory
// then a rename, so a crash mid-write cannot leave a truncated file behind for
// the next run to misread.
func saveState(path string, st state) error {
	st.Version = stateVersion
	if st.Entries == nil {
		st.Entries = map[string]Entry{}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("documents: mkdir state dir: %w", err)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("documents: encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".openviking-docs-*.tmp")
	if err != nil {
		return fmt.Errorf("documents: create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("documents: write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("documents: close temp state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("documents: replace state: %w", err)
	}
	return nil
}
