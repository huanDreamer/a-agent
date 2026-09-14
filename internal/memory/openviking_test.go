package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/huan/huan-agent/internal/openviking"
)

// fakeOV is a scriptable OpenViking client. Every test in this file drives the
// mirror through it, so batching, degradation and recall fallback are asserted
// without a server.
type fakeOV struct {
	mu sync.Mutex

	rememberCalls []openviking.RememberRequest
	rememberErr   error
	// commitErr makes the write succeed but the commit fail, which is the shape
	// a server with an unavailable VLM model produces.
	rememberResult *openviking.RememberResult

	findCalls []openviking.FindRequest
	findRes   *openviking.FindResult
	findErr   error

	writeCalls []openviking.WriteRequest
	writeErr   error
	// writeErrOnce is consumed after the first write, so a test can make the
	// append fail as NOT_FOUND and the create succeed.
	writeErrOnce error
}

func (f *fakeOV) Remember(_ context.Context, req openviking.RememberRequest) (*openviking.RememberResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rememberCalls = append(f.rememberCalls, req)
	if f.rememberErr != nil {
		return nil, f.rememberErr
	}
	if f.rememberResult != nil {
		return f.rememberResult, nil
	}
	return &openviking.RememberResult{SessionID: req.SessionID, Added: len(req.Messages)}, nil
}

func (f *fakeOV) Find(_ context.Context, req openviking.FindRequest) (*openviking.FindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls = append(f.findCalls, req)
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.findRes != nil {
		return f.findRes, nil
	}
	return &openviking.FindResult{}, nil
}

func (f *fakeOV) WriteContent(_ context.Context, req openviking.WriteRequest) (*openviking.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCalls = append(f.writeCalls, req)
	if f.writeErrOnce != nil {
		err := f.writeErrOnce
		f.writeErrOnce = nil
		return nil, err
	}
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return &openviking.WriteResult{URI: req.URI, Mode: req.Mode}, nil
}

func (f *fakeOV) remembers() []openviking.RememberRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]openviking.RememberRequest(nil), f.rememberCalls...)
}

func (f *fakeOV) writes() []openviking.WriteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]openviking.WriteRequest(nil), f.writeCalls...)
}

func newTestLocal(t *testing.T) Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func mirrorConfig() OpenVikingMirrorConfig {
	return OpenVikingMirrorConfig{
		SessionPrefix: "huan-agent",
		Commit:        true,
		FlushEvery:    2,
		RecallEnable:  true,
		RecallLimit:   5,
		RecallTarget:  "viking://user/default/huan-agent",
		JournalURI:    "viking://user/default/huan-agent/memory/journal.md",
		JournalEnable: true,
	}
}

func TestNewOpenVikingStoreWithoutClientIsTransparent(t *testing.T) {
	local := newTestLocal(t)
	got := NewOpenVikingStore(local, nil, mirrorConfig(), nil)
	if got != local {
		t.Fatal("NewOpenVikingStore(nil client) returned a mirror, want the local store itself")
	}
}

func TestAppendMirrorsTurnsLocallyAndRemotely(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 0 // submit every turn
	st := NewOpenVikingStore(local, ov, cfg, nil)
	ctx := context.Background()

	if err := st.Append(ctx, "ns1", NewEntry(KindUser, "部署负责人是谁？")); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := st.Append(ctx, "ns1", NewEntry(KindAssistant, "是 Alice。")); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	// Local truth is intact.
	entries, err := st.Read(ctx, "ns1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("local entries = %d, want 2", len(entries))
	}

	calls := ov.remembers()
	if len(calls) != 2 {
		t.Fatalf("remember calls = %d, want 2 (one per turn)", len(calls))
	}
	if got, want := calls[0].SessionID, "huan-agent-ns1"; got != want {
		t.Errorf("session id = %q, want %q", got, want)
	}
	if calls[0].Messages[0].Role != "user" || calls[1].Messages[0].Role != "assistant" {
		t.Errorf("roles = %q/%q, want user/assistant", calls[0].Messages[0].Role, calls[1].Messages[0].Role)
	}
	if !calls[0].Commit {
		t.Error("Commit = false, want true when configured")
	}
	if got := st.(*OpenVikingStore).Stats().Submitted; got != 2 {
		t.Errorf("Submitted = %d, want 2", got)
	}
}

