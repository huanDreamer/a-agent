// Package memory provides short-term and long-term memory for huan-agent.
//
// Short-term memory is an in-memory buffer that keeps the most recent N
// turns of a conversation (configurable). Long-term memory is persisted as
// JSONL files (one per user/session namespace) plus an append-only list of
// extracted key facts, indexed by keyword for later recall. The whole
// package is intentionally file + memory based for the MVP; swap the
// long-term backing for a vector store later if needed.
package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind discriminates the intent of an Entry.
type Kind string

const (
	// KindUser is a user-authored message.
	KindUser Kind = "user"
	// KindAssistant is a model-authored message.
	KindAssistant Kind = "assistant"
	// KindTool is a tool invocation record (name -> result/error).
	KindTool Kind = "tool"
	// KindFact is a durable fact extracted from a conversation.
	KindFact Kind = "fact"
	// KindSummary is a roll-up of a long conversation window.
	KindSummary Kind = "summary"
)

// Entry is a single memory record persisted to a namespace file.
type Entry struct {
	Kind      Kind      `json:"kind"`
	Content   string    `json:"content"`
	Meta      string    `json:"meta,omitempty"` // optional context (tool name, etc.)
	CreatedAt time.Time `json:"created_at"`
}

// EntryOpt mutates an Entry at creation time.
type EntryOpt func(*Entry)

// WithMeta attaches a metadata string (e.g. the tool name for a tool entry).
func WithMeta(meta string) EntryOpt { return func(e *Entry) { e.Meta = meta } }

// WithTime overrides the recorded timestamp (mainly for tests).
func WithTime(t time.Time) EntryOpt { return func(e *Entry) { e.CreatedAt = t } }

// NewEntry returns an Entry with the current local time unless overridden.
func NewEntry(k Kind, content string, opts ...EntryOpt) Entry {
	e := Entry{Kind: k, Content: content, CreatedAt: time.Now()}
	for _, o := range opts {
		o(&e)
	}
	return e
}

// Store is the long-term memory persistence. Namespace isolates entries by
// user/session (e.g. "user-<id>/session-<id>").
type Store interface {
	// Append persists an entry to the namespace.
	Append(ctx context.Context, namespace string, e Entry) error
	// Read returns entries in write order, optionally filtered by kind.
	Read(ctx context.Context, namespace string, kinds ...Kind) ([]Entry, error)
	// Facts returns the durable fact entries for a namespace.
	Facts(ctx context.Context, namespace string) ([]Fact, error)
	// AddFact records a new durable fact.
	AddFact(ctx context.Context, namespace string, f Fact) error
	// SearchFacts returns facts whose keyword index matches the query.
	SearchFacts(ctx context.Context, query string, limit int) ([]Fact, error)
	Close() error
}

var (
	// ErrNotFound is returned when a namespace has no data.
	ErrNotFound = errors.New("memory: not found")
	// ErrInvalidNamespace is returned for namespaces that cannot be safe paths.
	ErrInvalidNamespace = errors.New("memory: invalid namespace")
)

// Config configures the JSONL-backed long-term store.
type Config struct {
	// Dir is the root directory where namespace files are stored.
	Dir string
}

// fileStore is the JSONL-backed Store.
type fileStore struct {
	mu   sync.RWMutex
	dir  string
	ext  string // internal facts index extension (".facts")
	root string

	// lexicalIndex maps a normalized keyword to the namespaces whose facts
	// contain it. Used by SearchFacts.
	index map[string][]string
}

// indexExtension is the suffix for the central facts index file.
const indexExtension = ".index.json"

// NewStore opens (and lazily indexes) the JSONL-backed store rooted at dir.
func NewStore(dir string) (Store, error) {
	if dir == "" {
		return nil, errors.New("memory: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: mkdir dir: %w", err)
	}
	return &fileStore{dir: filepath.Clean(dir), index: map[string][]string{}}, nil
}

