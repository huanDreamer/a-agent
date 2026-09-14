// Package documents saves documents into OpenViking.
//
// Two kinds of document end up here, and the package treats them the same way
// once they are on the way out:
//
//   - Session documents — a report, a summary, a note the agent produced. They
//     are saved explicitly (the save_document tool, `huan-agent viking save`,
//     the console) and land under documents/<year>/<month>/.
//   - Workspace files — the files a person and the agent work on together. They
//     are synchronized in bulk and land under workspace/<relative path>.
//
// Both are written under one viking:// subtree, with deterministic URIs and an
// incremental state file, so "which of my files are already in OpenViking?" is
// answerable locally without asking the server.
package documents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/openviking"
)

// Client is the slice of the OpenViking client the syncer uses.
type Client interface {
	WriteContent(ctx context.Context, req openviking.WriteRequest) (*openviking.WriteResult, error)
	UploadTemp(ctx context.Context, filename string, content io.Reader) (string, error)
	AddResource(ctx context.Context, req openviking.AddResourceRequest) (*openviking.AddResourceResult, error)
	Find(ctx context.Context, req openviking.FindRequest) (*openviking.FindResult, error)
}

// Errors the syncer reports. They are distinct so a CLI can explain the state
// ("nothing configured to sync") rather than printing a transport failure.
var (
	// ErrDisabled means no root URI or no workspace directory is configured.
	ErrDisabled = errors.New("documents: disabled")
	// ErrNoWorkspace means workspace sync was asked for but no directory is
	// configured. It is separate from ErrDisabled because the fix is a config
	// value, not a feature switch.
	ErrNoWorkspace = errors.New("documents: no workspace directory configured")
	// ErrSyncing means another sync is already running in this process.
	ErrSyncing = errors.New("documents: a sync is already running")
)

// Config configures a Syncer.
type Config struct {
	// RootURI is the viking:// subtree everything is written under.
	RootURI string
	// Include and Exclude are glob patterns over the workspace-relative path.
	// A pattern without "/" is also matched against the file name, so "*.log"
	// excludes logs at any depth. "**" matches across directories.
	Include []string
	Exclude []string
	// MaxFileBytes caps one synchronized file; 0 means no cap.
	MaxFileBytes int64
	// UploadBinaries sends non-text files through temp_upload + add_resource
	// instead of skipping them.
	UploadBinaries bool
	// WaitForIndex blocks a workspace write until OpenViking's indexes reflect
	// it. Off by default: a bulk sync should not pay an index round-trip per
	// file, while an explicitly saved document always waits.
	WaitForIndex bool
	// TimeoutSeconds bounds one server-side wait (documents, and syncs when
	// WaitForIndex is on).
	TimeoutSeconds float64
	// StatePath is the JSON state file. Empty keeps state in memory only, which
	// means every sync is a full sync.
	StatePath string
}

// Document is one explicitly saved document.
type Document struct {
	Title   string
	Content string
	Tags    []string
	// Source records where the document came from ("chat", "cli", "feishu", a
	// file path). It is written into the frontmatter so a reader in OpenViking
	// can tell an agent-produced note from an imported file.
	Source string
}

// Report is what one sync did.
type Report struct {
	// StartedAt and FinishedAt bound the run.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Scanned is how many files were considered after filtering.
	Scanned int `json:"scanned"`
	// Uploaded is how many were written to OpenViking.
	Uploaded int `json:"uploaded"`
	// Unchanged is how many were already there with the same content hash.
	Unchanged int `json:"unchanged"`
	// Skipped is how many were left out (excluded, too large, binary).
	Skipped int `json:"skipped"`
	// Failed counts files whose write failed.
	Failed int `json:"failed"`
	// Errors lists one line per failure (relative path and reason).
	Errors []string `json:"errors,omitempty"`
	// Full reports whether the run ignored the previous state.
	Full bool `json:"full"`
	// RootURI is the subtree the run wrote under.
	RootURI string `json:"root_uri"`
}