func TestAppendDoesNotMirrorNonTurnKinds(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 0
	st := NewOpenVikingStore(local, ov, cfg, nil)

	for _, kind := range []Kind{KindTool, KindSummary} {
		if err := st.Append(context.Background(), "ns", NewEntry(kind, "payload")); err != nil {
			t.Fatalf("Append(%s): %v", kind, err)
		}
	}
	if got := len(ov.remembers()); got != 0 {
		t.Errorf("remember calls = %d, want 0 (tool results and summaries are not turns)", got)
	}
}

func TestAppendBatchesUntilFlushEvery(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 3
	st := NewOpenVikingStore(local, ov, cfg, nil).(*OpenVikingStore)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := st.Append(ctx, "ns", NewEntry(KindUser, "turn")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := len(ov.remembers()); got != 0 {
		t.Fatalf("calls after 2 turns = %d, want 0 (batch not full)", got)
	}
	if got := st.Stats().Pending; got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}

	if err := st.Append(ctx, "ns", NewEntry(KindAssistant, "reply")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	calls := ov.remembers()
	if len(calls) != 1 {
		t.Fatalf("calls after 3 turns = %d, want 1 batch", len(calls))
	}
	if got := len(calls[0].Messages); got != 3 {
		t.Errorf("batch size = %d, want 3", got)
	}
	if got := st.Stats().Pending; got != 0 {
		t.Errorf("Pending = %d, want 0 after the flush", got)
	}
}

func TestCloseFlushesRemainingBatch(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 10
	st := NewOpenVikingStore(local, ov, cfg, nil)

	if err := st.Append(context.Background(), "ns", NewEntry(KindUser, "unflushed")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := ov.remembers()
	if len(calls) != 1 || len(calls[0].Messages) != 1 {
		t.Fatalf("calls = %+v, want one batch of one message after Close", calls)
	}
}

func TestFlushKeepsOtherNamespacesBuffered(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 10
	st := NewOpenVikingStore(local, ov, cfg, nil).(*OpenVikingStore)
	ctx := context.Background()

	if err := st.Append(ctx, "a", NewEntry(KindUser, "from a")); err != nil {
		t.Fatalf("Append a: %v", err)
	}
	if err := st.Append(ctx, "b", NewEntry(KindUser, "from b")); err != nil {
		t.Fatalf("Append b: %v", err)
	}

	st.Flush(ctx, "a")
	calls := ov.remembers()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (only namespace a)", len(calls))
	}
	if got, want := calls[0].SessionID, "huan-agent-a"; got != want {
		t.Errorf("session = %q, want %q", got, want)
	}
	if got := st.Stats().Pending; got != 1 {
		t.Errorf("Pending = %d, want 1 (namespace b still buffered)", got)
	}

	st.Flush(ctx, "")
	if got := len(ov.remembers()); got != 2 {
		t.Errorf("calls = %d, want 2 after flushing everything", got)
	}
}

func TestMirrorDegradesWithoutFailingLocalWrites(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{rememberErr: &openviking.APIError{Status: 0, Code: "UNAVAILABLE", Message: "connection refused"}}
	cfg := mirrorConfig()
	cfg.FlushEvery = 0
	st := NewOpenVikingStore(local, ov, cfg, nil)
	ctx := context.Background()

	if err := st.Append(ctx, "ns", NewEntry(KindUser, "still recorded")); err != nil {
		t.Fatalf("Append with OpenViking down returned %v, want nil", err)
	}
	entries, err := st.Read(ctx, "ns")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("local entries = %d, want 1", len(entries))
	}
	stats := st.(*OpenVikingStore).Stats()
	if stats.Failures != 1 {
		t.Errorf("Failures = %d, want 1", stats.Failures)
	}
	if !strings.Contains(stats.LastError, "connection refused") {
		t.Errorf("LastError = %q, want the transport error", stats.LastError)
	}
	if stats.LastErrorAt == nil {
		t.Error("LastErrorAt is nil, want the failure time")
	}
}

