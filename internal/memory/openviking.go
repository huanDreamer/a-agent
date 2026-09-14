package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/openviking"
)

// OpenVikingClient is the slice of the OpenViking client the mirror uses. It is
// an interface so the mirror's behaviour — batching, degradation, recall
// fallback — is testable without an OpenViking server.
type OpenVikingClient interface {
	Remember(ctx context.Context, req openviking.RememberRequest) (*openviking.RememberResult, error)
	Find(ctx context.Context, req openviking.FindRequest) (*openviking.FindResult, error)
	WriteContent(ctx context.Context, req openviking.WriteRequest) (*openviking.WriteResult, error)
}

// OpenVikingMirrorConfig configures how a local store is mirrored.
type OpenVikingMirrorConfig struct {
	// SessionPrefix namespaces the OpenViking session id derived from a
	// namespace, so one huan-agent install can share a server without its
	// sessions colliding with another's.
	SessionPrefix string
	// Commit asks OpenViking to extract long-term memory from submitted turns.
	Commit bool
	// FlushEvery is how many messages are batched before submission. 0 submits
	// every turn.
	FlushEvery int
	// RecallEnable searches OpenViking semantically before falling back to the
	// local keyword index.
	RecallEnable bool
	// RecallLimit caps a semantic recall.
	RecallLimit int
	// RecallTarget scopes semantic recall to a viking:// subtree.
	RecallTarget string
	// JournalURI is the append-only, VLM-independent fact journal. When
	// JournalEnable is set it must be non-empty.
	JournalURI    string
	JournalEnable bool
}

// OpenVikingStats is what the mirror has done so far. It is exposed through the
// status API so an operator can tell "OpenViking is quietly failing" from
// "nothing has been written yet".
type OpenVikingStats struct {
	// Submitted is how many turns were successfully handed to OpenViking.
	Submitted int `json:"submitted"`
	// Facts is how many durable facts reached the fact journal.
	Facts int `json:"facts"`
	// Failures counts every degraded call (submit, extraction, recall, journal).
	Failures int `json:"failures"`
	// Pending is how many turns are still buffered locally.
	Pending int `json:"pending"`
	// LastError is the most recent failure message, empty when none.
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when that failure happened. Nil rather than the zero time,
	// so a consumer never has to special-case "year 1" in a JSON payload.
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// LastSubmitAt is when a batch last succeeded.
	LastSubmitAt *time.Time `json:"last_submit_at,omitempty"`
}

// OpenVikingStore mirrors a local Store into OpenViking.
//
// The local store remains the source of truth: every write lands there first
// and its error is the one returned. The mirror is best-effort by design — a
// chat turn must not fail because a context database is down — so remote
// failures are logged, counted and reported in Stats, never propagated.
//
// Conversation turns are batched, because each committed batch costs the server
// an LLM extraction pass; facts additionally go through a direct journal write
// (see AddFact), which is what keeps them retrievable even when the server's VLM
// model is not activated.
type OpenVikingStore struct {
	local  Store
	ov     OpenVikingClient
	cfg    OpenVikingMirrorConfig
	logger *zap.Logger

	mu      sync.Mutex
	pending []pendingTurn
	stats   OpenVikingStats
}

// pendingTurn is one buffered conversation turn plus the namespace it belongs
// to. Buffering is keyed by namespace because a batch is submitted against one
// OpenViking session, and the session id is derived from the namespace:
// interleaving two namespaces into one batch would file one conversation's
// turns under another's memory.
type pendingTurn struct {
	namespace string
	msg       openviking.Message
}

// NewOpenVikingStore wraps local with an OpenViking mirror. A nil client
// returns the local store unchanged, which is what "integration disabled"
// should mean to every caller.
func NewOpenVikingStore(local Store, ov OpenVikingClient, cfg OpenVikingMirrorConfig, logger *zap.Logger) Store {
	if ov == nil {
		return local
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.SessionPrefix == "" {
		cfg.SessionPrefix = "huan-agent"
	}
	if cfg.RecallLimit <= 0 {
		cfg.RecallLimit = 5
	}
	return &OpenVikingStore{local: local, ov: ov, cfg: cfg, logger: logger}
}

// Stats returns a snapshot of the mirror's counters.
func (s *OpenVikingStore) Stats() OpenVikingStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.Pending = len(s.pending)
	return out
}

// sessionID maps a local namespace onto a stable OpenViking session id.
func (s *OpenVikingStore) sessionID(namespace string) string {
	return s.cfg.SessionPrefix + "-" + namespace
}

