// Package store provides SQLite-backed persistence for huan-agent.
//
// The MVP uses pure-Go SQLite (modernc.org/sqlite) to avoid CGO. Schema
// migrations are run at Open() time and tracked in `schema_migrations`.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned by queries that yield zero rows.
var ErrNotFound = errors.New("not found")

// Store is the persistence interface.
type Store interface {
	Close() error

	// Usage log accessors (Phase 1).
	RecordUsage(ctx context.Context, e UsageEvent) error
	QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error)

	// Underlying handle, used sparingly (e.g. health checks).
	DB() *sql.DB
}

// UsageEvent is the input shape for recording an LLM call.
type UsageEvent struct {
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
}

// UsageRecord is the persisted view of a usage event.
type UsageRecord struct {
	ID               int64
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
	CreatedAt        string // RFC3339 string from SQLite
}

// UsageFilter narrows QueryUsage results. Zero-value fields are ignored.
type UsageFilter struct {
	SessionID string
	Provider  string
	Limit     int // default 100, max 1000
}

// Open returns a SQLite-backed Store. The caller MUST call Close().
func Open(ctx context.Context, path string) (Store, error) {
	s := &sqliteStore{path: path}
	if err := s.dbOpen(ctx); err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// newSQLite is the internal helper to construct a *sql.DB from a DSN.
func newSQLite(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; queue readers
	return db, nil
}