// Syncer writes documents and workspace files into OpenViking.
type Syncer struct {
	ov     Client
	cfg    Config
	logger *zap.Logger

	mu      sync.Mutex
	syncing bool
	state   state
	// now is injectable so tests can pin timestamps.
	now func() time.Time
}

// New builds a syncer and loads its state file. A client is required: callers
// that have none (integration disabled) should not build a Syncer at all.
func New(ov Client, cfg Config, logger *zap.Logger) (*Syncer, error) {
	if ov == nil {
		return nil, errors.New("documents: client is required")
	}
	if strings.TrimSpace(cfg.RootURI) == "" {
		return nil, errors.New("documents: root uri is required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Syncer{ov: ov, cfg: cfg, logger: logger, now: time.Now}
	s.state = loadState(cfg.StatePath)
	return s, nil
}

// RootURI returns the subtree documents are written under.
func (s *Syncer) RootURI() string { return strings.TrimRight(s.cfg.RootURI, "/") }

// Save writes one document and returns its URI.
//
// It always waits for indexing, so a successful Save means the document is
// already findable: that is what a caller asked for when it saved a document in
// the first place.
func (s *Syncer) Save(ctx context.Context, doc Document) (string, error) {
	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = "untitled"
	}
	content := strings.TrimSpace(doc.Content)
	if content == "" {
		return "", errors.New("documents: content is required")
	}
	now := s.now().UTC()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])[:8]
	rel := path.Join("documents", now.Format("2006"), now.Format("01"), slugify(title)+"-"+hash+".md")
	uri := s.RootURI() + "/" + rel

	body := frontmatter(title, doc.Tags, doc.Source, now) + content + "\n"
	_, err := s.ov.WriteContent(ctx, openviking.WriteRequest{
		URI:            uri,
		Content:        body,
		Mode:           "replace",
		Tags:           doc.Tags,
		Wait:           true,
		TimeoutSeconds: s.cfg.TimeoutSeconds,
	})
	if err != nil {
		return "", fmt.Errorf("documents: save %s: %w", rel, err)
	}

	s.mu.Lock()
	s.state.put(rel, Entry{
		Path:     rel,
		URI:      uri,
		Hash:     hex.EncodeToString(sum[:]),
		Size:     int64(len(body)),
		SyncedAt: now,
		Kind:     "document",
		Title:    title,
	})
	s.saveLocked()
	s.mu.Unlock()
	return uri, nil
}

// Find searches the documents this syncer owns, scoped to its subtree so a
// shared OpenViking server does not answer with another project's material.
func (s *Syncer) Find(ctx context.Context, query string, limit int) (*openviking.FindResult, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.ov.Find(ctx, openviking.FindRequest{Query: query, TargetURI: s.RootURI(), Limit: limit})
}

// SyncWorkspace synchronizes the configured workspace directory into
// docsRoot/workspace/<relative path>.
//
// It is incremental: a file whose sha256 matches the recorded one is counted as
// unchanged and never sent. A single file's failure is collected in the report
// and the run continues — a sync that stops at the first unreadable file is
// useless on a real workspace.
func (s *Syncer) SyncWorkspace(ctx context.Context, workspaceDir string, full bool) (Report, error) {
	dir := strings.TrimSpace(workspaceDir)
	if dir == "" {
		return Report{}, ErrNoWorkspace
	}
	info, err := os.Stat(dir)
	if err != nil {
		return Report{}, fmt.Errorf("documents: workspace %s: %w", dir, err)
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("documents: workspace %s is not a directory", dir)
	}

	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		return Report{}, ErrSyncing
	}
	s.syncing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.syncing = false
		s.mu.Unlock()
	}()

	rep := Report{StartedAt: s.now().UTC(), Full: full, RootURI: s.RootURI()}
	known := s.snapshot(full)

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is reported and skipped rather than
			// aborting the walk: the rest of the workspace is still worth
			// syncing.
			rel, relErr := filepath.Rel(dir, p)
			if relErr != nil {
				rel = p
			}
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", filepath.ToSlash(rel), err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", p, relErr))
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if excludedDir(rel, s.cfg.Exclude) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks, sockets and devices are not documents.
			rep.Skipped++
			return nil
		}
		if matchAny(s.cfg.Exclude, rel) || !matchAny(s.cfg.Include, rel) {
			rep.Skipped++
			return nil
		}
		rep.Scanned++

		outcome := s.syncFile(ctx, dir, rel, p, known[rel], full)
		switch outcome.status {
		case statusUploaded:
			rep.Uploaded++
		case statusUnchanged:
			rep.Unchanged++
		case statusSkipped:
			rep.Skipped++
		case statusFailed:
			rep.Failed++
			if outcome.err != nil {
				rep.Errors = append(rep.Errors, outcome.err.Error())
			}
		}
		return nil
	})
	if walkErr != nil {
		return rep, fmt.Errorf("documents: walk %s: %w", dir, walkErr)
	}

	rep.FinishedAt = s.now().UTC()
	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	s.logger.Info("openviking workspace sync finished",
		zap.Int("scanned", rep.Scanned),
		zap.Int("uploaded", rep.Uploaded),
		zap.Int("unchanged", rep.Unchanged),
		zap.Int("skipped", rep.Skipped),
		zap.Int("failed", rep.Failed))
	return rep, nil
}