// Append writes to the local store and mirrors conversation turns.
func (s *OpenVikingStore) Append(ctx context.Context, namespace string, e Entry) error {
	if err := s.local.Append(ctx, namespace, e); err != nil {
		return err
	}
	role, ok := mirrorRole(e.Kind)
	if !ok || strings.TrimSpace(e.Content) == "" {
		return nil
	}
	s.enqueue(namespace, openviking.Message{Role: role, Content: e.Content})
	return nil
}

// mirrorRole maps a memory kind onto what OpenViking's memory extractor
// accepts. Tool results, facts and summaries are deliberately not submitted as
// turns: a tool result is not something the extractor should learn from, and
// facts already have a direct path (AddFact).
func mirrorRole(k Kind) (string, bool) {
	switch k {
	case KindUser:
		return "user", true
	case KindAssistant:
		return "assistant", true
	default:
		return "", false
	}
}

// enqueue buffers a turn and submits when the batch is full.
func (s *OpenVikingStore) enqueue(namespace string, msg openviking.Message) {
	s.mu.Lock()
	s.pending = append(s.pending, pendingTurn{namespace: namespace, msg: msg})
	full := s.cfg.FlushEvery <= 0 || len(s.pending) >= s.cfg.FlushEvery
	s.mu.Unlock()
	if full {
		// The context is deliberately fresh: a chat turn that has already been
		// answered must not cancel the memory write, and a cancelled request is
		// exactly when the last turns matter most.
		s.flush(context.WithoutCancel(context.Background()), "")
	}
}

// Flush submits everything buffered for namespace. An empty namespace flushes
// every buffered turn, whatever its namespace.
//
// It never returns an error: buffered turns are dropped when submission fails
// (the local store still holds them) and the failure is recorded instead.
func (s *OpenVikingStore) Flush(ctx context.Context, namespace string) {
	s.flush(ctx, namespace)
}

func (s *OpenVikingStore) flush(ctx context.Context, namespace string) {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	batches := map[string][]openviking.Message{}
	order := make([]string, 0, 2)
	for _, turn := range s.pending {
		if namespace != "" && turn.namespace != namespace {
			continue
		}
		if _, seen := batches[turn.namespace]; !seen {
			order = append(order, turn.namespace)
		}
		batches[turn.namespace] = append(batches[turn.namespace], turn.msg)
	}
	// Keep the turns that this flush is not responsible for.
	remaining := make([]pendingTurn, 0, len(s.pending))
	for _, turn := range s.pending {
		if namespace == "" || turn.namespace == namespace {
			continue
		}
		remaining = append(remaining, turn)
	}
	s.pending = remaining
	s.mu.Unlock()

	for _, ns := range order {
		s.submit(ctx, ns, batches[ns])
	}
}

