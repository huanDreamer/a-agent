package documents

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/openviking"
)

// fakeClient records what the syncer asked OpenViking to do.
type fakeClient struct {
	mu sync.Mutex

	writes      []openviking.WriteRequest
	writeErr    error
	uploads     []string
	uploadErr   error
	resources   []openviking.AddResourceRequest
	resourceErr error
	finds       []openviking.FindRequest
	findRes     *openviking.FindResult
}

func (f *fakeClient) WriteContent(_ context.Context, req openviking.WriteRequest) (*openviking.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, req)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return &openviking.WriteResult{URI: req.URI}, nil
}

func (f *fakeClient) UploadTemp(_ context.Context, filename string, _ io.Reader) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, filename)
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	return "upload_" + filename, nil
}

func (f *fakeClient) AddResource(_ context.Context, req openviking.AddResourceRequest) (*openviking.AddResourceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = append(f.resources, req)
	if f.resourceErr != nil {
		return nil, f.resourceErr
	}
	return &openviking.AddResourceResult{Status: "success", RootURI: req.To}, nil
}

func (f *fakeClient) Find(_ context.Context, req openviking.FindRequest) (*openviking.FindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds = append(f.finds, req)
	if f.findRes != nil {
		return f.findRes, nil
	}
	return &openviking.FindResult{}, nil
}

func (f *fakeClient) writeURIs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.writes))
	for _, w := range f.writes {
		out = append(out, w.URI)
	}
	return out
}