// fileStatus is how one file ended up.
type fileStatus int

const (
	statusUploaded fileStatus = iota
	statusUnchanged
	statusSkipped
	statusFailed
)

type fileOutcome struct {
	status fileStatus
	err    error
}

// syncFile handles one file: hash, compare, write.
func (s *Syncer) syncFile(ctx context.Context, dir, rel, abs string, prev Entry, full bool) fileOutcome {
	info, err := os.Stat(abs)
	if err != nil {
		return fileOutcome{status: statusFailed, err: fmt.Errorf("%s: %w", rel, err)}
	}
	if s.cfg.MaxFileBytes > 0 && info.Size() > s.cfg.MaxFileBytes {
		// Named explicitly: "skipped" with no reason is the kind of silence an
		// operator spends an afternoon on.
		s.logger.Debug("openviking sync: file over the size cap",
			zap.String("path", rel), zap.Int64("size", info.Size()), zap.Int64("cap", s.cfg.MaxFileBytes))
		return fileOutcome{status: statusSkipped}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fileOutcome{status: statusFailed, err: fmt.Errorf("%s: %w", rel, err)}
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if !full && prev.Hash == hash && prev.URI != "" {
		return fileOutcome{status: statusUnchanged}
	}

	uri := s.RootURI() + "/workspace/" + rel
	if isText(data) {
		if _, werr := s.ov.WriteContent(ctx, openviking.WriteRequest{
			URI:            uri,
			Content:        string(data),
			Mode:           "replace",
			Wait:           s.cfg.WaitForIndex,
			TimeoutSeconds: s.cfg.TimeoutSeconds,
			Tags:           []string{"workspace", "huan-agent"},
		}); werr != nil {
			return fileOutcome{status: statusFailed, err: fmt.Errorf("%s: %w", rel, werr)}
		}
	} else {
		if !s.cfg.UploadBinaries {
			return fileOutcome{status: statusSkipped}
		}
		if uerr := s.uploadBinary(ctx, uri, rel, data); uerr != nil {
			return fileOutcome{status: statusFailed, err: fmt.Errorf("%s: %w", rel, uerr)}
		}
	}

	s.mu.Lock()
	s.state.put(rel, Entry{
		Path:     rel,
		URI:      uri,
		Hash:     hash,
		Size:     info.Size(),
		SyncedAt: s.now().UTC(),
		Kind:     "workspace",
	})
	s.mu.Unlock()
	return fileOutcome{status: statusUploaded}
}

// uploadBinary sends a non-text file through temp_upload + add_resource.
//
// The direct path form is not an option: the OpenViking HTTP server refuses
// host filesystem paths outright, so the file has to be uploaded first and
// referenced by temp id.
func (s *Syncer) uploadBinary(ctx context.Context, uri, rel string, data []byte) error {
	tempID, err := s.ov.UploadTemp(ctx, path.Base(rel), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if _, err := s.ov.AddResource(ctx, openviking.AddResourceRequest{
		TempFileID:     tempID,
		To:             uri,
		Reason:         "huan-agent workspace sync",
		Tags:           []string{"workspace", "huan-agent"},
		Wait:           s.cfg.WaitForIndex,
		TimeoutSeconds: s.cfg.TimeoutSeconds,
	}); err != nil {
		return fmt.Errorf("add resource: %w", err)
	}
	return nil
}

// Documents lists what this syncer has recorded, newest first.
func (s *Syncer) Documents() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.state.Entries))
	for _, e := range s.state.Entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SyncedAt.Equal(out[j].SyncedAt) {
			return out[i].Path < out[j].Path
		}
		return out[i].SyncedAt.After(out[j].SyncedAt)
	})
	return out
}