func TestAddFactWritesJournalEvenWhenExtractionIsUnavailable(t *testing.T) {
	local := newTestLocal(t)
	// The shape of a server whose VLM model is not activated: the write works,
	// the extraction does not.
	ov := &fakeOV{rememberErr: &openviking.APIError{Status: 500, Code: "ModelNotOpen", Message: "not activated"}}
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)
	ctx := context.Background()

	if err := st.AddFact(ctx, "ns", Fact{Key: "部署负责人", Value: "Alice", Keywords: []string{"部署"}}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	writes := ov.writes()
	if len(writes) != 1 {
		t.Fatalf("journal writes = %d, want 1 (the VLM-independent path)", len(writes))
	}
	if got, want := writes[0].URI, "viking://user/default/huan-agent/memory/journal.md"; got != want {
		t.Errorf("journal uri = %q, want %q", got, want)
	}
	if writes[0].Mode != "append" {
		t.Errorf("mode = %q, want append", writes[0].Mode)
	}
	if !strings.Contains(writes[0].Content, "部署负责人: Alice") {
		t.Errorf("journal content = %q, want the fact text", writes[0].Content)
	}
	facts, err := st.Facts(ctx, "ns")
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("local facts = %d, want 1", len(facts))
	}
	if got := st.(*OpenVikingStore).Stats().Facts; got != 1 {
		t.Errorf("Stats().Facts = %d, want 1", got)
	}
}

func TestAddFactCreatesJournalWhenAppendFindsNothing(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{writeErrOnce: &openviking.APIError{Status: 404, Code: "NOT_FOUND", Message: "no such file"}}
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)

	if err := st.AddFact(context.Background(), "ns", Fact{Value: "first fact"}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	writes := ov.writes()
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2 (append then create)", len(writes))
	}
	if writes[0].Mode != "append" || writes[1].Mode != "create" {
		t.Errorf("modes = %q/%q, want append/create", writes[0].Mode, writes[1].Mode)
	}
	if !strings.HasPrefix(writes[1].Content, "# huan-agent fact journal") {
		t.Errorf("create content = %q, want the journal header", writes[1].Content)
	}
}

func TestAddFactDoesNotClobberJournalOnAppendFailure(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{writeErr: &openviking.APIError{Status: 500, Code: "INTERNAL", Message: "boom"}}
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)

	if err := st.AddFact(context.Background(), "ns", Fact{Value: "f"}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	// A non-404 append failure must never fall back to "replace", which would
	// overwrite the only remote copy of the journal.
	if got := len(ov.writes()); got != 1 {
		t.Fatalf("writes = %d, want 1 (no destructive retry)", got)
	}
	if got := st.(*OpenVikingStore).Stats().Failures; got != 1 {
		t.Errorf("Failures = %d, want 1", got)
	}
}

func TestSearchFactsPrefersSemanticHits(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{findRes: &openviking.FindResult{
		Memories: []openviking.FindHit{{URI: "viking://user/default/memories/entities/1.md", Abstract: "部署负责人是 Alice"}},
	}}
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)

	facts, err := st.SearchFacts(context.Background(), "谁负责部署", 5)
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(facts) != 1 || !strings.Contains(facts[0].Value, "Alice") {
		t.Fatalf("facts = %+v, want the semantic hit", facts)
	}
	if got, want := facts[0].Keywords[0], "viking://user/default/memories/entities/1.md"; got != want {
		t.Errorf("keywords = %v, want the source uri", facts[0].Keywords)
	}
	if len(ov.findCalls) != 1 {
		t.Fatalf("find calls = %d, want 1", len(ov.findCalls))
	}
	if got, want := ov.findCalls[0].TargetURI, "viking://user/default/huan-agent"; got != want {
		t.Errorf("target uri = %q, want the configured scope %q", got, want)
	}
}