func newSyncer(t *testing.T, ov Client, cfg Config) *Syncer {
	t.Helper()
	if cfg.RootURI == "" {
		cfg.RootURI = "viking://user/default/huan-agent"
	}
	if cfg.Include == nil {
		cfg.Include = []string{"**/*.md", "**/*.txt", "**/*.go"}
	}
	if cfg.Exclude == nil {
		cfg.Exclude = []string{".git/**", "node_modules/**"}
	}
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	}
	s, err := New(ov, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewRequiresClientAndRoot(t *testing.T) {
	if _, err := New(nil, Config{RootURI: "viking://x"}, nil); err == nil {
		t.Error("New(nil client): want error, got nil")
	}
	if _, err := New(&fakeClient{}, Config{}, nil); err == nil {
		t.Error("New with no root uri: want error, got nil")
	}
}

func TestSaveWritesFrontmatterAndWaits(t *testing.T) {
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{RootURI: "viking://user/default/huan-agent"})
	fixed := time.Date(2026, 9, 13, 10, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	uri, err := s.Save(context.Background(), Document{
		Title:   "部署方案 评审",
		Content: "结论：先灰度。",
		Tags:    []string{"agent", "report"},
		Source:  "chat",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantPrefix := "viking://user/default/huan-agent/documents/2026/09/"
	if !strings.HasPrefix(uri, wantPrefix) {
		t.Fatalf("uri = %q, want prefix %q", uri, wantPrefix)
	}
	if !strings.HasSuffix(uri, ".md") {
		t.Errorf("uri = %q, want a .md file", uri)
	}
	if !strings.Contains(uri, "部署方案-评审") {
		t.Errorf("uri = %q, want the slug in it (a Chinese title should stay readable)", uri)
	}

	write := ov.writes[0]
	if !write.Wait {
		t.Error("Save did not wait for indexing; a saved document must be findable immediately")
	}
	if write.Mode != "replace" {
		t.Errorf("mode = %q, want replace", write.Mode)
	}
	for _, want := range []string{"title: 部署方案 评审", "generator: huan-agent", "created_at: 2026-09-13T10:30:00Z", "source: chat", "tags: [agent, report]"} {
		if !strings.Contains(write.Content, want) {
			t.Errorf("content missing %q:\n%s", want, write.Content)
		}
	}
	if !strings.HasSuffix(write.Content, "结论：先灰度。\n") {
		t.Errorf("content should end with the body: %q", write.Content)
	}

	docs := s.Documents()
	if len(docs) != 1 || docs[0].Kind != "document" || docs[0].Title != "部署方案 评审" {
		t.Fatalf("Documents() = %+v, want the saved document", docs)
	}
}

func TestSaveRejectsEmptyContent(t *testing.T) {
	s := newSyncer(t, &fakeClient{}, Config{})
	if _, err := s.Save(context.Background(), Document{Title: "x", Content: "   "}); err == nil {
		t.Error("Save with empty content: want error, got nil")
	}
}

func TestSaveSurfacesWriteFailure(t *testing.T) {
	ov := &fakeClient{writeErr: errors.New("boom")}
	s := newSyncer(t, ov, Config{})
	if _, err := s.Save(context.Background(), Document{Title: "x", Content: "y"}); err == nil {
		t.Fatal("Save with a failing server: want error, got nil")
	}
	if len(s.Documents()) != 0 {
		t.Error("a failed save was recorded in the state, want nothing recorded")
	}
}

func TestSlugifyAndFrontmatterEdgeCases(t *testing.T) {
	cases := map[string]string{
		"Hello World": "hello-world",
		"  部署 方案 ":    "部署-方案",
		"a/b\\c:d*e":  "abcde",
		"":            "doc",
		"!!!":         "doc",
		"---":         "doc",
		"ok.md":       "ok-md",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	if got := len([]rune(slugify(long))); got > 40 {
		t.Errorf("slugify(long) = %d runes, want <= 40", got)
	}
	if got := yamlScalar("plain"); got != "plain" {
		t.Errorf("yamlScalar(plain) = %q, want plain", got)
	}
	if got := yamlScalar("a: b"); got != `"a: b"` {
		t.Errorf("yamlScalar(a: b) = %q, want it quoted", got)
	}
}

func writeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"README.md":             "# readme\n",
		"docs/design.md":        "# design\n",
		"src/main.go":           "package main\n",
		"src/data.bin":          "\x00\x01\x02binary",
		"notes.txt":             "note\n",
		".git/config":           "[core]\n",
		"node_modules/pkg/a.md": "# dep\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func TestSyncWorkspaceUploadsTextAndSkipsNoise(t *testing.T) {
	dir := writeWorkspace(t)
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{RootURI: "viking://user/default/huan-agent"})

	rep, err := s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	// README.md, docs/design.md, src/main.go and notes.txt; .git/ and
	// node_modules/ are pruned by the exclude list.
	if rep.Uploaded != 4 {
		t.Errorf("Uploaded = %d, want 4", rep.Uploaded)
	}
	uris := ov.writeURIs()
	for _, uri := range uris {
		if strings.Contains(uri, ".git/") || strings.Contains(uri, "node_modules/") {
			t.Errorf("excluded path was uploaded: %s", uri)
		}
		if !strings.HasPrefix(uri, "viking://user/default/huan-agent/workspace/") {
			t.Errorf("uri = %q, want it under workspace/", uri)
		}
	}
	if len(ov.uploads) != 0 {
		t.Errorf("uploads = %v, want none (binary_mode skip)", ov.uploads)
	}
	if !rep.FinishedAt.After(rep.StartedAt) && !rep.FinishedAt.Equal(rep.StartedAt) {
		t.Error("FinishedAt is before StartedAt")
	}
	if got := len(s.Documents()); got != rep.Uploaded {
		t.Errorf("Documents() = %d, want %d", got, rep.Uploaded)
	}
}

func TestSyncWorkspaceIsIncremental(t *testing.T) {
	dir := writeWorkspace(t)
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{})

	if _, err := s.SyncWorkspace(context.Background(), dir, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := len(ov.writes)
	if first == 0 {
		t.Fatal("first sync wrote nothing")
	}

	rep, err := s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := len(ov.writes); got != first {
		t.Errorf("second sync wrote %d new files, want 0 (all unchanged)", got-first)
	}
	if rep.Unchanged != first {
		t.Errorf("Unchanged = %d, want %d", rep.Unchanged, first)
	}

	// Change one file: exactly one upload.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# changed\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rep, err = s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if rep.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1", rep.Uploaded)
	}

	// A full sync ignores the state and re-uploads everything.
	rep, err = s.SyncWorkspace(context.Background(), dir, true)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if !rep.Full {
		t.Error("Full = false, want true")
	}
	if rep.Uploaded != first {
		t.Errorf("full sync Uploaded = %d, want %d", rep.Uploaded, first)
	}
}

func TestSyncWorkspacePersistsStateAcrossInstances(t *testing.T) {
	dir := writeWorkspace(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	ov := &fakeClient{}
	cfg := Config{StatePath: statePath}
	s1 := newSyncer(t, ov, cfg)
	rep1, err := s1.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	s2 := newSyncer(t, ov, cfg)
	before := len(ov.writes)
	rep2, err := s2.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := len(ov.writes) - before; got != 0 {
		t.Errorf("reloaded syncer uploaded %d files, want 0 (state persisted)", got)
	}
	if rep2.Unchanged != rep1.Uploaded {
		t.Errorf("Unchanged = %d, want %d", rep2.Unchanged, rep1.Uploaded)
	}
	if got := len(s2.Documents()); got != rep1.Uploaded {
		t.Errorf("Documents() = %d, want %d after reload", got, rep1.Uploaded)
	}
}

func TestSyncWorkspaceRespectsSizeCapAndBinaryMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.md"), []byte{0x00, 0x01, 0x02}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{MaxFileBytes: 1024})

	rep, err := s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	if rep.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0 (one over the cap, one binary)", rep.Uploaded)
	}
	if rep.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", rep.Skipped)
	}

	// With uploads enabled the binary goes through temp_upload + add_resource.
	ov2 := &fakeClient{}
	s2 := newSyncer(t, ov2, Config{UploadBinaries: true})
	if _, err := s2.SyncWorkspace(context.Background(), dir, false); err != nil {
		t.Fatalf("SyncWorkspace (upload binaries): %v", err)
	}
	if len(ov2.uploads) != 1 {
		t.Fatalf("uploads = %v, want 1", ov2.uploads)
	}
	if len(ov2.resources) != 1 {
		t.Fatalf("resources = %v, want 1", ov2.resources)
	}
	if !strings.HasSuffix(ov2.resources[0].To, "/workspace/blob.md") {
		t.Errorf("resource target = %q, want the workspace uri", ov2.resources[0].To)
	}
	if ov2.resources[0].TempFileID != "upload_blob.md" {
		t.Errorf("temp id = %q, want it threaded through", ov2.resources[0].TempFileID)
	}
	// big.md is text and within the (unset) cap, so it still goes through the
	// text path; only the binary is uploaded as a resource.
	if got := ov2.writeURIs(); len(got) != 1 || !strings.HasSuffix(got[0], "/workspace/big.md") {
		t.Errorf("writes = %v, want just big.md", got)
	}
}