// snapshot copies the recorded entries for the walk to consult. A full sync
// hands back nothing, which turns every file into a candidate.
func (s *Syncer) snapshot(full bool) map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if full {
		return nil
	}
	out := make(map[string]Entry, len(s.state.Entries))
	for k, v := range s.state.Entries {
		out[k] = v
	}
	return out
}

// saveLocked persists the state file. Called with s.mu held.
func (s *Syncer) saveLocked() {
	if s.cfg.StatePath == "" {
		return
	}
	if err := saveState(s.cfg.StatePath, s.state); err != nil {
		// Losing the state file costs the next sync its incrementality; it does
		// not corrupt anything, so it is a warning rather than a failure.
		s.logger.Warn("openviking sync: could not persist state",
			zap.String("path", s.cfg.StatePath), zap.Error(err))
	}
}

// frontmatter renders the YAML header written above a document's content.
func frontmatter(title string, tags []string, source string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: " + yamlScalar(title) + "\n")
	b.WriteString("generator: huan-agent\n")
	b.WriteString("created_at: " + now.Format(time.RFC3339) + "\n")
	if source != "" {
		b.WriteString("source: " + yamlScalar(source) + "\n")
	}
	if len(tags) > 0 {
		b.WriteString("tags: [" + strings.Join(tags, ", ") + "]\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}

// yamlScalar quotes a value when it could otherwise change the document's
// structure. A title is free text and must not be able to inject a key.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\n\"'[]{}&*!|>%@`,") || strings.TrimSpace(s) != s {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

// slugify turns a title into a file-name-safe slug. Non-ASCII letters are kept
// (a Chinese title stays readable in the URL), everything that is not a letter,
// digit or dash is dropped, and a title with nothing usable left becomes "doc".
func slugify(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "doc"
	}
	// Keep URIs readable: 40 characters is plenty and bounds a pathological title.
	runes := []rune(out)
	if len(runes) > 40 {
		out = strings.Trim(string(runes[:40]), "-")
		if out == "" {
			return "doc"
		}
	}
	return out
}

// isText reports whether data looks like text: valid UTF-8 with no NUL byte.
// OpenViking's text endpoints reject binary content, so this is the check that
// routes a file to upload instead.
func isText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

// matchAny reports whether name matches any of the patterns.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchGlob(p, name) {
			return true
		}
	}
	return false
}

// matchGlob matches a slash-separated pattern against a slash-separated name.
//
// It supports the two things a config author actually writes: "*" within one
// path segment, and "**" across segments. A pattern with no "/" is also tried
// against the file name, so "*.log" excludes logs at any depth — which is what
// someone writing it means.
func matchGlob(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	name = strings.TrimSpace(name)
	if pattern == "" || name == "" {
		return false
	}
	if matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/")) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return matchSegments(strings.Split(pattern, "/"), []string{path.Base(name)})
	}
	return false
}

// matchSegments walks the pattern and name segment lists together, with "**"
// consuming any number of segments.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// excludedDir reports whether a directory's relative path is excluded, so the
// walk can prune it instead of descending into every file.
func excludedDir(rel string, patterns []string) bool {
	for _, p := range patterns {
		trimmed := strings.TrimSuffix(strings.TrimSpace(p), "/**")
		if trimmed == "" {
			continue
		}
		if trimmed == rel {
			return true
		}
	}
	return false
}