// submit sends one namespace's batch to OpenViking.
func (s *OpenVikingStore) submit(ctx context.Context, namespace string, batch []openviking.Message) {
	if len(batch) == 0 {
		return
	}
	req := openviking.RememberRequest{
		SessionID: s.sessionID(namespace),
		Messages:  batch,
		Commit:    s.cfg.Commit,
	}
	res, err := s.ov.Remember(ctx, req)
	if err != nil {
		// A commit failure still means the turns were stored, so it is worth
		// saying which half failed.
		s.recordFailure("submit memory", err)
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.stats.Submitted += len(batch)
	s.stats.LastSubmitAt = &now
	s.mu.Unlock()
	if res != nil && res.TaskID != "" {
		s.logger.Debug("openviking memory submitted",
			zap.Int("messages", len(batch)), zap.String("task_id", res.TaskID))
	}
}

// Facts delegates to the local store: the mirrored copy lives in OpenViking and
// is reached through SearchFacts, while this is the raw local truth.
func (s *OpenVikingStore) Facts(ctx context.Context, namespace string) ([]Fact, error) {
	return s.local.Facts(ctx, namespace)
}

// Read delegates to the local store.
func (s *OpenVikingStore) Read(ctx context.Context, namespace string, kinds ...Kind) ([]Entry, error) {
	return s.local.Read(ctx, namespace, kinds...)
}

// AddFact records a fact locally and mirrors it two ways.
//
// The two ways answer different failures. Submitting the fact as a turn lets
// OpenViking's extractor fold it into structured long-term memory — but that
// extractor is an LLM call, and when the server's VLM model is not available
// (a real state on a fresh install: `ModelNotOpen`) it fails. The journal is a
// plain file write, indexed by embedding only, so it works regardless. Neither
// failure can fail the local write, which is what actually has to succeed.
func (s *OpenVikingStore) AddFact(ctx context.Context, namespace string, f Fact) error {
	if err := s.local.AddFact(ctx, namespace, f); err != nil {
		return err
	}
	if s.cfg.Commit {
		s.submitFact(ctx, namespace, f)
	}
	if s.cfg.JournalEnable && s.cfg.JournalURI != "" {
		s.appendJournal(ctx, f)
	}
	return nil
}

// submitFact hands one fact to the extractor as a single-message session.
func (s *OpenVikingStore) submitFact(ctx context.Context, namespace string, f Fact) {
	req := openviking.RememberRequest{
		SessionID: s.sessionID(namespace),
		Messages:  []openviking.Message{{Role: "user", Content: f.Content()}},
		Commit:    true,
	}
	if _, err := s.ov.Remember(ctx, req); err != nil {
		s.recordFailure("submit fact", err)
	}
}

// appendJournal appends one line to the fact journal, creating it on first use.
//
// "append" is the mode that cannot lose data, so it is tried first; a missing
// file is the one failure worth retrying as a create. Any other failure is
// reported rather than retried with "replace", which would overwrite a journal
// that is the only remote copy of these facts.
func (s *OpenVikingStore) appendJournal(ctx context.Context, f Fact) {
	line := fmt.Sprintf("- %s %s\n", time.Now().UTC().Format(time.RFC3339), f.Content())
	if _, err := s.ov.WriteContent(ctx, openviking.WriteRequest{
		URI:     s.cfg.JournalURI,
		Content: line,
		Mode:    "append",
		Tags:    []string{"memory", "huan-agent"},
	}); err != nil {
		if !openviking.IsNotFound(err) {
			s.recordFailure("append fact journal", err)
			return
		}
		header := "# huan-agent fact journal\n\nFacts recorded by huan-agent. Appended in write order.\n\n"
		if _, cerr := s.ov.WriteContent(ctx, openviking.WriteRequest{
			URI:     s.cfg.JournalURI,
			Content: header + line,
			Mode:    "create",
			Tags:    []string{"memory", "huan-agent"},
		}); cerr != nil {
			s.recordFailure("create fact journal", cerr)
			return
		}
	}
	now := time.Now()
	s.mu.Lock()
	s.stats.Facts++
	s.stats.LastSubmitAt = &now
	s.mu.Unlock()
}

// SearchFacts recalls facts, preferring OpenViking's semantic search.
//
// Keyword matching is what this replaces: it cannot find a fact phrased
// differently from the query, in another language, or by synonym. When the
// semantic path is unavailable or finds nothing, the local keyword index still
// answers — a degraded recall is better than none, and neither path returns an
// error to the caller.
func (s *OpenVikingStore) SearchFacts(ctx context.Context, query string, limit int) ([]Fact, error) {
	if s.cfg.RecallEnable {
		if facts, ok := s.semanticFacts(ctx, query, limit); ok && len(facts) > 0 {
			return facts, nil
		}
	}
	return s.local.SearchFacts(ctx, query, limit)
}

// semanticFacts runs the semantic recall. The bool reports whether the result
// is usable as an answer (as opposed to "fall back to keywords").
func (s *OpenVikingStore) semanticFacts(ctx context.Context, query string, limit int) ([]Fact, bool) {
	if strings.TrimSpace(query) == "" {
		return nil, false
	}
	if limit <= 0 {
		limit = s.cfg.RecallLimit
	}
	res, err := s.ov.Find(ctx, openviking.FindRequest{
		Query:     query,
		TargetURI: s.cfg.RecallTarget,
		Limit:     limit,
	})
	if err != nil {
		s.recordFailure("semantic recall", err)
		return nil, false
	}
	hits := res.All()
	facts := make([]Fact, 0, len(hits))
	for _, h := range hits {
		text := strings.TrimSpace(h.Content)
		if text == "" {
			text = strings.TrimSpace(h.Abstract)
		}
		if text == "" {
			continue
		}
		facts = append(facts, Fact{
			// The value alone is what a reader wants; the URI is kept as a
			// keyword so a caller can trace where a fact came from.
			Value:     text,
			Keywords:  []string{h.URI},
			CreatedAt: time.Now(),
		})
		if len(facts) >= limit {
			break
		}
	}
	return facts, true
}

// Close flushes every buffered turn and closes the local store. Dropping the
// buffer on exit would lose exactly the turns that never reached a full batch.
func (s *OpenVikingStore) Close() error {
	s.flush(context.WithoutCancel(context.Background()), "")
	return s.local.Close()
}

// recordFailure counts and logs a degraded call.
func (s *OpenVikingStore) recordFailure(op string, err error) {
	unavailable := openviking.IsUnavailable(err)
	now := time.Now()
	s.mu.Lock()
	s.stats.Failures++
	s.stats.LastError = fmt.Sprintf("%s: %v", op, err)
	s.stats.LastErrorAt = &now
	s.mu.Unlock()
	s.logger.Warn("openviking memory degraded",
		zap.String("op", op),
		zap.Bool("unavailable", unavailable),
		zap.Error(err))
}