func TestSyncWorkspaceCollectsFailuresAndContinues(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content of "+name), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	ov := &fakeClient{writeErr: errors.New("server exploded")}
	s := newSyncer(t, ov, Config{})

	rep, err := s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	if rep.Failed != 3 {
		t.Errorf("Failed = %d, want 3", rep.Failed)
	}
	if len(rep.Errors) != 3 {
		t.Fatalf("Errors = %v, want one per file", rep.Errors)
	}
	if !strings.Contains(rep.Errors[0], "a.md") {
		t.Errorf("error = %q, want the relative path in it", rep.Errors[0])
	}
	if got := len(ov.writes); got != 3 {
		t.Errorf("attempts = %d, want 3 (a failure must not stop the walk)", got)
	}
}

func TestSyncWorkspaceUnreadableFileIsReportedNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.md")
	if err := os.WriteFile(ok, []byte("fine"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte("secret"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{})
	rep, err := s.SyncWorkspace(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	if rep.Failed != 1 || rep.Uploaded != 1 {
		t.Errorf("report = %+v, want 1 failed and 1 uploaded", rep)
	}
}

func TestSyncWorkspaceRejectsMissingDirectory(t *testing.T) {
	s := newSyncer(t, &fakeClient{}, Config{})
	if _, err := s.SyncWorkspace(context.Background(), "", false); !errors.Is(err, ErrNoWorkspace) {
		t.Errorf("error = %v, want ErrNoWorkspace", err)
	}
	if _, err := s.SyncWorkspace(context.Background(), filepath.Join(t.TempDir(), "nope"), false); err == nil {
		t.Error("SyncWorkspace on a missing directory: want error, got nil")
	}
	file := filepath.Join(t.TempDir(), "file.md")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.SyncWorkspace(context.Background(), file, false); err == nil {
		t.Error("SyncWorkspace on a file: want error, got nil")
	}
}

func TestSyncWorkspaceGuardsAgainstConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	blocking := &blockingClient{started: make(chan struct{}), ch: make(chan struct{})}
	s := newSyncer(t, blocking, Config{})

	done := make(chan error, 1)
	go func() {
		_, err := s.SyncWorkspace(context.Background(), dir, false)
		done <- err
	}()
	<-blocking.started

	if _, err := s.SyncWorkspace(context.Background(), dir, false); !errors.Is(err, ErrSyncing) {
		t.Errorf("concurrent sync error = %v, want ErrSyncing", err)
	}
	close(blocking.ch)
	if err := <-done; err != nil {
		t.Fatalf("first sync: %v", err)
	}
}

// blockingClient blocks the first write until its channel is closed.
type blockingClient struct {
	started chan struct{}
	ch      chan struct{}
	once    sync.Once
}

func (b *blockingClient) WriteContent(ctx context.Context, req openviking.WriteRequest) (*openviking.WriteResult, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &openviking.WriteResult{URI: req.URI}, nil
}

func (b *blockingClient) UploadTemp(context.Context, string, io.Reader) (string, error) {
	return "", errors.New("not used")
}

func (b *blockingClient) AddResource(context.Context, openviking.AddResourceRequest) (*openviking.AddResourceResult, error) {
	return nil, errors.New("not used")
}

func (b *blockingClient) Find(context.Context, openviking.FindRequest) (*openviking.FindResult, error) {
	return &openviking.FindResult{}, nil
}

func TestFindScopesToTheSubtree(t *testing.T) {
	ov := &fakeClient{}
	s := newSyncer(t, ov, Config{RootURI: "viking://user/default/huan-agent"})
	if _, err := s.Find(context.Background(), "部署", 0); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(ov.finds) != 1 {
		t.Fatalf("finds = %d, want 1", len(ov.finds))
	}
	if got, want := ov.finds[0].TargetURI, "viking://user/default/huan-agent"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
	if ov.finds[0].Limit != 10 {
		t.Errorf("limit = %d, want the default 10", ov.finds[0].Limit)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/a.md", true},
		{"**/*.md", "docs/a/b.md", true},
		{"**/*.md", "docs/a.txt", false},
		{"*.md", "docs/a.md", true}, // no slash: matched against the name too
		{"*.md", "docs/a.txt", false},
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "src/a.md", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/sub/main.go", false},
		{".git/**", ".git/config", true},
		{"", "a.md", false},
		{"*.md", "", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestLoadStateDegradesToFullSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(loadState(path).Entries); got != 0 {
		t.Errorf("corrupt state gave %d entries, want 0", got)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{"a":{"path":"a"}}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(loadState(path).Entries); got != 0 {
		t.Errorf("future-version state gave %d entries, want 0", got)
	}
	if got := loadState(filepath.Join(dir, "missing.json")).Version; got != stateVersion {
		t.Errorf("missing state version = %d, want %d", got, stateVersion)
	}

	st := state{Entries: map[string]Entry{"a.md": {Path: "a.md", Hash: "h"}}}
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	back := loadState(path)
	if back.Entries["a.md"].Hash != "h" {
		t.Errorf("round trip = %+v, want hash h", back.Entries["a.md"])
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".openviking-docs-") {
			t.Errorf("temp state file left behind: %s", e.Name())
		}
	}
}

func TestIsText(t *testing.T) {
	if !isText([]byte("hello 世界")) {
		t.Error("isText(utf8) = false, want true")
	}
	if !isText(nil) {
		t.Error("isText(empty) = false, want true")
	}
	if isText([]byte{0x00, 0x01}) {
		t.Error("isText(nul bytes) = true, want false")
	}
	if isText([]byte{0xff, 0xfe, 0xfd}) {
		t.Error("isText(invalid utf8) = true, want false")
	}
}