func TestSearchFactsFallsBackToKeywords(t *testing.T) {
	local := newTestLocal(t)
	ctx := context.Background()
	if err := local.AddFact(ctx, "ns", Fact{Key: "语言", Value: "Go", Keywords: []string{"golang"}}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ov := &fakeOV{findErr: &openviking.APIError{Status: 0, Code: "UNAVAILABLE", Message: "down"}}
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)

	facts, err := st.SearchFacts(ctx, "golang", 5)
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Value != "Go" {
		t.Fatalf("facts = %+v, want the local keyword hit", facts)
	}
	if got := st.(*OpenVikingStore).Stats().Failures; got != 1 {
		t.Errorf("Failures = %d, want 1 (the semantic attempt is recorded)", got)
	}
}

func TestSearchFactsFallsBackWhenSemanticFindsNothing(t *testing.T) {
	local := newTestLocal(t)
	ctx := context.Background()
	if err := local.AddFact(ctx, "ns", Fact{Key: "editor", Value: "neovim", Keywords: []string{"editor"}}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ov := &fakeOV{} // returns an empty result rather than an error
	st := NewOpenVikingStore(local, ov, mirrorConfig(), nil)

	facts, err := st.SearchFacts(ctx, "editor", 5)
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Value != "neovim" {
		t.Fatalf("facts = %+v, want the local fallback", facts)
	}
	if got := st.(*OpenVikingStore).Stats().Failures; got != 0 {
		t.Errorf("Failures = %d, want 0 (an empty result is not a failure)", got)
	}
}

func TestSearchFactsSkipsSemanticWhenDisabled(t *testing.T) {
	local := newTestLocal(t)
	ctx := context.Background()
	if err := local.AddFact(ctx, "ns", Fact{Value: "local only", Keywords: []string{"local"}}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	cfg := mirrorConfig()
	cfg.RecallEnable = false
	ov := &fakeOV{}
	st := NewOpenVikingStore(local, ov, cfg, nil)

	if _, err := st.SearchFacts(ctx, "local", 5); err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(ov.findCalls) != 0 {
		t.Errorf("find calls = %d, want 0 when recall_enable is false", len(ov.findCalls))
	}
}

func TestAppendLocalFailureIsReturnedAndNotMirrored(t *testing.T) {
	local := newTestLocal(t)
	ov := &fakeOV{}
	cfg := mirrorConfig()
	cfg.FlushEvery = 0
	st := NewOpenVikingStore(local, ov, cfg, nil)

	// An invalid namespace is rejected by the local store; the mirror must not
	// get a chance to write something the local truth does not contain.
	err := st.Append(context.Background(), "bad/ns", NewEntry(KindUser, "x"))
	if !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("Append error = %v, want ErrInvalidNamespace", err)
	}
	if got := len(ov.remembers()); got != 0 {
		t.Errorf("remember calls = %d, want 0", got)
	}
}

func TestReadAndFactsDelegateToLocal(t *testing.T) {
	local := newTestLocal(t)
	ctx := context.Background()
	if err := local.Append(ctx, "ns", NewEntry(KindUser, "hello")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st := NewOpenVikingStore(local, &fakeOV{}, mirrorConfig(), nil)
	entries, err := st.Read(ctx, "ns")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 || entries[0].Content != "hello" {
		t.Errorf("Read = %+v, want the local entry", entries)
	}
	facts, err := st.Facts(ctx, "ns")
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("Facts = %+v, want none", facts)
	}
}