// safeNS validates a namespace so it cannot escape the store root. A
// namespace must be a single path segment: no slashes, no "..", and not
// hidden (dot-prefixed).
func (s *fileStore) safeNS(ns string) (string, error) {
	if ns == "" || strings.ContainsAny(ns, `/\`) || strings.Contains(ns, "..") || strings.HasPrefix(ns, ".") {
		return "", ErrInvalidNamespace
	}
	return ns, nil
}

func (s *fileStore) pathFor(ns string) string {
	return filepath.Join(s.dir, ns+".jsonl")
}

// Append writes a single JSON line to the namespace file.
func (s *fileStore) Append(ctx context.Context, namespace string, e Entry) error {
	ns, err := s.safeNS(namespace)
	if err != nil {
		return err
	}
	p := s.pathFor(ns)
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendJSONL(ctx, p, e)
}

// Read returns the entries for a namespace, newest first. When kinds is
// non-empty only those kinds are returned.
func (s *fileStore) Read(ctx context.Context, namespace string, kinds ...Kind) ([]Entry, error) {
	ns, err := s.safeNS(namespace)
	if err != nil {
		return nil, err
	}
	p := s.pathFor(ns)
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := readJSONL[Entry](ctx, p)
	if err != nil {
		return nil, err
	}
	allow := map[Kind]bool{}
	for _, k := range kinds {
		allow[k] = true
	}
	filter := len(kinds) > 0
	out := make([]Entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if filter && !allow[entries[i].Kind] {
			continue
		}
		out = append(out, entries[i])
	}
	return out, nil
}

// Fact is a durable key-value-style fact with a keyword set for recall.
type Fact struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Keywords  []string  `json:"keywords,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (f Fact) Content() string {
	if f.Key == "" {
		return f.Value
	}
	return f.Key + ": " + f.Value
}

// Facts reads only fact entries from a namespace.
func (s *fileStore) Facts(ctx context.Context, namespace string) ([]Fact, error) {
	ns, err := s.safeNS(namespace)
	if err != nil {
		return nil, err
	}
	entries, err := s.Read(ctx, ns, KindFact)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(entries))
	for _, e := range entries {
		f, fErr := parseFact(e.Content)
		if fErr == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func parseFact(content string) (Fact, error) {
	// Minimal "key: value" parsing; fall back to whole content as value.
	if idx := strings.Index(content, ": "); idx > 0 {
		return Fact{Key: content[:idx], Value: content[idx+2:]}, nil
	}
	return Fact{Value: content}, nil
}

// AddFact persists a fact and indexes its keywords.
func (s *fileStore) AddFact(ctx context.Context, namespace string, f Fact) error {
	ns, err := s.safeNS(namespace)
	if err != nil {
		return err
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now()
	}
	kw := f.Keywords
	if len(kw) == 0 {
		kw = keywords(f.Key + " " + f.Value)
	}
	f.Keywords = kw
	e := NewEntry(KindFact, f.Content(), WithMeta("keyword="+strings.Join(kw, ",")))
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pathFor(ns)
	if err := appendRaw(ctx, p, e); err != nil {
		return err
	}
	// Update the in-memory index.
	for _, k := range kw {
		k = strings.ToLower(k)
		s.index[k] = appendIfMissing(s.index[k], ns)
	}
	return nil
}

// SearchFacts scans a query's keywords against the in-memory index and ranks
// matching namespaces by hit count; returns up to limit facts.
func (s *fileStore) SearchFacts(ctx context.Context, query string, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 10
	}
	query = strings.TrimSpace(query)
	q := keywords(query)
	if len(q) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	score := map[string]int{}
	for _, kw := range q {
		for _, ns := range s.index[kw] {
			score[ns]++
		}
	}
	// Sort namespaces by descending score.
	nss := make([]string, 0, len(score))
	for ns := range score {
		nss = append(nss, ns)
	}
	sort.Slice(nss, func(i, j int) bool { return score[nss[i]] > score[nss[j]] })
	var out []Fact
	for _, ns := range nss {
		entries, err := readFile(s.dir, ns)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Kind != KindFact {
				continue
			}
			if f, fErr := parseFact(e.Content); fErr == nil && matchesAny(f, q) {
				out = append(out, f)
				if len(out) >= limit {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// Close releases the store's resources. The JSONL backing is append-only and
// holds no open handles between calls, so Close is a no-op kept for Store
// interface completeness.
func (s *fileStore) Close() error { return nil }
